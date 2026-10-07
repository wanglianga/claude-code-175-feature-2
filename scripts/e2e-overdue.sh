#!/usr/bin/env bash
# 房东超时验房处理 端到端验证:
#   1. 交房即首提醒(送达/打开留痕) -> 逾期二次提醒+REMINDING -> 房东补开验房, 任务取消
#   2. 未关闭报修阻断自动释放 -> RELEASE_REVIEW -> 人工 REOPEN_INSPECTION 后正常结算
#   3. 未结水电账单阻断 -> 人工 RELEASE 仍被拒 -> 转验房流程扣款后结算
#   4. 未闭合争议阻断 -> 平台裁决闭合后再次巡检自动全额释放
#   5. 评估通过(READY)后、执行前状态发生变化 -> 暂停转 REVIEW_HUMAN, 绝不自动放款
#   6. 三检查通过自动全额退押; 释放后房东只能提交超时说明/新证据, 不能从已释放金额扣款
# 依赖: scripts/e2e-lib.sh。服务请以 OVERDUE_SWEEP_SECONDS=0 启动(由脚本手动触发巡检, 保证确定性)。
set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=e2e-lib.sh
source "$HERE/e2e-lib.sh"

TID=$(PSQL "SELECT id FROM users WHERE phone='13800000001'")
PID=$(PSQL "SELECT id FROM properties LIMIT 1")
SEQ=$(date +%s)

# new_lease <due_days> <final_days> <deposit> [rent]  签约+充押金, 输出 CID(LIVING)
new_lease() {
  local due=$1 final=$2 dep=$3 rent=${4:-5000}
  local body
  body=$(printf '{"property_id":"%s","tenant_id":"%s","start_date":"2026-01-01","end_date":"2026-12-31","rent_amount":%s,"deposit_amount":%s,"lease_no":"OV%s%s","notice_days":30,"inspection_due_days":%s,"final_release_days":%s}' \
    "$PID" "$TID" "$rent" "$dep" "$SEQ" "${RANDOM}" "$due" "$final")
  req POST /api/v1/contracts "$L" "$body"
  local cid; cid=$(echo "$REQ_BODY" | jqr id)
  req POST "/api/v1/contracts/$cid/deposit" "$T"
  echo "$cid"
}

# 模拟时间流逝: 回拨交房时间并同步自动释放任务的两个期限(基于合同约定重算)。
backdate_checkout() {
  local cid=$1 days=$2
  PSQL "UPDATE handovers SET occurred_at=now()-interval '$days days'
        WHERE contract_id='$cid' AND phase='CHECKOUT'" >/dev/null
  PSQL "UPDATE auto_release_jobs j
        SET first_deadline = h.occurred_at + make_interval(days => c.inspection_due_days),
            final_deadline = h.occurred_at + make_interval(days => c.final_release_days)
        FROM handovers h, contracts c
        WHERE h.contract_id=j.contract_id AND h.phase='CHECKOUT'
          AND c.id=j.contract_id AND j.contract_id='$cid'" >/dev/null
}

job_status() { PSQL "SELECT status FROM auto_release_jobs WHERE contract_id='$1'"; }
contract_status() { PSQL "SELECT status FROM contracts WHERE id='$1'"; }
job_reasons() { PSQL "SELECT block_reasons::text FROM auto_release_jobs WHERE contract_id='$1'"; }
escrow_bal() { PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$1'"; }

sweep_full() { req POST "/api/v1/admin/overdue-sweep" "$P"; }
sweep_eval_only() { req POST "/api/v1/admin/overdue-sweep?execute=false" "$P"; }

final_settle() {
  local cid=$1 exp_deduct=$2 exp_return=$3
  req POST "/api/v1/contracts/$cid/settlement/propose" "$L" '{}'
  assert_eq "[$cid] 生成结算单" 201 "$REQ_CODE"
  req POST "/api/v1/contracts/$cid/settlement/confirm" "$T"
  req POST "/api/v1/contracts/$cid/settlement/confirm" "$L"
  assert_eq "[$cid] 双方确认放款" 200 "$REQ_CODE"
  local deduct ret
  deduct=$(PSQL "SELECT paid_to_landlord FROM settlements WHERE contract_id='$cid'")
  ret=$(PSQL "SELECT returned_to_tenant FROM settlements WHERE contract_id='$cid'")
  assert_eq "[$cid] 支付房东=$exp_deduct" "$exp_deduct" "$deduct"
  assert_eq "[$cid] 退还租客=$exp_return" "$exp_return" "$ret"
}

CLEAN_INSPECTION='{"wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD","keys_status":"RETURNED","utility_status":"SETTLED","cleaning_status":"CLEAN"}'

info "0. 健康检查"
curl -sf "$BASE/healthz" >/dev/null && ok "GET /healthz 200" || bad "healthz"
req POST /api/v1/admin/overdue-sweep "$T"
assert_eq "租客不能触发平台巡检(403)" 403 "$REQ_CODE"

########## 场景 1: 首提醒/送达/打开 -> 逾期再提醒 -> 房东补验房, 任务取消 ##########
info "场景1: 首次提醒送达留痕 -> 逾期标记 -> 房东补开验房任务取消"
CID=$(new_lease 3 20 5000)
# 释放前不允许提交超时说明
req POST "/api/v1/contracts/$CID/post-release-claims" "$L" '{"explanation":"测试"}'
assert_eq "1 未释放时提交超时说明被拒(409)" 409 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
assert_eq "1 入住交割" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/checkout" "$T" '{"keys_returned":"钥匙2把"}'
assert_eq "1 退租交房" 201 "$REQ_CODE"
NR=$(PSQL "SELECT count(*) FROM inspection_reminders WHERE contract_id='$CID' AND kind='FIRST'")
assert_eq "1 交房即产生 FIRST 提醒" 1 "$NR"
DEL=$(PSQL "SELECT delivered FROM inspection_reminders WHERE contract_id='$CID' AND kind='FIRST'")
assert_eq "1 FIRST 通知已送达" t "$DEL"
OP=$(PSQL "SELECT opened FROM inspection_reminders WHERE contract_id='$CID' AND kind='FIRST'")
assert_eq "1 房东尚未打开验房单" f "$OP"
JS=$(job_status "$CID")
assert_eq "1 自动释放任务 PENDING" PENDING "$JS"
FD=$(PSQL "SELECT (first_deadline > now()) FROM auto_release_jobs WHERE contract_id='$CID'")
assert_eq "1 首次期限在未来" t "$FD"
req GET "/api/v1/contracts/$CID/reminders" "$L"
assert_eq "1 房东查询提醒列表" 200 "$REQ_CODE"
KINDS=$(echo "$REQ_BODY" | jqr items.0.kind)
assert_eq "1 提醒列表返回 FIRST" FIRST "$KINDS"

backdate_checkout "$CID" 4   # 已过首次期限(3天), 未到最终期限(20天)
sweep_full
assert_eq "1 巡检触发成功" 200 "$REQ_CODE"
INOV=$(echo "$REQ_BODY" | jqr overdue.0)
assert_eq "1 本次逾期列表命中本合同" "$CID" "$INOV"
ST=$(contract_status "$CID")
assert_eq "1 合同进入 REMINDING" REMINDING "$ST"
JS=$(job_status "$CID")
assert_eq "1 任务标记 OVERDUE" OVERDUE "$JS"
NR=$(PSQL "SELECT count(*) FROM inspection_reminders WHERE contract_id='$CID' AND kind='OVERDUE'")
assert_eq "1 产生 OVERDUE 二次提醒" 1 "$NR"
NN=$(PSQL "SELECT count(*) FROM notifications WHERE contract_id='$CID' AND template='inspection_reminder_overdue'")
assert_eq "1 二次提醒通知留痕" 1 "$NN"
# 房东打开验房单
req POST "/api/v1/contracts/$CID/inspection/opened" "$L"
assert_eq "1 房东打开验房单" 200 "$REQ_CODE"
NOP=$(echo "$REQ_BODY" | jqr newly_opened)
assert_eq "1 新打开提醒数=2(FIRST+OVERDUE)" 2 "$NOP"
req POST "/api/v1/contracts/$CID/inspection/opened" "$L"
assert_eq "1 重复打开幂等" 200 "$REQ_CODE"
NOP=$(echo "$REQ_BODY" | jqr newly_opened)
assert_eq "1 再次打开不重复留痕" 0 "$NOP"
# 房东在最终期限前补开验房 -> 任务取消
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
assert_eq "1 房东补开验房" 201 "$REQ_CODE"
JS=$(job_status "$CID")
assert_eq "1 自动释放任务 CANCELLED" CANCELLED "$JS"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" "$CLEAN_INSPECTION"
assert_eq "1 提交验房结论" 201 "$REQ_CODE"
final_settle "$CID" 0.00 5000.00

########## 场景 2: 未关闭报修阻断 -> 人工转验房 ##########
info "场景2: 未关闭报修 -> 暂停转人工(RELEASE_REVIEW) -> REOPEN_INSPECTION"
CID=$(new_lease 2 5 9000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}' >/dev/null
req POST "/api/v1/contracts/$CID/repairs" "$T" '{"title":"厨房漏水","description":"水槽下水管","category":"PLUMBING"}'
assert_eq "2 入住期间报修(未关闭)" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}'
assert_eq "2 退租交房" 201 "$REQ_CODE"
backdate_checkout "$CID" 6   # 超过最终期限
sweep_full
assert_eq "2 巡检成功" 200 "$REQ_CODE"
ST=$(contract_status "$CID")
assert_eq "2 合同 RELEASE_REVIEW" RELEASE_REVIEW "$ST"
JS=$(job_status "$CID")
assert_eq "2 任务 BLOCKED" BLOCKED "$JS"
job_reasons "$CID" | grep -q OPEN_REPAIR && ok "2 阻断原因含 OPEN_REPAIR" || bad "2 阻断原因: $(job_reasons "$CID")"
BAL=$(escrow_bal "$CID")
assert_eq "2 押金未释放, 余额仍=9000" 9000.00 "$BAL"
NR=$(PSQL "SELECT count(*) FROM inspection_reminders WHERE contract_id='$CID' AND kind='FINAL'")
assert_eq "2 最终期限已发 FINAL 提醒" 1 "$NR"
# 人工错误放行必须被拒(报修仍开)
req POST "/api/v1/admin/contracts/$CID/release-review" "$P" '{"decision":"RELEASE","note":"试试"}'
assert_eq "2 阻断未消除时人工 RELEASE 被拒(409)" 409 "$REQ_CODE"
# 转正常验房流程
req POST "/api/v1/admin/contracts/$CID/release-review" "$P" '{"decision":"REOPEN_INSPECTION","note":"先验房"}'
assert_eq "2 人工转验房" 200 "$REQ_CODE"
ST=$(contract_status "$CID")
assert_eq "2 合同回到 INSPECTING" INSPECTING "$ST"
RID=$(PSQL "SELECT id FROM repairs WHERE contract_id='$CID' LIMIT 1")
req POST "/api/v1/contracts/$CID/repairs/$RID/decision" "$L" \
  '{"status":"LANDLORD_RESPONSIBLE","cost_amount":0,"tenant_share":0,"decision_note":"水管老化房东责任"}'
assert_eq "2 报修定责房东(关闭报修)" 200 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" "$CLEAN_INSPECTION"
assert_eq "2 提交验房(0 扣款)" 201 "$REQ_CODE"
final_settle "$CID" 0.00 9000.00

########## 场景 3: 未结水电账单阻断 ##########
info "场景3: 未结水电账单 -> 阻断 -> 转验房计入扣款后结算"
CID=$(new_lease 2 5 6000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{
  "meters":[{"kind":"WATER","reading":10,"unit_price":5},{"kind":"ELECTRIC","reading":100,"unit_price":0.6}]}'
assert_eq "3 入住交割带读数" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/checkout" "$T" '{
  "meters":[{"kind":"WATER","reading":20,"unit_price":5},{"kind":"ELECTRIC","reading":200,"unit_price":0.6}]}'
assert_eq "3 退租交房(产生未结账单)" 201 "$REQ_CODE"
UBILL=$(PSQL "SELECT sum(billed_amount) FROM utility_readings WHERE contract_id='$CID' AND phase='CHECKOUT'")
assert_eq "3 未结水电账单=110.00" 110.00 "$UBILL"
backdate_checkout "$CID" 6
sweep_full
ST=$(contract_status "$CID")
assert_eq "3 合同 RELEASE_REVIEW" RELEASE_REVIEW "$ST"
job_reasons "$CID" | grep -q UNSETTLED_UTILITY_BILL && ok "3 阻断原因含 UNSETTLED_UTILITY_BILL" || bad "3 阻断原因: $(job_reasons "$CID")"
req POST "/api/v1/admin/contracts/$CID/release-review" "$P" '{"decision":"RELEASE","note":"账单还在"}'
assert_eq "3 账单未清时人工 RELEASE 被拒(409)" 409 "$REQ_CODE"
req POST "/api/v1/admin/contracts/$CID/release-review" "$P" '{"decision":"REOPEN_INSPECTION","note":"验房计欠费"}'
assert_eq "3 人工转验房" 200 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"UNPAID","cleaning_status":"CLEAN"}'
assert_eq "3 提交验房(含欠费)" 201 "$REQ_CODE"
CHID=$(PSQL "SELECT id FROM charge_items WHERE contract_id='$CID' AND kind='UTILITY_UNPAID'")
req POST "/api/v1/contracts/$CID/items/charge/$CHID/accept" "$T"
assert_eq "3 租客确认欠费扣款 110" 200 "$REQ_CODE"
final_settle "$CID" 110.00 5890.00

########## 场景 4: 未闭合争议阻断 -> 闭合后再次巡检自动释放 ##########
info "场景4: 未闭合争议 -> 阻断 -> 平台裁决闭合 -> 再次巡检自动全额释放"
CID=$(new_lease 2 5 7000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}' >/dev/null
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}' >/dev/null
backdate_checkout "$CID" 3   # 过首次期限(2), 未到最终(5)
sweep_full
assert_eq "4 第一次巡检标记逾期" 200 "$REQ_CODE"
ST=$(contract_status "$CID")
assert_eq "4 合同 REMINDING" REMINDING "$ST"
req POST "/api/v1/contracts/$CID/disputes/inspection-delay" "$T" '{"comment":"房东失联"}'
assert_eq "4 租客提起逾期争议" 201 "$REQ_CODE"
backdate_checkout "$CID" 6   # 到最终期限
sweep_full
assert_eq "4 第二次巡检(最终期限评估)" 200 "$REQ_CODE"
ST=$(contract_status "$CID")
assert_eq "4 争议打开时合同保持 DISPUTING" DISPUTING "$ST"
JS=$(job_status "$CID")
assert_eq "4 任务 BLOCKED" BLOCKED "$JS"
job_reasons "$CID" | grep -q OPEN_DISPUTE && ok "4 阻断原因含 OPEN_DISPUTE" || bad "4 阻断原因: $(job_reasons "$CID")"
BAL=$(escrow_bal "$CID")
assert_eq "4 押金未释放, 余额仍=7000" 7000.00 "$BAL"
DID=$(PSQL "SELECT id FROM disputes WHERE contract_id='$CID' AND reason='INSPECTION_DELAY' LIMIT 1")
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$P" '{"action":"ADJUDICATE","amount":0,"comment":"平台责令流程继续"}'
assert_eq "4 平台裁决闭合争议" 200 "$REQ_CODE"
sweep_full
REL=$(echo "$REQ_BODY" | jqr auto_released.0)
assert_eq "4 阻断消除后巡检自动释放" "$CID" "$REL"
ST=$(contract_status "$CID")
assert_eq "4 合同 RELEASED" RELEASED "$ST"
RET=$(PSQL "SELECT returned_to_tenant FROM settlements WHERE contract_id='$CID'")
AR=$(PSQL "SELECT auto_released FROM settlements WHERE contract_id='$CID'")
assert_eq "4 自动全额退还租客=7000" 7000.00 "$RET"
assert_eq "4 结算单标记 auto_released" t "$AR"

########## 场景 5: READY 后执行前状态变化 -> REVIEW_HUMAN ##########
info "场景5: 释放前最后一刻账单/争议状态变化 -> 暂停转人工, 绝不定时即放款"
CID=$(new_lease 2 5 8000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}' >/dev/null
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}' >/dev/null
backdate_checkout "$CID" 6
sweep_eval_only
assert_eq "5 仅评估巡检成功" 200 "$REQ_CODE"
JS=$(job_status "$CID")
assert_eq "5 三检查通过, 任务停在 READY(未放款)" READY "$JS"
BAL=$(escrow_bal "$CID")
assert_eq "5 READY 阶段押金未动=8000" 8000.00 "$BAL"
# 评估之后、执行之前, 租客刚好提起争议(状态发生变化)
req POST "/api/v1/contracts/$CID/disputes/inspection-delay" "$T" '{"comment":"突然有争议"}'
assert_eq "5 执行前争议被提起" 201 "$REQ_CODE"
sweep_full
JS=$(job_status "$CID")
assert_eq "5 任务转 REVIEW_HUMAN" REVIEW_HUMAN "$JS"
job_reasons "$CID" | grep -q STATE_CHANGED_BEFORE_RELEASE \
  && ok "5 记录释放前状态变化 STATE_CHANGED_BEFORE_RELEASE" \
  || bad "5 阻断原因: $(job_reasons "$CID")"
BAL=$(escrow_bal "$CID")
assert_eq "5 状态变化后押金仍未释放=8000" 8000.00 "$BAL"
ST=$(contract_status "$CID")
assert_eq "5 合同保持 DISPUTING(未被放款流程覆盖)" DISPUTING "$ST"
req POST "/api/v1/admin/contracts/$CID/release-review" "$P" '{"decision":"RELEASE","note":"争议还在"}'
assert_eq "5 争议未闭合时人工放行仍被拒(409)" 409 "$REQ_CODE"
DID=$(PSQL "SELECT id FROM disputes WHERE contract_id='$CID' AND reason='INSPECTION_DELAY' LIMIT 1")
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$P" '{"action":"ADJUDICATE","amount":0,"comment":"争议闭合"}'
assert_eq "5 平台闭合争议" 200 "$REQ_CODE"
req POST "/api/v1/admin/contracts/$CID/release-review" "$P" '{"decision":"RELEASE","note":"复核通过放行"}'
assert_eq "5 人工复核 RELEASE 成功" 200 "$REQ_CODE"
ST=$(contract_status "$CID")
assert_eq "5 合同 RELEASED" RELEASED "$ST"
RET=$(PSQL "SELECT returned_to_tenant FROM settlements WHERE contract_id='$CID'")
assert_eq "5 人工放行全额退还=8000" 8000.00 "$RET"

########## 场景 6: 自动释放后房东只能申诉不能扣款 ##########
info "场景6: 三检查通过自动退押 -> 房东只能提交超时说明/新证据, 不能扣款"
CID=$(new_lease 2 5 10000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}' >/dev/null
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}' >/dev/null
backdate_checkout "$CID" 6
sweep_full
REL=$(echo "$REQ_BODY" | jqr auto_released.0)
assert_eq "6 自动释放命中" "$CID" "$REL"
LED=$(PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$CID' AND txn_type='RELEASE_TO_TENANT' AND amount=10000")
assert_eq "6 台账记录全额 RELEASE_TO_TENANT" 1 "$LED"
EV=$(PSQL "SELECT count(*) FROM lease_events WHERE contract_id='$CID' AND event_type='AutoDepositReleased'")
assert_eq "6 记录 AutoDepositReleased 事件" 1 "$EV"
NN=$(PSQL "SELECT count(*) FROM notifications WHERE contract_id='$CID' AND template='deposit_auto_released_landlord'")
assert_eq "6 通知房东押金已自动释放" 1 "$NN"
# 房东不能再走结算扣款
req POST "/api/v1/contracts/$CID/settlement/propose" "$L" '{}'
assert_eq "6 释放后房东无法生成结算扣款(409)" 409 "$REQ_CODE"
# 租客不能替房东提交超时说明
req POST "/api/v1/contracts/$CID/post-release-claims" "$T" '{"explanation":"我是租客"}'
assert_eq "6 租客不能提交房东超时说明(403)" 403 "$REQ_CODE"
# 房东提交超时说明 + 新证据(登记一条 OTHER 证据)
req POST "/api/v1/contracts/$CID/evidence" "$L" '{"category":"OTHER","sha256":"0000000000000000000000000000000000000000000000000000000000000000","size_bytes":12,"storage_uri":"oss://late-note.txt","caption":"超时情况说明附件"}'
assert_eq "6 房东登记新证据" 201 "$REQ_CODE"
EVID=$(echo "$REQ_BODY" | jqr id)
req POST "/api/v1/contracts/$CID/post-release-claims" "$L" \
  "{\"explanation\":\"出差错过验房, 事后发现墙面损坏并补了票据\",\"evidence_ids\":[\"$EVID\"]}"
assert_eq "6 房东提交超时说明与新证据" 201 "$REQ_CODE"
CLID=$(echo "$REQ_BODY" | jqr claim_id)
CST=$(PSQL "SELECT status FROM post_release_claims WHERE id='$CLID'")
assert_eq "6 申诉状态 OPEN" OPEN "$CST"
req GET "/api/v1/contracts/$CID/timeline" "$L"
assert_eq "6 timeline 含释放后申诉" 200 "$REQ_CODE"
NPC=$(echo "$REQ_BODY" | jqr post_release_claims.0.id)
assert_eq "6 timeline 返回申诉 id" "$CLID" "$NPC"
# 平台裁决支持房东, 但不自动扣款
req POST "/api/v1/admin/post-release-claims/$CLID/decision" "$P" '{"decision":"UPHELD","note":"情况部分属实, 建议另案追偿"}'
assert_eq "6 平台裁决 UPHELD" 200 "$REQ_CODE"
AD=$(echo "$REQ_BODY" | jqr auto_deducted)
assert_eq "6 裁决不自动划扣已释放金额" False "$AD"
CST=$(PSQL "SELECT status FROM post_release_claims WHERE id='$CLID'")
assert_eq "6 申诉终态 UPHELD" UPHELD "$CST"
BAL=$(escrow_bal "$CID")
REL_TOT=$(PSQL "SELECT released_total FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "6 托管余额仍为 0(无资金回流)" 0.00 "$BAL"
assert_eq "6 累计释放仍=10000(未从租客处扣回)" 10000.00 "$REL_TOT"
NLED=$(PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$CID' AND txn_type='DEDUCT'")
assert_eq "6 释放后无任何 DEDUCT 台账记录" 0 "$NLED"

echo
echo "超时验房场景结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
