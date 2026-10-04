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

# ③ (可选)故障注入五臂 —— 见 §4
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

**一键形态(T16.3,`braid seed`)**——配置后单命令跑通整条链:

```bash
docker compose -f deploy/dev.docker-compose.yml up -d   # 起基础设施
deploy/dev-up.sh
# dev-up.sh = MySQL 就绪等待 → seed db 段(迁移 + 建 alice/bob 演示号)→
# 四进程 → seed rest 段(发布 agent-council + 触发一次评审 + 等待落点)。
# 全程幂等,重跑安全(迁移 no-op / 已建号跳过 / 定义按名判存 / 触发幂等
# 键命中回显);BRAID_DEV_SEED=0 跳过 seed 只起进程。
# seed ⑤ 行即落点:WAITING_APPROVAL → 开 /ui/ 用 alice 或 bob 登录批准
# (dev 默认口令 dev-author-1 / dev-approver-1;经 BRAID_SEED_*_PASSWORD
# 覆盖,覆盖后不回显)。触发幂等键含输入摘要:改 seed 内置演示方案文案
# → 下次重跑触发新执行。跳过 demo seed 独立重跑:`go run ./cmd/braid seed`
# (栈已在跑时的 all 形态)。
```

分步形态(交互式调试 / 理解每一步;dev-up.sh 已把 ②③④⑤ 的机械部分自动化):

```bash
# ② 起基础设施(MySQL 3306 / 队列 Redis 6379 / 缓存 Redis 6380 / Asynqmon 8080,均仅绑回环)
docker compose -f deploy/dev.docker-compose.yml up -d
#    MySQL 首次初始化要十几秒,等 healthy 再做 ③(否则 migrate 报 connection refused):
until docker exec braid-dev-mysql mysqladmin ping -uroot -pbraid_dev_root --silent 2>/dev/null; do sleep 1; done

# ③ 建库(golang-migrate CLI 指向仓内迁移;DSN 四参数是全库连接契约,缺一不可。
#    一键形态不需要此步:seed db 段进程内跑同一套嵌入迁移,CLI 安装仅分步
#    形态需要。装到 $(go env GOPATH)/bin,不在 PATH 的机器先 export
#    PATH="$PATH:$(go env GOPATH)/bin";DSN 口令按 .env 的
#    MYSQL_ROOT_PASSWORD 默认占位写死,改过口令则同步改)
go install -tags 'mysql' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.20.1
migrate -path internal/db/migrations \
  -database "mysql://root:braid_dev_root@tcp(127.0.0.1:3306)/braid?loc=UTC&parseTime=true&multiStatements=true&time_zone=%27%2B00%3A00%27" up
# 重复执行是 no-op;dirty 恢复 runbook 见 docs/ROADMAP-P0.md T7.4(P0 归档)

# ④ 建号(T16.2 CLI,连接形态 D-2026-10-03-t162-cli-connection:DB 直连
#    复用 auth 原语,词域/哈希/角色校验与服务端同则;示例 DSL 审批名单 =
#    alice / bob,账号名必须一致——一键形态的 seed db 段即下面两条命令的
#    幂等版(口令走 BRAID_SEED_*_PASSWORD,dev 默认同占位值)。
#    凭证纪律:口令只经 BRAID_CLI_PASSWORD
#    env 注入,绝不写进命令行参数(shell history / ps 可见)——真实口令
#    交互输入用 read -s(bash/zsh 通用:read -s pw;回车后 export
#    BRAID_CLI_PASSWORD=$pw && unset pw)。下面两行 env 前缀明文写口令,
#    仅因 dev 占位口令非机密;生产/真实口令一律走 read -s 形态。
#    重跑报 "username already exists"(auth: conflict)= 已建过,属预期
#    ——彻底重置用 docker compose -f deploy/dev.docker-compose.yml down -v
#    后从 ② 重来。dev 占位口令生产必须换强密码。退出码:0 成功 / 1 失败 / 2 用法错。
go build -o /tmp/braid ./cmd/braid
BRAID_MYSQL_DSN='root:braid_dev_root@tcp(127.0.0.1:3306)/braid' \
BRAID_CLI_PASSWORD='dev-author-1' \
  /tmp/braid user create -username alice -roles author,approver
BRAID_MYSQL_DSN='root:braid_dev_root@tcp(127.0.0.1:3306)/braid' \
BRAID_CLI_PASSWORD='dev-approver-1' \
  /tmp/braid user create -username bob -roles approver

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

CLI 等价形态(T16.2):②(登录换 token)由 env 凭据自动完成,③ 提交+发布、
触发各是一行命令;④ 审批决定与 ⑤ 取报告仍走上面的 API/UI 面。凭证纪律同
建号:token/凭据只经 env 注入,不进命令行参数——CLI 的解析链 `BRAID_TOKEN`
直用,否则 `BRAID_USERNAME`+`BRAID_PASSWORD` 自动 login 换 token(真实凭据
用 read -s 输入后 export,勿明文):

```bash
set -a; source deploy/.env; set +a   # BRAID_API_ADDR 兼容 host:port 监听值与完整 URL(自动补 http://)
export BRAID_USERNAME=alice BRAID_PASSWORD=dev-author-1
/tmp/braid definition publish docs/examples/doc-compliance-review.json
echo '{"doc_path":"<仓库绝对路径>/docs/examples/sample-doc.txt"}' \
  | /tmp/braid execution trigger -definition doc-compliance-review -input -
# 触发可带幂等键(同键重放 200 回显原执行,退出码同为 0):-idempotency-key <key>;钉版:-version <n>
```

审计对账(高频四类事件经 reaper 搬运,默认 ≤1s 滞后;`execution.triggered` / `approval.*` 同步直写):

```sql
SELECT id, event_type, node_id, actor_id, trace_id, linked_trace_id FROM audit_log
  WHERE execution_id = ? ORDER BY id;   -- trace_id 全链一致;审批 decided 行 trace=段链 Z、linked_trace_id=root
```

也可走 T6.3 查询端点 `GET /api/v1/audit?execution_id=...`(或 `?trace_id=`;`?actor_id=`
维度 admin-only)。队列现场看 Asynqmon:http://127.0.0.1:8080。

### 2.3 多智能体编排示例(agent-council)

`docs/examples/agent-council.json`:技术方案评审会——主持人提炼 →
架构/安全/成本三评审员**并行评审**(DAG 分叉)→ 首席仲裁**菱形汇合**裁决 →
人工审批(审批语义 = 放行/否决,与仲裁结论正交)→ branch 按仲裁 verdict 路由
approve 臂(决策层终报)或 revise 臂(分条修订计划)。§2.2 的一键 seed
触发的就是本示例(输入 = seed 内置演示方案文案 `demoProposal`);手动
走链提交发布后触发:

```bash
curl -s -X POST $API/executions -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{
    "definition_name": "agent-council",
    "input": {"proposal": "<待评审的技术方案文本,数百字>"} }'
# 挂起 WAITING_APPROVAL 后在 /ui/ 审批;批准 → 按仲裁结论出 decision_report 或 revision_plan
```

**真模型(思考型)注意事项**——桩测试发现不了,真 key 首跑即踩(2026-09-28 智谱
glm-5.3 实测):

- llm 节点声明了 outputs 时,prompt 必须写明 JSON 键名与类型(本仓示例的
  「以 JSON 对象返回:键 X 为…」句式)。模型不会猜声明形状,缺契约 = 键名自由
  发挥 → `bad_llm_output` 重试耗尽 fail-fast;
- **不要给思考型模型设 `max_tokens`**:上限框住的是思考+正文总预算,预算内
  思考未结束响应 content 为空 → `bad_llm_output: ... not a JSON object: EOF`。
  容量控制用 prompt 篇幅约束(「N 字以内」),超时控制用 `BRAID_LLM_TIMEOUT`。

本示例有发布同口径守护测试(`internal/engine/dsl/example_asset_test.go`:
最严工具白名单态 Parse + 拓扑锚,DSL 校验或资产漂移即红);真模型端到端
走链仍需按需人工复跑(桩测试发现不了思考型模型行为,见上方注意事项)。

幂等提示:触发可带 `Idempotency-Key` 头(重放 200 回显原执行);审批重放同为 200 回显(UI 双击安全)。
完整走链文档(含驳回/合规不过的事件序表)见 [docs/examples/doc-compliance-review.md](docs/examples/doc-compliance-review.md)。

### 2.4 交付镜像构建与导出(T16.5)

发布通道 = 本地导出(零 Secrets 姿态;GHCR 评估结论与重访触发器见
D-2026-10-03-t165-image-delivery):

```
deploy/release.sh build                 # braid:<short-sha> 钉版 + braid:prod 浮动
deploy/release.sh save                  # dist/braid-<sha>.tar + .sha256
deploy/release.sh load <tar>            # 目标机:验和后 docker load
deploy/release.sh k3s-import <tar>      # k3s 面导入 containerd
```

钉版部署 = Helm `--set image.tag=<short-sha>`;完整口径(完整性校验、
`--provenance=false`、GOPROXY 解析链、脏树守卫)见
[deploy/README.md §12](deploy/README.md)。

## 3. 性能基线(T15.5 实测)

数字真源 = [docs/CAPACITY.md](docs/CAPACITY.md)(本章逐字引用;测量环境
与复跑口径见其 §1/§2)。引用口径先讲清,不夸大:

- **保守下界**:测量窗机器 load average ≈ 52(1/5/15min 全程)、同形状
  运行间方差实测 ±2×(300 行档 fixture 1.78s vs 真路径 0.42s)——本章
  全部数字是负载竞争下的**保守下界**,独占窗口下 CPU 侧只会更好;DB
  行锁/事务形状类指标的斜率结论对 CPU 竞争相对不敏感,但绝对墙时仍偏保守。
- **存储乐观包络**:t13 测试栈(MySQL 8.0.46 默认 innodb,buffer_pool=128MB
  默认),datadir 在 tmpfs——写路径无落盘成本,生产盘速下写侧打折;
  Redis 7.4 noeviction(队列语义真源在 MySQL)。
- **负载模型**:即回执行器(echo,零外部调用)× 线性 llm 链 / fan-out
  网格,隔离 engine 与管道成本;真实 LLM/工具调用窗不占 engine 资源,
  吞吐结论不降,只占执行时延与 worker 并发槽(§3.5 外推)。

### 3.1 端到端吞吐 / 时延(真 relay + asynq + worker)

| 并发 | relay 轮询 | nodes/s | execs/s | p50 ms/节点 | p99 ms/节点 |
|---|---|---|---|---|---|
| 1 | 1s(生产缺省) | 6.51 | 0.41 | 1155 | 1173 |
| 4 | 1s | 6.30 | 0.39 | 1252 | 1377 |
| 8 | 1s | 6.37 | 0.40 | 1162 | 1217 |
| 1 | 20ms(压缩对照) | **29.5** | 1.85 | **208** | 225 |
| 8 | 20ms(压缩对照) | 7.41 | 0.46 | 971 | 1148 |

三条结构性结论(管道模型,不随机器变):

1. **时延地板 = relay 轮询周期**:链式 DAG 每跳经 outbox,稀疏流量下
   relay 空转间隔直接成为每节点时延地板——生产缺省 1s 档 p50 ≈
   1155-1252ms/节点(逐跳等轮询),压缩 20ms 档 p50 = 208ms = 引擎
   管道净成本(调度事务 ×2 + 队列往返 + asynq 派发)。
2. **吞吐帽 = relay 单事件单事务**:worker 并发 1→4→8 在生产轮询档下
   吞吐纹丝不动(6.51 / 6.30 / 6.37 nodes/s,差 < 3%)——帽在 relay
   主循环(单行 FOR UPDATE SKIP LOCKED + 单 Redis 入队/事务),不在
   worker。
3. **高并发 × 快轮询在 DB 争用下反噬**:20ms+8 并发档吞吐塌至 7.4
   nodes/s、p50 回升 971ms——8 路 Complete 收口事务(各自全行锁)+
   relay 单事件事务在共享 innodb 上互相挤压(中转瞬态错误触发 relay
   退避链 5s→20s→80s→320s)。「加并发先过 DB 写串行段」:并发度 =
   f(DB 写延迟),不是免费旋钮。

### 3.2 核心事务成本(scheduler 面)

- Instantiate 物化(触发事务):16 / 64 / 256 节点 = 8.7 / 51.6 / 218.7
  ms/op(≈0.54→0.85 ms/行,随节点数线性);
- Complete 收口(生产 worker 收口事务:全行锁 + attempt 收口 + CAS +
  advance 派发 + touch):16 / 64 节点链(行集 18 / 66)= 20.3 / 16.8
  nodes/s(≈49 / 60 ms/节点)——单节点收口成本随执行总行数(含
  parallel 实例行)单调增长。

绝对值受负载窗影响,斜率形状是结构性结论。

### 3.3 大 DAG 回滚锁窗与 MaxRollbackRows 定档

两段线性分解:纯锁行/扫描 ≈ **1.1 ms/行**;回滚事务内 fan 重派
(fanout 重解析 + 逐实例 claim + 快照重求值)≈ **5.8 ms/实例**——
≤4k 行段的主导项是 fan 重派而非锁本身。定档 `DefaultMaxRollbackRows =
4000`:负载态最差形状(单 fan × 997 实例)≈ 10s、典型形状 ≤10s 包络;
env `BRAID_ROLLBACK_MAX_ROWS` 覆盖,`0` 关闭;超限 409 `rollback_too_large`,
拒绝面成本 12.9 ms(COUNT 索引范围扫,= 每笔回滚的闸内开销)。DSL
上限形状(512 fan × 1000 实例 ≈ 512k 行)外推锁行项 ≈ 分钟级 = 该
守卫的结构性存在理由。

### 3.4 审计查询延迟(v1 不建旁路表,D-t63#3 维持)

| 体量 | trace hit / miss | actor hit / miss | 深页(50 页游标) |
|---|---|---|---|
| 200k 行 / 13 分区 | 2.4 / 0.5 ms | 6.8 / 4.5 ms | 4.9 ms/页(50 页 244ms) |
| 1M 行 / 13 分区 | 3.5 / 3.5 ms | 17.7 / 3.1 ms | 16.9 ms/页(50 页 846ms) |

判据 = 交互容忍线 1s:实测最重形态(actor 深页,1M 行)16.9ms/页、
整走 846ms,距该线 ≈ **59×**;体量 5×(200k→1M)延迟增长 ≈ 2.5×
(actor-hit 6.8→17.7ms),外推破线需数千万行级。

### 3.5 容量外推

- 单 worker 进程吞吐帽:生产形态(relay 1s)≈ **6.4 nodes/s**;轮询
  压缩后净帽 ≈ **29.5 nodes/s**(单并发)——两帽都在 relay,worker
  并发不解除;
- 日吞吐外推(生产形态):6.4 nodes/s ≈ 55 万节点/天 ≈ 3.4 万次 16
  节点执行/天(单副本、echo 负载;真实 LLM 调用窗不占 engine 资源,
  吞吐不降,但受并发槽与外部配额另限);
- 每节点端到端成本 ≈ 管道净成本 208ms + relay 轮询等待(稀疏 ≈
  1s/跳,密集摊零)+ 执行器外部调用窗;DB 写压 = 每节点 2 事务
  (收口 + 心跳摊销)+ Instantiate/执行级事件;
- 大 DAG 预算:节点收口成本随行集线性增长(§3.2),回滚锁窗同斜率
  (§3.3)——>4000 行的执行在触发侧即受 DSL 上限约束,设计上应拆分。

## 4. 故障注入演示(T8.1 套件)

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

## 5. 文档地图

| 文档 | 内容 |
|------|------|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 架构定稿:18 项决策全表、技术栈选型依据、耐久性设计 |
| [docs/CAPACITY.md](docs/CAPACITY.md) | 压测基线与容量模型(§3 性能章数字真源;复跑口径其 §2) |
| [docs/examples/](docs/examples/) | 端到端示例:DSL、样例输入、走链文档、审计事件序表 |
| [deploy/README.md](deploy/README.md) | 部署配额与容量:连接池公式、Redis 契约、保留期、账号授权矩阵 |
| [deploy/.env.example](deploy/.env.example) | env 旋钮单源(逐键注释) |
| [docs/ROADMAP.md](docs/ROADMAP.md) | 路线图(P3 活文档,M18-M20) |
| [docs/ROADMAP-P2.md](docs/ROADMAP-P2.md) | P2 交付归档(M15-M17 + P2-DELIVERY) |
| [docs/ROADMAP-P1.md](docs/ROADMAP-P1.md) | P1 交付归档(18 任务注记 + P1-DELIVERY) |
| [docs/ROADMAP-P0.md](docs/ROADMAP-P0.md) | P0 交付归档(38 任务全量注记) |
| [docs/DECISIONS.md](docs/DECISIONS.md) | 全部实现级决策条目(D-<date>-<slug>) |
