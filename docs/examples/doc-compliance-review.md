# T8.2 端到端示例 — 文档合规审查(doc-compliance-review)

状态:✅ 2026-09-24 随 T8.2(issue #51)交付。示例资产与守护测试单一真源:
[`doc-compliance-review.json`](doc-compliance-review.json)(DSL)+ [`sample-doc.txt`](sample-doc.txt)(样例输入)+ [`internal/nodes/e2e_doc_example_test.go`](../../internal/nodes/e2e_doc_example_test.go)(真链守护,资产烂即红)。

## 场景

一份业务文档(如费用报销申请)从文件路径进入,机器先读、再解析、再合规审查;
合规通过才生成摘要、进入人工审批,批准后产出最终报告。合规不过则改道产出
修订建议,不进入人工审批。

```
read_doc ──▶ parse ──▶ compliance ──▶ gate(branch)
                                  │ verdict==pass        │ default
                                  ▼                      ▼
human_review ◀── summary ◀────(主线)              report_revision(独立尾)
    │ decision
    ▼
  report ──▶ outputs.report / outputs.revision
```

## 语义要点(三条,都与引擎真实行为对齐)

1. **branch 挂在合规结论上,修订尾不与主线汇聚**。调度器的剪枝语义是
   「任一 SKIPPED 前驱 → 后继一并剪枝」(`internal/scheduler/sched.go` advance),
   菱形汇聚点(如 revision→report 回边)在 pass 路径会被级联剪掉,形成
   「SUCCEEDED 但报告为 null」的假成功。因此修订尾是独立终点,两个工作流
   输出键各锚各的节点:pass → `report` 有值、`revision` 为 null;fail 反之
   (null 是显式物化,不是缺键)。
2. **驳回 = 终败收口,不是分支输入**。审批节点被驳回时以
   `approval_rejected` 终败并触发 fail-fast(下游不推进,执行 FAILED,无
   outputs)——这是引擎现义(`internal/scheduler/resume.go` failApprovalNode)。
   本示例不把驳回改写为自动转修订分支；人工回滚/续跑沿用现有协议，
   不改变这里的驳回终败语义。
3. **trace 已贯穿(T6.2)**。审计行 `trace_id` = 触发请求 root trace(全主链
   一致);审批决定事务起段链 Z(decided 行 `trace_id`=Z、`linked_trace_id`
   =root,是两链唯一交点);T6.3 查询端点支持 `?trace_id=` 按链取行。
   已知例外:reaper 进程行与 notify 链见 D-t62 已知边界。

> **DSL 作者注意**:llm 节点只要声明了 `outputs`,引擎就要求模型回复是
> JSON 对象(键名与声明一致)——散文回复会让节点以 `bad_llm_output`
> 失败收口。因此本示例全部 llm 节点都带 `response_format:"json"`,模板
> 里也显式写明返回键(如「以 JSON 对象返回,键 text 为摘要正文」)。

## 本地走链

前置:在主仓根目录运行，使用自己的 dev 栈(MySQL/Redis/Asynqmon)和已配置的
OpenAI 兼容端点。先确定真实调用预算；下面的准备步骤只迁移/建号及启动进程，
定义发布与模型执行在后文手动触发。worktree 只跑测试，测试隔离按 AGENTS.md。

```bash
# 1) 基础设施(compose 只起 MySQL/Redis/Asynqmon;业务进程在 host 跑)
test -f deploy/.env || cp deploy/.env.example deploy/.env
# 编辑 deploy/.env,至少打开:
#   BRAID_MYSQL_DSN / BRAID_REDIS_QUEUE_ADDR / BRAID_AUTH_JWT_SECRET
#   BRAID_LLM_BASE_URL / BRAID_LLM_API_KEY / BRAID_LLM_PRICE_TABLE
#   (OpenAI 兼容端点 + 模型单价表;worker 缺 PRICE_TABLE 启动即败)
#   BRAID_FILEREAD_DIRS='["/abs/path/to/braid/docs/examples"]' # 换为真实宿主路径
# DSN、JSON 单价表/白名单等特殊值整体用单引号包住,规则见 deploy/.env.example。
docker compose -f deploy/dev.docker-compose.yml up -d --wait --wait-timeout 60

# 2) 基础设施已就绪,导出配置,执行仓内嵌入迁移并建演示账号(只触 MySQL)
set -a
. deploy/.env
set +a
go run ./cmd/braid seed -stage db

# 3) 启动 host 四进程;跳过默认 agent-council 的发布/触发,不自动调用模型
BRAID_DEV_SEED=0 deploy/dev-up.sh
# 脚本先起 reaper,通过 audit-ready 分区水位闸后再起 api/relay/worker。
# api 默认 127.0.0.1:8081;日志在 log/latest,停止用 deploy/dev-down.sh。
```

`seed -stage db` 使用仓内嵌入迁移，重复执行迁移为 no-op、已有账号跳过，
创建 `alice`(author/approver)与 `bob`(approver)。dev 占位口令及
`BRAID_SEED_*_PASSWORD` 配置见 [README §2.2](../../README.md#22-dev-栈--真实-llm-完整链路开发联调)；
已有账号不会被 seed 改口令。自建账号用 `braid user create`，DSL 名单中的
`alice`/`bob` 须对应 approver。若手动开四终端，每个终端先
`set -a; . deploy/.env; set +a`，并保持 reaper→audit-ready→其余进程的顺序，
详见 [dev-up.sh](../../deploy/dev-up.sh)。Go 入口读进程环境，不读取 `.env` 文件，
也没有 `--env-file` 参数。

### 正式 Compose 的文件路径

正式栈使用 `deploy/prod.env`；首账号、模型配置与部署步骤见
[部署说明](../../deploy/README.md)，不要把 host dev 的路径直接用于容器执行。
以宿主 `/srv/braid/docs/sample-doc.txt` 为例，在 `prod.env` 配置：

```dotenv
BRAID_FILEREAD_HOST_DIR=/srv/braid/docs
BRAID_FILEREAD_DIRS='["/var/braid/docs"]'
```

宿主目录和文件须预先存在、允许镜像 uid 65532 遍历/读取。Compose 只读挂载
到 worker 的 `/var/braid/docs`，因此后文触发请求的 `input.doc_path` 改为
`/var/braid/docs/sample-doc.txt`；api 只注册/校验工具，不需要文件挂载。
默认 `deploy/tool-data` 是空目录，不自带本示例文档。具体配置与预算口径见
[生产 worker 工具与预算配置](../../deploy/README.md#13-生产-worker-工具与预算配置t233)。
正式配置完整业务旅程的验收仍归 T23.13，本节只是操作说明。

走链(认证面 = login 换 Bearer token,全程同一个 api 入口):

```bash
API=http://127.0.0.1:8081/api/v1

# ① author 登录
curl -s $API/auth/login -d '{"username":"<author>","password":"..."}'
TOKEN='<上一步 token>'

# ② 提交定义(请求体 = DSL 原样字节)→ ③ 发布(发布点校验工具白名单)
curl -s -X POST $API/definitions -H "Authorization: Bearer $TOKEN" \
  --data-binary @docs/examples/doc-compliance-review.json
DRAFT_ID='<草稿 id>'
curl -s -X POST "$API/drafts/$DRAFT_ID/publish" -H "Authorization: Bearer $TOKEN"

# ④ 触发执行(输入 = 文件路径,须在 BRAID_FILEREAD_DIRS 白名单内)
curl -s -X POST $API/executions -H "Authorization: Bearer $TOKEN" \
  -d '{"definition_name":"doc-compliance-review",
       "input":{"doc_path":"/abs/path/to/braid/docs/examples/sample-doc.txt"}}'
EXEC='<上一步 id>'

# ⑤ 观察挂起:状态变 WAITING_APPROVAL,待办列表出现该执行(approver 登录)
curl -s $API/executions/$EXEC -H "Authorization: Bearer $TOKEN"
curl -s $API/approvals/pending -H "Authorization: Bearer <approver token>"

# ⑥ 审批:浏览器开 http://127.0.0.1:8081/approvals/(登录 → 待办 → 详情 → 决定),
#    或直接走页面同源 API:
curl -s -X POST $API/executions/$EXEC/approvals/human_review/decision \
  -H "Authorization: Bearer <approver token>" \
  -d '{"decision":"approve","comment":"同意,归档"}'

# ⑦ 取报告(批准 → outputs.report;合规 fail → outputs.revision;驳回 → FAILED 无产物)
curl -s $API/executions/$EXEC -H "Authorization: Bearer $TOKEN"
```

幂等提示:触发可带 `Idempotency-Key` 头(作用域 = 主体+定义,重放 200 回显
原执行);审批重放同为 200 回显(UI 双击安全)。

上例省略 `version` 时沿用 API 的按名取最新已发布版本语义；需要指定版本时，
在触发 JSON 顶层加整数 `version`，值取刚发布的响应。界面选中版本的钉版触发
仍由 T23.6 验收，不能因已有该 API 字段而记为界面已通过。

## 审计链(降级口径:按 execution 串联)

批准主线的完整事件序(`ORDER BY id`,雪花序 = 落行序):

| 段 | 事件 |
|---|---|
| 触发 | `execution.triggered`(执行级,node_id NULL) |
| read_doc | `node.state_changed` → `tool.call.completed` → `node.state_changed` |
| parse / compliance / summary | 各:`node.state_changed` → `llm.call.completed` → `node.state_changed` |
| gate | `node.state_changed` ×2(内联执行)→ `branch.evaluated` → `node.state_changed`(独立尾剪枝) |
| human_review | `node.state_changed` ×2(执行→挂起)→ `approval.requested` → `approval.decided`(决定事务)→ `node.state_changed` ×2(续跑→成功) |
| report | `node.state_changed` → `llm.call.completed` → `node.state_changed` |

对账查询:

```sql
SELECT id, event_type, node_id, actor_type, actor_id
  FROM audit_log WHERE execution_id = ? ORDER BY id;
```

`trace_id` = root trace 全链一致;审批 decided 行 `trace_id`=段链 Z 且
`linked_trace_id`=root(T6.2 落地;reaper/notify 例外见 D-t62 已知边界)。

可见性滞后(T6.1 异步批量):表中高频四类(`node.state_changed` /
`branch.evaluated` / `llm.call.completed` / `tool.call.completed`)先同事务
写入 `audit_pending` 暂存表,由 reaper 周期搬运(默认间隔 1s,
`BRAID_REAPER_AUDIT_FLUSH_INTERVAL` 可调)——对账查询前若刚跑完执行,等 ≤ 该间隔
再查;`execution.triggered` 与 `approval.*` 系同步直写,无滞后。

## 守护测试

`internal/nodes/e2e_doc_example_test.go` 三用例(t13 栈不可达自动 skip):

- **ApproveProducesReport**:主线全链(定义经 API 提交+发布 → 触发 → 页面
  同源序列批准 → SUCCEEDED),审计链全序逐行断言 + trace 一致性三件套
  (root 全链一致 / 审批 linked 段链 / 按 trace 查询面)+ 文件
  内容真实流到 LLM 桩;
- **ComplianceFailRoutesToRevision**:branch default 边 + 主线级联 SKIPPED
  + `outputs.revision` 物化;
- **ApprovalRejectFailsFast**:驳回 → `approval_rejected` 终败 + fail-fast。
