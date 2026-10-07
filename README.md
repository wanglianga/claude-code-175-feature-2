# 城市租房押金托管·退租验房·争议服务

租客与房东签约后，本服务把**房源、合同期限、押金金额、托管账户、交割照片、家具家电清单、
水电表读数、维修责任**记录在同一条租赁档案上；入住期间关联维修工单；退租时按合同与入住交割
材料生成**可扣款项建议**；对墙面损坏 vs 自然磨损、家电维修责任不清、水电欠费、钥匙丢失、
房东迟迟不验房、租客提前退租、清洁费用争议，统一走证据驱动的争议流程；**只有争议全部闭合、
扣款全部定论、双方确认结算后，押金才会释放**。

**房东超时验房**由平台自动跟进：租客按时搬离并提交退租申请后，按房屋与合同约定向房东发送
验房提醒（站内信+短信，记录是否送达、房东是否打开验房单、是否还有未关闭报修）；超过首次
期限仍未处理则再次提醒并把任务标为逾期；到最终期限**先核验未关闭报修、未结账单、双方正在
处理的争议**，不会因为定时到了就直接退押金；门禁干净且两轮快照一致才自动全额释放押金，
释放前一刻状态发生变化则暂停并转人工复核。押金释放后房东只能提交**超时说明和新证据**（账外
登记），不能从已释放金额中扣款。

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
# 超时验房定时巡检可用 SCHEDULER_ENABLED=false 关闭, 改用平台端点手动驱动
# export SCHEDULER_ENABLED=true SCHEDULER_INTERVAL_SECONDS=30
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

# 房东超时验房场景（98 个断言，建议 SCHEDULER_ENABLED=false 手动驱动巡检）：
bash scripts/e2e-overdue.sh
```

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

### 5.6 房东超时验房：提醒 → 逾期 → 门禁 → 自动释放/转人工

租客提交退租交房（`CHECKOUT` handover）时，同事务建立一条 `inspection_tasks`：

```
交房 0 点 ── 首次提醒(站内 INBOX + 短信 SMS, 记录送达) ── first_deadline = 交房 + inspection_due_days
     │  房东打开验房单(POST /inspection/view): PENDING→OPENED, 提醒置 opened_at
     │  房东发起验房: 任务 COMPLETED, 回到正常验房流程
     ▼ 首次期限已过仍未处理(巡检1 POST /admin/overdue-reminders/scan)
二次提醒(站内+短信) + 告知租客 + 任务 OVERDUE
     ▼ final_deadline = first_deadline + final_release_days(巡检2 POST /admin/final-release/scan)
第一轮: 记录门禁快照 gate_snapshot(不释放)
第二轮: 复核快照 ── 指纹一致且门禁干净 ──▶ 自动全额退还租客(release_mode=OVERDUE_AUTO)
                  └─ 任一状态变化(报修/账单/争议/资金/提前退租) ──▶ MANUAL_REVIEW 暂停转人工
```

**自动释放前的门禁（应用层 `gateMetrics` + 数据库 `assert_overdue_release_clean` 双保险，绕过应用层
直接改结算单也会被触发器回滚）：**

- 无未闭合争议（`OPEN/EVIDENCE_PENDING/UNDER_REVIEW`）；
- 无未关闭报修（`SUBMITTED/RESPONSIBILITY_PENDING`）；
- 无未结账单：`PROPOSED/CONTESTED` 费用项，或退租水电已计费却没有定论费用项；
- `hard_hold = disputed_hold = 0`（没有任何定论/待定扣款）；
- 合同不能停留在 `EARLY_TERMINATED`（空置损失尚未处理）。

**状态突变检测**：最终期限先写一次门禁快照，下一轮才释放；两轮之间用结构体语义比较快照指纹，
任何报修/账单/争议/资金变化都会 `STATE_CHANGED_SUSPEND`，押金不动并转人工，杜绝"定时到了就
放款"和检查后被钻空子。

**人工复核** `POST /api/v1/contracts/{id}/manual-review`（仅平台）：`RELEASE`（门禁已过后按
`OVERDUE_MANUAL` 释放，触发器再次校验）/ `RETAIN`（暂不释放继续跟进）/ `REINSPECT`（平台代开
验房，转正常验房/争议/结算流程）。

**释放后只能账外主张**：合同已 `RELEASED` 后房东 `POST /late-claims` 提交超时说明与新证据
（`evidence` 在 RELEASED 状态仍可登记），只写入 `late_inspection_claims`，**不回流 escrow、不写
`fund_ledger`、不产生扣款项**；平台复核即使支持（`RESOLVED_AWARD`）也只是房东另行追讨的依据，
不会从已释放金额扣款。

**全程留痕**：任务每次状态变化写 `inspection_task_events`（16 类事件，含通知送达回执
`REMINDER_DELIVERED`、打开验房单 `SHEET_OPENED`、门禁快照 `GATE_SNAPSHOT`、拦截
`GATE_BLOCKED`、状态突变 `STATE_CHANGED_SUSPEND`），同时写 `lease_events` outbox 投递 Kafka；
通知的渠道/送达状态/送达时间/打开时间在 `notifications` 中可查（`GET /api/v1/notifications`，
网关回执走 `POST /admin/notifications/{id}/delivery`）。后台调度器默认开启
（`SCHEDULER_ENABLED=true`，`SCHEDULER_INTERVAL_SECONDS=30`），两个巡检也可由平台端点手动触发。

## 6. 项目结构

```
cmd/server/            启动入口(迁移/seed/relay/超时巡检调度器/HTTP)
migrations/            SQL 迁移(0001 表/约束/结算守卫; 0002 超时验房任务/通知回执/账外主张/超时释放闸门)
internal/
  config/              环境变量配置(含 SCHEDULER_ENABLED/SCHEDULER_INTERVAL_SECONDS)
  db/                  PG 连接池与迁移执行
  domain/              领域模型与状态常量
  rules/               退租扣款建议规则引擎(纯函数, 带单测)
  app/                 事务用例: 签约/托管/交割/维修/验房/争议/结算/资金台账/查询/seed
                       overdue.go 房东超时验房(提醒/逾期/门禁快照/自动释放/人工复核/账外主张)
                       scheduler.go 周期触发两段巡检
  events/              lease_events outbox -> Kafka relay
  auth/                Redis 会话 Token
  api/                 HTTP 路由、处理器、Swagger 静态资源
web/
  openapi.yaml         OpenAPI 3.x 规范(34 个路径)
  swagger-ui/          内嵌 Swagger UI 静态资源(离线可用)
scripts/
  e2e.sh               主生命周期端到端验证(41 断言, 含 DB 触发器闸门与跨租户隔离)
  e2e-scenarios.sh     7 类争议场景验证(51 断言)
  e2e-overdue.sh       房东超时验房 7 大场景验证(98 断言)
  e2e-lib.sh           脚本公共函数
Dockerfile / docker-compose.yml
```

## 7. 合同状态机

```
DRAFT ─签约→ SIGNED ─押金入托管→ LIVING ─退租交房→ CHECKOUT_PENDING
                                   │                 └─提前退租→ EARLY_TERMINATED
                                   └──────────────────────────────┐
                                                                   ▼
                                          INSPECTING ←─ 租客要求复验(REINSPECT)
                                               │
                                               ▼
                                      DEDUCTION_PROPOSED ◄──────┐
                                      │ 租客逐项接受/申诉          │ 争议全部闭合
                                      ▼                           │
                                     DISPUTING ─协商/裁决/撤回─────┘
                                               │
                                               ▼
                                      结算 PROPOSED → CONFIRMED(双方+触发器校验)
                                               │
                                               ▼
                                          RELEASED (余额退还租客 / hard_hold 付房东)
```

超时验房是叠加在 `CHECKOUT_PENDING/EARLY_TERMINATED` 之上的**旁路任务**（不改变合同状态机，
只推动它）：`inspection_tasks` 走 `PENDING → OPENED/OVERDUE → COMPLETED/SUPERSEDED`（房东或平台
开始验房）或 `OVERDUE → AUTO_RELEASED`（门禁通过）/ `MANUAL_REVIEW`（有未决项或状态突变）。
自动/人工释放同样把合同置为 `RELEASED`，并受 `settlement_guard` + `assert_overdue_release_clean`
两级数据库闸门保护。

## 8. 已执行的验证结论

以下验证在交付环境中实际执行通过：

- `gofmt -l` 无输出；`go vet ./...` 无告警；`go build ./...` 通过；`go test ./...` 全部通过
  （规则引擎 14 个用例 + 超时验房门禁 `gateMetrics`/快照指纹 9 个用例）。
- 使用本机 PostgreSQL 15（用户级实例，端口 5433）、Redis 启动服务（`KAFKA_DISABLED=true`，
  本机无 Kafka 二进制；领域事件仍全部写入 `lease_events` outbox 表）：
  - `scripts/e2e.sh` **39/40** 业务断言通过，唯一未通过项为"事件已投递 Kafka"的 broker 侧断言
    （1 条 skip：无 console-consumer）；outbox `published` 前置断言通过，Kafka 投递需在具备
    broker 的 Compose 环境复验。
  - `scripts/e2e-scenarios.sh` **51/51** 断言通过，覆盖 7 类争议，含"房东逾期不验房 → 租客申诉
    → 平台代开验房并自动闭合争议"（与新超时任务流程并存兼容）。
  - `scripts/e2e-overdue.sh` **98/98** 断言通过，覆盖 7 大场景：
    **A** 退房建任务+首次提醒（站内 DELIVERED/短信 SENT）+打开验房单（PENDING→OPENED、提醒置
    已打开、重复打开幂等）+正常验房 COMPLETED；
    **B** 超首次期限二次提醒并 OVERDUE、短信送达/失败回执留痕（REMINDER_DELIVERED）、逾期后打开
    不回退状态、最终期限未到不释放、通知列表；
    **C** 最终期限先留快照不释放、复核一致后自动全额退还（OVERDUE_AUTO，余额清零、台账
    RELEASE_TO_TENANT）、释放后禁止再验房/再生成结算、释放后可登记证据、房东提交超时说明+新证据
    为账外主张（资金台账与托管三层金额始终不变）、租客无权提交、平台部分支持 300 仍不划扣；
    **D** 未关闭报修阻断自动释放转 MANUAL_REVIEW（OPEN_REPAIRS，快照记录未决数）、房东不能复核、
    门禁未过 RELEASE 返回 409、**绕过应用层直接 CONFIRMED OVERDUE_AUTO 结算被 DB 触发器
    CONFLICT_OPEN_REPAIR 回滚**、定责闭合后平台 OVERDUE_MANUAL 释放；
    **E** 快照后租客提起逾期争议（释放前状态突变）→ STATE_CHANGED_SUSPEND 暂停转人工、押金不动，
    平台 REINSPECT 代开验房、争议闭合、合同回 INSPECTING；
    **F** 提前退租到最终期限不得自动放款（EARLY_TERMINATION），平台 REINSPECT 后验房单保留提前
    退租标记；
    **G** 退租水电已计费（240 元）但没有定论费用项也未经验房确认结清时，最终期限按
    UNSETTLED_BILLS 拦截转人工，且 DB 触发器以 CONFLICT_UNSETTLED_BILL 回滚绕过应用层的 CONFIRMED。
- 另以 `SCHEDULER_ENABLED=true SCHEDULER_INTERVAL_SECONDS=2` 启动，回拨期限后**不调用任何巡检
  端点**，验证后台调度器自动完成 `OVERDUE → 门禁快照 → AUTO_RELEASED`，合同 RELEASED、余额 0、
  全额退还租客。
- OpenAPI 规范可解析（**34 个路径、22 个 schema**），`/swagger/`、`/openapi.yaml` 均返回 200，
  `/readyz` 报告 postgres/redis ok。

> 说明：交付环境没有 Docker daemon，因此 `docker compose up` 未在本机实际执行；
> Dockerfile 为标准多阶段 Go 构建，compose 中 postgres/redis/kafka 均使用官方镜像与健康检查，
> 请在具备 Docker 的环境按第 1 节启动验证（Compose 默认 `SCHEDULER_ENABLED=true`）。
