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
   「驳回后走修订回路」属 P1 停车场(回滚/断点续跑族),示例不伪造该语义。
3. **trace 已贯穿(T6.2)**。审计行 `trace_id` = 触发请求 root trace(全主链
   一致);审批决定事务起段链 Z(decided 行 `trace_id`=Z、`linked_trace_id`
   =root,是两链唯一交点);T6.3 查询端点支持 `?trace_id=` 按链取行。
   已知例外:reaper 进程行与 notify 链见 D-t62 已知边界。

> **DSL 作者注意**:llm 节点只要声明了 `outputs`,引擎就要求模型回复是
> JSON 对象(键名与声明一致)——散文回复会让节点以 `bad_llm_output`
> 失败收口。因此本示例全部 llm 节点都带 `response_format:"json"`,模板
> 里也显式写明返回键(如「以 JSON 对象返回,键 text 为摘要正文」)。

## 本地走链

前置:dev 栈(MySQL/Redis/Asynqmon)+ 四个 host 业务进程(api、relay、worker、reaper):

```bash
# 1) 基础设施(compose 只起 MySQL/Redis/Asynqmon;业务进程在 host 跑)
cp deploy/.env.example deploy/.env   # 首次
# 编辑 deploy/.env,至少打开:
#   BRAID_MYSQL_DSN / BRAID_AUTH_JWT_SECRET / BRAID_TOOL_TOKEN_SECRET
#   BRAID_LLM_BASE_URL / BRAID_LLM_API_KEY(OpenAI 兼容端点)
#   BRAID_FILEREAD_DIRS=["/abs/path/to/braid/docs/examples"]   # file_read 白名单
docker compose -f deploy/dev.docker-compose.yml up -d

# 2) 迁移 + 业务进程(四个终端,各自 source deploy/.env 或用 --env-file 注入)
go run ./cmd/api      # 默认 127.0.0.1:8081
go run ./cmd/relay
go run ./cmd/worker
go run ./cmd/reaper   # 审计异步批量搬运 + 审批超时扫描——不跑则下表高频
                      # 四类事件滞留 audit_pending 暂存表,对账查询看不到(不丢)
```

账号:当前版本无 CLI 建号入口;先用 MySQL 插入 author 与 approver 两个
账号(`users` + `user_role`,密码列存 bcrypt hash,对应
`internal/auth.HashPassword`),DSL 审批名单里的 `alice`/`bob` 须与
approver 账号名一致。

走链(认证面 = login 换 Bearer token,全程同一个 api 入口):

```bash
API=http://127.0.0.1:8081/api/v1

# ① author 登录
curl -s $API/auth/login -d '{"username":"<author>","password":"..."}'
TOKEN=<上一步 token>

# ② 提交定义(请求体 = DSL 原样字节)→ ③ 发布(发布点校验工具白名单)
curl -s -X POST $API/definitions -H "Authorization: Bearer $TOKEN" \
  --data-binary @docs/examples/doc-compliance-review.json
curl -s -X POST $API/drafts/<草稿 id>/publish -H "Authorization: Bearer $TOKEN"

# ④ 触发执行(输入 = 文件路径,须在 BRAID_FILEREAD_DIRS 白名单内)
curl -s -X POST $API/executions -H "Authorization: Bearer $TOKEN" \
  -d '{"definition_name":"doc-compliance-review",
       "input":{"doc_path":"/abs/path/to/braid/docs/examples/sample-doc.txt"}}'
EXEC=<上一步 id>

# ⑤ 观察挂起:状态变 WAITING_APPROVAL,待办列表出现该执行(approver 登录)
curl -s $API/executions/$EXEC -H "Authorization: Bearer $TOKEN"
curl -s $API/approvals/pending -H "Authorization: Bearer <approver token>"

# ⑥ 审批:浏览器开 http://127.0.0.1:8081/ui/(登录 → 待办 → 详情 → 决定),
#    或直接走页面同源 API:
curl -s -X POST $API/executions/$EXEC/approvals/human_review/decision \
  -H "Authorization: Bearer <approver token>" \
  -d '{"decision":"approve","comment":"同意,归档"}'

# ⑦ 取报告(批准 → outputs.report;合规 fail → outputs.revision;驳回 → FAILED 无产物)
curl -s $API/executions/$EXEC -H "Authorization: Bearer $TOKEN"
```

幂等提示:触发可带 `Idempotency-Key` 头(作用域 = 主体+定义,重放 200 回显
原执行);审批重放同为 200 回显(UI 双击安全)。

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
`BRAID_AUDIT_FLUSH_INTERVAL` 可调)——对账查询前若刚跑完执行,等 ≤ 该间隔
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
