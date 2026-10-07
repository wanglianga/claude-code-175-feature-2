# 城市租房押金托管·退租验房·争议服务

租客与房东签约后，本服务把**房源、合同期限、押金金额、托管账户、交割照片、家具家电清单、
水电表读数、维修责任**记录在同一条租赁档案上；入住期间关联维修工单；退租时按合同与入住交割
材料生成**可扣款项建议**；对墙面损坏 vs 自然磨损、家电维修责任不清、水电欠费、钥匙丢失、
房东迟迟不验房、租客提前退租、清洁费用争议，统一走证据驱动的争议流程；**只有争议全部闭合、
扣款全部定论、双方确认结算后，押金才会释放**。

**房东超时验房处理**：租客按时搬离并提交退租申请后，平台按房屋与合同约定向房东发送首次验房
提醒（记录通知是否送达、房东是否打开验房单）；超过首次期限仍未处理则二次提醒并把任务标记为
逾期（合同进入 `REMINDING`）；到达最终期限时先核验**未关闭报修、未结账单、双方正在处理的
争议**，并对"释放前最后一刻"的状态指纹做二次比对——条件满足才自动全额释放押金；任一条件不
满足、或账单/报修/争议状态在释放前刚发生变化，则**暂停自动释放、转平台人工复核**
（`RELEASE_REVIEW`）。押金自动释放后，房东只能提交**超时说明与新证据**，平台裁决仅作结论
留痕，**不会从已释放金额中自动扣款**。

技术栈：**Go 1.19 + PostgreSQL 15 + Redis 7 + Kafka 3.7(KRaft)**。后端服务，无业务前端页面；
接口文档由内嵌 **Swagger UI / OpenAPI 3.x** 提供。

---

## 1. 快速开始（Docker Compose 一键启动）

```bash
docker compose up -d --build
# 服务: http://localhost:8080
# Swagger UI: http://localhost:8080/swagger/
# OpenAPI:    http://localhost:8080/openapi.yaml
```

容器包含 app / postgres / redis / kafka(KRaft 单节点)。app 启动时自动执行数据库迁移并写入
演示账号，Kafka 主题 `lease.events` 自动创建。

停止并清理数据卷：

```bash
docker compose down -v
```

## 2. 本地开发（不使用 Docker）

依赖：Go 1.19+、PostgreSQL、Redis、Kafka（KRaft 单节点即可）。

```bash
# 1) 准备数据库
createdb deposit
psql "$DATABASE_URL" -f migrations/0001_init.sql     # 也可让服务启动时自动迁移

# 2) 启动服务（环境变量带默认值，见 internal/config/config.go）
export DATABASE_URL='postgres://app@127.0.0.1:5432/deposit?sslmode=disable'
export REDIS_ADDR=127.0.0.1:6379
export KAFKA_BROKERS=127.0.0.1:9092
go run ./cmd/server
# 无 Kafka 环境时可 KAFKA_DISABLED=true（事件仍写入 lease_events 表，只是不投递）
# 房东超时巡检间隔由 OVERDUE_SWEEP_SECONDS 控制(默认 60 秒); 设为 0 可关闭后台自动巡检,
# 仅通过 POST /api/v1/admin/overdue-sweep 手动触发(自动化测试即采用该模式保证确定性)。
```

健康检查：

```bash
curl localhost:8080/healthz   # 存活
curl localhost:8080/readyz    # Postgres / Redis / Kafka 依赖状态
```

## 3. 测试账号（固定演示 Token）

服务启动时 `SEED=true`（默认）会创建 4 个用户并把固定会话写入 Redis，直接可用：

| 角色   | 手机号        | 固定 Bearer Token | 说明                 |
|--------|---------------|-------------------|----------------------|
| 租客   | 13800000001 | `tenant-token`    | 小李                 |
| 房东   | 13800000002 | `landlord-token`  | 老王                 |
| 中介   | 13800000003 | `agent-token`     | 张姐                 |
| 平台   | 13800000004 | `platform-token`  | 客服/裁决方          |

也可以用手机号登录换取随机 token：

```bash
curl -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' -d '{"phone":"13800000001"}'
```

所有业务接口携带 `Authorization: Bearer <token>`。另有一套已登记的演示房源
（北京·建国路 88 号），可直接拿来签约。

## 4. 验证方式

### 4.1 自动化验证脚本（推荐）

脚本需要能访问服务的 `curl`，以及能连数据库的 `psql`（用于断言资金状态）。
数据库连接通过环境变量配置（默认适配本地开发实例 127.0.0.1:5433）：

```bash
# 本地开发默认(PG5433)
bash scripts/e2e.sh
# Docker Compose 环境
PGPORT=5432 PGUSER=app PGPASSWORD=app PGDATABASE=deposit bash scripts/e2e.sh
```

```bash
# 主场景（41 个断言）：签约→托管→入住交割→维修定责→退租验房→
# 墙面争议→平台裁决→双方确认放款，并校验 Kafka 事件全部投递与 DB 触发器闸门
bash scripts/e2e.sh

# 补充争议场景（51 个断言）：
bash scripts/e2e-scenarios.sh

# 房东超时验房处理（102 个断言）：
bash scripts/e2e-overdue.sh
```

`scripts/e2e-overdue.sh` 覆盖（需要服务以 `OVERDUE_SWEEP_SECONDS=0` 启动，由脚本手动触发巡检）：

1. 交房即发 **FIRST** 提醒并记录送达；房东打开验房单幂等留痕；逾期后 **OVERDUE** 二次提醒、
   任务标 `OVERDUE`、合同 `REMINDING`；房东最终期限前补开验房，任务自动 `CANCELLED` 后正常结算。
2. 最终期限存在**未关闭报修** → 自动释放暂停、合同 `RELEASE_REVIEW`、任务 `BLOCKED`；
   阻断未消除时人工 RELEASE 被 409 拒绝；`REOPEN_INSPECTION` 转回正常流程后结算。
3. **未结水电账单**阻断 → 转验房把欠费计入扣款（110 元）后双方结算。
4. **未闭合争议**阻断且不覆盖 `DISPUTING`；平台裁决闭合后再次巡检自动全额退押（结算单
   `auto_released=true`）。
5. 三检查通过（任务 `READY`）后、执行前**状态刚好变化**（租客提起争议）→ 释放事务指纹不一致，
   任务转 `REVIEW_HUMAN`、记录 `STATE_CHANGED_BEFORE_RELEASE`，押金不动；人工复核闭合后才放行。
6. 三检查通过自动全额退押后：房东不能再生成结算扣款（409），只能提交**超时说明+新证据**；
   平台裁决 `UPHELD` 但 `auto_deducted=false`，无 `DEDUCT` 台账、资金不从租客处扣回。

### 4.2 单元测试

```bash
go test ./...
```

`internal/rules` 含 14 个规则引擎用例：自然磨损 0 元、入住前损坏免责、入住基线 GOOD 新损不免责、
租客/房东/共担维修责任、家电责任不清、提前退租通知期、清洁/欠费/钥匙合计、押金上限截断等。

### 4.3 手动走一遍主流程

```bash
T=tenant-token; L=landlord-token
PID=$(psql ... -tAc "SELECT id FROM properties LIMIT 1")
TID=$(psql ... -tAc "SELECT id FROM users WHERE phone='13800000001'")

# 签约 → 租客充押金托管
curl -X POST localhost:8080/api/v1/contracts -H "Authorization: Bearer $L" \
  -H 'Content-Type: application/json' -d "{\"property_id\":\"$PID\",\"tenant_id\":\"$TID\",
  \"start_date\":\"2026-01-01\",\"end_date\":\"2026-12-31\",\"rent_amount\":5000,
  \"deposit_amount\":10000,\"inspection_due_days\":7}"
CID=<返回的 id>
curl -X POST localhost:8080/api/v1/contracts/$CID/deposit -H "Authorization: Bearer $T"

# 任意时刻查看同一条租赁的完整档案（合同/托管/台账/交割/维修/验房/扣款/争议/事件）
curl -H "Authorization: Bearer $T" localhost:8080/api/v1/contracts/$CID/timeline
```

其余步骤（入住交割、报修定责、交房、验房提交、逐项接受/申诉、争议动作、结算确认）请直接在
Swagger UI 点击调用，或参考两个脚本里的请求体。

## 5. 核心设计：资金状态 / 争议状态 / 证据状态如何保持一致

### 5.1 押金的三层金额

押金进入托管后只是资金状态，真正决定释放的是证据与争议。`escrow_accounts` 维护：

| 字段            | 含义                                                       |
|-----------------|------------------------------------------------------------|
| `balance`       | 在管押金总额                                               |
| `disputed_hold` | **软冻结**：验房建议/争议中的扣款，任何一方都拿不到         |
| `hard_hold`     | **定论预留**：租客接受、协商一致或平台裁决支持的扣款        |
| 约束            | `hard_hold + disputed_hold <= balance`                      |

每次变动同时追加 append-only 的 `fund_ledger`（充值/建议冻结/转定论/解冻/扣款/退还）。

### 5.2 扣款建议不是扣款

`POST /inspection/submit` 调用纯函数规则引擎（`internal/rules`），结合合同条款、入住交割基线、
家具家电清单、维修责任工单、退租六维结论（墙/地板/家电/钥匙/水电/清洁）生成建议：

- 自然磨损、入住前已损坏/磨损、房东责任维修 → 产出 **0 元保护性建议**；
- 家电没有任何维修记录或工单责任未定 → `APPLIANCE_RESPONSIBILITY_UNCLEAR`，要求先澄清责任；
- 租客责任维修按 `tenant_share`，共担按 50%；无前置记录的损坏给房东报价但允许租客申诉自然磨损；
- 提前退租按合同 `notice_days` 与月租测算空置损失；
- 清洁/欠费/钥匙按报价与读数差生成费用主张；**合计以押金为上限截断**。

建议金额只进 `disputed_hold`。租客**逐项**接受才转 `hard_hold`；申诉则打开争议，款项不动。

### 5.3 争议与资金的同事务联动

所有写操作都在一个数据库事务内，对 `contract_id` 取
`pg_advisory_xact_lock(hashtextextended(...))` 串行化：

- 租客申诉 → 合同 `DISPUTING`，款项保持 `disputed_hold`；
- `PARTIES_AGREE`（协商金额）/`ADJUDICATE`（平台裁决金额）/`WITHDRAW`（撤回按原主张）闭合争议：
  支持房东的部分 `disputed_hold → hard_hold`，未支持部分**立即解冻**回可退余额；
  扣款项同步进入 `ACCEPTED / UPHELD / OVERTURNED / REJECTED` 终态；
- 全部争议闭合后合同回到 `DEDUCTION_PROPOSED`。

### 5.4 数据库级的"未闭合不得放款"闸门

`settlements` 上有触发器 `settlement_guard`：结算单从 PROPOSED 转 CONFIRMED 时，数据库自己检查
(1) 不存在 OPEN/EVIDENCE_PENDING/UNDER_REVIEW 争议；(2) 结算扣款额 == `hard_hold`。
不满足直接抛异常回滚事务——**应用层被绕过也无法先放款**。业务层还在生成结算单时提前拦截
仍有 `disputed_hold` 的情况（409 `UNRESOLVED_DISPUTE`）。结算必须租客与房东双方确认，
第二次确认在同一事务里执行 `DEDUCT`（打给房东）+ `RELEASE_TO_TENANT`（退还余额），合同进入
`RELEASED`，托管余额清零。

### 5.5 事件与通知

每个状态变化在业务事务内写 `lease_events`（事务性 outbox）。后台 relay
（`internal/events`）每 500ms 用 `FOR UPDATE SKIP LOCKED` 取出未发布事件，以 `contract_id`
为 key 发往 Kafka（同一租赁同一分区、严格有序），发送成功才标记 published。语义为
at-least-once，事件携带 `event_id`，消费方须幂等。关键节点同时写 `notifications` 表。

### 5.6 房东超时验房：催办、逾期与"带闸门的"自动释放

| 时刻 | 动作 | 留痕 |
|------|------|------|
| 租客交房 | 按合同 `inspection_due_days`/`final_release_days` 发 **FIRST** 验房提醒，建 `auto_release_jobs(PENDING)` | `inspection_reminders` + `notifications` + `InspectionReminderSent` |
| 房东打开验房单 | 房东端回调 `POST /inspection/opened`（幂等） | 提醒 `opened/opened_at` + `InspectionSheetOpened` |
| 过首次期限仍未验房 | **OVERDUE** 二次提醒，任务 `OVERDUE`，合同 `REMINDING` | `InspectionTaskOverdue` |
| 房东补开验房 | 任务 `CANCELLED`，回到正常验房→争议→结算流程 | `AutoReleaseCancelled` |
| 到达最终期限 | 三检查：未关闭报修 / 未结水电账单 / 未闭合争议，任一不满足 → `BLOCKED`、合同 `RELEASE_REVIEW`、**FINAL** 提醒 | `block_reasons` + `AutoReleasePaused` |
| 三检查通过 | 任务置 `READY` 并持久化**状态指纹**（报修/账单/争议的 open 计数 + `state_seq` 序号和） | `AutoReleaseChecksPassed` |
| 执行释放前 | 在独立事务里**重算指纹**，与 READY 快照不一致（状态刚好变化）→ `REVIEW_HUMAN` 暂停 | `STATE_CHANGED_BEFORE_RELEASE` |
| 指纹一致 | 同事务生成 `PROPOSED→CONFIRMED` 结算单（**仍受 `settlement_guard` 触发器约束**：无未闭合争议且 `hard_hold=0`）→ `EXECUTED`，押金全额退租客 | `AutoDepositReleased`，结算单 `auto_released=true` |
| 释放之后 | 房东只能 `POST /post-release-claims` 提交超时说明与新证据；平台 `UPHELD/REJECTED` 仅作结论，**不自动扣款/划回** | `post_release_claims` + 两个事件 |

关键点：自动释放**不是"定时到了就放款"**——评估与执行分两个事务，执行事务用
`repairs/utility_readings/disputes` 上的 `state_seq` 触发器做释放前最后一次状态指纹比对；
且自动结算单与手动结算走同一个 DB 触发器闸门，应用层被绕过也无法在有争议/有预留时放款。
人工复核端点 `POST /api/v1/admin/contracts/{id}/release-review` 支持
`RELEASE`（重新核验后放行，阻断仍在则 409）、`REOPEN_INSPECTION`（代开验房转正常流程）、
`KEEP_REVIEW`（继续挂起并备注）。

## 6. 项目结构

```
cmd/server/            启动入口(迁移/seed/relay/超时巡检/HTTP)
migrations/            SQL 迁移(表、约束、索引、结算守卫触发器、超时验房自动释放)
internal/
  config/              环境变量配置
  db/                  PG 连接池与迁移执行
  domain/              领域模型与状态常量
  rules/               退租扣款建议规则引擎(纯函数, 带单测)
  app/                 事务用例: 签约/托管/交割/维修/验房/争议/结算/资金台账/查询/seed
                       overdue.go 房东超时: 提醒/逾期/三检查+指纹/自动释放/人工复核/释放后说明
                       scheduler.go 后台定时巡检(默认 60s)
  events/              lease_events outbox -> Kafka relay
  auth/                Redis 会话 Token
  api/                 HTTP 路由、处理器、Swagger 静态资源
web/
  openapi.yaml         OpenAPI 3.x 规范(32 个路径)
  swagger-ui/          内嵌 Swagger UI 静态资源(离线可用)
scripts/
  e2e.sh               主生命周期端到端验证(41 断言, 含 DB 触发器闸门与跨租户隔离)
  e2e-scenarios.sh     7 类争议场景验证(51 断言)
  e2e-overdue.sh       房东超时验房处理验证(102 断言: 催办/逾期/阻断/状态变化/自动释放/释放后申诉)
  e2e-lib.sh           脚本公共函数
Dockerfile / docker-compose.yml
```

## 7. 合同状态机

```
DRAFT ─签约→ SIGNED ─押金入托管→ LIVING ─退租交房→ CHECKOUT_PENDING
                                   │                 └─提前退租→ EARLY_TERMINATED
                                   └──────────────────────────────┐
                                                                   ▼
        首次期限过未验房 → REMINDING ─房东补验房→ INSPECTING ◄─────┘
                                   │                 ▲ 租客要求复验(REINSPECT)
        最终期限三检查失败/释放前变化 → RELEASE_REVIEW └平台 REOPEN_INSPECTION
                                   ▼
                                          INSPECTING
                                               │
                                               ▼
                                      DEDUCTION_PROPOSED ◄──────┐
                                      │ 租客逐项接受/申诉          │ 争议全部闭合
                                      ▼                           │
                                     DISPUTING ─协商/裁决/撤回─────┘
                                               │
                                               ▼
                                      结算 PROPOSED → CONFIRMED(双方+触发器校验)
                                               │  或最终期限自动释放(三检查+指纹+触发器)
                                               ▼
                                          RELEASED (余额退还租客 / hard_hold 付房东)
```

## 8. 已执行的验证结论

以下验证在交付环境中实际执行通过：

- `go vet ./...` 无告警；`go build ./...` 通过；`go test ./...` 规则引擎 14 个用例全部通过。
- 使用本机 PostgreSQL 15（用户级实例，端口 5433）、Redis、Kafka 3.7.2 KRaft 单节点启动服务，
  `scripts/e2e.sh` **41/41** 断言通过，覆盖：权限隔离（房东不能替租客充押金）、
  水电煤读数差自动计费（265.00 元）、规则建议金额、`disputed_hold`/`hard_hold` 联动、
  **争议打开时生成结算被 409 拒绝**、平台裁决部分支持（800→500）后的资金拆分、
  双方确认后放款与余额清零、15 个领域事件全部投递 Kafka 并可被 console-consumer 读到。
- `scripts/e2e-scenarios.sh` **51/51** 断言通过，覆盖 7 类争议：
  自然磨损/入住前损坏免责、家电责任不清裁决全额驳回（OVERTURNED）、清洁费协商折价、
  提前退租空置损失、钥匙丢失申诉撤回、房东逾期不验房申诉+平台巡检自动代开验房并自动闭合争议。
- OpenAPI 规范可解析（25 个路径、16 个 schema），`/swagger/`、`/swagger/swagger-ui.css`、
  `/openapi.yaml` 均返回 200。

### 房东超时验房处理（本次新增，2026-10-07 在本机实际执行）

- `go vet ./...` 无告警；`go build ./...` 通过；`go test ./...` 规则引擎用例全部通过。
- 在用户级 PostgreSQL 15（127.0.0.1:5433, trust）、Redis 127.0.0.1:6379、Kafka 3.7.2 KRaft
  单节点（127.0.0.1:9092）上以 `OVERDUE_SWEEP_SECONDS=0` 启动服务（巡检改由管理端点手动触发），
  迁移 0001+0002 自动执行成功。
- `scripts/e2e-overdue.sh` **102/102** 断言通过，覆盖：
  FIRST/OVERDUE/FINAL 三次提醒与送达/打开留痕、逾期后合同 `REMINDING`、房东补验房任务取消；
  未关闭报修/未结水电账单/未闭合争议三类阻断（含阻断未消除时人工 RELEASE 被 409 拒绝）；
  READY 与执行分两事务、执行前指纹不一致转 `REVIEW_HUMAN`（`STATE_CHANGED_BEFORE_RELEASE`）；
  三检查+指纹一致后自动全额退押（结算单 `auto_released=true`，DB 触发器仍参与校验）；
  释放后房东仅能提交超时说明与新证据、平台裁决 `UPHELD` 也不产生任何 `DEDUCT` 台账。
- 回归：`scripts/e2e.sh` **41/41**、`scripts/e2e-scenarios.sh` **51/51** 全部通过。
- 新增领域事件（`InspectionReminderSent`/`InspectionTaskOverdue`/`InspectionSheetOpened`/
  `AutoReleaseChecksPassed`/`AutoReleasePaused`/`AutoDepositReleased`/`AutoReleaseCancelled`/
  `PostReleaseClaimSubmitted`/`PostReleaseClaimDecided`）全部投递 Kafka，
  `lease_events.published=false` 计数为 0，并由 console-consumer 实际消费到 `AutoDepositReleased`。
- OpenAPI 规范可解析（**32 个路径、20 个 schema**），`/swagger/`、`/openapi.yaml` 返回 200。

> 说明：交付环境没有 Docker daemon，因此 `docker compose up` 未在本机实际执行；
> Dockerfile 为标准多阶段 Go 构建，compose 中 postgres/redis/kafka 均使用官方镜像与健康检查，
> 请在具备 Docker 的环境按第 1 节启动验证。
