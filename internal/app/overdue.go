package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5"
)

// 房东超时验房处理。
//
// 时间线(锚定退租交房 handovers.occurred_at):
//
//	交房 -> FIRST 提醒 + 自动释放任务(PENDING)
//	  超过首次期限(inspection_due_days)房东未开验房单 -> OVERDUE 再提醒, 任务逾期, 合同 REMINDING
//	  到达最终期限(final_release_days) -> 校验未关闭报修/未结账单/未闭合争议,
//	     并对释放前最后一刻的状态指纹做二次比对: 全部满足才自动全额释放押金;
//	     任一不满足或指纹变化 -> BLOCKED/REVIEW_HUMAN, 合同 RELEASE_REVIEW, 转人工复核。
//	押金释放后房东只能提交超时说明与新证据(post_release_claims), 系统不会自动从已释放金额扣款。

// 增加一个业务错误, 便于 handler 映射。
var ErrManualReview = errors.New("MANUAL_REVIEW")

// SweepResult 是一次平台巡检的处理结果。
type SweepResult struct {
	Overdue      []string `json:"overdue"`       // 本次标记逾期并二次提醒
	AutoReleased []string `json:"auto_released"` // 本次自动释放押金
	Paused       []string `json:"paused"`        // 本次暂停转人工
	Cancelled    []string `json:"cancelled"`     // 房东已自行处理, 任务取消
}

type deadlines struct {
	checkoutAt time.Time
	first      time.Time
	final      time.Time
	dueDays    int
	finalDays  int
}

func (s *Service) loadDeadlines(tx pgx.Tx, ctx context.Context, contractID string) (deadlines, error) {
	var d deadlines
	err := tx.QueryRow(ctx, `
		SELECT h.occurred_at, c.inspection_due_days, c.final_release_days,
		       h.occurred_at + make_interval(days => c.inspection_due_days),
		       h.occurred_at + make_interval(days => c.final_release_days)
		FROM contracts c
		JOIN handovers h ON h.contract_id=c.id AND h.phase='CHECKOUT'
		WHERE c.id=$1`, contractID).Scan(&d.checkoutAt, &d.dueDays, &d.finalDays, &d.first, &d.final)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fmt.Errorf("%w: 尚未退租交房", ErrBadState)
	}
	return d, err
}

// sendReminder 在同一事务内写 notifications + inspection_reminders 并发出事件。
// 演示环境投递渠道为站内信, 写入即视为送达(delivered=true)。
func (s *Service) sendReminder(tx pgx.Tx, ctx context.Context, contractID, kind, template string,
	vars map[string]any, d deadlines) error {
	c, err := s.getContract(tx, ctx, contractID)
	if err != nil {
		return err
	}
	if vars == nil {
		vars = map[string]any{}
	}
	vars["first_deadline"] = d.first.Format(time.RFC3339)
	vars["final_deadline"] = d.final.Format(time.RFC3339)
	var notifID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO notifications(contract_id,user_id,template,vars)
		 VALUES($1,$2,$3,$4) RETURNING id`,
		contractID, c.LandlordID, template, vars).Scan(&notifID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO inspection_reminders(contract_id,kind,channel,notification_id,
		                                 delivered,delivered_at,first_deadline,final_deadline)
		VALUES($1,$2,'INBOX',$3,true,now(),$4,$5)`,
		contractID, kind, notifID, d.first, d.final); err != nil {
		return err
	}
	return s.emit(tx, ctx, contractID, "InspectionReminderSent", map[string]any{
		"kind": kind, "template": template, "notification_id": notifID,
		"first_deadline": d.first, "final_deadline": d.final,
		"delivered": true,
	}, actor(ctx).ID)
}

// createReminderAndJob 在退租交房时发送首次提醒并建立自动释放任务。
func (s *Service) createReminderAndJob(tx pgx.Tx, ctx context.Context, contractID, kind, template string,
	vars map[string]any) error {
	d, err := s.loadDeadlines(tx, ctx, contractID)
	if err != nil {
		return err
	}
	if err := s.sendReminder(tx, ctx, contractID, kind, template, vars, d); err != nil {
		return err
	}
	if kind == domain.ReminderFirst {
		_, err = tx.Exec(ctx, `
			INSERT INTO auto_release_jobs(contract_id,status,first_deadline,final_deadline,reminded_first_at)
			VALUES($1,'PENDING',$2,$3,now())
			ON CONFLICT (contract_id) DO NOTHING`, contractID, d.first, d.final)
		return err
	}
	return nil
}

// MarkInspectionOpened 记录房东打开验房单(幂等: 只更新尚未打开的提醒)。
func (s *Service) MarkInspectionOpened(ctx context.Context, contractID string) (int64, error) {
	var opened int64
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
			UPDATE inspection_reminders SET opened=true, opened_at=now()
			WHERE contract_id=$1 AND opened=false`, contractID)
		if err != nil {
			return err
		}
		opened = ct.RowsAffected()
		if opened == 0 {
			return nil
		}
		return s.emit(tx, ctx, contractID, "InspectionSheetOpened", map[string]any{
			"newly_opened": opened,
		}, actor(ctx).ID)
	})
	return opened, err
}

// ---------- 平台定时巡检 ----------

// SweepOverdueInspections 平台定时任务:
// 1) 首次期限已过仍未验房 -> 二次提醒并标记逾期(合同 REMINDING);
// 2) 最终期限已到 -> 三检查 + 状态指纹, 自动释放或转人工。
// executeReady=false 时只评估到 READY(释放前挂起), 供运营预检/测试在评估与执行之间制造状态变化。
func (s *Service) SweepOverdueInspections(ctx context.Context, executeReady bool) (*SweepResult, error) {
	if actor(ctx).Role != "PLATFORM" {
		return nil, fmt.Errorf("%w: platform only", ErrForbidden)
	}
	res := &SweepResult{}

	// 阶段 1: 超过首次期限仍未开验房单。
	// DISPUTING(如租客已就逾期申诉)不在此处理, 避免覆盖争议状态; 由既有代开验房巡检负责。
	overdueRows, err := s.pool.Query(ctx, `
		SELECT j.contract_id::text
		FROM auto_release_jobs j
		JOIN contracts c ON c.id=j.contract_id
		WHERE j.status='PENDING' AND j.first_deadline < now()
		  AND c.status IN ('CHECKOUT_PENDING','EARLY_TERMINATED','REMINDING')
		  AND NOT EXISTS (SELECT 1 FROM inspections i WHERE i.contract_id=j.contract_id)
		ORDER BY j.first_deadline
		FOR UPDATE OF j SKIP LOCKED`)
	if err != nil {
		return nil, err
	}
	var phase1 []string
	for overdueRows.Next() {
		var id string
		if err := overdueRows.Scan(&id); err != nil {
			overdueRows.Close()
			return res, err
		}
		phase1 = append(phase1, id)
	}
	overdueRows.Close()
	for _, id := range phase1 {
		if err := s.markOverdue(ctx, id); err != nil {
			return res, err
		}
		res.Overdue = append(res.Overdue, id)
	}

	// 阶段 2a: 最终期限评估(OVERDUE/BLOCKED -> READY 或继续 BLOCKED)。
	// READY 表示三检查已通过, 不再重复评估; 状态变化的最终防线在阶段 2b 的执行前指纹比对。
	candidates, err := s.pool.Query(ctx, `
		SELECT contract_id::text FROM auto_release_jobs
		WHERE status IN ('OVERDUE','BLOCKED') AND final_deadline < now()
		ORDER BY final_deadline`)
	if err != nil {
		return nil, err
	}
	var phase2 []string
	for candidates.Next() {
		var id string
		if err := candidates.Scan(&id); err != nil {
			candidates.Close()
			return res, err
		}
		phase2 = append(phase2, id)
	}
	candidates.Close()
	for _, id := range phase2 {
		state, err := s.evaluateAutoRelease(ctx, id)
		if err != nil {
			return res, err
		}
		switch state {
		case domain.ReleaseJobReleased:
			res.AutoReleased = append(res.AutoReleased, id)
		case domain.ReleaseJobBlocked:
			res.Paused = append(res.Paused, id)
		case domain.ReleaseJobCancelled:
			res.Cancelled = append(res.Cancelled, id)
		}
	}
	if !executeReady {
		// 预检模式: READY 任务保留到下一次完整巡检执行。
		return res, nil
	}

	// 阶段 2b: 执行所有 READY 任务, 释放前重算状态指纹; 刚发生变化则转 REVIEW_HUMAN。
	readyRows, err := s.pool.Query(ctx, `
		SELECT contract_id::text FROM auto_release_jobs
		WHERE status='READY' AND final_deadline < now()
		ORDER BY final_deadline`)
	if err != nil {
		return nil, err
	}
	var readyIDs []string
	for readyRows.Next() {
		var id string
		if err := readyRows.Scan(&id); err != nil {
			readyRows.Close()
			return res, err
		}
		readyIDs = append(readyIDs, id)
	}
	readyRows.Close()
	for _, id := range readyIDs {
		did, st, err := s.executeReadyAutoRelease(ctx, id)
		if err != nil {
			return res, err
		}
		if did {
			switch st {
			case domain.ReleaseJobReleased:
				res.AutoReleased = append(res.AutoReleased, id)
			case domain.ReleaseJobReview:
				res.Paused = append(res.Paused, id)
			}
		}
	}
	return res, nil
}

// markOverdue 二次提醒并把验房任务标记为逾期。
func (s *Service) markOverdue(ctx context.Context, contractID string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		var jobStatus, cst string
		if err := tx.QueryRow(ctx, `
			SELECT j.status, c.status FROM auto_release_jobs j
			JOIN contracts c ON c.id=j.contract_id
			WHERE j.contract_id=$1 FOR UPDATE OF j`, contractID).
			Scan(&jobStatus, &cst); err != nil {
			return err
		}
		if jobStatus != domain.ReleaseJobPending {
			return nil // 已被其他 worker 处理
		}
		if err := checkStatus(cst, domain.StatusCheckoutPending, domain.StatusEarlyTerminated,
			domain.StatusReminding); err != nil {
			// 合同已离开待验房态(如进入争议), 本轮跳过, 保留任务等待后续巡检或人工处理。
			return nil
		}
		d, err := s.loadDeadlines(tx, ctx, contractID)
		if err != nil {
			return err
		}
		if err := s.sendReminder(tx, ctx, contractID, domain.ReminderOverdue,
			"inspection_reminder_overdue", map[string]any{
				"note": "首次验房期限已过, 请立即验房; 逾期未处理将在最终期限触发自动退押流程",
			}, d); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE auto_release_jobs SET status='OVERDUE', overdue_at=now(),
			       reminded_overdue_at=now(), updated_at=now()
			WHERE contract_id=$1`, contractID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE contracts SET status='REMINDING' WHERE id=$1`, contractID); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.TenantID, "inspection_marked_overdue",
			map[string]any{"final_deadline": d.final.Format(time.RFC3339)})
		return s.emit(tx, ctx, contractID, "InspectionTaskOverdue", map[string]any{
			"first_deadline": d.first, "final_deadline": d.final,
		}, actor(ctx).ID)
	})
}

// stateFingerprint 是自动释放前对"未关闭报修/未结账单/争议"的状态快照。
type stateFingerprint struct {
	RepairOpen    int64 `json:"repair_open"`
	RepairSeqSum  int64 `json:"repair_seq_sum"`
	UtilityUnpaid int64 `json:"utility_unpaid"`
	UtilitySeqSum int64 `json:"utility_seq_sum"`
	DisputeOpen   int64 `json:"dispute_open"`
	DisputeSeqSum int64 `json:"dispute_seq_sum"`
}

func (fp stateFingerprint) JSON() []byte {
	b, _ := json.Marshal(fp)
	return b
}

func (s *Service) snapshotState(tx pgx.Tx, ctx context.Context, contractID string) (stateFingerprint, error) {
	var fp stateFingerprint
	err := tx.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT count(*) FROM repairs WHERE contract_id=$1
		            AND status IN ('SUBMITTED','RESPONSIBILITY_PENDING')),0),
		  COALESCE((SELECT sum(state_seq) FROM repairs WHERE contract_id=$1),0),
		  COALESCE((SELECT count(*) FROM utility_readings WHERE contract_id=$1
		            AND phase='CHECKOUT' AND billed_amount > 0),0),
		  COALESCE((SELECT sum(state_seq) FROM utility_readings WHERE contract_id=$1),0),
		  COALESCE((SELECT count(*) FROM disputes WHERE contract_id=$1
		            AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW')),0),
		  COALESCE((SELECT sum(state_seq) FROM disputes WHERE contract_id=$1),0)`,
		contractID).Scan(&fp.RepairOpen, &fp.RepairSeqSum,
		&fp.UtilityUnpaid, &fp.UtilitySeqSum, &fp.DisputeOpen, &fp.DisputeSeqSum)
	return fp, err
}

// evaluateAutoRelease 最终期限评估(独立事务):
// 房东已自行开验房单 -> 任务取消; 三检查任一不满足 -> BLOCKED 转人工; 否则 READY 并持久化快照。
// 返回任务的最新状态。
func (s *Service) evaluateAutoRelease(ctx context.Context, contractID string) (string, error) {
	finalState := ""
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		var jobStatus string
		var finalReminded bool
		if err := tx.QueryRow(ctx,
			`SELECT status, reminded_final_at IS NOT NULL FROM auto_release_jobs WHERE contract_id=$1 FOR UPDATE`,
			contractID).Scan(&jobStatus, &finalReminded); err != nil {
			return err
		}
		if jobStatus == domain.ReleaseJobReleased || jobStatus == domain.ReleaseJobCancelled {
			finalState = jobStatus
			return nil
		}
		// 房东已在最终期限前发起验房 -> 正常流程继续, 自动释放任务取消。
		var hasInsp bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM inspections WHERE contract_id=$1)`, contractID).
			Scan(&hasInsp); err != nil {
			return err
		}
		if hasInsp {
			if _, err := tx.Exec(ctx, `
				UPDATE auto_release_jobs SET status='CANCELLED', updated_at=now(),
				       review_note='房东已发起验房, 自动释放取消'
				WHERE contract_id=$1`, contractID); err != nil {
				return err
			}
			finalState = domain.ReleaseJobCancelled
			return s.emit(tx, ctx, contractID, "AutoReleaseCancelled",
				map[string]any{"reason": "INSPECTION_STARTED"}, actor(ctx).ID)
		}

		d, err := s.loadDeadlines(tx, ctx, contractID)
		if err != nil {
			return err
		}
		// 最终提醒只发一次(后续巡检即使重复评估也不重复通知)。
		if !finalReminded {
			if err := s.sendReminder(tx, ctx, contractID, domain.ReminderFinal,
				"inspection_reminder_final", map[string]any{
					"note": "最终期限到达, 平台正在核验未关闭报修/未结账单/争议",
				}, d); err != nil {
				return err
			}
		}

		fp, err := s.snapshotState(tx, ctx, contractID)
		if err != nil {
			return err
		}
		reasons := []string{}
		if fp.RepairOpen > 0 {
			reasons = append(reasons, fmt.Sprintf("OPEN_REPAIR:%d", fp.RepairOpen))
		}
		if fp.UtilityUnpaid > 0 {
			reasons = append(reasons, fmt.Sprintf("UNSETTLED_UTILITY_BILL:%d", fp.UtilityUnpaid))
		}
		if fp.DisputeOpen > 0 {
			reasons = append(reasons, fmt.Sprintf("OPEN_DISPUTE:%d", fp.DisputeOpen))
		}
		rb, _ := json.Marshal(reasons)
		if len(reasons) > 0 {
			if _, err := tx.Exec(ctx, `
				UPDATE auto_release_jobs SET status='BLOCKED', block_reasons=$2::jsonb,
				       state_snapshot=$3::jsonb, reminded_final_at=COALESCE(reminded_final_at, now()), updated_at=now()
				WHERE contract_id=$1`, contractID, string(rb), string(fp.JSON())); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE contracts SET status='RELEASE_REVIEW' WHERE id=$1 AND status <> 'DISPUTING'`,
				contractID); err != nil {
				return err
			}
			if err := s.emit(tx, ctx, contractID, "AutoReleasePaused", map[string]any{
				"block_reasons": reasons, "fingerprint": fp, "stage": "EVALUATE",
			}, actor(ctx).ID); err != nil {
				return err
			}
			finalState = domain.ReleaseJobBlocked
			return nil
		}
		// 条件满足: 持久化快照并置 READY, 等待执行事务做释放前最后一次指纹比对。
		if _, err := tx.Exec(ctx, `
			UPDATE auto_release_jobs SET status='READY', block_reasons='[]'::jsonb,
			       state_snapshot=$2::jsonb, reminded_final_at=COALESCE(reminded_final_at, now()), updated_at=now()
			WHERE contract_id=$1`, contractID, string(fp.JSON())); err != nil {
			return err
		}
		finalState = domain.ReleaseJobReady
		return s.emit(tx, ctx, contractID, "AutoReleaseChecksPassed", map[string]any{
			"fingerprint": fp,
		}, actor(ctx).ID)
	})
	return finalState, err
}

// executeReadyAutoRelease 对 READY 任务执行自动释放(独立事务)。
// 释放前重算状态指纹: 若账单/报修/争议状态在评估之后发生变化, 暂停并转人工复核。
// 返回 (是否处理了 READY 任务, 最新状态, error)。
func (s *Service) executeReadyAutoRelease(ctx context.Context, contractID string) (bool, string, error) {
	did := false
	finalState := ""
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		var jobStatus, snapJSON string
		if err := tx.QueryRow(ctx, `
			SELECT status, state_snapshot::text FROM auto_release_jobs
			WHERE contract_id=$1 FOR UPDATE`, contractID).Scan(&jobStatus, &snapJSON); err != nil {
			return err
		}
		if jobStatus != domain.ReleaseJobReady {
			return nil // 已被人工或其他 worker 处理
		}
		did = true
		var saved stateFingerprint
		if err := json.Unmarshal([]byte(snapJSON), &saved); err != nil {
			return err
		}
		current, err := s.snapshotState(tx, ctx, contractID)
		if err != nil {
			return err
		}
		if current != saved {
			// 释放前最后一刻状态发生变化(报修重新打开/账单补录/新争议...), 绝不放款。
			reasons := []string{
				"STATE_CHANGED_BEFORE_RELEASE",
				fmt.Sprintf("snapshot=%s", string(saved.JSON())),
				fmt.Sprintf("current=%s", string(current.JSON())),
			}
			rb, _ := json.Marshal(reasons)
			if _, err := tx.Exec(ctx, `
				UPDATE auto_release_jobs SET status='REVIEW_HUMAN', block_reasons=$2::jsonb,
				       state_snapshot=$3::jsonb, updated_at=now()
				WHERE contract_id=$1`, contractID, string(rb), string(current.JSON())); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE contracts SET status='RELEASE_REVIEW' WHERE id=$1 AND status <> 'DISPUTING'`,
				contractID); err != nil {
				return err
			}
			finalState = domain.ReleaseJobReview
			return s.emit(tx, ctx, contractID, "AutoReleasePaused", map[string]any{
				"block_reasons": reasons, "stage": "EXECUTE",
				"snapshot": saved, "current": current,
			}, actor(ctx).ID)
		}
		sid, err := s.performAutoRelease(tx, ctx, contractID)
		if err != nil {
			return err
		}
		finalState = domain.ReleaseJobReleased
		_ = sid
		return nil
	})
	return did, finalState, err
}

// performAutoRelease 在当前事务内执行全额自动退押(无任何定论扣款):
// 生成 PROPOSED 结算单 -> CONFIRMED(DB settlement_guard 触发器再次校验
// 无未闭合争议且 hard_hold=0) -> EXECUTED, 押金全额退还租客。
func (s *Service) performAutoRelease(tx pgx.Tx, ctx context.Context, contractID string) (string, error) {
	var balance, hard, disputed, deposit float64
	if err := tx.QueryRow(ctx, `
		SELECT e.balance, e.hard_hold, e.disputed_hold, c.deposit_amount
		FROM escrow_accounts e JOIN contracts c ON c.id=e.contract_id
		WHERE e.contract_id=$1 FOR UPDATE OF e`, contractID).
		Scan(&balance, &hard, &disputed, &deposit); err != nil {
		return "", err
	}
	if hard > 0 || disputed > 0 {
		// 存在任何扣款预留都不能自动全额释放。
		return "", fmt.Errorf("%w: hard_hold=%.2f disputed_hold=%.2f 必须先定论或闭合",
			ErrManualReview, hard, disputed)
	}
	toTenant := round2(balance)
	var sid string
	if err := tx.QueryRow(ctx, `
		INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,
		                        paid_to_landlord,status,note)
		VALUES($1,$2,0,$3,0,'PROPOSED','房东超时未验房, 最终期限三检查通过, 平台自动释放押金')
		RETURNING id::text`, contractID, deposit, toTenant).Scan(&sid); err != nil {
		return "", fmt.Errorf("auto settlement: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO deductions(contract_id,total_amount,status,finding_ids,charge_ids,created_by)
		VALUES($1,0,'APPROVED','{}','{}',NULLIF($2,'')::uuid)
		ON CONFLICT (contract_id) DO UPDATE SET status='APPROVED', approved_at=now()`,
		contractID, actor(ctx).ID); err != nil {
		return "", err
	}
	// 触发器: 无未闭合争议 + deducted_total(0)==hard_hold(0), 否则回滚。
	if _, err := tx.Exec(ctx,
		`UPDATE settlements SET status='CONFIRMED' WHERE id=$1`, sid); err != nil {
		return "", mapPgGuardError(err)
	}
	if toTenant > 0 {
		if err := s.ledger(tx, ctx, contractID, "RELEASE_TO_TENANT", toTenant, -toTenant, 0, 0,
			"settlement", sid, "房东超时未验房, 自动释放: 押金全额退还租客", ""); err != nil {
			return "", err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE escrow_accounts SET released_total = released_total + $2 WHERE contract_id=$1`,
		contractID, balance); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE settlements SET status='EXECUTED', executed_at=now(),
		       auto_released=true, auto_released_at=now(), release_job_id=
		           (SELECT id FROM auto_release_jobs WHERE contract_id=$2)
		WHERE id=$1`, sid, contractID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE auto_release_jobs SET status='RELEASED', released_at=now(), settlement_id=$2,
		       reviewed_by=NULLIF($3,'')::uuid, reviewed_at=now(), updated_at=now()
		WHERE contract_id=$1`, contractID, sid, actor(ctx).ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE contracts SET status='RELEASED' WHERE id=$1`, contractID); err != nil {
		return "", err
	}
	c, _ := s.getContract(tx, ctx, contractID)
	_ = s.notify(tx, ctx, contractID, c.TenantID, "deposit_auto_returned",
		map[string]any{"amount": toTenant, "reason": "LANDLORD_INSPECTION_TIMEOUT"})
	_ = s.notify(tx, ctx, contractID, c.LandlordID, "deposit_auto_released_landlord",
		map[string]any{"returned_to_tenant": toTenant,
			"note": "押金已自动退还; 如有新证据可提交超时说明, 不会自动扣款"})
	return sid, s.emit(tx, ctx, contractID, "AutoDepositReleased", map[string]any{
		"settlement_id": sid, "to_tenant": toTenant, "to_landlord": 0.0,
		"reason": "LANDLORD_INSPECTION_TIMEOUT",
	}, actor(ctx).ID)
}

// ---------- 人工复核 ----------

// ResolveReleaseReview 平台对 RELEASE_REVIEW 的合同做人工复核:
// decision=RELEASE 重新核验后执行自动释放; REOPEN_INSPECTION 代开验房转正常流程;
// KEEP_REVIEW 仅记录备注, 维持人工挂起。
func (s *Service) ResolveReleaseReview(ctx context.Context, contractID, decision, note string) error {
	if actor(ctx).Role != "PLATFORM" {
		return fmt.Errorf("%w: platform only", ErrForbidden)
	}
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		var jobStatus, cst string
		if err := tx.QueryRow(ctx, `
			SELECT j.status, c.status FROM auto_release_jobs j
			JOIN contracts c ON c.id=j.contract_id
			WHERE j.contract_id=$1 FOR UPDATE OF j`, contractID).
			Scan(&jobStatus, &cst); err != nil {
			return err
		}
		if jobStatus != domain.ReleaseJobBlocked && jobStatus != domain.ReleaseJobReview {
			return fmt.Errorf("%w: 任务状态 %s 不处于人工复核", ErrBadState, jobStatus)
		}
		switch decision {
		case "KEEP_REVIEW":
			if _, err := tx.Exec(ctx, `
				UPDATE auto_release_jobs SET review_note=$2, reviewed_by=$3, reviewed_at=now(),
				       updated_at=now()
				WHERE contract_id=$1`, contractID, note, actor(ctx).ID); err != nil {
				return err
			}
			return s.emit(tx, ctx, contractID, "ReleaseReviewUpdated",
				map[string]any{"note": note}, actor(ctx).ID)

		case "REOPEN_INSPECTION":
			var inspID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO inspections(contract_id,initiated_by,status,checkout_handover_id,
				                        inspection_deadline,early_termination)
				SELECT $1,$2,'OPEN',h.id, now(), COALESCE(i.early_termination,false)
				FROM handovers h
				LEFT JOIN inspections i ON i.contract_id=h.contract_id
				WHERE h.contract_id=$1 AND h.phase='CHECKOUT'
				RETURNING id::text`, contractID, actor(ctx).ID).Scan(&inspID); err != nil {
				return fmt.Errorf("reopen inspection: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE auto_release_jobs SET status='CANCELLED', review_note=$2,
				       reviewed_by=$3, reviewed_at=now(), updated_at=now()
				WHERE contract_id=$1`, contractID, "人工复核: 转正常验房流程 "+note,
				actor(ctx).ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE contracts SET status='INSPECTING' WHERE id=$1`, contractID); err != nil {
				return err
			}
			return s.emit(tx, ctx, contractID, "InspectionStarted", map[string]any{
				"inspection_id": inspID, "by": "PLATFORM_REVIEW",
			}, actor(ctx).ID)

		case "RELEASE":
			// 人工放行: 重新核验, 任一阻断仍存在则拒绝并返回具体原因。
			fp, err := s.snapshotState(tx, ctx, contractID)
			if err != nil {
				return err
			}
			var reasons []string
			if fp.RepairOpen > 0 {
				reasons = append(reasons, fmt.Sprintf("OPEN_REPAIR:%d", fp.RepairOpen))
			}
			if fp.UtilityUnpaid > 0 {
				reasons = append(reasons, fmt.Sprintf("UNSETTLED_UTILITY_BILL:%d", fp.UtilityUnpaid))
			}
			if fp.DisputeOpen > 0 {
				reasons = append(reasons, fmt.Sprintf("OPEN_DISPUTE:%d", fp.DisputeOpen))
			}
			if len(reasons) > 0 {
				return fmt.Errorf("%w: 阻断事项仍未消除: %v", ErrUnresolved, reasons)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE auto_release_jobs SET state_snapshot=$2::jsonb, block_reasons='[]'::jsonb,
				       review_note=$3, reviewed_by=$4, reviewed_at=now(), updated_at=now()
				WHERE contract_id=$1`, contractID, string(fp.JSON()), note, actor(ctx).ID); err != nil {
				return err
			}
			if _, err := s.performAutoRelease(tx, ctx, contractID); err != nil {
				return err
			}
			return s.emit(tx, ctx, contractID, "ReleaseReviewResolved", map[string]any{
				"decision": "RELEASE", "note": note,
			}, actor(ctx).ID)

		default:
			return fmt.Errorf("%w: decision must be RELEASE|REOPEN_INSPECTION|KEEP_REVIEW",
				ErrValidation)
		}
	})
}

// ---------- 押金释放后: 房东超时说明与新证据 ----------

type PostReleaseClaimInput struct {
	Explanation string   `json:"explanation"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// SubmitPostReleaseClaim 房东在押金自动释放后提交超时说明与新证据。
// 注意: 仅登记与复核, 不会从已释放金额中自动扣款。
func (s *Service) SubmitPostReleaseClaim(ctx context.Context, contractID string,
	in PostReleaseClaimInput) (string, error) {
	if in.Explanation == "" {
		return "", fmt.Errorf("%w: explanation required", ErrValidation)
	}
	var id string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		var st string
		if err := tx.QueryRow(ctx, `SELECT status FROM contracts WHERE id=$1`, contractID).Scan(&st); err != nil {
			return err
		}
		if st != domain.StatusReleased {
			return fmt.Errorf("%w: 仅在押金已释放后提交超时说明(当前 %s)", ErrBadState, st)
		}
		c, err := s.getContract(tx, ctx, contractID)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO post_release_claims(contract_id,landlord_id,explanation,evidence_ids)
			VALUES($1,$2,$3,$4::uuid[]) RETURNING id::text`,
			contractID, c.LandlordID, in.Explanation, pgUUIDArrayLiteral(in.EvidenceIDs)).
			Scan(&id); err != nil {
			return fmt.Errorf("post-release claim: %w", err)
		}
		_ = s.notify(tx, ctx, contractID, c.TenantID, "post_release_claim_submitted",
			map[string]any{"claim_id": id, "note": "房东提交了超时说明, 不影响已退款金额"})
		return s.emit(tx, ctx, contractID, "PostReleaseClaimSubmitted", map[string]any{
			"claim_id": id, "explanation": in.Explanation, "evidence": len(in.EvidenceIDs),
		}, actor(ctx).ID)
	})
	return id, err
}

// DecidePostReleaseClaim 平台对释放后申诉作出裁决(UPHELD/REJECTED)。
// 裁决只产生结论留痕与通知, 不自动划扣已释放资金(如确需追偿须另案处理)。
func (s *Service) DecidePostReleaseClaim(ctx context.Context, claimID, decision, note string) error {
	if actor(ctx).Role != "PLATFORM" {
		return fmt.Errorf("%w: platform only", ErrForbidden)
	}
	switch decision {
	case domain.PostReleaseClaimUpheld, domain.PostReleaseClaimRejected:
	default:
		return fmt.Errorf("%w: decision must be UPHELD|REJECTED", ErrValidation)
	}
	var contractID, cur string
	if err := s.pool.QueryRow(ctx,
		`SELECT contract_id::text, status FROM post_release_claims WHERE id=$1`, claimID).
		Scan(&contractID, &cur); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if cur != domain.PostReleaseClaimOpen && cur != domain.PostReleaseClaimReviewing {
		return fmt.Errorf("%w: claim already %s", ErrBadState, cur)
	}
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			UPDATE post_release_claims SET status=$2, decision_note=$3, decided_by=$4, decided_at=now()
			WHERE id=$1 AND status IN ('OPEN','REVIEWING')`,
			claimID, decision, note, actor(ctx).ID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("%w: claim already decided", ErrBadState)
		}
		var landlordID, tenantID string
		if err := tx.QueryRow(ctx,
			`SELECT landlord_id::text, tenant_id::text FROM contracts WHERE id=$1`, contractID).
			Scan(&landlordID, &tenantID); err != nil {
			return err
		}
		_ = s.notify(tx, ctx, contractID, landlordID, "post_release_claim_decided",
			map[string]any{"claim_id": claimID, "decision": decision})
		_ = s.notify(tx, ctx, contractID, tenantID, "post_release_claim_decided",
			map[string]any{"claim_id": claimID, "decision": decision,
				"note": "裁决不自动划扣已释放押金"})
		return s.emit(tx, ctx, contractID, "PostReleaseClaimDecided", map[string]any{
			"claim_id": claimID, "decision": decision, "note": note,
			"auto_deducted": false,
		}, actor(ctx).ID)
	})
}
