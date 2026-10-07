#!/usr/bin/env bash
# 房东超时验房处理 端到端验证:
#   A. 退房即建任务 + 首次提醒(站内+短信) + 房东打开验房单 + 正常验房任务 COMPLETED
#   B. 超首次期限 -> 二次提醒 + OVERDUE; 短信送达回执; 逾期后打开只记 opened_at
#   C. 最终期限门禁: 先留快照, 复核一致 -> 自动全额释放(OVERDUE_AUTO);
#      释放后房东只能补交超时说明/新证据(账外主张, 不能扣已释放金额)
#   D. 未关闭报修 -> 转人工; DB 触发器闸门; 定责后人工释放(OVERDUE_MANUAL)
#   E. 释放前一刻租客提起逾期争议(状态突变) -> 暂停自动释放 -> 平台 REINSPECT
#   F. 提前退租 -> 最终期限转人工 -> REINSPECT
#   G. 退租水电已计费但未定论 -> 门禁按未结账单拦截(应用层 + DB 触发器)
# 依赖: scripts/e2e-lib.sh。建议以 SCHEDULER_ENABLED=false 启动服务, 用巡检端点手动驱动。
set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=e2e-lib.sh
source "$HERE/e2e-lib.sh"

TID=$(PSQL "SELECT id FROM users WHERE phone='13800000001'")
PID=$(PSQL "SELECT id FROM properties WHERE address='建国路 88 号 SOHO 现代城 3-1207' LIMIT 1")
[ -z "$PID" ] && PID=$(PSQL "SELECT id FROM properties LIMIT 1")
SEQ=$(date +%s)

# new_lease <rent> <deposit> <inspection_due_days> [final_release_days]
new_lease() {
  local rent=$1 dep=$2 due=${3:-2} final=${4:-2}
  local body cid
  body=$(printf '{"property_id":"%s","tenant_id":"%s","start_date":"2026-01-01","end_date":"2026-12-31","rent_amount":%s,"deposit_amount":%s,"lease_no":"OV%s%s","notice_days":30,"inspection_due_days":%s,"final_release_days":%s}' \
    "$PID" "$TID" "$rent" "$dep" "$SEQ" "${RANDOM}" "$due" "$final")
  req POST /api/v1/contracts "$L" "$body"; cid=$(echo "$REQ_BODY" | jqr id)
  req POST "/api/v1/contracts/$cid/deposit" "$T"
  echo "$cid"
}

# 把任务的首次/最终期限回拨到过去(任务期限在退房时按 occurred_at 计算, 这里直接模拟时间流逝)
backdate_task() {
  local cid=$1 firstAgo=$2 finalAgo=$3
  PSQL "UPDATE inspection_tasks SET first_deadline=now()-interval '$firstAgo days',
        final_deadline=now()-interval '$finalAgo days' WHERE contract_id='$cid'" >/dev/null
  PSQL "UPDATE handovers SET occurred_at=now()-interval '$firstAgo days'
        WHERE contract_id='$cid' AND phase='CHECKOUT'" >/dev/null
}

########## 场景 A: 首次提醒 / 打开验房单 / 正常验房关闭任务 ##########
info "场景A: 退房 -> 首次提醒 -> 打开验房单 -> 房东正常验房(COMPLETED)"
CID=$(new_lease 5000 9000 7 7)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/checkout" "$T" '{"keys_returned":"钥匙2把"}'
assert_eq "A 退租交房" 201 "$REQ_CODE"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "A 超时任务创建为 PENDING" PENDING "$TST"
NINBOX=$(PSQL "SELECT count(*) FROM notifications WHERE contract_id='$CID' AND template='inspection_reminder' AND channel='INBOX'")
NSMS=$(PSQL "SELECT count(*) FROM notifications WHERE contract_id='$CID' AND template='inspection_reminder' AND channel='SMS'")
assert_eq "A 首次提醒站内信 1 条" 1 "$NINBOX"
assert_eq "A 首次提醒短信 1 条" 1 "$NSMS"
DST=$(PSQL "SELECT delivery_status FROM notifications WHERE contract_id='$CID' AND template='inspection_reminder' AND channel='INBOX'")
assert_eq "A 站内信直接 DELIVERED" DELIVERED "$DST"
DST=$(PSQL "SELECT delivery_status FROM notifications WHERE contract_id='$CID' AND template='inspection_reminder' AND channel='SMS'")
assert_eq "A 短信下发后 SENT(待回执)" SENT "$DST"
NEV=$(PSQL "SELECT count(*) FROM inspection_task_events WHERE contract_id='$CID' AND event_type IN ('CREATED','FIRST_REMINDER')")
assert_eq "A 任务流水 CREATED+FIRST_REMINDER" 2 "$NEV"

# 房东打开验房单
req POST "/api/v1/contracts/$CID/inspection/view" "$L"
assert_eq "A 房东打开验房单" 200 "$REQ_CODE"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "A 任务转为 OPENED" OPENED "$TST"
OPN=$(PSQL "SELECT CASE WHEN opened_at IS NOT NULL THEN 'true' ELSE 'false' END FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "A 记录打开时间" true "$OPN"
NOPEN=$(PSQL "SELECT count(*) FROM notifications WHERE contract_id='$CID' AND user_id=(SELECT landlord_id FROM contracts WHERE id='$CID') AND opened_at IS NOT NULL")
[ "$NOPEN" -ge 1 ] && ok "A 房东侧提醒标记已打开($NOPEN 条)" || bad "A 提醒未标记打开"
req POST "/api/v1/contracts/$CID/inspection/view" "$L"
assert_eq "A 重复打开幂等" 200 "$REQ_CODE"

# 房东正常发起验房 -> 任务 COMPLETED, 回到主流程
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
assert_eq "A 房东发起验房" 201 "$REQ_CODE"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "A 任务 COMPLETED" COMPLETED "$TST"

########## 场景 B: 逾期二次提醒 + 送达回执 + 逾期后打开 ##########
info "场景B: 超首次期限 -> 二次提醒并 OVERDUE; 短信回执; 逾期打开不回退状态"
CID=$(new_lease 5000 9000 2 5)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}'
backdate_task "$CID" 1 0   # 首次期限已过1天, 最终期限仍在未来
req POST /api/v1/admin/overdue-reminders/scan "$P"
assert_eq "B 逾期提醒巡检" 200 "$REQ_CODE"
HIT=$(echo "$REQ_BODY" | jqr reminded.0)
assert_eq "B 巡检命中本合同" "$CID" "$HIT"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "B 任务标记 OVERDUE" OVERDUE "$TST"
N2=$(PSQL "SELECT count(*) FROM notifications WHERE contract_id='$CID' AND template='inspection_overdue_notice'")
assert_eq "B 房东二次提醒(站内+短信) 2 条" 2 "$N2"
OD=$(PSQL "SELECT CASE WHEN overdue_at IS NOT NULL THEN 'true' ELSE 'false' END FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "B 记录逾期时间" true "$OD"
NEV=$(PSQL "SELECT count(*) FROM inspection_task_events WHERE contract_id='$CID' AND event_type='OVERDUE_MARKED'")
assert_eq "B OVERDUE_MARKED 流水" 1 "$NEV"
# 最终期限未到, 最终释放巡检不应处理它
req POST /api/v1/admin/final-release/scan "$P"
REL=$(echo "$REQ_BODY" | jqr auto_released.0)
[ -z "$REL" ] && ok "B 最终期限未到不释放" || bad "B 不该释放却释放了 $REL"

# 短信送达回执
SMS1=$(PSQL "SELECT id FROM notifications WHERE contract_id='$CID' AND template='inspection_reminder' AND channel='SMS'")
req POST "/api/v1/admin/notifications/$SMS1/delivery" "$P" '{"delivered":true}'
assert_eq "B 首次短信送达回执" 200 "$REQ_CODE"
DST=$(PSQL "SELECT delivery_status FROM notifications WHERE id=$SMS1")
assert_eq "B 首次短信 DELIVERED" DELIVERED "$DST"
SMS2=$(PSQL "SELECT id FROM notifications WHERE contract_id='$CID' AND template='inspection_overdue_notice' AND channel='SMS'")
req POST "/api/v1/admin/notifications/$SMS2/delivery" "$P" '{"delivered":false,"failure_reason":"手机关机"}'
assert_eq "B 二次短信送达失败回执" 200 "$REQ_CODE"
DST=$(PSQL "SELECT delivery_status FROM notifications WHERE id=$SMS2")
assert_eq "B 二次短信 FAILED" FAILED "$DST"
NEV=$(PSQL "SELECT count(*) FROM inspection_task_events WHERE contract_id='$CID' AND event_type='REMINDER_DELIVERED'")
assert_eq "B 送达回执留痕 2 条" 2 "$NEV"
# 逾期后打开: 状态保持 OVERDUE, 仅记录 opened_at
req POST "/api/v1/contracts/$CID/inspection/view" "$L"
assert_eq "B 逾期后打开验房单" 200 "$REQ_CODE"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "B 逾期后打开状态仍 OVERDUE" OVERDUE "$TST"
# 通知列表
req GET /api/v1/notifications "$L"
assert_eq "B 房东通知列表" 200 "$REQ_CODE"
NN=$(echo "$REQ_BODY" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['items']))" 2>/dev/null)
[ "${NN:-0}" -ge 3 ] && ok "B 房东可查到全部提醒($NN 条)" || bad "B 通知列表数量异常: $NN"

########## 场景 C: 最终期限 -> 快照 -> 自动全额释放 -> 释放后账外主张 ##########
info "场景C: 门禁干净 -> 先快照后复核 -> 自动全额释放; 释放后只能账外主张"
CID=$(new_lease 5000 8000 2 2)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/checkout" "$T" '{"keys_returned":"钥匙2把"}'
backdate_task "$CID" 4 2
req POST /api/v1/admin/overdue-reminders/scan "$P" >/dev/null
# 第一轮最终巡检: 只留快照, 不释放
req POST /api/v1/admin/final-release/scan "$P"
assert_eq "C 最终巡检(快照轮)" 200 "$REQ_CODE"
SNAP=$(echo "$REQ_BODY" | jqr snapshots_taken.0)
assert_eq "C 本轮记录门禁快照" "$CID" "$SNAP"
CST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "C 快照轮合同仍 CHECKOUT_PENDING" CHECKOUT_PENDING "$CST"
GS=$(PSQL "SELECT CASE WHEN gate_snapshot IS NOT NULL THEN 'true' ELSE 'false' END FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "C 门禁快照已落库" true "$GS"
SID_EXISTS=$(PSQL "SELECT count(*) FROM settlements WHERE contract_id='$CID'")
assert_eq "C 快照轮不生成结算单" 0 "$SID_EXISTS"
# 第二轮: 指纹一致且干净 -> 自动释放
req POST /api/v1/admin/final-release/scan "$P"
REL=$(echo "$REQ_BODY" | jqr auto_released.0)
assert_eq "C 复核轮自动释放本合同" "$CID" "$REL"
CST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "C 合同 RELEASED" RELEASED "$CST"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "C 任务 AUTO_RELEASED" AUTO_RELEASED "$TST"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
RET=$(PSQL "SELECT returned_to_tenant FROM settlements WHERE contract_id='$CID'")
PAID=$(PSQL "SELECT paid_to_landlord FROM settlements WHERE contract_id='$CID'")
MODE=$(PSQL "SELECT release_mode FROM settlements WHERE contract_id='$CID'")
REL_TOT=$(PSQL "SELECT released_total FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "C 托管余额清零" 0.00 "$BAL"
assert_eq "C 全额退还租客 8000" 8000.00 "$RET"
assert_eq "C 支付房东 0" 0.00 "$PAID"
assert_eq "C 释放模式 OVERDUE_AUTO" OVERDUE_AUTO "$MODE"
assert_eq "C 累计释放 8000" 8000.00 "$REL_TOT"
LED=$(PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$CID' AND txn_type='RELEASE_TO_TENANT'")
assert_eq "C 台账记录 RELEASE_TO_TENANT" 1 "$LED"
LED_BEFORE_CLAIM=$(PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$CID'")

# 释放后: 房东不能再发起验房 / 生成结算扣款
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
assert_eq "C 释放后禁止再发起验房(409)" 409 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "C 释放后禁止再生成结算(409)" 409 "$REQ_CODE"

# 登记新证据(释放后仍允许登记证据)
EV=$(printf '%064d' 0)
req POST "/api/v1/contracts/$CID/evidence" "$L" "{
  \"category\":\"INSPECTION\",\"sha256\":\"$EV\",\"size_bytes\":1024,
  \"storage_uri\":\"oss://late/wall-crack.jpg\",\"caption\":\"释放后发现墙面破损的照片\"}"
assert_eq "C 释放后登记证据" 201 "$REQ_CODE"
EVID=$(echo "$REQ_BODY" | jqr id)
# 房东提交超时说明 + 新证据(账外主张)
req POST "/api/v1/contracts/$CID/late-claims" "$L" "{
  \"explanation\":\"交房后出差被隔离未能及时验房, 返回后发现墙面破损\",
  \"evidence_ids\":[\"$EVID\"],\"claimed_amount\":500}"
assert_eq "C 房东提交超时说明/新证据" 201 "$REQ_CODE"
CLM=$(echo "$REQ_BODY" | jqr claim_id)
[ -n "$CLM" ] && ok "C 账外主张 id=$CLM" || bad "C 账外主张 id 为空"
LED_AFTER_CLAIM=$(PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$CID'")
assert_eq "C 账外主张不动资金台账" "$LED_BEFORE_CLAIM" "$LED_AFTER_CLAIM"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "C 账外主张后托管余额仍为 0" 0.00 "$BAL"
# 租客不能提交账外主张
req POST "/api/v1/contracts/$CID/late-claims" "$T" '{"explanation":"我是租客","claimed_amount":1}'
assert_eq "C 租客不能提交账外主张(403)" 403 "$REQ_CODE"
# 平台部分支持 300: 只是账外追讨依据, 依然不划扣
req POST "/api/v1/contracts/$CID/late-claims/$CLM/review" "$P" \
  '{"upheld":true,"amount":300,"note":"墙面证据有效,支持另行追讨300元,已释放押金不划扣"}'
assert_eq "C 平台复核账外主张" 200 "$REQ_CODE"
CST_CLAIM=$(PSQL "SELECT status FROM late_inspection_claims WHERE id='$CLM'")
AWARD=$(PSQL "SELECT award_amount FROM late_inspection_claims WHERE id='$CLM'")
assert_eq "C 主张状态 RESOLVED_AWARD" RESOLVED_AWARD "$CST_CLAIM"
assert_eq "C 账外支持金额 300(不扣款)" 300.00 "$AWARD"
LED_FINAL=$(PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$CID'")
assert_eq "C 复核后资金台账仍无变化" "$LED_BEFORE_CLAIM" "$LED_FINAL"
BAL=$(PSQL "SELECT balance+hard_hold+disputed_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "C 托管三层金额始终为 0" 0.00 "$BAL"

########## 场景 D: 未关闭报修 -> 转人工; DB 闸门; 定责后人工释放 ##########
info "场景D: 存在未关闭报修 -> 暂停转人工; 定责后平台人工释放"
CID=$(new_lease 5000 7000 2 2)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/repairs" "$T" '{"title":"热水器漏水","description":"待房东处理","category":"PLUMBING"}'
assert_eq "D 租客报修(未关闭)" 201 "$REQ_CODE"
RID=$(echo "$REQ_BODY" | jqr repair_id)
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}'
backdate_task "$CID" 4 2
req POST /api/v1/admin/overdue-reminders/scan "$P" >/dev/null
req POST /api/v1/admin/final-release/scan "$P"
assert_eq "D 最终巡检" 200 "$REQ_CODE"
MR=$(echo "$REQ_BODY" | jqr manual_review.0)
assert_eq "D 存在未关闭报修 -> 转人工" "$CID" "$MR"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
BR=$(PSQL "SELECT blocked_reason FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "D 任务 MANUAL_REVIEW" MANUAL_REVIEW "$TST"
assert_eq "D 拦截原因 OPEN_REPAIRS" OPEN_REPAIRS "$BR"
SNAP_REP=$(PSQL "SELECT gate_snapshot->>'open_repairs' FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "D 门禁快照记录未关闭报修数=1" 1 "$SNAP_REP"
# 非平台不能处理人工复核
req POST "/api/v1/contracts/$CID/manual-review" "$L" '{"outcome":"RELEASE"}'
assert_eq "D 房东不能做人工复核(403)" 403 "$REQ_CODE"
# 门禁未过时平台要求释放 -> 409
req POST "/api/v1/contracts/$CID/manual-review" "$P" '{"outcome":"RELEASE","note":"先放了再说"}'
assert_eq "D 门禁未通过禁止人工释放(409)" 409 "$REQ_CODE"
# DB 级闸门: 绕过应用层直接确认 OVERDUE_AUTO 结算也必须回滚
DEP=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
PSQL "INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,paid_to_landlord,status,release_mode)
      VALUES('$CID',$DEP,0,$DEP,0,'PROPOSED','OVERDUE_AUTO')" >/dev/null
GUARD=$(PSQL "UPDATE settlements SET status='CONFIRMED' WHERE contract_id='$CID'" 2>&1 || true)
echo "$GUARD" | grep -q CONFLICT_OPEN_REPAIR \
  && ok "D DB 触发器拦截有未关闭报修的超时释放" || bad "D DB 闸门未生效: $GUARD"
PSQL "DELETE FROM settlements WHERE contract_id='$CID'" >/dev/null
# 房东定责(报修闭合): 房东责任 -> 干净
req POST "/api/v1/contracts/$CID/repairs/$RID/decision" "$L" \
  '{"status":"LANDLORD_RESPONSIBLE","cost_amount":0,"tenant_share":0,"decision_note":"热水器老化,房东责任"}'
assert_eq "D 房东定责闭合报修" 200 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/manual-review" "$P" '{"outcome":"RELEASE","note":"报修已闭合,无其他未决项"}'
assert_eq "D 人工复核释放" 200 "$REQ_CODE"
CST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
MODE=$(PSQL "SELECT release_mode FROM settlements WHERE contract_id='$CID'")
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "D 合同 RELEASED" RELEASED "$CST"
assert_eq "D 释放模式 OVERDUE_MANUAL" OVERDUE_MANUAL "$MODE"
assert_eq "D 托管余额清零" 0.00 "$BAL"

########## 场景 E: 释放前状态突变 -> 暂停转人工 -> REINSPECT ##########
info "场景E: 快照后租客提起逾期争议(状态突变) -> 暂停自动释放 -> 平台代开验房"
CID=$(new_lease 5000 6000 2 3)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}'
backdate_task "$CID" 5 1
req POST /api/v1/admin/overdue-reminders/scan "$P" >/dev/null
req POST /api/v1/admin/final-release/scan "$P" >/dev/null
SNAP=$(PSQL "SELECT CASE WHEN gate_snapshot IS NOT NULL THEN 'true' ELSE 'false' END FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "E 已留门禁快照" true "$SNAP"
# 释放前一刻: 租客就逾期不验房提起争议(状态突变: open_disputes 0->1, 合同->DISPUTING)
req POST "/api/v1/contracts/$CID/disputes/inspection-delay" "$T" '{"comment":"最终期限前我要申诉"}'
assert_eq "E 快照后租客提起逾期争议" 201 "$REQ_CODE"
# 复核轮发现指纹变化 -> 暂停转人工
req POST /api/v1/admin/final-release/scan "$P"
MR=$(echo "$REQ_BODY" | jqr manual_review.0)
assert_eq "E 状态突变暂停转人工" "$CID" "$MR"
BR=$(PSQL "SELECT blocked_reason FROM inspection_tasks WHERE contract_id='$CID'")
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "E 拦截原因 STATE_CHANGED" STATE_CHANGED "$BR"
assert_eq "E 任务 MANUAL_REVIEW" MANUAL_REVIEW "$TST"
CST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "E 押金未释放, 合同保持 DISPUTING" DISPUTING "$CST"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "E 押金仍在托管 6000" 6000.00 "$BAL"
# 平台复核: 代开验房, 转正常流程
req POST "/api/v1/contracts/$CID/manual-review" "$P" '{"outcome":"REINSPECT","note":"转验房流程处理争议"}'
assert_eq "E 平台复核 REINSPECT" 200 "$REQ_CODE"
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
CST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
ND=$(PSQL "SELECT count(*) FROM disputes WHERE contract_id='$CID' AND reason='INSPECTION_DELAY' AND status='RESOLVED_AGREEMENT'")
assert_eq "E 任务 SUPERSEDED" SUPERSEDED "$TST"
assert_eq "E 合同进入 INSPECTING" INSPECTING "$CST"
assert_eq "E 逾期争议随代开验房闭合" 1 "$ND"

########## 场景 F: 提前退租 -> 最终期限转人工 -> REINSPECT ##########
info "场景F: 提前退租未处理 -> 最终期限不得自动释放 -> 人工转验房"
CID=$(new_lease 6000 5000 2 2)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/checkout" "$T" '{"early":true,"keys_returned":"钥匙2把"}'
assert_eq "F 提前退租交房" 201 "$REQ_CODE"
backdate_task "$CID" 4 2
req POST /api/v1/admin/overdue-reminders/scan "$P" >/dev/null
TST=$(PSQL "SELECT status FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "F 提前退租同样被二次提醒 OVERDUE" OVERDUE "$TST"
req POST /api/v1/admin/final-release/scan "$P"
MR=$(echo "$REQ_BODY" | jqr manual_review.0)
assert_eq "F 提前退租转人工" "$CID" "$MR"
BR=$(PSQL "SELECT blocked_reason FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "F 拦截原因 EARLY_TERMINATION" EARLY_TERMINATION "$BR"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "F 定时到了也没有释放押金" 5000.00 "$BAL"
req POST "/api/v1/contracts/$CID/manual-review" "$P" '{"outcome":"REINSPECT","note":"提前退租需验房测算空置损失"}'
assert_eq "F 平台代开验房" 200 "$REQ_CODE"
CST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
EARLY=$(PSQL "SELECT CASE WHEN early_termination THEN 'true' ELSE 'false' END FROM inspections WHERE contract_id='$CID'")
assert_eq "F 合同进入 INSPECTING" INSPECTING "$CST"
assert_eq "F 验房单保留提前退租标记" true "$EARLY"

########## 场景 G: 退租水电已计费但未定论 -> 按未结账单拦截 ##########
info "场景G: 退租水电已计费但没有定论费用项/验房结清 -> 最终期限按未结账单拦截"
CID=$(new_lease 5000 7500 2 2)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{
  "meters":[{"kind":"WATER","reading":100,"unit_price":5},{"kind":"ELECTRIC","reading":2000,"unit_price":0.6}]}'
assert_eq "G 入住交割" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/checkout" "$T" '{
  "keys_returned":"钥匙2把",
  "meters":[{"kind":"WATER","reading":112,"unit_price":5},{"kind":"ELECTRIC","reading":2300,"unit_price":0.6}]}'
assert_eq "G 退租交房" 201 "$REQ_CODE"
UTIL=$(PSQL "SELECT COALESCE(sum(billed_amount),0) FROM utility_readings WHERE contract_id='$CID' AND phase='CHECKOUT'")
assert_eq "G 退租水电应结=240.00" 240.00 "$UTIL"
backdate_task "$CID" 4 2
req POST /api/v1/admin/overdue-reminders/scan "$P" >/dev/null
req POST /api/v1/admin/final-release/scan "$P"
assert_eq "G 最终巡检" 200 "$REQ_CODE"
MR=$(echo "$REQ_BODY" | jqr manual_review.0)
assert_eq "G 未结水电账单 -> 转人工" "$CID" "$MR"
BR=$(PSQL "SELECT blocked_reason FROM inspection_tasks WHERE contract_id='$CID'")
assert_eq "G 拦截原因 UNSETTLED_BILLS" UNSETTLED_BILLS "$BR"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "G 押金没有因定时到达而释放" 7500.00 "$BAL"
# DB 级闸门同样拦截未结账单
DEP=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
PSQL "INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,paid_to_landlord,status,release_mode)
      VALUES('$CID',$DEP,0,$DEP,0,'PROPOSED','OVERDUE_AUTO')" >/dev/null
GUARD=$(PSQL "UPDATE settlements SET status='CONFIRMED' WHERE contract_id='$CID'" 2>&1 || true)
echo "$GUARD" | grep -q CONFLICT_UNSETTLED_BILL \
  && ok "G DB 触发器拦截未结账单的超时释放" || bad "G DB 账单闸门未生效: $GUARD"
PSQL "DELETE FROM settlements WHERE contract_id='$CID'" >/dev/null

echo
echo "超时验房场景结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
