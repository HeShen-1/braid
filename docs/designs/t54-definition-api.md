# T5.4 definition CRUD API — 接口契约

状态:已实现(feat/t5.4)。真源:issue #18 / ROADMAP T5.4;本文是 REST 面的精简契约文档(不上 OpenAPI)。

## 认证与角色

- `Authorization: Bearer <JWT>`(HS256,`auth.ParseToken` 校验;签发面归 T5.2)。
- 写操作(创建/更新/发布/归档):`author` 或 `admin`;读操作:任意有效 token。
- 认证失败:401 `missing_token` / `token_invalid` / `token_expired`;角色不足:403 `forbidden`。
- T5.2 RBAC 中间件落地时整块替换 `internal/api/middleware.go`,路由声明面不变。

## 路由

| 方法 | 路径 | 说明 | 角色 |
|---|---|---|---|
| POST | `/api/v1/definitions` | 创建草稿(body = DSL 原文,≤1MiB) | 写 |
| GET | `/api/v1/definitions?status=&limit=&offset=` | 列表(updated_at DESC;status ∈ DRAFT/PUBLISHED/ARCHIVED/空) | 读 |
| GET | `/api/v1/definitions/{name}` | 最新 PUBLISHED 详情(默认触发解析目标) | 读 |
| GET | `/api/v1/definitions/{name}/versions` | 已发布版本列表(升序) | 读 |
| GET | `/api/v1/definitions/{name}/versions/{version}` | 指定版本详情 | 读 |
| POST | `/api/v1/definitions/{name}/versions/{version}/archive` | 归档(PUBLISHED→ARCHIVED,单向) | 写 |
| GET | `/api/v1/drafts/{id}` | 草稿详情 | 读 |
| PUT | `/api/v1/drafts/{id}` | 更新草稿(重校验+重算哈希) | 写 |
| POST | `/api/v1/drafts/{id}/publish` | 发布(白名单校验+版本分配) | 写 |
| GET | `/healthz` | 存活探针 | 无认证 |

草稿走独立 `/api/v1/drafts` 前缀:gin 路由树不允许同层 static 与 param 混挂(`/definitions/drafts/{id}` 与 `/definitions/{name}` 冲突)。

## 状态码与错误体

统一 `{ "error": "<code>" }`;DSL 校验失败另带 `issues: [{code,node,ref,detail}]`(dsl.Parse 聚合,不 fail-fast)。

| 码 | error | 语义 |
|---|---|---|
| 400 | `invalid_dsl` | schema/语义校验失败或工具不在白名单(`tool_not_allowed`) |
| 400 | `name_mismatch` | 更新草稿时 DSL `$.name` 与行 name 不一致(改名=新工作流) |
| 400 | `bad_id` / `bad_version` / `bad_status` / `bad_limit` / `bad_offset` / `bad_request` | 参数形态非法;`bad_request` = 请求体传输层读失败(断连/坏 chunked——**不是**超限) |
| 401/403 | 见认证节 | |
| 404 | `not_found` | 未知 id/name/version;全归档后的 `{name}`(禁新触发) |
| 409 | `conflict` | 状态机违例(重发布/更新非草稿)、并发发布 uk 兜底或 InnoDB 死锁回滚(1213,重试安全) |
| 413 | `body_too_large` | DSL 请求体 >1MiB(仅真超限 `*http.MaxBytesError`) |
| 500 | `internal` | 存储故障(细节只进日志,响应体零泄漏) |

## 关键语义

- **ID 线形态**:`id` 序列化为**字符串**——snowflake id ≈ 9.6e16 已超 float64 精确整数界 2^53,JSON 数字形态会被 JS/Python 等消费方静默截断成相邻 id。`version` 为小整数,保持数字。
- **版本分配**:发布事务内 `MAX(version)+1`;同名并发发布由 `uk_def_name_version` 兜底 → 一方 409(显式失败是并发闸,败方草稿保留可重试)。
- **发布校验的原子性**:Publish 起手 `SELECT ... FOR UPDATE` 锁定草稿行——白名单校验的对象必须是即将翻转的那份内容;纯快照读 + 只钉状态的 guarded UPDATE 会让并发草稿更新绕过白名单直接落 PUBLISHED(TOCTOU)。
- **dsl_hash**:SHA-256(**存储形态字节**,MySQL JSON 列规范化后)——写入两步(INSERT/UPDATE + 读回定稿 hash,事务包裹);执行侧(T5.5)对存储字节重算比对即漂移检测。
- **工具白名单**:发布点收紧(`dsl.WithToolAllowlist`);名单来自 tools.Registry 注册名(api 装配注入,`BRAID_TOOL_ALLOWLIST` env 已随 T3.2 退役——见 D-2026-09-23-t32-tool-runtime #3);空 registry = 禁用一切工具;草稿创建/更新不校验白名单。
- **归档回退**:归档最新版后 `{name}` 解析回退到上一个 PUBLISHED;全归档 → 404。
- **分页**:`limit` 1..100(默认 20)、`offset` 0..10000(深翻页防护);响应 `page: {limit, offset, total}`。
- **草稿定位**:按 id(同名多草稿共存下唯一无歧义);DSL `$.name` 不可变。
- **草稿并发更新**:v1 无 If-Match/版本前置条件,同草稿并发 PUT 为 last-writer-wins(写路径行级锁定读串行化,无中间态);乐观锁前置条件列为后续增强,非本任务验收面。
- **幂等保存**:PUT 回放存储形态字节(赋值与现值全同)是成功而非 409(changed-rows 语义下重读状态判真伪)。
