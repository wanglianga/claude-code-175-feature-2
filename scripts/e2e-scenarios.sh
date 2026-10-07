#!/usr/bin/env bash
# 补充争议场景端到端验证:
#   A. 自然磨损/入住前损坏免责
#   B. 家电维修责任不清 -> 申诉 -> 平台裁决全额驳回
#   C. 清洁费争议 -> 双方协商折价
#   D. 租客提前退租 -> 空置损失
#   E. 钥匙丢失 -> 申诉后撤回(按原主张)
#   F. 房东迟迟不验房 -> 平台催办自动开验房
# 依赖: scripts/e2e-lib.sh
set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=e2e-lib.sh
source "$HERE/e2e-lib.sh"

TID=$(PSQL "SELECT id FROM users WHERE phone='13800000001'")
PID=$(PSQL "SELECT id FROM properties LIMIT 1")
SEQ=$(date +%s)

# new_lease <rent> <deposit> <inspection_due_days>  输出 CID(已充押金, LIVING)
new_lease() {
  local rent=$1 dep=$2 due=${3:-7}
  local body code
  body=$(printf '{"property_id":"%s","tenant_id":"%s","start_date":"2026-01-01","end_date":"2026-12-31","rent_amount":%s,"deposit_amount":%s,"lease_no":"LT%s%s","notice_days":30,"inspection_due_days":%s}' \
    "$PID" "$TID" "$rent" "$dep" "$SEQ" "${RANDOM}" "$due")
  req POST /api/v1/contracts "$L" "$body"; local cid; cid=$(echo "$REQ_BODY" | jqr id)
  req POST "/api/v1/contracts/$cid/deposit" "$T"
  echo "$cid"
}

finish_to_proposed() {
  local cid=$1
  req POST "/api/v1/contracts/$cid/checkout" "$T" '{"keys_returned":"钥匙2把"}'
  req POST "/api/v1/contracts/$cid/inspection/start" "$L"
}

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

########## 场景 A: 入住前已损坏 + 自然磨损, 全部免责 ##########
info "场景A: 入住前损坏/自然磨损 -> 0 扣款"
CID=$(new_lease 5000 8000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{
  "inventory":[
    {"name":"次卧墙面","category":"WALL","checkin_state":"DAMAGED","checkin_note":"入住前已有霉斑"},
    {"name":"客厅地板","category":"FLOOR","checkin_state":"WORN","checkin_note":"老地板磨损"}
  ]}'
assert_eq "A 入住交割" 201 "$REQ_CODE"
finish_to_proposed "$CID"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"DAMAGED","floor_status":"DAMAGED","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"SETTLED","cleaning_status":"CLEAN",
  "wall_repair_fee":900,"floor_repair_fee":1200}'
assert_eq "A 提交验房" 201 "$REQ_CODE"
TOTAL=$(echo "$REQ_BODY" | jqr deductible_total)
assert_eq "A 建议扣款合计=0(均有入住前基线)" 0 "$TOTAL"
final_settle "$CID" 0.00 8000.00

########## 场景 B: 家电责任不清, 平台裁决租客不承担 ##########
info "场景B: 家电维修责任不清 -> 裁决全额驳回"
CID=$(new_lease 5000 9000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{
  "inventory":[{"name":"洗衣机","category":"APPLIANCE","checkin_state":"GOOD"}]}'
finish_to_proposed "$CID"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"DAMAGED",
  "keys_status":"RETURNED","utility_status":"SETTLED","cleaning_status":"CLEAN",
  "appliance_fee":2000}'
assert_eq "B 提交验房" 201 "$REQ_CODE"
CODE=$(PSQL "SELECT code FROM findings WHERE contract_id='$CID' AND proposed_amount>0 LIMIT 1")
assert_eq "B 生成责任不清建议 APPLIANCE_RESPONSIBILITY_UNCLEAR" APPLIANCE_RESPONSIBILITY_UNCLEAR "$CODE"
FID=$(PSQL "SELECT id FROM findings WHERE contract_id='$CID' AND code='APPLIANCE_RESPONSIBILITY_UNCLEAR'")
req POST "/api/v1/contracts/$CID/items/finding/$FID/contest" "$T" '{"comment":"使用8年自然老化"}'
assert_eq "B 租客申诉" 201 "$REQ_CODE"
DID=$(echo "$REQ_BODY" | jqr dispute_id)
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$L" '{"action":"REBUT","comment":"入住时完好"}'
assert_eq "B 房东反驳" 200 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$P" '{"action":"ADJUDICATE","amount":0,"comment":"无维修记录且超使用年限,举证不足"}'
assert_eq "B 平台裁决 0 元" 200 "$REQ_CODE"
IST=$(PSQL "SELECT status FROM findings WHERE id='$FID'")
assert_eq "B 扣款项被 OVERTURNED" OVERTURNED "$IST"
final_settle "$CID" 0.00 9000.00

########## 场景 C: 清洁费争议, 协商 600 -> 200 ##########
info "场景C: 清洁费争议 -> 协商折价"
CID=$(new_lease 5000 7000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
finish_to_proposed "$CID"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"SETTLED","cleaning_status":"NEEDS_CLEANING",
  "cleaning_fee":600}'
CHID=$(PSQL "SELECT id FROM charge_items WHERE contract_id='$CID' AND kind='CLEANING'")
req POST "/api/v1/contracts/$CID/items/charge/$CHID/contest" "$T" '{"reason":"CLEANING_FEE","comment":"报价虚高"}'
assert_eq "C 清洁费申诉" 201 "$REQ_CODE"
DID=$(echo "$REQ_BODY" | jqr dispute_id)
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$L" '{"action":"PARTIES_AGREE","amount":200,"comment":"协商清洁费200"}'
assert_eq "C 协商一致 200 元" 200 "$REQ_CODE"
final_settle "$CID" 200.00 6800.00

########## 场景 D: 提前退租 ##########
info "场景D: 租客提前退租 -> 空置损失按通知期"
CID=$(new_lease 6000 6000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/checkout" "$T" '{"early":true,"keys_returned":"钥匙2把"}'
assert_eq "D 提前退租交房" 201 "$REQ_CODE"
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "D 状态 EARLY_TERMINATED" EARLY_TERMINATED "$ST"
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"SETTLED","cleaning_status":"CLEAN"}'
assert_eq "D 提交验房" 201 "$REQ_CODE"
TOTAL=$(echo "$REQ_BODY" | jqr deductible_total)
# notice_days=30, rent=6000 -> 6000*30/30 = 6000
assert_eq "D 空置损失建议=6000" 6000 "$TOTAL"
FID=$(PSQL "SELECT id FROM findings WHERE contract_id='$CID' AND code='EARLY_TERMINATION_LIABILITY'")
req POST "/api/v1/contracts/$CID/items/finding/$FID/accept" "$T"
assert_eq "D 租客确认空置损失" 200 "$REQ_CODE"
final_settle "$CID" 6000.00 0.00

########## 场景 E: 钥匙丢失, 申诉后撤回 ##########
info "场景E: 钥匙丢失 -> 申诉 -> 撤回按原主张"
CID=$(new_lease 4000 5000)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{"keys_handover":"钥匙3把门禁3张"}'
finish_to_proposed "$CID"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"MISSING","utility_status":"SETTLED","cleaning_status":"CLEAN",
  "key_replacement_fee":350}'
assert_eq "E 提交验房" 201 "$REQ_CODE"
CHID=$(PSQL "SELECT id FROM charge_items WHERE contract_id='$CID' AND kind='KEY_LOSS'")
req POST "/api/v1/contracts/$CID/items/charge/$CHID/contest" "$T" '{"comment":"钥匙其实还在找"}'
assert_eq "E 租客申诉钥匙费" 201 "$REQ_CODE"
DID=$(echo "$REQ_BODY" | jqr dispute_id)
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$T" '{"action":"WITHDRAW","comment":"确认丢失,接受扣款"}'
assert_eq "E 撤回申诉" 200 "$REQ_CODE"
IST=$(PSQL "SELECT status FROM charge_items WHERE id='$CHID'")
assert_eq "E 费用项 ACCEPTED" ACCEPTED "$IST"
final_settle "$CID" 350.00 4650.00

########## 场景 F: 房东逾期不验房, 平台催办 ##########
info "场景F: 房东迟迟不验房 -> 租客申诉 -> 平台自动开验房"
CID=$(new_lease 5000 10000 3)
req POST "/api/v1/contracts/$CID/checkin" "$L" '{}'
req POST "/api/v1/contracts/$CID/checkout" "$T" '{}'
# 把交房时间回拨到 10 天前, 超过 inspection_due_days=3
PSQL "UPDATE handovers SET occurred_at=now()-interval '10 days' WHERE contract_id='$CID' AND phase='CHECKOUT'" >/dev/null
req POST "/api/v1/contracts/$CID/disputes/inspection-delay" "$T" '{"comment":"已交房10天房东不验房"}'
assert_eq "F 逾期验房申诉成立" 201 "$REQ_CODE"
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "F 合同进入 DISPUTING" DISPUTING "$ST"
# 未超期时的合同不能申诉(负向用例, 复用当前 CID 再请求 -> 状态不允许)
req POST "/api/v1/contracts/$CID/disputes/inspection-delay" "$T" '{}'
assert_eq "F 重复逾期申诉被拒" 409 "$REQ_CODE"
req POST "/api/v1/admin/overdue-inspections" "$P"
assert_eq "F 平台催办自动开验房" 200 "$REQ_CODE"
OP=$(echo "$REQ_BODY" | jqr opened.0)
assert_eq "F 自动验房命中本合同" "$CID" "$OP"
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "F 合同进入 INSPECTING" INSPECTING "$ST"
ND=$(PSQL "SELECT count(*) FROM disputes WHERE contract_id='$CID' AND reason='INSPECTION_DELAY' AND status='RESOLVED_AGREEMENT'")
assert_eq "F 逾期争议自动闭合" 1 "$ND"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"SETTLED","cleaning_status":"CLEAN"}'
final_settle "$CID" 0.00 10000.00

echo
echo "补充场景结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
