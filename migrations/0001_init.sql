-- 城市租房押金托管·退租验房·争议服务
-- 单条租赁记录(lease)贯穿: 合同 -> 资金托管 -> 入住交割 -> 维修 -> 退租验房 -> 扣款 -> 争议 -> 押金释放
-- 设计原则: 资金状态/争议状态/证据状态在同一事务内联动; 任何一方都不能单方面扣款或释放。

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- 金额统一 numeric(12,2), 单位元
CREATE DOMAIN cny_amount AS numeric(12,2) CHECK (VALUE >= 0);

-- ============ 用户与角色 ============
CREATE TABLE users (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone           text NOT NULL UNIQUE,
    display_name    text NOT NULL,
    role            text NOT NULL CHECK (role IN ('TENANT','LANDLORD','AGENT','PLATFORM')),
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- ============ 房源 ============
CREATE TABLE properties (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    landlord_id     uuid NOT NULL REFERENCES users(id),
    address         text NOT NULL,
    city            text NOT NULL,
    rooms           text,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- ============ 合同与租赁主记录 ============
-- 合同/押金/托管/交割/报修/验房/扣款/申诉/释放全部挂在这一条 lease 上。
-- 状态机:
--   DRAFT -> SIGNED -> ESCROWED -> LIVING -> CHECKOUT_PENDING -> INSPECTING
--        -> DEDUCTION_PROPOSED -> DISPUTING(可循环) -> SETTLED -> RELEASED -> CLOSED
--   EARLY_TERMINATED 为租客提前退租时 LIVING 的特殊迁出态, 之后同样进入 INSPECTING 流程
CREATE TABLE contracts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    lease_no            text NOT NULL UNIQUE,
    property_id         uuid NOT NULL REFERENCES properties(id),
    landlord_id         uuid NOT NULL REFERENCES users(id),
    tenant_id           uuid NOT NULL REFERENCES users(id),
    agent_id            uuid REFERENCES users(id),
    start_date          date NOT NULL,
    end_date            date NOT NULL CHECK (end_date > start_date),
    rent_amount         cny_amount NOT NULL,
    deposit_amount      cny_amount NOT NULL,
    -- 自然磨损与维修责任的合同约定
    wear_clause         text,
    notice_days         int  NOT NULL DEFAULT 30,           -- 提前退租通知期
    inspection_due_days int  NOT NULL DEFAULT 7,            -- 交房后房东须在 N 天内验房
    payment_due_days    int  NOT NULL DEFAULT 7,            -- 验房后须在 N 天内完成结算
    status              text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT','SIGNED','ESCROWED','LIVING','CHECKOUT_PENDING',
                          'INSPECTING','DEDUCTION_PROPOSED','DISPUTING','SETTLED',
                          'RELEASED','CLOSED','EARLY_TERMINATED')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (tenant_id <> landlord_id)
);
CREATE INDEX idx_contracts_tenant   ON contracts(tenant_id);
CREATE INDEX idx_contracts_landlord ON contracts(landlord_id);
CREATE INDEX idx_contracts_status   ON contracts(status);

-- ============ 托管账户与资金账 ============
-- 押金在托管账户里"只是资金状态"; 能不能释放由合同/交割/报修/验房证据决定。
-- 三层金额:
--   balance          托管总余额
--   disputed_hold    争议/提议扣款冻结(软冻结, 双方确认或裁决前不得释放)
--   hard_hold        已确认扣款(双方确认或平台裁决最终定论)
CREATE TABLE escrow_accounts (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL UNIQUE REFERENCES contracts(id),
    currency        text NOT NULL DEFAULT 'CNY',
    balance         cny_amount NOT NULL DEFAULT 0,        -- 当前在管押金
    hard_hold       cny_amount NOT NULL DEFAULT 0,        -- 已定论扣款预留
    disputed_hold   cny_amount NOT NULL DEFAULT 0,        -- 争议/待确认扣款预留
    released_total  cny_amount NOT NULL DEFAULT 0,
    version         bigint NOT NULL DEFAULT 0,       -- 乐观锁
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (hard_hold + disputed_hold <= balance)
);

-- append-only 资金台账(充值/冻结/解冻/扣款/释放)
CREATE TABLE fund_ledger (
    id              bigserial PRIMARY KEY,
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    seq             bigint NOT NULL,
    txn_type        text NOT NULL CHECK (txn_type IN
                        ('DEPOSIT_IN','HOLD_PROPOSE','HOLD_CONFIRM','HOLD_RELEASE',
                         'DEDUCT','RELEASE_TO_TENANT','RELEASE_TO_LANDLORD','ADJUST')),
    amount          cny_amount NOT NULL,
    balance_after   cny_amount NOT NULL,
    hard_after      cny_amount NOT NULL,
    disputed_after  cny_amount NOT NULL,
    ref_type        text,                            -- dispute / deduction / settlement
    ref_id          uuid,
    reason          text NOT NULL,
    actor_id        uuid REFERENCES users(id),
    idempotency_key text UNIQUE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, seq)
);
CREATE INDEX idx_fund_ledger_contract ON fund_ledger(contract_id, seq);

-- ============ 证据文件(交割照片/验房照片/票据等) ============
-- 证据只记录内容哈希与元数据, 文件本体可存放对象存储; 服务对哈希负责。
-- evidence 同时被 handover / inspection / repair / dispute / finding 引用。
CREATE TABLE evidence (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    category        text NOT NULL CHECK (category IN
                        ('HANDOVER_CHECKIN','HANDOVER_CHECKOUT','INSPECTION','REPAIR',
                         'DISPUTE','INVOICE','KEY','UTILITY','CLEANING','OTHER')),
    content_type    text NOT NULL DEFAULT 'image/jpeg',
    sha256          text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    size_bytes      bigint NOT NULL CHECK (size_bytes >= 0),
    storage_uri     text NOT NULL,
    caption         text,
    uploader_id     uuid NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_evidence_contract ON evidence(contract_id, category);

-- ============ 入住/退租交割 ============
-- phase=CHECKIN 入住交割, phase=CHECKOUT 退租交割(交房)。
CREATE TABLE handovers (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL REFERENCES contracts(id),
    phase               text NOT NULL CHECK (phase IN ('CHECKIN','CHECKOUT')),
    handed_by           uuid NOT NULL REFERENCES users(id),
    received_by         uuid NOT NULL REFERENCES users(id),
    -- 水电表读数
    water_meter_reading numeric(12,2),
    elec_meter_reading  numeric(12,2),
    gas_meter_reading   numeric(12,2),
    keys_handover       text,                        -- 钥匙套数/门禁卡描述
    keys_returned       text,
    cleaning_note       text,
    note                text,
    occurred_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, phase)
);

-- 交割证据关联
CREATE TABLE handover_evidence (
    handover_id  uuid NOT NULL REFERENCES handovers(id) ON DELETE CASCADE,
    evidence_id  uuid NOT NULL REFERENCES evidence(id),
    PRIMARY KEY (handover_id, evidence_id)
);

-- 家具家电清单(逐项状态; 入住与退租可对比)
CREATE TABLE inventory_items (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    name            text NOT NULL,                  -- 墙面(主卧)/冰箱/洗衣机...
    category        text NOT NULL CHECK (category IN
                        ('WALL','FLOOR','DOOR_WINDOW','FURNITURE','APPLIANCE','KEY','FIXTURE','OTHER')),
    brand_model     text,
    checkin_state   text NOT NULL CHECK (checkin_state IN ('NEW','GOOD','WORN','DAMAGED','MISSING')),
    checkin_note    text,
    checkout_state  text CHECK (checkout_state IN ('NEW','GOOD','WORN','DAMAGED','MISSING')),
    checkout_note   text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_inventory_contract ON inventory_items(contract_id);

-- ============ 水电煤读数与欠费 ============
CREATE TABLE utility_readings (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    kind            text NOT NULL CHECK (kind IN ('WATER','ELECTRIC','GAS')),
    phase           text NOT NULL CHECK (phase IN ('CHECKIN','CHECKOUT')),
    reading         numeric(12,2) NOT NULL,
    unit_price      numeric(10,4) NOT NULL DEFAULT 0,
    billed_amount   cny_amount NOT NULL DEFAULT 0,        -- 入住->退租区间应结费用(退租时填)
    recorded_by     uuid NOT NULL REFERENCES users(id),
    recorded_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, kind, phase)
);

-- ============ 维修报修(入住期间) ============
-- submitted_by(租客) -> 房东确认责任与费用 -> 关联房屋档案; 退租时作为责任判定证据。
CREATE TABLE repairs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    item_name       text,                           -- 可关联家具家电
    title           text NOT NULL,
    description     text NOT NULL,
    category        text NOT NULL CHECK (category IN
                        ('WALL','FLOOR','APPLIANCE','FURNITURE','PLUMBING','ELECTRICAL','DOOR_WINDOW','OTHER')),
    status          text NOT NULL DEFAULT 'SUBMITTED'
        CHECK (status IN ('SUBMITTED','RESPONSIBILITY_PENDING','LANDLORD_RESPONSIBLE',
                          'TENANT_RESPONSIBLE','SHARED','COMPLETED','REJECTED')),
    cost_amount     cny_amount NOT NULL DEFAULT 0,
    tenant_share    cny_amount NOT NULL DEFAULT 0,       -- 租客承担部分(退租扣款依据之一)
    decision_note   text,
    submitted_by    uuid NOT NULL REFERENCES users(id),
    decided_by      uuid REFERENCES users(id),
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    decided_at      timestamptz
);
CREATE INDEX idx_repairs_contract ON repairs(contract_id, status);

CREATE TABLE repair_evidence (
    repair_id    uuid NOT NULL REFERENCES repairs(id) ON DELETE CASCADE,
    evidence_id  uuid NOT NULL REFERENCES evidence(id),
    PRIMARY KEY (repair_id, evidence_id)
);

-- ============ 退租验房 ============
-- 由房东或中介发起; 租客可对结论逐项举证。
CREATE TABLE inspections (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL UNIQUE REFERENCES contracts(id),
    initiated_by    uuid NOT NULL REFERENCES users(id),
    status          text NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN','SUBMITTED','TENANT_REVIEWED','REINSPECT','CLOSED')),
    checkout_handover_id uuid REFERENCES handovers(id),
    early_termination    boolean NOT NULL DEFAULT false, -- 是否租客提前退租(状态流转后保留该事实)
    -- 退租各维度结论
    wall_status         text CHECK (wall_status  IN ('GOOD','NORMAL_WEAR','DAMAGED')),
    floor_status        text CHECK (floor_status IN ('GOOD','NORMAL_WEAR','DAMAGED')),
    appliance_status    text CHECK (appliance_status IN ('GOOD','NORMAL_WEAR','DAMAGED','MISSING')),
    keys_status         text CHECK (keys_status IN ('RETURNED','MISSING','DAMAGED')),
    utility_status      text CHECK (utility_status IN ('SETTLED','UNPAID')),
    cleaning_status     text CHECK (cleaning_status IN ('CLEAN','NEEDS_CLEANING')),
    summary             text,
    inspection_deadline timestamptz,               -- 房东须在此前完成验房(迟迟不验房的判定依据)
    submitted_at        timestamptz,
    tenant_reviewed_at  timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- ============ 可扣款项建议(规则引擎产物 + 人工增补) ============
-- status: PROPOSED 系统建议 -> ACCEPTED 双方接受(冻结转硬预留) -> REJECTED 驳回
--         CONTESTED 申诉中(产生 dispute) -> UPHELD 裁决支持房东 / OVERTURNED 裁决撤销
CREATE TABLE findings (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    inspection_id   uuid REFERENCES inspections(id),
    code            text NOT NULL,                  -- WALL_DAMAGE / NORMAL_WEAR / APPLIANCE ...
    title           text NOT NULL,
    detail          text,
    basis           text NOT NULL,                  -- 判定依据: 合同条款/交割对比/维修记录
    evidence_ids    uuid[] NOT NULL DEFAULT '{}',
    linked_repair_id uuid REFERENCES repairs(id),
    proposed_amount cny_amount NOT NULL,
    -- 规则引擎给出的"建议"与置信依据; 最终金额需双方确认或裁决
    status          text NOT NULL DEFAULT 'PROPOSED'
        CHECK (status IN ('PROPOSED','ACCEPTED','REJECTED','CONTESTED','UPHELD','OVERTURNED')),
    source          text NOT NULL DEFAULT 'RULE' CHECK (source IN ('RULE','MANUAL')),
    created_by      uuid REFERENCES users(id),
    decided_by      uuid REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    decided_at      timestamptz
);
CREATE INDEX idx_findings_contract ON findings(contract_id, status);

-- 清洁费/欠费等独立费用主张
CREATE TABLE charge_items (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    kind            text NOT NULL CHECK (kind IN ('CLEANING','UTILITY_UNPAID','KEY_LOSS','OTHER')),
    title           text NOT NULL,
    amount          cny_amount NOT NULL,
    status          text NOT NULL DEFAULT 'PROPOSED'
        CHECK (status IN ('PROPOSED','ACCEPTED','REJECTED','CONTESTED','UPHELD','OVERTURNED')),
    evidence_ids    uuid[] NOT NULL DEFAULT '{}',
    created_by      uuid REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_charge_items_contract ON charge_items(contract_id, status);

-- ============ 扣款单(一组已定论项的汇总, 结算时使用) ============
-- 在争议闭合前, 扣款单不得 APPROVED; APPROVED 后押金才允许按金额释放给房东。
CREATE TABLE deductions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL UNIQUE REFERENCES contracts(id),
    total_amount    cny_amount NOT NULL,
    status          text NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT','PROPOSED','APPROVED','CANCELLED')),
    finding_ids     uuid[] NOT NULL DEFAULT '{}',
    charge_ids      uuid[] NOT NULL DEFAULT '{}',
    created_by      uuid REFERENCES users(id),
    approved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- ============ 争议/申诉 ============
-- 覆盖: 墙面损坏vs自然磨损 / 家电维修责任不清 / 水电欠费 / 钥匙丢失 /
--       房东迟迟不验房 / 租客提前退租 / 清洁费用争议
CREATE TABLE disputes (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    reason          text NOT NULL CHECK (reason IN
                        ('WALL_DAMAGE_VS_WEAR','FLOOR_DAMAGE_VS_WEAR','APPLIANCE_RESPONSIBILITY',
                         'UTILITY_ARREARS','KEY_LOSS','INSPECTION_DELAY','EARLY_TERMINATION',
                         'CLEANING_FEE','OTHER')),
    subject         text NOT NULL,
    claimed_amount  cny_amount NOT NULL DEFAULT 0,       -- 争议涉及金额
    finding_id      uuid REFERENCES findings(id),
    charge_id       uuid REFERENCES charge_items(id),
    opened_by       uuid NOT NULL REFERENCES users(id),
    status          text NOT NULL DEFAULT 'OPEN'
        CHECK (status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW','RESOLVED_AGREEMENT',
                          'RESOLVED_ADJUDICATION','WITHDRAWN')),
    -- 裁决结果
    award_to_landlord cny_amount NOT NULL DEFAULT 0,     -- 裁决支持房东的金额
    award_to_tenant   cny_amount NOT NULL DEFAULT 0,
    adjudicated_by  uuid REFERENCES users(id),
    resolution_note text,
    deadline        timestamptz,                    -- 补证/响应期限
    created_at      timestamptz NOT NULL DEFAULT now(),
    resolved_at     timestamptz,
    CHECK (NOT (status IN ('RESOLVED_AGREEMENT','RESOLVED_ADJUDICATION')) OR resolved_at IS NOT NULL)
);
CREATE INDEX idx_disputes_contract ON disputes(contract_id, status);
-- 同一笔扣款项不得同时挂两个未闭合争议(由应用层 + 部分唯一索引保证)
CREATE UNIQUE INDEX uq_dispute_open_finding ON disputes(finding_id)
    WHERE finding_id IS NOT NULL AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW');
CREATE UNIQUE INDEX uq_dispute_open_charge  ON disputes(charge_id)
    WHERE charge_id IS NOT NULL AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW');

-- 争议时间线: 每一次主张/确认/驳回/补证都留痕并驱动同一笔押金状态
CREATE TABLE dispute_actions (
    id              bigserial PRIMARY KEY,
    dispute_id      uuid NOT NULL REFERENCES disputes(id),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    actor_id        uuid NOT NULL REFERENCES users(id),
    action          text NOT NULL CHECK (action IN
                        ('OPEN','SUBMIT_EVIDENCE','ACKNOWLEDGE','REBUT','REQUEST_MORE_EVIDENCE',
                         'PARTIES_AGREE','ESCALATE','ADJUDICATE','WITHDRAW')),
    amount_effect   cny_amount NOT NULL DEFAULT 0,       -- 该动作主张的金额(如有)
    evidence_ids    uuid[] NOT NULL DEFAULT '{}',
    comment         text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_dispute_actions_dispute ON dispute_actions(dispute_id, id);

-- ============ 结算与押金释放 ============
-- 一条租赁最终只有一份生效结算; 生效前提: 无未闭合争议 + 扣款已定论。
CREATE TABLE settlements (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL UNIQUE REFERENCES contracts(id),
    deposit_amount      cny_amount NOT NULL,
    deducted_total      cny_amount NOT NULL,
    returned_to_tenant  cny_amount NOT NULL,
    paid_to_landlord    cny_amount NOT NULL,
    status              text NOT NULL DEFAULT 'PROPOSED'
        CHECK (status IN ('PROPOSED','CONFIRMED','EXECUTED')),
    finding_ids         uuid[] NOT NULL DEFAULT '{}',
    charge_ids          uuid[] NOT NULL DEFAULT '{}',
    -- 双方确认(协商一致); 超时由平台裁决确认
    tenant_confirmed_by uuid REFERENCES users(id),
    landlord_confirmed_by uuid REFERENCES users(id),
    tenant_confirmed_at timestamptz,
    landlord_confirmed_at timestamptz,
    note                text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    executed_at         timestamptz,
    CHECK (deducted_total + returned_to_tenant = deposit_amount),
    CHECK (paid_to_landlord = deducted_total)
);

-- ============ 领域事件 + 事务性 Outbox ============
-- 所有状态变化在同一 DB 事务内写此表; 后台 relay 投递 Kafka, at-least-once。
-- 消费方依据 event_id 幂等。lease 维度 key 保证同一租赁事件有序。
CREATE TABLE lease_events (
    id              bigserial PRIMARY KEY,
    event_id        uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL,
    event_type      text NOT NULL,
    aggregate       text NOT NULL DEFAULT 'lease',
    payload         jsonb NOT NULL,
    actor_id        uuid,
    published       boolean NOT NULL DEFAULT false,
    published_at    timestamptz,
    kafka_partition int,
    kafka_offset    bigint,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_lease_events_unpublished ON lease_events(id) WHERE published = false;

-- 站内/短信通知(由事件驱动, 这里只留通知记录)
CREATE TABLE notifications (
    id              bigserial PRIMARY KEY,
    contract_id     uuid REFERENCES contracts(id),
    user_id         uuid NOT NULL REFERENCES users(id),
    template        text NOT NULL,
    vars            jsonb NOT NULL DEFAULT '{}',
    event_id        uuid REFERENCES lease_events(event_id),
    read_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_notifications_user ON notifications(user_id, id);

-- ============ 跨表守卫: 存在未闭合争议时禁止结算生效/押金释放 ============
CREATE OR REPLACE FUNCTION assert_no_open_dispute(p_contract uuid) RETURNS void AS $$
DECLARE
    n int;
BEGIN
    SELECT count(*) INTO n FROM disputes
      WHERE contract_id = p_contract
        AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW');
    IF n > 0 THEN
        RAISE EXCEPTION 'CONFLICT_OPEN_DISPUTE: contract=% has % unresolved dispute(s), settlement/release blocked',
            p_contract, n USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION trg_settlement_guard() RETURNS trigger AS $$
BEGIN
    -- 只守卫 PROPOSED -> CONFIRMED 这一步; EXECUTED 是同事务内 CONFIRMED 之后的放款动作,
    -- 此时 hard_hold 已随放款清零, 不应再与 deducted_total 比较。
    IF NEW.status = 'CONFIRMED' AND OLD.status = 'PROPOSED' THEN
        PERFORM assert_no_open_dispute(NEW.contract_id);
        -- 结算扣款金额必须等于当前已定论扣款预留
        IF (SELECT hard_hold FROM escrow_accounts WHERE contract_id = NEW.contract_id)
           IS DISTINCT FROM NEW.deducted_total THEN
            RAISE EXCEPTION 'CONFLICT_HOLD_MISMATCH: deducted_total % != hard_hold',
                NEW.deducted_total USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER settlement_guard
BEFORE INSERT OR UPDATE OF status ON settlements
FOR EACH ROW EXECUTE FUNCTION trg_settlement_guard();
