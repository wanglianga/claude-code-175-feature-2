-- 房东超时验房处理(0002)
-- 背景: 租客按时搬离并提交退租申请后, 房东可能迟迟不验房。平台需要:
--   1. 按房屋与合同约定向房东发送验房提醒, 记录通知送达/房东是否打开验房单/是否有未关闭报修;
--   2. 超过首次期限仍未处理 -> 再次提醒并把任务标记逾期;
--   3. 最终期限到达时先核验 未关闭报修 / 未结账单 / 未闭合争议, 不能仅凭定时就释放押金;
--   4. 条件满足才自动释放押金; 释放前一刻发现状态变化 -> 暂停自动释放转人工复核;
--   5. 释放后房东只能补交超时说明与新证据, 不得直接从已释放金额中扣款。

-- ============ 合同: 最终自动释放宽限期 ============
-- 首次验房期限 = 交房时间 + inspection_due_days(既有字段)
-- 最终释放期限 = 交房时间 + inspection_due_days + final_release_days
ALTER TABLE contracts ADD COLUMN final_release_days int NOT NULL DEFAULT 7;

-- ============ 结算单: 释放来源(双方确认 / 超时自动 / 超时人工) ============
ALTER TABLE settlements ADD COLUMN release_mode text NOT NULL DEFAULT 'MUTUAL_CONFIRM'
    CHECK (release_mode IN ('MUTUAL_CONFIRM','OVERDUE_AUTO','OVERDUE_MANUAL'));

-- ============ 通知: 渠道与送达/打开回执 ============
ALTER TABLE notifications ADD COLUMN channel text NOT NULL DEFAULT 'INBOX'
    CHECK (channel IN ('INBOX','SMS','PUSH'));
ALTER TABLE notifications ADD COLUMN delivery_status text NOT NULL DEFAULT 'PENDING'
    CHECK (delivery_status IN ('PENDING','SENT','DELIVERED','FAILED'));
ALTER TABLE notifications ADD COLUMN delivered_at timestamptz;
ALTER TABLE notifications ADD COLUMN opened_at timestamptz;
ALTER TABLE notifications ADD COLUMN failure_reason text;
CREATE INDEX idx_notifications_status ON notifications(delivery_status, id);

-- ============ 超时验房任务(每次退房一条) ============
CREATE TABLE inspection_tasks (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL UNIQUE REFERENCES contracts(id),
    checkout_handover_id uuid REFERENCES handovers(id),
    -- PENDING 首次提醒后待处理 / OPENED 房东已打开验房单 / OVERDUE 已二次提醒并标逾期 /
    -- COMPLETED 房东正常发起验房, 任务被正常流程接管 /
    -- SUPERSEDED 租客逾期申诉后平台代开验房, 任务被正常流程接管 /
    -- AUTO_RELEASED 最终期限条件满足, 押金已自动全额释放 /
    -- MANUAL_REVIEW 存在未决项或释放前状态突变, 暂停自动释放转人工
    status              text NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','OPENED','OVERDUE','COMPLETED','SUPERSEDED',
                          'AUTO_RELEASED','MANUAL_REVIEW')),
    checkout_at         timestamptz NOT NULL,
    first_deadline      timestamptz NOT NULL,   -- 房东首次验房期限
    final_deadline      timestamptz NOT NULL,   -- 平台自动释放的最终期限
    -- 两次提醒及其送达状态(送达明细在 notifications 行内)
    first_reminder_inbox_id  bigint REFERENCES notifications(id),
    first_reminder_sms_id    bigint REFERENCES notifications(id),
    second_reminder_inbox_id bigint REFERENCES notifications(id),
    second_reminder_sms_id   bigint REFERENCES notifications(id),
    first_reminded_at   timestamptz,
    second_reminded_at  timestamptz,
    overdue_at          timestamptz,
    opened_at           timestamptz,            -- 房东首次打开验房单时间
    -- 门禁快照(最终期限扫描时记录, 便于审计为什么放行/为什么拦截)
    gate_snapshot       jsonb,
    blocked_reason      text,                   -- OPEN_REPAIRS/UNSETTLED_BILLS/OPEN_DISPUTES/EARLY_TERMINATION/HOLDS/STATE_CHANGED
    -- 自动释放
    auto_released_at    timestamptz,
    release_settlement_id uuid REFERENCES settlements(id),
    -- 人工复核
    review_outcome      text CHECK (review_outcome IN ('NONE','RELEASED','RETAINED','REINSPECT')),
    review_note         text,
    reviewed_by         uuid REFERENCES users(id),
    reviewed_at         timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (final_deadline > first_deadline)
);
CREATE INDEX idx_inspection_tasks_status ON inspection_tasks(status, final_deadline);

-- 任务状态变化流水(与 lease_events 双留痕: 本表面向运营审计, lease_events 面向 Kafka 消费方)
CREATE TABLE inspection_task_events (
    id          bigserial PRIMARY KEY,
    task_id     uuid NOT NULL REFERENCES inspection_tasks(id),
    contract_id uuid NOT NULL REFERENCES contracts(id),
    event_type  text NOT NULL CHECK (event_type IN
                    ('CREATED','FIRST_REMINDER','SECOND_REMINDER','REMINDER_DELIVERED',
                     'SHEET_OPENED','OVERDUE_MARKED','COMPLETED','SUPERSEDED',
                     'GATE_SNAPSHOT','GATE_BLOCKED','STATE_CHANGED_SUSPEND','AUTO_RELEASED',
                     'MANUAL_REVIEW','REVIEW_RESOLVED',
                     'LATE_CLAIM_FILED','LATE_CLAIM_RESOLVED')),
    detail      jsonb NOT NULL DEFAULT '{}',
    actor_id    uuid REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_inspection_task_events_task ON inspection_task_events(task_id, id);

-- ============ 房东释放后的超时说明与新证据(账外主张, 不能扣已释放金额) ============
CREATE TABLE late_inspection_claims (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    task_id         uuid NOT NULL REFERENCES inspection_tasks(id),
    explanation     text NOT NULL,
    evidence_ids    uuid[] NOT NULL DEFAULT '{}',
    claimed_amount  cny_amount NOT NULL DEFAULT 0,   -- 房东主张金额(仅登记, 不得从已释放押金扣)
    status          text NOT NULL DEFAULT 'SUBMITTED'
        CHECK (status IN ('SUBMITTED','REVIEWING','RESOLVED_AWARD','RESOLVED_REJECTED')),
    -- 平台复核: 支持部分只能作为账外追讨(线下/另案), 不回流 escrow、不写扣款台账
    award_amount    cny_amount NOT NULL DEFAULT 0,
    decision_note   text,
    submitted_by    uuid NOT NULL REFERENCES users(id),
    decided_by      uuid REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    decided_at      timestamptz,
    CHECK (award_amount <= claimed_amount)
);
CREATE INDEX idx_late_claims_contract ON late_inspection_claims(contract_id);

-- ============ 数据库级闸门: 超时自动释放前必须"干净" ============
-- 应用层即使被绕过, 直接把 OVERDUE_* 结算单置 CONFIRMED 也会被这里回滚:
--   * 无未闭合争议
--   * 无未关闭报修 (SUBMITTED/RESPONSIBILITY_PENDING)
--   * 无未结账单 (PROPOSED/CONTESTED 费用项; 退租水电已计费却没有定论费用项)
--   * escrow 两层预留必须为 0 (没有任何定论/待定扣款)
--   * 合同不能停留在 EARLY_TERMINATED (空置损失尚未处理)
CREATE OR REPLACE FUNCTION assert_overdue_release_clean(p_contract uuid) RETURNS void AS $$
DECLARE
    n int;
BEGIN
    PERFORM assert_no_open_dispute(p_contract);

    SELECT count(*) INTO n FROM repairs
      WHERE contract_id = p_contract
        AND status IN ('SUBMITTED','RESPONSIBILITY_PENDING');
    IF n > 0 THEN
        RAISE EXCEPTION 'CONFLICT_OPEN_REPAIR: contract=% has % unclosed repair(s), auto release blocked',
            p_contract, n USING ERRCODE = 'check_violation';
    END IF;

    SELECT count(*) INTO n FROM charge_items
      WHERE contract_id = p_contract AND status IN ('PROPOSED','CONTESTED');
    IF n > 0 THEN
        RAISE EXCEPTION 'CONFLICT_UNSETTLED_BILL: contract=% has % unresolved charge item(s), auto release blocked',
            p_contract, n USING ERRCODE = 'check_violation';
    END IF;

    -- 退租时已计费的水电煤, 在"验房确认已结清"或存在定论(接受/支持/驳回)费用项前视为未结账单
    SELECT count(*) INTO n FROM utility_readings u
      WHERE u.contract_id = p_contract AND u.phase = 'CHECKOUT' AND u.billed_amount > 0
        AND COALESCE((SELECT utility_status FROM inspections
                       WHERE contract_id = p_contract), '') <> 'SETTLED'
        AND NOT EXISTS (
            SELECT 1 FROM charge_items ci
              WHERE ci.contract_id = p_contract AND ci.kind = 'UTILITY_UNPAID'
                AND ci.status IN ('ACCEPTED','UPHELD','REJECTED','OVERTURNED'));
    IF n > 0 THEN
        RAISE EXCEPTION 'CONFLICT_UNSETTLED_BILL: contract=% has % unsettled checkout utility bill(s), auto release blocked',
            p_contract, n USING ERRCODE = 'check_violation';
    END IF;

    IF EXISTS (SELECT 1 FROM escrow_accounts
                WHERE contract_id = p_contract AND (hard_hold <> 0 OR disputed_hold <> 0)) THEN
        RAISE EXCEPTION 'CONFLICT_HOLD: contract=% still has holds, auto release blocked',
            p_contract USING ERRCODE = 'check_violation';
    END IF;

    IF EXISTS (SELECT 1 FROM contracts WHERE id = p_contract AND status = 'EARLY_TERMINATED') THEN
        RAISE EXCEPTION 'CONFLICT_EARLY_TERMINATION: contract=% early termination unprocessed, auto release blocked',
            p_contract USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql;

-- 扩展既有结算守卫: 超时自动/人工释放走更强的"干净"校验; 双方确认流程沿用原校验。
CREATE OR REPLACE FUNCTION trg_settlement_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.status = 'CONFIRMED' AND OLD.status = 'PROPOSED' THEN
        PERFORM assert_no_open_dispute(NEW.contract_id);
        IF (SELECT hard_hold FROM escrow_accounts WHERE contract_id = NEW.contract_id)
           IS DISTINCT FROM NEW.deducted_total THEN
            RAISE EXCEPTION 'CONFLICT_HOLD_MISMATCH: deducted_total % != hard_hold',
                NEW.deducted_total USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.release_mode IN ('OVERDUE_AUTO','OVERDUE_MANUAL') THEN
            PERFORM assert_overdue_release_clean(NEW.contract_id);
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
