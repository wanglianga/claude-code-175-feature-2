package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5"
)

// 房东超时验房处理。
//
// 时间线(以租客退租交房为零点):
//
//	交房 0 点 ── 首次提醒(站内+短信) ── first_deadline(=交房+inspection_due_days)
//	      │ 房东在此之前打开/发起验房 -> 任务 COMPLETED, 回到正常验房流程
//	      ▼
//	首次期限已过仍未处理 -> 再次提醒 + 任务 OVERDUE
//	      ▼
//	final_deadline(=first+final_release_days) 到达:
//	      先记录门禁快照(未关闭报修/未结账单/未闭合争议/两层预留/提前退租);
//	      下一轮复核时指纹一致且全部干净 -> 自动全额释放押金给租客;
//	      快照之后任一状态发生变化 -> 暂停自动释放, 转人工复核(MANUAL_REVIEW)。
//	释放后房东只能补交超时说明与新证据(late_inspection_claims), 仅作账外登记,
//	平台即使支持也不得回流托管/不得从已释放金额扣款。

// 通知模板
const (
	tplInspectionReminder   = "inspection_reminder"          // 首次验房提醒
	tplInspectionOverdue    = "inspection_overdue_notice"    // 逾期二次提醒(房东)
	tplInspectionOverdueTen = "inspection_overdue_tenant"    // 逾期进展告知(租客)
	tplAutoReleaseScheduled = "auto_release_scheduled"       // 门禁快照已记录, 即将自动释放
	tplAutoReleaseBlocked   = "auto_release_blocked"         // 最终期限存在未决项, 转人工
	tplAutoReleaseSuspended = "auto_release_suspended"       // 释放前状态突变, 暂停转人工
	tplDepositAutoReturned  = "deposit_auto_returned"        // 押金自动退还租客
	tplLandlordAutoReleased = "landlord_inspection_released" // 房东: 验房超时, 押金已退
	tplLateClaimReceived    = "late_claim_received"          // 租客/平台: 房东补交超时说明
	tplLateClaimResolved    = "late_claim_resolved"          // 房东: 账外主张复核结果
)

// gateMetrics 是自动释放前的门禁指纹。任一字段非"干净"都不得自动放款。
type gateMetrics struct {
	OpenDisputes       int     `json:"open_disputes"`
	OpenRepairs        int     `json:"open_repairs"`
	UnsettledCharges   int     `json:"unsettled_charges"`
	UnsettledUtilities int     `json:"unsettled_utility_bills"`
	HardHold           float64 `json:"hard_hold"`
	DisputedHold       float64 `json:"disputed_hold"`
	ContractStatus     string  `json:"contract_status"`
}

// clean 返回门禁是否放行; blocker 返回首个拦截原因(供审计/通知)。
func (m gateMetrics) clean() bool { return m.blocker() == "" }

func (m gateMetrics) blocker() string {
	switch {
	case m.OpenDisputes > 0:
		return domain.BlockOpenDisputes
	case m.OpenRepairs > 0:
		return domain.BlockOpenRepairs
	case m.UnsettledCharges > 0 || m.UnsettledUtilities > 0:
		return domain.BlockUnsettledBills
	case m.ContractStatus == domain.StatusEarlyTerminated:
		return domain.BlockEarlyTermination
	case m.HardHold != 0 || m.DisputedHold != 0:
		return domain.BlockHolds
	}
	return ""
}

func (s *Service) loadGateMetrics(tx pgx.Tx, ctx context.Context, contractID string) (gateMetrics, error) {
	var m gateMetrics
	err := tx.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM disputes WHERE contract_id=$1
		     AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')),
		  (SELECT count(*) FROM repairs WHERE contract_id=$1
		     AND status IN ('SUBMITTED','RESPONSIBILITY_PENDING')),
		  (SELECT count(*) FROM charge_items WHERE contract_id=$1
		     AND status IN ('PROPOSED','CONTESTED')),
		  (SELECT count(*) FROM utility_readings u
		     WHERE u.contract_id=$1 AND u.phase='CHECKOUT' AND u.billed_amount > 0
		       AND COALESCE((SELECT utility_status FROM inspections
		                      WHERE contract_id=$1),'') <> 'SETTLED'
		       AND NOT EXISTS (
		          SELECT 1 FROM charge_items ci WHERE ci.contract_id=$1
		            AND ci.kind='UTILITY_UNPAID'
		            AND ci.status IN ('ACCEPTED','UPHELD','REJECTED','OVERTURNED'))),
		  COALESCE((SELECT hard_hold FROM escrow_accounts WHERE contract_id=$1),0),
		  COALESCE((SELECT disputed_hold FROM escrow_accounts WHERE contract_id=$1),0),
		  (SELECT status FROM contracts WHERE id=$1)`,
		contractID).Scan(&m.OpenDisputes, &m.OpenRepairs, &m.UnsettledCharges,
		&m.UnsettledUtilities, &m.HardHold, &m.DisputedHold, &m.ContractStatus)
	if err != nil {
		return m, err
	}
	m.HardHold, m.DisputedHold = round2(m.HardHold), round2(m.DisputedHold)
	return m, nil
}

// systemActor 返回定时任务使用的平台身份(seed 的平台账号; seed 关闭时回退查询)。
func (s *Service) systemActor(ctx context.Context) (domain.Actor, error) {
	s.sysMu.Lock()
	defer s.sysMu.Unlock()
	if s.sysActor.ID != "" {
		return s.sysActor, nil
	}
	var a domain.Actor
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, display_name FROM users
		 WHERE role='PLATFORM' ORDER BY created_at LIMIT 1`).Scan(&a.ID, &a.Name)
	if err != nil {
		return a, fmt.Errorf("resolve platform actor: %w", err)
	}
	a.Role = "PLATFORM"
	s.sysActor = a
	return a, nil
}

func (s *Service) systemCtx(ctx context.Context) (context.Context, error) {
	a, err := s.systemActor(ctx)
	if err != nil {
		return ctx, err
	}
	return WithActor(ctx, a), nil
}

// ---------- 退房: 建立超时验房任务 + 首次提醒 ----------

// createInspectionTask 在租客退租交房事务内调用。
func (s *Service) createInspectionTask(tx pgx.Tx, ctx context.Context, contractID, handoverID string) error {
	var c domain.Contract
	var checkoutAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT c.inspection_due_days, c.final_release_days, c.landlord_id::text, c.tenant_id::text,
		       h.occurred_at
		  FROM contracts c JOIN handovers h ON h.id=$2
		 WHERE c.id=$1`,
		contractID, handoverID).Scan(
		&c.InspectionDueDays, &c.FinalReleaseDays, &c.LandlordID, &c.TenantID, &checkoutAt); err != nil {
		return fmt.Errorf("load checkout for task: %w", err)
	}
	if c.FinalReleaseDays <= 0 {
		c.FinalReleaseDays = 7
	}
	first := checkoutAt.UTC().AddDate(0, 0, c.InspectionDueDays)
	final := first.AddDate(0, 0, c.FinalReleaseDays)

	var taskID string
	err := tx.QueryRow(ctx, `
		INSERT INTO inspection_tasks(contract_id,checkout_handover_id,status,checkout_at,
		                             first_deadline,final_deadline)
		VALUES($1,$2,'PENDING',$3,$4,$5)
		ON CONFLICT (contract_id) DO NOTHING
		RETURNING id::text`,
		contractID, handoverID, checkoutAt, first, final).Scan(&taskID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("create inspection task: %w", err)
	}
	if taskID == "" {
		// 幂等: 已存在任务(异常重复交房)直接沿用, 不重复提醒。
		if err := tx.QueryRow(ctx,
			`SELECT id::text FROM inspection_tasks WHERE contract_id=$1`, contractID).Scan(&taskID); err != nil {
			return err
		}
		return nil
	}

	vars := map[string]any{
		"first_deadline":      first.Format(time.RFC3339),
		"final_deadline":      final.Format(time.RFC3339),
		"inspection_due_days": c.InspectionDueDays,
	}
	inboxID, err := s.notifyCh(tx, ctx, contractID, c.LandlordID, tplInspectionReminder, vars, "INBOX")
	if err != nil {
		return err
	}
	smsID, err := s.notifyCh(tx, ctx, contractID, c.LandlordID, tplInspectionReminder, vars, "SMS")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE inspection_tasks SET first_reminder_inbox_id=$2, first_reminder_sms_id=$3,
		                            first_reminded_at=now()
		 WHERE id=$1`, taskID, inboxID, smsID); err != nil {
		return err
	}
	if err := s.taskEvent(tx, ctx, taskID, contractID, "CREATED", map[string]any{
		"checkout_at": checkoutAt, "first_deadline": first, "final_deadline": final,
	}); err != nil {
		return err
	}
	if err := s.taskEvent(tx, ctx, taskID, contractID, "FIRST_REMINDER", map[string]any{
		"inbox_notification_id": inboxID, "sms_notification_id": smsID,
		"channels": []string{"INBOX", "SMS"},
	}); err != nil {
		return err
	}
	return s.emit(tx, ctx, contractID, "InspectionTaskCreated", map[string]any{
		"task_id": taskID, "checkout_at": checkoutAt,
		"first_deadline": first, "final_deadline": final,
		"reminder_channels": []string{"INBOX", "SMS"},
	}, actor(ctx).ID)
}

// finishInspectionTask 在房东/平台发起验房时结束任务, 由正常验房流程接管。
// supervisor=true(平台代开) 记为 SUPERSEDED; 房东/中介主动验房记为 COMPLETED。
func (s *Service) finishInspectionTask(tx pgx.Tx, ctx context.Context, contractID string, supervisor bool) error {
	var taskID, st string
	err := tx.QueryRow(ctx,
		`SELECT id::text, status FROM inspection_tasks WHERE contract_id=$1`, contractID).
		Scan(&taskID, &st)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // 该租赁不在超时任务流程内
	}
	if err != nil {
		return err
	}
	if st == domain.InspectionTaskCompleted || st == domain.InspectionTaskSuperseded ||
		st == domain.InspectionTaskAutoReleased {
		return nil
	}
	newSt := domain.InspectionTaskCompleted
	etype := "COMPLETED"
	if supervisor {
		newSt = domain.InspectionTaskSuperseded
		etype = "SUPERSEDED"
	}
	if _, err := tx.Exec(ctx,
		`UPDATE inspection_tasks SET status=$2, updated_at=now() WHERE id=$1`, taskID, newSt); err != nil {
		return err
	}
	if err := s.taskEvent(tx, ctx, taskID, contractID, etype, map[string]any{
		"by": actor(ctx).Role,
	}); err != nil {
		return err
	}
	return s.emit(tx, ctx, contractID, "InspectionTaskClosed", map[string]any{
		"task_id": taskID, "outcome": newSt,
	}, actor(ctx).ID)
}

// ---------- 房东打开验房单 ----------

// MarkInspectionSheetOpened 房东/中介打开验房单: 记录打开时间并把发给房东的提醒置为已读。
// 幂等: 重复打开不产生新事件。
func (s *Service) MarkInspectionSheetOpened(ctx context.Context, contractID string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var taskID, st string
		err := tx.QueryRow(ctx,
			`SELECT id::text, status FROM inspection_tasks WHERE contract_id=$1`, contractID).
			Scan(&taskID, &st)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // 无超时任务(房东在提醒发出前/正常流程中打开), 无需记录
		}
		if err != nil {
			return err
		}
		var already bool
		if err := tx.QueryRow(ctx, `SELECT opened_at IS NOT NULL FROM inspection_tasks WHERE id=$1`,
			taskID).Scan(&already); err != nil {
			return err
		}
		// 任务已终结(已验房/已代开/已释放)后再打开, 不再写任何状态与事件。
		if st != domain.InspectionTaskPending && st != domain.InspectionTaskOpened &&
			st != domain.InspectionTaskOverdue && st != domain.InspectionTaskManualReview {
			return nil
		}
		// 首次打开: PENDING -> OPENED; 已逾期则保留 OVERDUE(逾期事实不变), 仅记录打开时间。
		if st == domain.InspectionTaskPending {
			if _, err := tx.Exec(ctx,
				`UPDATE inspection_tasks SET status='OPENED', opened_at=now(), updated_at=now() WHERE id=$1`,
				taskID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx,
				`UPDATE inspection_tasks SET opened_at=COALESCE(opened_at,now()), updated_at=now() WHERE id=$1`,
				taskID); err != nil {
				return err
			}
		}
		// 房东侧未读提醒全部置已打开。
		if _, err := tx.Exec(ctx, `
			UPDATE notifications SET opened_at=COALESCE(opened_at,now())
			 WHERE contract_id=$1 AND user_id=(SELECT landlord_id FROM contracts WHERE id=$1)
			   AND opened_at IS NULL`, contractID); err != nil {
			return err
		}
		if already {
			return nil
		}
		if err := s.taskEvent(tx, ctx, taskID, contractID, "SHEET_OPENED", map[string]any{
			"task_status": st,
		}); err != nil {
			return err
		}
		return s.emit(tx, ctx, contractID, "InspectionSheetOpened", map[string]any{
			"task_id": taskID,
		}, actor(ctx).ID)
	})
}

// ---------- 定时扫描 1: 超过首次期限 -> 二次提醒 + 逾期 ----------

// OverdueScanReport 汇总一次巡检动作。
type OverdueScanReport struct {
	Reminded       []string          `json:"reminded"`        // 本轮二次提醒并标逾期的合同
	AutoReleased   []string          `json:"auto_released"`   // 本轮自动释放的合同
	SnapshotsTaken []string          `json:"snapshots_taken"` // 本轮记录门禁快照、等待复核释放
	ManualReview   []string          `json:"manual_review"`   // 本轮转人工的合同
	BlockedReasons map[string]string `json:"blocked_reasons,omitempty"`
}

// RunOverdueReminderScan 平台定时任务: 首次期限已过、房东仍未发起验房 -> 再次提醒并标逾期。
func (s *Service) RunOverdueReminderScan(ctx context.Context) ([]string, error) {
	sctx, err := s.systemCtx(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(sctx, `
		SELECT contract_id::text FROM inspection_tasks
		 WHERE status IN ('PENDING','OPENED') AND first_deadline < now()
		   AND second_reminded_at IS NULL`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	var reminded []string
	for _, id := range ids {
		if err := s.markOverdue(sctx, id); err != nil {
			return reminded, err
		}
		reminded = append(reminded, id)
	}
	return reminded, nil
}

func (s *Service) markOverdue(ctx context.Context, contractID string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		c, err := s.getContract(tx, ctx, contractID)
		if err != nil {
			return err
		}
		var taskID string
		if err := tx.QueryRow(ctx, `
			SELECT id::text FROM inspection_tasks
			 WHERE contract_id=$1 AND status IN ('PENDING','OPENED')
			   AND first_deadline < now() AND second_reminded_at IS NULL
			 FOR UPDATE`, contractID).Scan(&taskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // 已被其他 worker 处理
			}
			return err
		}
		var finalDeadline time.Time
		if err := tx.QueryRow(ctx, `SELECT final_deadline FROM inspection_tasks WHERE id=$1`,
			taskID).Scan(&finalDeadline); err != nil {
			return err
		}
		// 二次提醒(站内 + 短信), 明确告知最终期限与自动释放后果。
		vars := map[string]any{
			"final_deadline": finalDeadline.Format(time.RFC3339),
			"consequence":    "到期仍未验房且无未决报修/账单/争议时, 平台将自动全额退还押金",
		}
		inboxID, err := s.notifyCh(tx, ctx, contractID, c.LandlordID, tplInspectionOverdue, vars, "INBOX")
		if err != nil {
			return err
		}
		smsID, err := s.notifyCh(tx, ctx, contractID, c.LandlordID, tplInspectionOverdue, vars, "SMS")
		if err != nil {
			return err
		}
		// 同步告知租客进展。
		if _, err := s.notifyCh(tx, ctx, contractID, c.TenantID, tplInspectionOverdueTen, map[string]any{
			"final_deadline": finalDeadline.Format(time.RFC3339),
		}, "INBOX"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE inspection_tasks SET status='OVERDUE', overdue_at=now(), updated_at=now(),
			       second_reminder_inbox_id=$2, second_reminder_sms_id=$3, second_reminded_at=now()
			 WHERE id=$1`, taskID, inboxID, smsID); err != nil {
			return err
		}
		if err := s.taskEvent(tx, ctx, taskID, contractID, "SECOND_REMINDER", map[string]any{
			"inbox_notification_id": inboxID, "sms_notification_id": smsID,
		}); err != nil {
			return err
		}
		if err := s.taskEvent(tx, ctx, taskID, contractID, "OVERDUE_MARKED", map[string]any{
			"final_deadline": finalDeadline,
		}); err != nil {
			return err
		}
		return s.emit(tx, ctx, contractID, "InspectionOverdueMarked", map[string]any{
			"task_id": taskID, "final_deadline": finalDeadline,
		}, actor(ctx).ID)
	})
}

// ---------- 定时扫描 2: 最终期限门禁 + 自动释放 / 转人工 ----------

// RunFinalReleaseScan 平台定时任务: 最终期限到达后, 先确认未关闭报修、未结账单、
// 未闭合争议等; 干净才释放。第一次先留门禁快照, 下一轮复核快照未变化才真正放款,
// 以此捕获"释放前一刻状态刚好变化"。
func (s *Service) RunFinalReleaseScan(ctx context.Context) (*OverdueScanReport, error) {
	sctx, err := s.systemCtx(ctx)
	if err != nil {
		return nil, err
	}
	rep := &OverdueScanReport{BlockedReasons: map[string]string{}}
	rows, err := s.pool.Query(sctx, `
		SELECT contract_id::text FROM inspection_tasks
		 WHERE status='OVERDUE' AND final_deadline < now()
		 ORDER BY final_deadline`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		outcome, reason, err := s.evaluateFinalDeadline(sctx, id)
		if err != nil {
			return rep, err
		}
		switch outcome {
		case "snapshot":
			rep.SnapshotsTaken = append(rep.SnapshotsTaken, id)
		case "released":
			rep.AutoReleased = append(rep.AutoReleased, id)
		case "manual":
			rep.ManualReview = append(rep.ManualReview, id)
			rep.BlockedReasons[id] = reason
		}
	}
	return rep, nil
}

// evaluateFinalDeadline 在合同粒度排他事务内处理一单最终期限任务。
// 返回 outcome: snapshot(已留快照, 下轮复核) / released(已自动释放) / manual(转人工)。
func (s *Service) evaluateFinalDeadline(ctx context.Context, contractID string) (outcome, reason string, err error) {
	err = s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		c, err := s.getContract(tx, ctx, contractID)
		if err != nil {
			return err
		}
		var taskID string
		var snapshot []byte
		if err := tx.QueryRow(ctx, `
			SELECT id::text, COALESCE(gate_snapshot,'null'::jsonb)
			  FROM inspection_tasks
			 WHERE contract_id=$1 AND status='OVERDUE' AND final_deadline < now()
			 FOR UPDATE`, contractID).Scan(&taskID, &snapshot); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // 状态已被其他流程改变(房东已验房等), 跳过
			}
			return err
		}
		metrics, err := s.loadGateMetrics(tx, ctx, contractID)
		if err != nil {
			return err
		}
		fingerprint, _ := json.Marshal(metrics)

		hasSnapshot := string(snapshot) != "null"
		if !hasSnapshot {
			// 第一轮: 最终期限到达, 先留下门禁快照。
			if b := metrics.blocker(); b != "" {
				// 期限到了但存在未决项 -> 直接转人工, 不能等到"干净"才放款也不能自动放款。
				if err := s.suspendToManual(tx, ctx, taskID, contractID, b, metrics,
					"GATE_BLOCKED", tplAutoReleaseBlocked,
					"最终期限存在未决事项: "+b); err != nil {
					return err
				}
				outcome, reason = "manual", b
				return nil
			}
			if _, err := tx.Exec(ctx,
				`UPDATE inspection_tasks SET gate_snapshot=$2, updated_at=now() WHERE id=$1`,
				taskID, string(fingerprint)); err != nil {
				return err
			}
			if err := s.taskEvent(tx, ctx, taskID, contractID, "GATE_SNAPSHOT", map[string]any{
				"passed": true, "metrics": metrics,
			}); err != nil {
				return err
			}
			_ = s.notify(tx, ctx, contractID, c.TenantID, tplAutoReleaseScheduled, map[string]any{
				"metrics": metrics,
			})
			if err := s.emit(tx, ctx, contractID, "AutoReleaseScheduled", map[string]any{
				"task_id": taskID, "gate_snapshot": metrics,
			}, actor(ctx).ID); err != nil {
				return err
			}
			outcome = "snapshot"
			return nil
		}

		// 第二轮: 真正释放前复核。快照之后报修/账单/争议/资金任一发生变化 -> 暂停转人工。
		// 注意: 快照以 jsonb 存储, PG 会规范化表示格式, 必须反序列化后做结构比较, 不能比对原始字符串。
		var prior gateMetrics
		if err := json.Unmarshal(snapshot, &prior); err != nil {
			return err
		}
		if !reflect.DeepEqual(prior, metrics) {
			detail := map[string]any{"before": prior, "after": metrics}
			if err := s.suspendToManual(tx, ctx, taskID, contractID, domain.BlockStateChanged, metrics,
				"STATE_CHANGED_SUSPEND", tplAutoReleaseSuspended,
				"释放前门禁状态发生变化, 暂停自动释放"); err != nil {
				return err
			}
			if err := s.taskEvent(tx, ctx, taskID, contractID, "STATE_CHANGED_SUSPEND", detail); err != nil {
				return err
			}
			outcome, reason = "manual", domain.BlockStateChanged
			return nil
		}
		if b := metrics.blocker(); b != "" {
			if err := s.suspendToManual(tx, ctx, taskID, contractID, b, metrics,
				"GATE_BLOCKED", tplAutoReleaseBlocked,
				"复核时发现未决事项: "+b); err != nil {
				return err
			}
			outcome, reason = "manual", b
			return nil
		}
		// 指纹一致且全部干净 -> 自动全额释放。
		sid, err := s.executeOverdueRelease(tx, ctx, contractID, "OVERDUE_AUTO",
			"房东超时未验房, 最终期限门禁通过, 平台自动全额退还押金")
		if err != nil {
			return mapPgGuardError(err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE inspection_tasks SET status='AUTO_RELEASED', auto_released_at=now(),
			       release_settlement_id=$2, updated_at=now()
			 WHERE id=$1`, taskID, sid); err != nil {
			return err
		}
		if err := s.taskEvent(tx, ctx, taskID, contractID, "AUTO_RELEASED", map[string]any{
			"settlement_id": sid, "gate_snapshot": metrics,
		}); err != nil {
			return err
		}
		outcome = "released"
		return nil
	})
	return outcome, reason, err
}

// suspendToManual 把任务转人工复核并通知双方。
func (s *Service) suspendToManual(tx pgx.Tx, ctx context.Context, taskID, contractID, blocker string,
	metrics gateMetrics, taskEventType, tpl, note string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE inspection_tasks SET status='MANUAL_REVIEW', blocked_reason=$2,
		       gate_snapshot=$3, review_outcome='NONE', updated_at=now()
		 WHERE id=$1`, taskID, blocker, mustJSON(metrics)); err != nil {
		return err
	}
	if err := s.taskEvent(tx, ctx, taskID, contractID, taskEventType, map[string]any{
		"blocked_reason": blocker, "metrics": metrics, "note": note,
	}); err != nil {
		return err
	}
	c, _ := s.getContract(tx, ctx, contractID)
	_ = s.notify(tx, ctx, contractID, c.LandlordID, tpl, map[string]any{
		"blocked_reason": blocker, "note": note,
	})
	_ = s.notify(tx, ctx, contractID, c.TenantID, tpl, map[string]any{
		"blocked_reason": blocker, "note": note,
	})
	if err := s.emit(tx, ctx, contractID, "AutoReleaseSuspended", map[string]any{
		"task_id": taskID, "blocked_reason": blocker, "metrics": metrics,
	}, actor(ctx).ID); err != nil {
		return err
	}
	return s.taskEvent(tx, ctx, taskID, contractID, "MANUAL_REVIEW", map[string]any{
		"blocked_reason": blocker,
	})
}

// executeOverdueRelease 执行超时自动/人工全额释放: 无扣款, 全部余额退还租客。
// 调用方必须已通过 gateMetrics 门禁; settlement_guard 触发器会在 CONFIRMED 时再次校验。
func (s *Service) executeOverdueRelease(tx pgx.Tx, ctx context.Context, contractID, mode, note string) (string, error) {
	var balance float64
	if err := tx.QueryRow(ctx,
		`SELECT balance FROM escrow_accounts WHERE contract_id=$1 FOR UPDATE`,
		contractID).Scan(&balance); err != nil {
		return "", err
	}
	balance = round2(balance)
	plat := actor(ctx).ID
	var sid string
	if err := tx.QueryRow(ctx, `
		INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,
		                        paid_to_landlord,status,release_mode,note,
		                        tenant_confirmed_by,landlord_confirmed_by,
		                        tenant_confirmed_at,landlord_confirmed_at)
		VALUES($1,$2,0,$2,0,'PROPOSED',$3,$4,$5,$5,now(),now())
		RETURNING id::text`,
		contractID, balance, mode, note, plat).Scan(&sid); err != nil {
		return "", fmt.Errorf("overdue settlement: %w", err)
	}
	// DB 闸门: 无未闭合争议 + 无未关闭报修 + 无未结账单 + 无任何预留 + 非提前退租未处理。
	if _, err := tx.Exec(ctx, `UPDATE settlements SET status='CONFIRMED' WHERE id=$1`, sid); err != nil {
		return "", err
	}
	if balance > 0 {
		if err := s.ledger(tx, ctx, contractID, "RELEASE_TO_TENANT", balance, -balance, 0, 0,
			"settlement", sid, note, ""); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE escrow_accounts SET released_total = released_total + $2 WHERE contract_id=$1`,
		contractID, balance); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE settlements SET status='EXECUTED', executed_at=now() WHERE id=$1`, sid); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE contracts SET status='RELEASED' WHERE id=$1`, contractID); err != nil {
		return "", err
	}
	c, _ := s.getContract(tx, ctx, contractID)
	_ = s.notify(tx, ctx, contractID, c.TenantID, tplDepositAutoReturned, map[string]any{
		"amount": balance, "release_mode": mode,
	})
	_ = s.notify(tx, ctx, contractID, c.LandlordID, tplLandlordAutoReleased, map[string]any{
		"amount": balance, "release_mode": mode,
		"note": "押金已全额退还租客; 如有新证据可在复核期内提交超时说明",
	})
	if err := s.emit(tx, ctx, contractID, "DepositReleased", map[string]any{
		"settlement_id": sid, "to_tenant": balance, "to_landlord": 0,
		"release_mode": mode,
	}, actor(ctx).ID); err != nil {
		return "", err
	}
	return sid, nil
}

// ---------- 人工复核 ----------

// ResolveManualReviewInput 平台对转人工任务的处理决定。
type ResolveManualReviewInput struct {
	Outcome string `json:"outcome"` // RELEASE 复核通过后释放 / RETAIN 暂不释放, 继续人工跟进 / REINSPECT 平台代开验房
	Note    string `json:"note"`
}

// ResolveManualReview 平台处理 MANUAL_REVIEW 任务。
func (s *Service) ResolveManualReview(ctx context.Context, contractID string, in ResolveManualReviewInput) (map[string]any, error) {
	res := map[string]any{"outcome": in.Outcome}
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if actor(ctx).Role != "PLATFORM" {
			return fmt.Errorf("%w: platform only", ErrForbidden)
		}
		var taskID string
		if err := tx.QueryRow(ctx, `
			SELECT id::text FROM inspection_tasks
			 WHERE contract_id=$1 AND status='MANUAL_REVIEW' FOR UPDATE`,
			contractID).Scan(&taskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 没有待人工复核的超时验房任务", ErrBadState)
			}
			return err
		}
		switch in.Outcome {
		case "RELEASE":
			metrics, err := s.loadGateMetrics(tx, ctx, contractID)
			if err != nil {
				return err
			}
			if b := metrics.blocker(); b != "" {
				return fmt.Errorf("%w: 门禁仍未通过: %s (%v)", ErrUnresolved, b, metrics)
			}
			sid, err := s.executeOverdueRelease(tx, ctx, contractID, "OVERDUE_MANUAL",
				"人工复核门禁通过, 平台释放押金: "+in.Note)
			if err != nil {
				return mapPgGuardError(err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE inspection_tasks SET status='AUTO_RELEASED', auto_released_at=now(),
				       release_settlement_id=$2, review_outcome='RELEASED',
				       review_note=$3, reviewed_by=$4, reviewed_at=now(), updated_at=now()
				 WHERE id=$1`, taskID, sid, in.Note, actor(ctx).ID); err != nil {
				return err
			}
			if err := s.taskEvent(tx, ctx, taskID, contractID, "REVIEW_RESOLVED", map[string]any{
				"outcome": "RELEASED", "settlement_id": sid, "note": in.Note,
			}); err != nil {
				return err
			}
			res["settlement_id"] = sid

		case "RETAIN":
			// 暂不释放: 记录人工决定, 任务保持 MANUAL_REVIEW, 等待争议/报修/账单闭合后再释放。
			if _, err := tx.Exec(ctx, `
				UPDATE inspection_tasks SET review_outcome='RETAINED', review_note=$2,
				       reviewed_by=$3, reviewed_at=now(), updated_at=now()
				 WHERE id=$1`, taskID, in.Note, actor(ctx).ID); err != nil {
				return err
			}
			if err := s.taskEvent(tx, ctx, taskID, contractID, "REVIEW_RESOLVED", map[string]any{
				"outcome": "RETAINED", "note": in.Note,
			}); err != nil {
				return err
			}

		case "REINSPECT":
			// 平台代开验房, 转正常验房/争议/结算流程。
			if _, err := tx.Exec(ctx, `
				UPDATE inspection_tasks SET status='SUPERSEDED', review_outcome='REINSPECT',
				       review_note=$2, reviewed_by=$3, reviewed_at=now(), updated_at=now()
				 WHERE id=$1`, taskID, in.Note, actor(ctx).ID); err != nil {
				return err
			}
			if err := s.taskEvent(tx, ctx, taskID, contractID, "REVIEW_RESOLVED", map[string]any{
				"outcome": "REINSPECT", "note": in.Note,
			}); err != nil {
				return err
			}
			// 合同可能停在 CHECKOUT_PENDING/EARLY_TERMINATED, 复用既有代开逻辑(同事务)。
			if err := s.openInspectionSupervised(tx, ctx, contractID); err != nil {
				return err
			}

		default:
			return fmt.Errorf("%w: outcome must be RELEASE|RETAIN|REINSPECT", ErrValidation)
		}
		return s.emit(tx, ctx, contractID, "ManualReviewResolved", map[string]any{
			"task_id": taskID, "outcome": in.Outcome, "note": in.Note,
		}, actor(ctx).ID)
	})
	return res, err
}

// openInspectionSupervised 在已有事务内由平台代开验房(人工复核 REINSPECT 复用)。
func (s *Service) openInspectionSupervised(tx pgx.Tx, ctx context.Context, contractID string) error {
	c, err := s.getContract(tx, ctx, contractID)
	if err != nil {
		return err
	}
	deadline := nowUTC().AddDate(0, 0, c.InspectionDueDays)
	var inspID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO inspections(contract_id,initiated_by,status,checkout_handover_id,
		                        inspection_deadline,early_termination)
		SELECT $1,$2,'OPEN',h.id,$3,($4='EARLY_TERMINATED') FROM handovers h
		 WHERE h.contract_id=$1 AND h.phase='CHECKOUT'
		RETURNING id::text`,
		contractID, actor(ctx).ID, deadline, c.Status).Scan(&inspID); err != nil {
		return fmt.Errorf("supervised open inspection: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE contracts SET status='INSPECTING' WHERE id=$1`, contractID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE disputes SET status='RESOLVED_AGREEMENT', resolved_at=now(),
		                    resolution_note='平台人工复核代开验房, 逾期争议闭合', award_to_landlord=0
		WHERE contract_id=$1 AND reason='INSPECTION_DELAY'
		  AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')`, contractID); err != nil {
		return err
	}
	_ = s.notify(tx, ctx, contractID, c.TenantID, "inspection_started",
		map[string]any{"deadline": deadline.Format(time.RFC3339), "by": "PLATFORM"})
	return s.emit(tx, ctx, contractID, "InspectionStarted", map[string]any{
		"inspection_id": inspID, "deadline": deadline, "by": "PLATFORM",
	}, actor(ctx).ID)
}

// ---------- 释放后: 房东只能补交超时说明与新证据(账外主张) ----------

type LateClaimInput struct {
	Explanation   string   `json:"explanation"`
	EvidenceIDs   []string `json:"evidence_ids"`
	ClaimedAmount float64  `json:"claimed_amount"`
}

// SubmitLateInspectionClaim 房东在押金自动释放后补交超时说明与新证据。
// 仅作账外登记: 不碰 escrow、不写资金台账、不产生扣款项; 即使平台支持也只能另行追讨。
func (s *Service) SubmitLateInspectionClaim(ctx context.Context, contractID string, in LateClaimInput) (string, error) {
	if in.Explanation == "" {
		return "", fmt.Errorf("%w: explanation required", ErrValidation)
	}
	if in.ClaimedAmount < 0 {
		return "", fmt.Errorf("%w: claimed_amount must be >= 0", ErrValidation)
	}
	var claimID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT"); err != nil {
			return err
		}
		var st string
		var taskID string
		if err := tx.QueryRow(ctx, `
			SELECT c.status, t.id::text FROM contracts c
			 JOIN inspection_tasks t ON t.contract_id=c.id
			 WHERE c.id=$1`, contractID).Scan(&st, &taskID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: 仅在超时验房流程已结束(押金已释放)后可补交说明", ErrBadState)
			}
			return err
		}
		if st != domain.StatusReleased {
			return fmt.Errorf("%w: 合同状态 %s, 押金释放后才能补交超时说明", ErrBadState, st)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO late_inspection_claims(contract_id,task_id,explanation,evidence_ids,
			                                   claimed_amount,status,submitted_by)
			VALUES($1,$2,$3,$4::uuid[],$5,'SUBMITTED',$6) RETURNING id::text`,
			contractID, taskID, in.Explanation, pgUUIDArrayLiteral(in.EvidenceIDs),
			round2(in.ClaimedAmount), actor(ctx).ID).Scan(&claimID); err != nil {
			return fmt.Errorf("insert late claim: %w", err)
		}
		if err := s.taskEvent(tx, ctx, taskID, contractID, "LATE_CLAIM_FILED", map[string]any{
			"claim_id": claimID, "claimed_amount": round2(in.ClaimedAmount),
			"evidence_count": len(in.EvidenceIDs),
		}); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.TenantID, tplLateClaimReceived, map[string]any{
			"claim_id": claimID, "claimed_amount": round2(in.ClaimedAmount),
			"note": "该主张为账外登记, 不影响已释放押金",
		})
		return s.emit(tx, ctx, contractID, "LateInspectionClaimFiled", map[string]any{
			"claim_id": claimID, "claimed_amount": round2(in.ClaimedAmount),
			"evidence_count": len(in.EvidenceIDs),
		}, actor(ctx).ID)
	})
	return claimID, err
}

// ReviewLateClaim 平台复核房东账外主张。award 只表示"支持另行追讨的金额",
// 不会回流 escrow、不会从已释放金额扣款。
func (s *Service) ReviewLateClaim(ctx context.Context, contractID, claimID string,
	upheld bool, award float64, note string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if actor(ctx).Role != "PLATFORM" {
			return fmt.Errorf("%w: platform only", ErrForbidden)
		}
		var taskID string
		var claimed float64
		if err := tx.QueryRow(ctx, `
			SELECT task_id::text, claimed_amount FROM late_inspection_claims
			 WHERE id=$1 AND contract_id=$2 AND status IN ('SUBMITTED','REVIEWING')`,
			claimID, contractID).Scan(&taskID, &claimed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: late claim not found or already resolved", ErrBadState)
			}
			return err
		}
		status := domain.LateClaimResolvedReject
		if upheld {
			status = domain.LateClaimResolvedAward
		} else {
			award = 0
		}
		award = round2(award)
		if award > claimed+0.001 {
			return fmt.Errorf("%w: award %.2f 超过主张金额 %.2f", ErrValidation, award, claimed)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE late_inspection_claims SET status=$3, award_amount=$4, decision_note=$5,
			                                   decided_by=$6, decided_at=now()
			 WHERE id=$1 AND contract_id=$2`, claimID, contractID, status, award, note, actor(ctx).ID); err != nil {
			return err
		}
		if err := s.taskEvent(tx, ctx, taskID, contractID, "LATE_CLAIM_RESOLVED", map[string]any{
			"claim_id": claimID, "status": status, "award_amount": award,
			"note": "账外主张, 不回流托管/不从已释放金额扣款",
		}); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, tplLateClaimResolved, map[string]any{
			"claim_id": claimID, "status": status, "award_amount": award,
			"note": "如需追讨请凭复核结论另行主张, 平台不会直接划扣已释放押金",
		})
		return s.emit(tx, ctx, contractID, "LateInspectionClaimResolved", map[string]any{
			"claim_id": claimID, "status": status, "award_amount": award,
		}, actor(ctx).ID)
	})
}

// ---------- 通知送达 / 查询 ----------

// MarkNotificationDelivery 供短信/推送网关回执回调(平台侧)更新送达状态。
func (s *Service) MarkNotificationDelivery(ctx context.Context, notificationID int64, delivered bool, reason string) error {
	sctx, err := s.systemCtx(ctx)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(sctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(sctx)
	var contractID, template string
	var userID string
	if err := tx.QueryRow(sctx, `
		SELECT COALESCE(contract_id::text,''), template, user_id::text FROM notifications WHERE id=$1`,
		notificationID).Scan(&contractID, &template, &userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	status := "DELIVERED"
	if !delivered {
		status = "FAILED"
	}
	ct, err := tx.Exec(sctx, `
		UPDATE notifications SET delivery_status=$2,
		       delivered_at=CASE WHEN $2='DELIVERED' THEN now() ELSE delivered_at END,
		       failure_reason=$3
		 WHERE id=$1 AND delivery_status IN ('PENDING','SENT')`,
		notificationID, status, reason)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return nil // 已终态, 回执重复, 幂等忽略
	}
	if contractID != "" {
		if err := s.runTaskEventForNotification(tx, sctx, contractID, notificationID, template, delivered); err != nil {
			return err
		}
	}
	return tx.Commit(sctx)
}

func (s *Service) runTaskEventForNotification(tx pgx.Tx, ctx context.Context,
	contractID string, notificationID int64, template string, delivered bool) error {
	var taskID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM inspection_tasks WHERE contract_id=$1`,
		contractID).Scan(&taskID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if template != tplInspectionReminder && template != tplInspectionOverdue {
		return nil
	}
	return s.taskEvent(tx, ctx, taskID, contractID, "REMINDER_DELIVERED", map[string]any{
		"notification_id": notificationID, "template": template, "delivered": delivered,
	})
}

func (s *Service) taskEvent(tx pgx.Tx, ctx context.Context, taskID, contractID, etype string,
	detail any) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO inspection_task_events(task_id,contract_id,event_type,detail,actor_id)
		VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid)`,
		taskID, contractID, etype, mustJSON(detail), func() string {
			a := actor(ctx)
			return a.ID
		}())
	return err
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
