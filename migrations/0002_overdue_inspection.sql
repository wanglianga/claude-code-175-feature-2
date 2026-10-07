-- 房东超时验房处理(Feature 2-A)
--
-- 租客按时搬离并提交退租申请后:
--   1. 平台按房屋和合同约定向房东发送验房提醒, 记录是否送达 / 房东是否打开验房单。
--   2. 超过首次期限仍未处理: 再次提醒并把验房任务标记为逾期。
--   3. 到最终期限: 先确认未关闭报修、未结账单和双方正在处理的争议,
--      且校验"释放前最后一刻"账单/报修状态未发生变化(状态指纹), 才自动释放押金;
--      否则暂停自动释放, 转平台人工复核。
--   4. 押金已自动释放后, 房东只能提交超时说明与新证据(POST_RELEASE_CLAIM),
--      不能直接从已释放金额中扣款; 平台可裁决, 但资金不再被自动划扣。
-- 每次通知与状态变化均留痕(inspection_reminders / auto_release_jobs / lease_events / notifications)。

-- ============ 合同: 最终释放期限与新状态 ============
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS final_release_days int NOT NULL DEFAULT 15;
-- REMINDING: 首次验房期限已过, 平台已二次提醒, 验房任务逾期, 等待房东在最终期限前处理。
-- RELEASE_REVIEW: 最终期限自动释放被暂停(账单/报修/争议未清或释放前状态发生变化), 转人工复核。
ALTER TABLE contracts DROP CONSTRAINT IF EXISTS contracts_status_check;
ALTER TABLE contracts ADD CONSTRAINT contracts_status_check CHECK (status IN
    ('DRAFT','SIGNED','ESCROWED','LIVING','CHECKOUT_PENDING','INSPECTING',
     'DEDUCTION_PROPOSED','DISPUTING','SETTLED','RELEASED','CLOSED',
     'EARLY_TERMINATED','REMINDING','RELEASE_REVIEW'));

-- ============ 验房提醒(首次/逾期二次/最终) ============
-- 每一次平台提醒一行; delivered 记录通知是否送达; opened_at 记录房东首次打开验房单时间。
CREATE TABLE IF NOT EXISTS inspection_reminders (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    kind            text NOT NULL CHECK (kind IN ('FIRST','OVERDUE','FINAL')),
    channel         text NOT NULL DEFAULT 'INBOX',
    notification_id bigint REFERENCES notifications(id),
    delivered       boolean NOT NULL DEFAULT false,     -- 通知是否送达(当前演示环境置 true)
    delivered_at    timestamptz,
    opened          boolean NOT NULL DEFAULT false,     -- 房东是否打开验房单
    opened_at       timestamptz,
    first_deadline  timestamptz,                        -- 首次验房期限快照
    final_deadline  timestamptz,                        -- 最终释放期限快照
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_inspection_reminders_contract ON inspection_reminders(contract_id, created_at);

-- ============ 自动释放任务(每个超时验房租赁一个, 全程留痕) ============
CREATE TABLE IF NOT EXISTS auto_release_jobs (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL UNIQUE REFERENCES contracts(id),
    status              text NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','OVERDUE','READY','BLOCKED','REVIEW_HUMAN','RELEASED','CANCELLED')),
    first_deadline      timestamptz NOT NULL,          -- 首次验房期限(交房 + inspection_due_days)
    final_deadline      timestamptz NOT NULL,          -- 最终释放期限(交房 + final_release_days)
    reminded_first_at   timestamptz,
    reminded_overdue_at timestamptz,
    reminded_final_at   timestamptz,
    overdue_at          timestamptz,                   -- 任务被标记为逾期的时间
    -- 最终期限评估时的阻断原因(未关闭报修/未结账单/争议/释放前状态变化)
    block_reasons       jsonb NOT NULL DEFAULT '[]',
    -- 释放前最后一刻的状态指纹: 报修/账单(utility_readings)/争议计数与最大 updated_at
    state_snapshot      jsonb NOT NULL DEFAULT '{}',
    reviewed_by         uuid REFERENCES users(id),
    reviewed_at         timestamptz,
    review_note         text,
    released_at         timestamptz,
    settlement_id       uuid REFERENCES settlements(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_auto_release_jobs_status ON auto_release_jobs(status, final_deadline);

-- ============ 押金释放后房东的超时说明与新证据 ============
-- 已释放金额不会因此被自动扣回; 平台可裁决支持/不支持, 仅生成结论留痕。
CREATE TABLE IF NOT EXISTS post_release_claims (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    landlord_id     uuid NOT NULL REFERENCES users(id),
    explanation     text NOT NULL,                     -- 超时说明
    evidence_ids    uuid[] NOT NULL DEFAULT '{}',      -- 新证据
    status          text NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN','REVIEWING','UPHELD','REJECTED')),
    decision_note   text,
    decided_by      uuid REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    decided_at      timestamptz
);
CREATE INDEX IF NOT EXISTS idx_post_release_claims_contract ON post_release_claims(contract_id, created_at);

-- ============ 结算单补充: 自动释放留痕 ============
ALTER TABLE settlements ADD COLUMN IF NOT EXISTS auto_released boolean NOT NULL DEFAULT false;
ALTER TABLE settlements ADD COLUMN IF NOT EXISTS release_job_id uuid REFERENCES auto_release_jobs(id);
ALTER TABLE settlements ADD COLUMN IF NOT EXISTS auto_released_at timestamptz;

-- ============ 释放前状态变化检测 ============
-- repairs / utility_readings / disputes 追加状态序号, 最终期限评估时快照;
-- 释放执行前重算指纹, 若发生变化(例如刚好有报修被重新打开/账单补录/争议被提起),
-- 则暂停自动释放并转人工复核, 绝不"定时到了就放款"。
ALTER TABLE repairs         ADD COLUMN IF NOT EXISTS state_seq bigint NOT NULL DEFAULT 0;
ALTER TABLE utility_readings ADD COLUMN IF NOT EXISTS state_seq bigint NOT NULL DEFAULT 0;
ALTER TABLE disputes        ADD COLUMN IF NOT EXISTS state_seq bigint NOT NULL DEFAULT 0;

CREATE OR REPLACE FUNCTION trg_bump_state_seq() RETURNS trigger AS $$
BEGIN
    -- 任何更新都使状态序号 +1(自动释放以 state_seq 总和作为指纹, 配合 open 计数)。
    NEW.state_seq := OLD.state_seq + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS repairs_state_seq ON repairs;
CREATE TRIGGER repairs_state_seq BEFORE UPDATE ON repairs
FOR EACH ROW EXECUTE FUNCTION trg_bump_state_seq();
DROP TRIGGER IF EXISTS utility_readings_state_seq ON utility_readings;
CREATE TRIGGER utility_readings_state_seq BEFORE UPDATE ON utility_readings
FOR EACH ROW EXECUTE FUNCTION trg_bump_state_seq();
DROP TRIGGER IF EXISTS disputes_state_seq ON disputes;
CREATE TRIGGER disputes_state_seq BEFORE UPDATE ON disputes
FOR EACH ROW EXECUTE FUNCTION trg_bump_state_seq();
