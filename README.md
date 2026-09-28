# braid

企业级 Agent 工作流编排平台(Go):JSON DSL 定义 DAG,人在回路审批,全链路审计,
工具权限隔离;私有化交付,单二进制四入口 + docker-compose。

braid 的护城河不在「有审批、有审计」(2026 年这已是标配),而在三件做到安全审查级深度的东西:
**工具调用的 SSRF/沙箱隔离、append-only 审计链、合规交付形态**(设计依据与竞品对照见
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md))。

## 1. 架构总览

### 1.1 引擎主线

状态真相源只有 MySQL(每次节点状态迁移 = 条件更新 CAS:`WHERE status=? AND version=?`);
Redis 只做队列(Asynq = 基于 Redis 的任务队列库,载于独立 noeviction 实例)、reaper 选主锁
与热缓存——状态不落 Redis。任务通道 = outbox + relay + reaper 对账,
节点执行进程(worker)随时可被 SIGKILL,不丢、不重、不悬死:

```mermaid
flowchart TB
    U["author<br/>POST /api/v1/executions"] --> API

    subgraph processes["四个常驻进程(host 或容器)"]
        API["api<br/>REST + Web 审批页"]
        RELAY["relay<br/>轮询投递"]
        WORKER["worker<br/>Asynq 消费者"]
        REAPER["reaper<br/>对账兜底(必启)"]
    end

    subgraph storage[" "]
        MYSQL[("MySQL —— 状态真相源<br/>workflow_execution / node_execution / outbox / audit")]
        REDIS[["Redis 队列实例<br/>noeviction + AOF(Asynq)"]]
    end

    API -- "① Instantiate(同事务:执行 + 全部节点行<br/>+ execution.start outbox 行)" --> MYSQL
    MYSQL -- "② PENDING 行" --> RELAY
    RELAY -- "③ EnqueueContext" --> REDIS
    REDIS -- "④ 拉取任务" --> WORKER
    WORKER -- "⑤ 执行节点(llm / tool / branch / approval)<br/>⑥ Complete(同事务:CAS 迁移 + 下游 outbox + 审计暂存)" --> MYSQL
    MYSQL -. "⑦ 下游 PENDING 行" .-> RELAY

    WORKER -- "approval 节点 → WAITING_APPROVAL<br/>(任务结束,worker 释放)" --> MYSQL
    U2["approver<br/>Web 审批页 / decision API"] -- "决定 → outbox resume 任务<br/>(确定性 TaskID,重投天然幂等)" --> API
    REAPER -. "判死重派 / 审批超时升级 /<br/>审计暂存搬运 / 分区水位" .-> MYSQL
```

审批不是分支入口而是挂起点:审批节点把执行置为 `WAITING_APPROVAL` 后正常结束任务释放
worker;审批决定经同一 outbox 通道触发续跑任务(`resume:<execution_id>:<node_id>:<attempt>`
确定性 TaskID,Asynq `ErrTaskIDConflict` 即幂等去重)。**reaper 必启**:审计高频事件由它从暂存表
周期搬运(默认 1s),不跑则对账查询看不到新行(不丢);worker 判死、审批超时升级也都在它身上。

### 1.2 设计取向(18 项决策的浓缩)

完整决策表与审计事实链见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) §1;此处只列读者最常问的取向:

| 域 | 取向 |
|----|------|
| 引擎底座 | 自研状态机(不用 Temporal):MySQL 单一真相源 + CAS 迁移;Redis 不参与状态裁决 |
| 任务通道 | outbox(状态变更同事务)→ relay 投递 Asynq(MaxRetry=1,重试权威在平台节点层)→ reaper 对账兜底 |
| 人在回路 | 挂起不占 worker;或签 P0;超时升级通知 + 保持等待,永不自动通过 |
| 数据流 | 显式引用 `{{nodes.<id>.output.<field>}}`,无共享黑板;每节点存输入快照(回滚/续跑地基) |
| 安全 | 定义时白名单 × 执行时 RBAC 双重强制;http 工具 SSRF 三层修(静态白名单 + 拨号瞬间 IP 复核 + 每跳重校验);凭证 per-call scoped token |
| 审计 | append-only(无 update 接口)、按执行日期分区、每行携带 OTel trace_id、PII 脱敏钩子、保留期可配 |
| LLM | OpenAI 兼容端点薄封装(不用 Responses API);tiktoken 词表随安装包离线分发,冷启动零外网请求 |
| 可观测 | OTel trace 贯穿(API→worker→LLM/工具,审批段 trace link 串联)+ Prometheus 告警集(死信/审批积压/卡死节点/429/队列 Redis 水线) |
| 用户体系 | 内置 admin / author / approver + JWT;OIDC/LDAP 留接口桩(P1) |
| 部署 | 单二进制四入口(api/relay/worker/reaper)+ compose 编排 MySQL + Redis×2(队列/缓存物理分离)+ Asynqmon |

## 2. 快速上手

两条路径:**L0** 用测试栈 10 分钟零外部依赖跑通端到端示例(引擎、审批、审计、trace 全链,
LLM 用桩供应商);**L1** 起 dev 栈接真实 LLM 端点走完整业务链(含 Web 审批页)。
耗时按 Go 依赖与 Docker 镜像已缓存口径;首次运行另加 5–15 分钟(拉模块/镜像 + 首次编译)。
以下所有命令的执行目录 = **仓库根**(相对路径 `deploy/...`、`docs/...`、`internal/...` 均由此起)。

### 2.1 L0 —— 10 分钟跑通端到端示例(零密钥)

前置:Go ≥ 1.27、Docker(能起 MySQL 8.0 / Redis 7.4 容器)。

```bash
# ① 起测试栈(MySQL 127.0.0.1:13306 / Redis 16379,仅绑回环;测试自举 schema 与迁移)
docker compose -f deploy/migrate-test.docker-compose.yml -p braid-t13 up -d

# ② 跑端到端示例(文档合规审查:file_read → LLM 解析 → 合规审查 → 分支 → 摘要 → 人工审批 → 报告)
go test ./internal/nodes -run 'TestE2EDocExample' -v -count=1

# ③ (可选)故障注入五臂 —— 见 §3
# ④ 收栈(数据全在 tmpfs,down 即清;栈不在时上面的测试自动 skip)
docker compose -f deploy/migrate-test.docker-compose.yml -p braid-t13 down
```

② 的三个用例各走一条业务真相(全部对真 MySQL/真 Redis,API 触发、页面同源序列审批):

| 用例 | 断言主线 |
|------|----------|
| `TestE2EDocExampleApproveProducesReport` | 批准 → SUCCEEDED,`outputs.report` 有值;审计链全序逐行断言 + trace 全链一致 + 文件内容真实流到 LLM |
| `TestE2EDocExampleComplianceFailRoutesToRevision` | 合约不过 → 改道 `outputs.revision`,主线级联 SKIPPED |
| `TestE2EDocExampleApprovalRejectFailsFast` | 驳回 → `approval_rejected` 终败,fail-fast 下游不推进 |

示例 DSL、样例输入与语义解读见 [docs/examples/doc-compliance-review.md](docs/examples/doc-compliance-review.md)。

> 栈不在时测试**自动 skip**(不是失败);本仓 CI 同一套测试加 `BRAID_TEST_REQUIRE_STACK=1`
> 把 skip 升级为硬失败,防假绿。

### 2.2 L1 —— dev 栈 + 真实 LLM 完整链路

前置:L0 之外,一个 OpenAI 兼容 LLM 端点(如智谱 GLM、DeepSeek 或本地 vLLM)。

```bash
# ① 配置(全部 env 旋钮与逐键注释以 deploy/.env.example 为单源,此处只列必填)
cp deploy/.env.example deploy/.env
```

编辑 `deploy/.env`,打开并填好(其余保持注释即默认值):

```bash
BRAID_MYSQL_DSN=root:braid_dev_root@tcp(127.0.0.1:3306)/braid
BRAID_REDIS_QUEUE_ADDR=127.0.0.1:6379          # relay/worker 必填(无默认,空值启动即败)
# JWT 密钥无默认值,缺省/短于 32 字节启动即败;生成:openssl rand -hex 32
BRAID_AUTH_JWT_SECRET=
BRAID_LLM_BASE_URL=<OpenAI 兼容端点,含版本路径>
BRAID_LLM_API_KEY=<密钥仅经 env 注入,不进代码/日志/审计>
BRAID_LLM_PRICE_TABLE={"glm-5.3":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"},"fallback":{"input_per_mtok":"2","output_per_mtok":"8","currency":"CNY"}}
# file_read 白名单;不设则工具不注册,发布即被拒。<仓库绝对路径> 必须换成
# 本机真实绝对路径——照抄占位会静默把白名单指向不存在目录,执行期 file_read 被拒
BRAID_FILEREAD_DIRS=["<仓库绝对路径>/docs/examples"]
```

```bash
# ② 起基础设施(MySQL 3306 / 队列 Redis 6379 / 缓存 Redis 6380 / Asynqmon 8080,均仅绑回环)
docker compose -f deploy/dev.docker-compose.yml up -d
#    MySQL 首次初始化要十几秒,等 healthy 再做 ③(否则 migrate 报 connection refused):
until docker exec braid-dev-mysql mysqladmin ping -uroot -pbraid_dev_root --silent 2>/dev/null; do sleep 1; done

# ③ 建库(golang-migrate CLI 指向仓内迁移;DSN 四参数是全库连接契约,缺一不可。
#    装到 $(go env GOPATH)/bin,不在 PATH 的机器先 export PATH="$PATH:$(go env GOPATH)/bin";
#    DSN 口令按 .env 的 MYSQL_ROOT_PASSWORD 默认占位写死,改过口令则同步改)
go install -tags 'mysql' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1
migrate -path internal/db/migrations \
  -database "mysql://root:braid_dev_root@tcp(127.0.0.1:3306)/braid?loc=UTC&parseTime=true&multiStatements=true&time_zone=%27%2B00%3A00%27" up
# 重复执行是 no-op;dirty 恢复 runbook 见 docs/ROADMAP.md T7.4

# ④ 建号(P0 无 CLI 建号入口;示例 DSL 审批名单 = alice / bob,账号名必须一致。
#    口令经容器内环境变量取用(env_file 注入),不落宿主命令行参数;
#    主键是固定小 id,重跑本块报 1062 重复键 = 已建过,属预期——彻底重置
#    用 docker compose -f deploy/dev.docker-compose.yml down -v 后从 ② 重来。
#    下面 hash 对应明文 dev-author-1 / dev-approver-1——纯 dev 占位,生产必须换强密码
#    并自行生成 hash,如交互式:htpasswd -nBC 10 "" 然后输入密码,取输出接 tr -d ':\n')
docker exec -i braid-dev-mysql sh -c 'exec mysql -uroot -p"$MYSQL_ROOT_PASSWORD" braid' <<'EOF'
INSERT INTO users (id, username, password_hash) VALUES
  (9001, 'alice', '$2a$10$z.GbtBYJa/n.nsM0k8AJPuMb2h1pZWvR2e2Fm4/QtcF6pWa0cZSP.'),
  (9002, 'bob',   '$2a$10$MD1r186iUjr0SsS7JtXH7OWtn2g710rTrlozsnBZASzi9xw2DfeXS');
INSERT INTO user_role (id, user_id, role) VALUES
  (9101, 9001, 'author'),
  (9102, 9001, 'approver'),
  (9103, 9002, 'approver');
EOF

# ⑤ 四进程 —— 开四个终端,每个都先注入 env 再跑各自入口(不是顺序执行:
#    每个进程都是前台常驻,少一个都是静默失败——执行停 PENDING 缺 relay/worker,
#    审计看不到新行缺 reaper)
#    每个终端:set -a; source deploy/.env; set +a
#    (值含 () {} " , : 等——如 DSN、JSON 单价表/白名单——必须整体加单引号,
#     否则 source 解析中断、后续键静默失效;详见 deploy/.env.example 文件头)
go run ./cmd/api      # 终端 1:127.0.0.1:8081,REST + Web 审批页(/ui/)+ /healthz + /metrics
go run ./cmd/relay    # 终端 2:outbox → Asynq 投递
go run ./cmd/worker   # 终端 3:节点执行(连 LLM 端点)
go run ./cmd/reaper   # 终端 4(必启):审计搬运 + 判死重派 + 审批超时扫描
# 一键替代(无需四终端;交互式调试仍可手动起):
#   deploy/dev-up.sh    # 起四进程;日志按启动时间戳归档 log/<ts>/<组件>.log,
#                       # log/latest 软链指向本次,历史全部保留(防双实例)
#   deploy/dev-down.sh  # TERM→宽限→KILL 三段停;日志保留
```

走链(认证面 = login 换 Bearer token,全程同一个 api 入口;块内 `<...>` 均为填空占位,
替换成真实值后再执行,照抄会在 shell 里炸或打到错误 URL):

```bash
API=http://127.0.0.1:8081/api/v1

# ① author(alice)登录;响应 JSON 的 token 字段即 Bearer token
curl -s $API/auth/login -d '{"username":"alice","password":"dev-author-1"}'
TOKEN=<响应的 token 字段>

# ② 提交 + 发布示例定义(草稿 id = 提交响应 JSON 的 id 字段)
curl -s -X POST $API/definitions -H "Authorization: Bearer $TOKEN" \
  --data-binary @docs/examples/doc-compliance-review.json
curl -s -X POST $API/drafts/<提交响应的 id 字段>/publish -H "Authorization: Bearer $TOKEN"

# ③ 触发执行(输入 = 文件路径,须在 BRAID_FILEREAD_DIRS 白名单内;响应 id 字段 = 执行号)
curl -s -X POST $API/executions -H "Authorization: Bearer $TOKEN" \
  -d '{"definition_name":"doc-compliance-review",
       "input":{"doc_path":"<仓库绝对路径>/docs/examples/sample-doc.txt"}}'
EXEC=<响应的 id 字段>

# ④ 浏览器开 http://127.0.0.1:8081/ui/(bob 登录 → 待办 → 详情 → 批准);
#    或页面同源 API(bob 的 token:重复 ①,用户名 bob / 密码 dev-approver-1):
curl -s -X POST $API/executions/$EXEC/approvals/human_review/decision \
  -H "Authorization: Bearer <bob 的 token>" \
  -d '{"decision":"approve","comment":"同意,归档"}'

# ⑤ 取报告(批准 → outputs.report;合规不过 → outputs.revision;驳回 → FAILED 无产物)
curl -s $API/executions/$EXEC -H "Authorization: Bearer $TOKEN"
```

审计对账(高频四类事件经 reaper 搬运,默认 ≤1s 滞后;`execution.triggered` / `approval.*` 同步直写):

```sql
SELECT id, event_type, node_id, actor_id, trace_id, linked_trace_id FROM audit_log
  WHERE execution_id = ? ORDER BY id;   -- trace_id 全链一致;审批 decided 行 trace=段链 Z、linked_trace_id=root
```

也可走 T6.3 查询端点 `GET /api/v1/audit?execution_id=...`(或 `?trace_id=`;`?actor_id=`
维度 admin-only)。队列现场看 Asynqmon:http://127.0.0.1:8080。

幂等提示:触发可带 `Idempotency-Key` 头(重放 200 回显原执行);审批重放同为 200 回显(UI 双击安全)。
完整走链文档(含驳回/合规不过的事件序表)见 [docs/examples/doc-compliance-review.md](docs/examples/doc-compliance-review.md)。

## 3. 故障注入演示(T8.1 套件)

耐久性承诺的可复现证明:**worker / relay / reaper 任意时刻 SIGKILL、队列断连(下表五臂注入面),
执行在扫描间隔×3 内收敛到终态,不丢、不重、trace 不换段**。原理 = outbox at-least-once +
目标侧 CAS/确定性 TaskID 幂等 + reaper 对账。

前置 = L0 的 ①(测试栈已起),然后按臂跑:

```bash
# 五臂一次全跑(也可按 -run 单选)
go test ./internal/nodes -run 'TestFault' -v -count=1
go test ./internal/relay -run 'RedisUnreachable|Kill9' -v -count=1
go test ./internal/reaper -run 'FlushKillMidDrain' -v -count=1
```

| 臂 | 测试 | 注入 | 收敛断言 |
|----|------|------|----------|
| worker 中途 kill | `internal/nodes` `TestFaultWorkerKillMidRunConvergesTraceIntact` | 子进程 claim 停车点 SIGKILL | reaper 判死重派(携原行 trace),接管者 SUCCEEDED,trace 与 root 一致 |
| 审批 enqueue 失败 | `internal/nodes` `TestFaultApprovalEnqueueFailureTraceIntact` | 决定事务后投递失败 | 经 outbox/relay 补投,审批链三面断言(root / 段链 / linked 单点) |
| 队列断连 | `internal/relay` `TestRelayRedisUnreachableThenReconnect` | 死端口断连 → 恢复 | 恢复后恰一次补投,零丢失 |
| relay kill-9 | `internal/relay` `TestRelayKill9BeforeMarkThenIdempotentRedelivery` | 投递后、标记 DELIVERED 前 SIGKILL | 重启后幂等重投,目标侧恰一次生效 |
| reaper flush 中途 kill | `internal/reaper` `TestFlushKillMidDrainReplaysExactlyOnce` | 审计搬运排水中途 SIGKILL | 恢复后恰一次重放,暂存行×状态迁移原子性保持 |

预期输出:各臂 `-v` 下 `--- PASS: Test...`(SLA 口径 = 扫描间隔×3,验收臂实测 2.3–3.9s 级
收敛;压缩了 grace/scan 间隔的测试参数,生产默认更宽)。测试栈不在时同样自动 skip。

## 4. 文档地图

| 文档 | 内容 |
|------|------|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 架构定稿:18 项决策全表、技术栈选型依据、耐久性设计 |
| [docs/examples/](docs/examples/) | 端到端示例:DSL、样例输入、走链文档、审计事件序表 |
| [deploy/README.md](deploy/README.md) | 部署配额与容量:连接池公式、Redis 契约、保留期、账号授权矩阵 |
| [deploy/.env.example](deploy/.env.example) | env 旋钮单源(逐键注释) |
| [docs/ROADMAP.md](docs/ROADMAP.md) | 里程碑与任务验收记录 |
| [docs/DECISIONS.md](docs/DECISIONS.md) | 全部实现级决策条目(D-<date>-<slug>) |
