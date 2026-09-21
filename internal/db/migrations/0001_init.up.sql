-- ============================================================================
-- braid 0001_init.up.sql — M1 T1.3「契约先行」:引擎域全部核心表 v1
-- 方言:MySQL 8.0.16+(CHECK 约束强制执行的最低版本);InnoDB;utf8mb4 / utf8mb4_0900_ai_ci。
-- golang-migrate(mysql driver)DSN 必须带 multiStatements=true(internal/db.Migrate 已封装);
--   全部对象 IF NOT EXISTS / down 全 DROP IF EXISTS → up/down 重复执行均 no-op。
--
-- 时间:一律 DATETIME(3) 毫秒精度、语义一律 UTC。连接契约:DSN loc=UTC&parseTime=true
--   且连接初始化执行 SET time_zone='+00:00'(DATETIME 存字面值,DEFAULT CURRENT_TIMESTAMP(3)
--   按会话时区求值 → 会话 UTC 即真相;不要求 mysqld 级 default-time-zone)。
--   否决 TIMESTAMP:2038 上限 + 隐式时区换算在复制/导出/比对路径不可控。
-- ID:全部主键 = 应用侧 Snowflake(BIGINT UNSIGNED:1bit 符号 + 41bit 毫秒 + 10bit worker
--   + 12bit 序列)。否决自增:①确定性 Asynq TaskID 与 attempt 幂等键要求在事务提交前引用行 ID;
--   ②audit_log 分区主键 (exec_date,id) 中 id 非首列,与自增列必须为某索引首列的规则不相容;
--   ③雪花近似时序白拿 outbox relay FIFO、审计链行序、游标分页三个查询形态。
--   worker_id P0 = compose 容器槽位静态配置(T7.1);时钟回拨 = 生成器有界阻塞(≤2s)+ 超限快速失败 + NTP 偏移监控
--   告警;lease 式自动分配进 P1 停车场。
-- 枚举:ENUM 只允许「末尾追加」(尾部追加的 ENUM MODIFY 为 INSTANT/仅元数据);禁止重排/改名/
--   复用已废弃值。开放词表(event_type/error_code/actor_type)用 VARCHAR —— 演进快于 DDL,
--   审计绝不能被枚举卡写。
-- append-only(audit_log)= 业务 DB 账号仅授 INSERT/SELECT(部署层授权;T7.3 安装自检断言
--   information_schema 中该账号对 audit_log 无 UPDATE/DELETE 权限)。不用触发器:golang-migrate
--   mysql 驱动执行链路无法处理 DELIMITER/复合语句体;授权层构成可验证强制。保留期 = DROP
--   PARTITION(DDL,不触行级 DELETE,与 append-only 同向)。
-- 状态迁移:node_execution 走 CAS(UPDATE ... WHERE id=? AND status=? AND version=?);
--   workflow_execution 状态迁移低频,走 status-only CAS(目标态互斥收敛,不设 version 列);
--   均禁止裸 UPDATE(AGENTS 铁律)。CAS 参考语句见文件尾。
-- 审计记录:2026-09-21 对抗审计(6 视角,51 发现,blocking/high 双反驳验证)修复版——
--   CHARACTER SET 列属性顺序(3×blocking 合并)、audit 批量写契约 INSERT IGNORE→ODKU、
--   heartbeat_at 初值契约、asynq_task_id 写入方、分区维护职责(ROADMAP T2.5/T6.1/T7.3/T7.4 承载)。
-- ============================================================================

SET NAMES utf8mb4;

-- -----------------------------------------------------------------------------
-- 1. workflow_definition — 工作流定义:一行 = 一个版本;草稿 version 为 NULL,
--    发布时才分配号(uq 对同名同号并发发布显式失败,是 T5.4 的并发闸)。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_definition (
  id           BIGINT UNSIGNED  NOT NULL COMMENT 'Snowflake,应用生成(发布前可知,供钉版引用)',
  name         VARCHAR(64)      NOT NULL COMMENT '= DSL $.name,规则 ^[a-z][a-z0-9-]{1,63}$;跨版本稳定(改名=新工作流)',
  version      INT UNSIGNED     NULL COMMENT '发布版本号(同名内自 1 单调递增);NULL=草稿。唯一索引允许多 NULL ⇒ 同名多草稿共存,发布事务内分配 MAX(version)+1;RR 隔离下并发发布的间隙锁竞态由 uk 兜底(同名同号显式失败)',
  dsl_version  VARCHAR(8)       NOT NULL COMMENT '= DSL $.dsl(如 1.0);T7.4 N-1 兼容窗口按列圈选存量',
  status       ENUM('DRAFT','PUBLISHED','ARCHIVED') NOT NULL DEFAULT 'DRAFT' COMMENT 'DRAFT 可变 → PUBLISHED 不可变(可触发)→ ARCHIVED 禁新触发、运行中实例不受影响;单向不可逆(可审计)',
  description  VARCHAR(2000)    NOT NULL DEFAULT '' COMMENT '= DSL $.description,列表/详情直读',
  dsl          JSON             NOT NULL COMMENT '完整 DSL 文档原文(真相源,Q3);写前必经 T1.1 Schema + T1.2 语义双层校验',
  dsl_hash     CHAR(64)         CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '规范化 DSL 字节 SHA-256(hex),写入即算(草稿亦然):定义行被误改可被 execution 侧比对检出(漂移检测)',
  created_by   VARCHAR(128)     NOT NULL COMMENT '创建者主体字符串(非 FK:M5 users 落地后可增量加 FK;字符串形态兼容 Q13 IdentityProvider 换源)',
  updated_by   VARCHAR(128)     NOT NULL COMMENT '最后修改者主体',
  published_by VARCHAR(128)     NULL COMMENT '发布操作者;DRAFT 阶段 NULL(尚无发布事实),与 published_at 同受 CHECK 钉死',
  published_at DATETIME(3)      NULL COMMENT '发布时刻 UTC;DRAFT 阶段 NULL',
  created_at   DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC,库端默认=写入路径零遗漏',
  updated_at   DATETIME(3)      NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '仅 DRAFT 期应变化,异常变化本身是告警信号',
  PRIMARY KEY (id),
  UNIQUE KEY uk_def_name_version (name, version) COMMENT '版本定位 + 同名同号并发发布闸(T5.4);NULL 草稿不占号',
  KEY idx_def_name_status_version (name, status, version) COMMENT 'T5.4 默认触发:该工作流最新 PUBLISHED 版本一次后向索引扫描',
  KEY idx_def_status_updated (status, updated_at) COMMENT '草稿箱/发布列表按近序分页(T5.4)',
  CONSTRAINT chk_def_published CHECK (status <> 'PUBLISHED' OR (published_at IS NOT NULL AND published_by IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='工作流定义:一行=一个版本,发布版不可变(应用约束 + dsl_hash 可验证)';

-- -----------------------------------------------------------------------------
-- 2. workflow_execution — 运行实例:钉住定义版本四件套、触发者、trace、状态。
--    reconciler 语义:架构 §3.1 的「QUEUED/RUNNING」对应本表 PENDING/RUNNING
--    (PENDING=已建未跑,含已入队未认领);扫描谓词走 idx(status,last_activity_at)。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workflow_execution (
  id                BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake;确定性 TaskID 前缀(exec:<id> / resume:<id>:<node>)',
  definition_id     BIGINT UNSIGNED NOT NULL COMMENT '钉住的定义行 id(T5.4 唯一真相);FK RESTRICT:定义行永不物理删除',
  definition_name   VARCHAR(64)     NOT NULL COMMENT '冗余自定义行(跨版本不变):T5.5 按工作流列执行免 join',
  definition_version INT UNSIGNED   NOT NULL COMMENT '冗余版本号快照:定义行即使未来清理,执行记录仍自证跑的是哪版(T5.4)',
  definition_hash   CHAR(64)        CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '钉住版本 DSL hash 快照:与 definition.dsl_hash 比对即知漂移(T8.1 对账证据)',
  status            ENUM('PENDING','RUNNING','WAITING_APPROVAL','SUCCEEDED','FAILED','CANCELLED') NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING=已建未跑(覆盖 §3.1 QUEUED 语义)→ RUNNING →(WAITING_APPROVAL⇄RUNNING)*→ SUCCEEDED|FAILED|CANCELLED;PENDING 可直接→FAILED(reconciler 判死);终态=SUCCEEDED/FAILED/CANCELLED;迁移:仅末尾追加',
  trigger_type      ENUM('API','SYSTEM','TEST','SCHEDULE') NOT NULL DEFAULT 'API' COMMENT '触发来源;每个执行的来源可审计;T8.x CI 注入与真实流量可分离;新来源末尾追加',
  triggered_by      VARCHAR(128)    NOT NULL COMMENT '触发者主体字符串;系统触发=system;T2.6 审计触发者身份的事实源',
  idempotency_key   VARCHAR(128)    COLLATE utf8mb4_bin NULL COMMENT '外部触发幂等键(客户端 HTTP header 提供,T5.5 定名;NULL=不去重,唯一键允许多 NULL);binary 排序规则防大小写折叠误判同键;去重窗口=永久。作用域(是否叠加主体/定义维度)T5.5 设计时定——全局键在多用户下有抢注面,P0 单租户可接受',
  trace_id          CHAR(32)        CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '根 W3C trace id(32 hex 小写);T6.2 前 NULL(不伪造占位值),T6.2 起 API 触发路径应用层强制(内部测试路径允许 NULL)',
  input             JSON            NOT NULL COMMENT '触发入参原文({{inputs.*}} 真相源、全部节点输入快照复现链的根,T2.1);零输入存 {}。注:本列与 node_execution.input_snapshot 是输入复现真相源,按原文保留,不在 PII 脱敏钩子范围(钩子只管 audit payload,T6.1)',
  outputs           JSON            NULL COMMENT '工作流级 outputs(DSL 顶层 outputs 的物化);终态写入,T5.5/T8.2 报告直读',
  error_code        VARCHAR(64)     NULL COMMENT '执行级失败分类(reconciler_timeout 等):系统级失败无归属节点,必须有落点;T6.4 告警分组',
  error_message     VARCHAR(2000)   NULL COMMENT '执行级失败摘要;全文归 audit_log payload',
  status_changed_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '最近状态迁移时刻(应用随 CAS 写);T6.4 审批积压年龄读本列',
  last_activity_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '最近活动时刻:任一节点/attempt 推进的同事务刷新;attempt 心跳 UPDATE 也同刷本列(worker 在长外部调用期间按心跳间隔续写)——否则健康长执行会被 reconciler 误报(T2.5)',
  started_at        DATETIME(3)     NULL COMMENT '首次 RUNNING;触发→起跑延迟指标(T6.4);NULL=未起跑',
  finished_at       DATETIME(3)     NULL COMMENT '进入终态时刻;T2.5「扫描间隔×3 内达终态」的直接量测点',
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  updated_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  PRIMARY KEY (id),
  UNIQUE KEY uk_exec_idem (idempotency_key) COMMENT '触发幂等第一道收敛(T8.1 双投递)',
  KEY idx_exec_status_activity (status, last_activity_at) COMMENT 'reconciler 主扫:WHERE status IN (PENDING,RUNNING) AND last_activity_at < now-N(T2.5)',
  KEY idx_exec_def_created (definition_id, created_at) COMMENT '按定义版本列执行历史、版本采用度(T5.4/T5.5)',
  KEY idx_exec_name_created (definition_name, created_at) COMMENT '按工作流(跨版本)列执行,T5.5 列表',
  KEY idx_exec_trace (trace_id) COMMENT '按 trace 反查执行(T6.2/T6.3/T8.2)',
  CONSTRAINT fk_exec_definition FOREIGN KEY (definition_id) REFERENCES workflow_definition (id) ON DELETE RESTRICT ON UPDATE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='运行实例:钉版四件套、触发者、trace、执行状态真相源(Q3);状态迁移 status-only CAS';

-- -----------------------------------------------------------------------------
-- 3. node_execution — 节点执行:状态机 + CAS 乐观锁 + 输入快照 + 统一截止时刻。
--    状态机:PENDING→EXECUTING(CAS 目标态)→ SUCCEEDED|FAILED|WAITING_APPROVAL;
--    WAITING_APPROVAL→(批准/驳回经审批 API 后)EXECUTING 复入(CAS,新 attempt);
--    EXECUTING→PENDING 仅限 reconciler 重入队(worker 判死后);PENDING→SKIPPED
--    (分支剪枝,T2.4);任意非终态→CANCELLED;FAILED/SUCCEEDED/SKIPPED/CANCELLED 为终态
--    (否决 FAILED→PENDING:FAILED 必须终态,重入队只从 EXECUTING 走)。
--    挂起契约(T4.1):进入 WAITING_APPROVAL 的 CAS 与当前 attempt 收口(RUNNING→SUCCEEDED)
--    同事务——挂起是设计内释放 worker,attempt 不得留 RUNNING,否则心跳判死扫描误报 DEAD。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS node_execution (
  id                BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake;CAS 目标行',
  execution_id      BIGINT UNSIGNED NOT NULL COMMENT '所属执行',
  node_id           VARCHAR(64)     NOT NULL COMMENT 'DSL 节点 id ^[a-z][a-z0-9_]{0,63}$;指向钉住 DSL 文档内节点(非 FK:节点语义随钉住版本冻结)',
  type              ENUM('llm','tool','branch','approval') NOT NULL COMMENT '自 DSL 冗余(schema 枚举逐字):状态机分发/按类型超时/T6.4 指标免 JSON 解析;P1 末尾追加 parallel/fan_in',
  status            ENUM('PENDING','EXECUTING','WAITING_APPROVAL','SUCCEEDED','FAILED','SKIPPED','CANCELLED') NOT NULL DEFAULT 'PENDING' COMMENT '全集见表注释;迁移仅末尾追加',
  attempts          SMALLINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '已开始 attempt 计数(claim 时 +1,CAS);与 max_attempts 比对决定重试或终败',
  max_attempts      SMALLINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '钉住 DSL retry.max_attempts 快照;CHECK 与 schema 上限逐字一致',
  version           INT UNSIGNED    NOT NULL DEFAULT 1 COMMENT 'CAS 乐观锁:WHERE id=? AND status=? AND version=?,两 worker 抢同节点仅一胜(T2.1)',
  worker_id         VARCHAR(64)     NULL COMMENT '赢得 PENDING→EXECUTING CAS 的执行者实例;节点级归因;NULL=未被执行/剪枝',
  input_snapshot    JSON            NULL COMMENT '已解析输入快照(T2.1 验收);每次进入 EXECUTING 的 CAS 同语句写入/重写(恰好由唯一获胜者写一次);PENDING 未解析/SKIPPED 恒 NULL。复现真相源:按原文保留,不脱敏',
  output            JSON            NULL COMMENT '已提交节点输出({{nodes.<id>.output.<field>}} 解析真相源);branch 恒 NULL(T1.2:branch 无 outputs;路由决定落 audit branch.evaluated 事件,不写本列)',
  deadline_at       DATETIME(3)     NULL COMMENT '统一截止时刻:进入 EXECUTING 时=now+按类型执行超时;进入 WAITING_APPROVAL 时=now+DSL timeout_hours(缺省 72h,Q9);NULL=无截止。NULL 不满足 <NOW() 谓词,天然排除',
  error_code        VARCHAR(64)     NULL COMMENT '终局失败码(llm_rate_limited/tool_ssrf_blocked/approval_timeout/attempts_exhausted 等);T6.4 聚合维度',
  error_message     VARCHAR(2000)   NULL COMMENT '失败摘要;全文与堆栈归 audit_log payload',
  status_changed_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '最近状态迁移时刻(随 CAS 写);卡死年龄/积压年龄读本列',
  started_at        DATETIME(3)     NULL COMMENT '首次 EXECUTING(跨重试不变);NULL=未起跑。注:CAS 语句必须显式写本列,勿依赖应用层遗漏',
  finished_at       DATETIME(3)     NULL COMMENT '进入终态时刻;NULL=未终态',
  escalated_at      DATETIME(3)     NULL COMMENT '审批超时升级通知时刻(Q9:escalate 仅一次,时间戳足够);非审批节点恒 NULL',
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  updated_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  PRIMARY KEY (id),
  UNIQUE KEY uk_node_exec (execution_id, node_id) COMMENT '每执行每节点恰一行:upsert 锚、CAS 定位键、T5.5 节点列表;fan-in 汇聚按本索引 node_id 字典序读前驱输出 = ROADMAP「确定性排序」的等价实现(node_id 在钉住 DSL 内唯一不可变,与到达顺序/重试无关;字典序 ≠ DSL 声明序,是有意选择)。P1 parallel 的 fan-out 多行度由此键扩展,(execution_id,node_id) 语义不变',
  KEY idx_node_status_deadline (status, deadline_at) COMMENT '一条索引服务两个扫描:T2.5 卡死节点 + T4.3 审批超时(WHERE status=? AND deadline_at<NOW(3))',
  KEY idx_node_exec_status (execution_id, status) COMMENT 'T2.4 入度推进(数前驱 SUCCEEDED/列后继 PENDING)+ fan-in 就绪收敛 + T5.5 节点状态',
  KEY idx_node_worker (worker_id) COMMENT '按执行者反查持有节点(T2.5/T8.1 故障归因)',
  CONSTRAINT fk_node_execution FOREIGN KEY (execution_id) REFERENCES workflow_execution (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT chk_node_max_attempts CHECK (max_attempts BETWEEN 1 AND 10),
  CONSTRAINT chk_node_version CHECK (version >= 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='节点执行:状态机真相源,CAS 迁移,输入快照,attempt 计数,统一截止时刻';

-- -----------------------------------------------------------------------------
-- 4. node_attempt — attempt 级明细:幂等键、心跳判死、队列对账、usage/cost 台账。
--    状态:RUNNING(已派发未收口)→ SUCCEEDED|FAILED(观察到的终态)|DEAD
--    (reconciler 以心跳陈旧判定持有者已死,未观察到终态;触发节点 EXECUTING→PENDING
--    重入队)。DEAD 与 FAILED 分开,才能区分「供应商侧失败」与「worker 失联」。
--    心跳契约:worker 周期刷新 heartbeat_at(初值=派发时刻,行创建即写,消除首跳前
--    kill 的判死盲窗);判死阈值 > 心跳间隔×N 且 > 单次外部调用上限(具体值 T7.1 配置,
--    防长 LLM 调用误判)。伪死后重入队是必然重跑(风险清单 #3):收敛依赖 attempt 唯一键
--    与供应商幂等键防副作用,双计费由 usage 台账对账暴露。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS node_attempt (
  id                BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake',
  node_execution_id BIGINT UNSIGNED NOT NULL COMMENT '所属节点执行',
  execution_id      BIGINT UNSIGNED NOT NULL COMMENT '冗余执行 id:台账按执行聚合(T3.1)、审计对账(T6.1)免一级 join',
  attempt_no        SMALLINT UNSIGNED NOT NULL COMMENT 'attempt 号(1 起);(node_execution_id,attempt_no) 唯一 = 双投递第二发命中唯一键即幂等返回(T2.3/T8.1)',
  status            ENUM('RUNNING','SUCCEEDED','FAILED','DEAD') NOT NULL DEFAULT 'RUNNING' COMMENT 'RUNNING=已派发;SUCCEEDED/FAILED=观察终态;DEAD=reconciler 判持有者已死;行只在派发事务内创建,无 PENDING',
  idempotency_key   VARCHAR(128)    NOT NULL COMMENT '发往 LLM/工具的外部调用幂等键(AGENTS 铁律);确定性 att:<node_exec_id>:<attempt_no>,供应商侧同键去重;全局唯一,碰撞即生成器缺陷',
  worker_id         VARCHAR(64)     NULL COMMENT '持有该 attempt 的 worker:T8.1 kill -9 场景的检测与归因字段',
  heartbeat_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '最后已知存活时刻;初值=派发时刻(行创建即写,NOT NULL 消除首跳前 kill 的扫描盲窗);worker 周期刷新,同事务刷 workflow_execution.last_activity_at;判死谓词 RUNNING AND heartbeat_at<NOW(3)-grace 全覆盖(T2.5/T8.1)',
  asynq_task_id     VARCHAR(128)    NOT NULL COMMENT '本次派发的确定性 Asynq TaskID(att:<node_exec_id>:<attempt_no>);worker 在派发事务内与 attempt 行同写(relay 只投不写业务表)——与 outbox.task_id 同值域;ErrTaskIDConflict(T2.3)与队列↔DB 对账的连接列',
  model             VARCHAR(100)    NULL COMMENT '实际解析后的模型名(别名/降级后可能与 DSL 不同);计费归因;仅 llm attempt 非空',
  prompt_tokens     INT UNSIGNED    NULL COMMENT 'API 返回 usage(计费真相源,§2);NULL=无用量语义(tool/branch)',
  completion_tokens INT UNSIGNED    NULL COMMENT '同上',
  total_tokens      INT UNSIGNED    NULL COMMENT '冗余存 API 返回原值而非求和,便于与供应商账单逐行核对',
  cost_amount       DECIMAL(14,6)   NULL COMMENT '单 attempt 成本;NULL=不计费,与 0(免费模型)区分;币种成对由 CHECK 双向钉死',
  cost_currency     CHAR(3)         CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'ISO-4217,按 attempt 原样记币种(聚合按币种分组);与 cost_amount 成对(CHECK 双向)',
  estimated         BOOLEAN         NOT NULL DEFAULT 0 COMMENT '1=未知模型 tiktoken 估算 fallback(T3.1 estimated=true 的承载列)',
  error_code        VARCHAR(64)     NULL COMMENT 'attempt 级失败码(429/超时等在此细分,T6.4 429 率);attempt 失败≠节点终局',
  error_message     VARCHAR(2000)   NULL COMMENT '截断摘要',
  started_at        DATETIME(3)     NOT NULL COMMENT '派发时刻(应用显式写);僵尸/DEAD 年龄判据',
  finished_at       DATETIME(3)     NULL COMMENT '收口时刻;DEAD 恒 NULL(未观察到终态)',
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT 'UTC;attempt 行无 updated_at:心跳走 heartbeat_at,收口是一次终写,终态后禁改',
  PRIMARY KEY (id),
  UNIQUE KEY uk_attempt (node_execution_id, attempt_no) COMMENT '双投递 DB 层收敛锚(T2.3/T8.1)',
  UNIQUE KEY uk_attempt_idem (idempotency_key) COMMENT '外部调用幂等键全局唯一',
  KEY idx_attempt_status_heartbeat (status, heartbeat_at) COMMENT '判死扫描:WHERE status=RUNNING AND heartbeat_at<NOW()-grace(T2.5/T8.1);heartbeat_at NOT NULL ⇒ 无盲行',
  KEY idx_attempt_exec (execution_id) COMMENT '按执行聚合台账与 attempt 时间线(T3.1/T5.5/T6.3)',
  KEY idx_attempt_task (asynq_task_id) COMMENT '由 TaskID 反查 DB 行:队列↔DB 对账主路径(T2.5/T8.1)',
  CONSTRAINT fk_attempt_node FOREIGN KEY (node_execution_id) REFERENCES node_execution (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  CONSTRAINT chk_attempt_no CHECK (attempt_no >= 1),
  CONSTRAINT chk_attempt_cost CHECK ((cost_amount IS NULL) = (cost_currency IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='attempt 级明细:幂等键、心跳判死、队列对账、usage/cost 台账(T3.1)';

-- -----------------------------------------------------------------------------
-- 5. outbox — 事务性发件箱(T2.2):与状态变更同事务写入;relay 异步投 Asynq。
--    无 FK:写在每次状态迁移热路径上,省外键开销;完整性由「状态行与事件行同存亡」
--    的事务契约 + idx_outbox_exec 对账兜底(T2.2 判别性故障测试断言)。
--    task_id 确定性且在写入时已知(故 NOT NULL);不唯一:reconciler 幂等重入队可合法
--    产生携带同 task_id 的新事件行,Asynq 侧 ErrTaskIDConflict 幂等去重(T2.3)。
--    TaskID 词表(全库统一):exec:<execution_id> 执行启动 / resume:<execution_id>:<node_id>
--    审批续跑 / att:<node_execution_id>:<attempt_no> attempt 派发。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS outbox (
  id               BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake,近似时序 → relay 批扫 ORDER BY id 即 FIFO 免排序',
  execution_id     BIGINT UNSIGNED NOT NULL COMMENT '事件归属执行(全事件执行域)',
  event_type       VARCHAR(64)     NOT NULL COMMENT '开放词表(node.task_enqueue/execution.resume/approval.notify/…):演进快于 DDL,不用 ENUM',
  task_id          VARCHAR(128)    NOT NULL COMMENT '确定性 Asynq TaskID(词表见表注释):重启重投同 ID ⇒ Redis 侧幂等;T2.3/T8.1 对账连接列',
  queue            VARCHAR(64)     NOT NULL DEFAULT 'default' COMMENT 'Asynq 队列名:审批/续跑与重 LLM 任务隔离,T6.4 按队列告警',
  payload          JSON            NOT NULL COMMENT '自描述事件体(仅 id 引用,业务数据回读 MySQL ⇒ 重投永远重放同一真相);relay 不回查业务表即可投递',
  status           ENUM('PENDING','DISPATCHED','DEAD') NOT NULL DEFAULT 'PENDING' COMMENT 'PENDING=待投;DISPATCHED 仅在 relay 入队成功后标记(标记前 kill -9 → 重启重投,T2.2);DEAD=投递尝试穷尽(T6.4 死信告警源;重放=人工重置 PENDING,T7.4 runbook)',
  deliver_attempts SMALLINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'relay 投递尝试计数,达配置上限转 DEAD;0=尚未尝试',
  last_error       VARCHAR(2000)   NULL COMMENT '最近投递失败摘要(enqueue 失败注入的直接取证,T8.1);NULL=尚无失败',
  available_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '最早可投时刻:初写=事务时刻(立即可投),重试=now+退避;relay 扫描谓词',
  dispatched_at    DATETIME(3)     NULL COMMENT '实际入队时刻(投递时延度量);NULL=未 DISPATCHED;DISPATCHED 行保留 7 天后批量清理(按 dispatched_at 等值+范围小扫,status 无专索引亦可:DEAD 告警先于量增)',
  trace_id         CHAR(32)        CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '随事件交给 worker 续 OTel 链(T6.2 贯穿);NULL 仅限无 trace 上下文的运维手工事件',
  created_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '同事务写入时刻',
  updated_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT 'UTC',
  PRIMARY KEY (id),
  KEY idx_outbox_poll (status, available_at, id) COMMENT 'relay 主扫:WHERE status=PENDING AND available_at<=NOW(3) ORDER BY id LIMIT n(status 等值+available_at 范围过滤;id 排序非纯索引序,小批量 LIMIT 下轻量 sort 可接受——实测有 Sort 节点,流量上来再评估替代索引);多 relay 并发轮询用 FOR UPDATE SKIP LOCKED',
  KEY idx_outbox_exec (execution_id, created_at) COMMENT '按执行取事件全集:T2.2 同存亡断言、T8.1 对账诊断',
  KEY idx_outbox_task (task_id) COMMENT '由 TaskID 反查事件:ErrTaskIDConflict 收敛/重投去重对账'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='事务性 outbox:状态变更同事务写,relay 独立投递,回滚/断点续跑的依赖通道';

-- -----------------------------------------------------------------------------
-- 6. audit_log — 审计:append-only、按日 RANGE 分区、prev_hash 建表即留(Q10)。
--    分区键 exec_date = DATE(execution.created_at) UTC(执行起始日期,非事件日期):
--    一个执行的全部审计行恒同分区(跨天长执行不分裂),按执行查询先从 execution 行
--    钉 exec_date 即裁剪单分区,保留期到期该执行的整条审计链一起 DROP(合规友好)。
--    MySQL 分区表硬约束:①不支持 FK(审计行本就不应随业务行级联删除,同向);②分区键
--    必须含于全部唯一键 ⇒ PK=(exec_date,id),id 非索引首列 ⇒ 必须应用侧 Snowflake。
--    全局索引都是分区本地:trace/actor 维度无日期谓词时逐分区探测(保留期有界可接受);
--    演化路径:保留期 >1 年或 T6.3 延迟超标 → 增量迁移加 audit_trace_map 旁路表
--    (PK (trace_id,audit_id),与审计行同事务写入),v1 不建。
--    usage/cost 冗余列与 node_attempt 双录:审计须独立于状态表自成证据链(合规),
--    且 T6.4 按模型聚合成本/429 率免 JSON_EXTRACT;T6.1 批量写对账按 attempt_id 核销
--    (attempt 级事件;执行级事件无 attempt 锚,对账口径见 T6.1 任务)。
--    哈希链(P1):行序 = (exec_date,id);event_at 是事件时刻(≠排序键)。哨兵语义:
--    prev_hash NOT NULL,''=未启链期(P0 全部)或启链后首行(无前驱);row_hash NULL=本行
--    未参与链(未启链期),链校验跳过 NULL。两列哨兵形态不同是有意的:prev_hash 表
--    「链上位置」,row_hash 表「本行是否已哈希」。链粒度(per-execution)P1 启链前拍板。
--    PII(pii_masked=1)只声明 audit payload 已脱敏;execution.input 与
--    node_execution.input_snapshot 是复现真相源,按原文保留,不在脱敏范围(设计边界)。
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_log (
  id                BIGINT UNSIGNED NOT NULL COMMENT 'Snowflake(应用生成):(exec_date,id) 即 P1 哈希链行序',
  exec_date         DATE            NOT NULL COMMENT '分区键 = DATE(execution.created_at) UTC,应用写入时物化;按执行查询的裁剪锚',
  event_at          DATETIME(3)     NOT NULL COMMENT '事件发生时刻(应用显式传;异步批量写 ⇒ 事件时刻≠落库时刻,必须分立);非链排序键',
  event_type        VARCHAR(64)     NOT NULL COMMENT '开放词表(execution.triggered/node.state_changed/llm.call.completed/tool.call.completed/approval.requested/branch.evaluated/…)',
  execution_id      BIGINT UNSIGNED NOT NULL COMMENT '查询维度 1(T6.3)',
  node_execution_id BIGINT UNSIGNED NULL COMMENT '节点事件目标行;NULL=执行级事件(触发/审批决定)',
  node_id           VARCHAR(64)     NULL COMMENT 'DSL 节点 id 冗余:导出证据行自描述、按节点过滤免 join',
  attempt_id        BIGINT UNSIGNED NULL COMMENT '关联 node_attempt.id:T6.1 批量写对账核销键(attempt 级行必携);执行级事件 NULL',
  actor_type        VARCHAR(16)     NOT NULL COMMENT '开放词表:user/system/worker/tool/llm/approval',
  actor_id          VARCHAR(128)    NOT NULL COMMENT '行为主体字符串(查询维度 3,按人,T6.3);每行必有归因',
  trace_id          CHAR(32)        CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '本行 OTel trace id(32 hex 小写,§3.5 每行携带);T6.2 前 NULL(占位不伪造),T6.2 起必填;查询维度 2',
  linked_trace_id   CHAR(32)        CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '关联 trace:审批挂起→续跑经 trace link 串联(T6.2 验收);NULL=无跨 trace 关联',
  pii_masked        BOOLEAN         NOT NULL DEFAULT 0 COMMENT '1=payload 已过 PII 脱敏钩子(T6.1 验收可验证标记;脱敏写前完成,原始永不入 payload;append-only 禁止事后 UPDATE)。脱敏范围仅 audit payload,不含 input/快照真相源列',
  model             VARCHAR(100)    NULL COMMENT '模型名(llm 事件冗余):按模型聚合成本/429 率(T6.4)免解 JSON',
  prompt_tokens     INT UNSIGNED    NULL COMMENT 'usage 冗余列(与 node_attempt 双录,合规证据链独立于状态表)',
  completion_tokens INT UNSIGNED    NULL COMMENT 'usage 冗余列',
  total_tokens      INT UNSIGNED    NULL COMMENT 'usage 冗余列',
  cost_amount       DECIMAL(14,6)   NULL COMMENT '成本冗余列(合规副本,T3.1 断言对照);与 cost_currency 成对由 CHECK 双向钉死',
  cost_currency     CHAR(3)         CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'ISO-4217',
  cost_estimated    BOOLEAN         NOT NULL DEFAULT 0 COMMENT '1=估算值(合规披露)',
  payload           JSON            NOT NULL COMMENT '事件全量:prompt/模型返回/工具入出参/审批上下文/分支路由决定(Q10 记录范围);密钥永不入内(铁律);无正文事件存 {}',
  prev_hash         CHAR(64)        CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' COMMENT '哈希链前驱行 hash:建表即留(Q10);''=未启链期或链首;链粒度 P1 启链前拍板',
  row_hash          CHAR(64)        CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '本行规范化哈希:与后行 prev_hash 对拼验链;NULL=未启链期行,链校验跳过(P1 启用)',
  created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '落库时刻:异步批量写滞后观测 + 保留兜底(保留期语义按 exec_date 计算)',
  PRIMARY KEY (exec_date, id) COMMENT '分区列必须含于全部唯一键(MySQL 规则);行身份=二元组,唯一性由 Snowflake 全局保证',
  KEY idx_audit_exec (execution_id) COMMENT '维度 1:按 execution 查(先钉 exec_date 裁剪单分区);InnoDB 隐式拼 PK ⇒ 物理含 exec_date,id 有序',
  KEY idx_audit_trace (trace_id) COMMENT '维度 2:按 trace 查(T6.3);无日期谓词时逐分区本地索引探测',
  KEY idx_audit_actor (actor_id) COMMENT '维度 3:按人查(T6.3);建议查询携日期范围',
  KEY idx_audit_attempt (attempt_id) COMMENT 'T6.1 批量写对账按 attempt 核销的支撑索引',
  CONSTRAINT chk_audit_cost CHECK ((cost_amount IS NULL) = (cost_currency IS NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci ROW_FORMAT=DYNAMIC
  COMMENT='审计日志:append-only(业务账号仅 INSERT/SELECT,T7.3 自检断言),按执行起始日期日分区,prev_hash 预留,usage/cost 双录;批量写契约=INSERT..ON DUPLICATE KEY UPDATE id=id + 行数比对(见 T6.1,禁 INSERT IGNORE——越界/截断会静默降级)'
PARTITION BY RANGE COLUMNS (exec_date) (
  PARTITION p_min     VALUES LESS THAN ('2026-01-01'),
  PARTITION p20260921 VALUES LESS THAN ('2026-09-22'),
  PARTITION p20260922 VALUES LESS THAN ('2026-09-23'),
  PARTITION p20260923 VALUES LESS THAN ('2026-09-24'),
  PARTITION p20260924 VALUES LESS THAN ('2026-09-25'),
  PARTITION p20260925 VALUES LESS THAN ('2026-09-26'),
  PARTITION p20260926 VALUES LESS THAN ('2026-09-27'),
  PARTITION p20260927 VALUES LESS THAN ('2026-09-28'),
  PARTITION p20260928 VALUES LESS THAN ('2026-09-29'),
  PARTITION p20260929 VALUES LESS THAN ('2026-09-30'),
  PARTITION p20260930 VALUES LESS THAN ('2026-10-01'),
  PARTITION p20261001 VALUES LESS THAN ('2026-10-02'),
  PARTITION p20261002 VALUES LESS THAN ('2026-10-03'),
  PARTITION p20261003 VALUES LESS THAN ('2026-10-04'),
  PARTITION p20261004 VALUES LESS THAN ('2026-10-05')
);
-- 分区运维(reaper 内置日常作业[ROADMAP T2.5 承载] + T7.3 安装自检 + T7.4 运维手册):
--   文件内固定引导分区仅供首装(2026-09-21..2026-10-04),水位维护职责:
--   1) 水位预建:reaper 每日保持 ≥7 天未来分区;不设 MAXVALUE 兜底 —— 越界写入 ERROR 1526
--      响亮失败(批量写契约 ODKU 下错误照常抛出),属受监控条件(T6.4);避免静默堆积进
--      大分区导致 REORGANIZE 长锁。
--        ALTER TABLE audit_log ADD PARTITION (PARTITION p20261005 VALUES LESS THAN ('2026-10-06'));
--   2) 保留期清理(默认 90 天可配,T7.1):整分区 DROP(DDL,无 undo/长事务/行级 binlog):
--        ALTER TABLE audit_log DROP PARTITION p20260601;
--   3) p_min 为时钟漂移兜底:监控其行数,>0 告警人工处置。
--   4) T7.3 自检:对照 INFORMATION_SCHEMA.PARTITIONS 校验水位,不足拒绝启动并提示
--      修复命令;并断言业务账号对 audit_log 无 UPDATE/DELETE 权限。
--
-- 附:CAS 迁移参考语句(T2.1,引擎层实现;获胜方 affected_rows=1):
--   UPDATE node_execution
--      SET status='EXECUTING', worker_id=?, input_snapshot=?, deadline_at=?,
--          started_at=IFNULL(started_at, NOW(3)), attempts=attempts+1,
--          status_changed_at=NOW(3), version=version+1
--    WHERE id=? AND status='PENDING' AND version=?;
-- 附:reconciler 三扫描形态(T2.5):
--   execution: WHERE status IN ('PENDING','RUNNING') AND last_activity_at < NOW(3)-阈值
--   node:      WHERE status IN ('EXECUTING','WAITING_APPROVAL') AND deadline_at < NOW(3)
--   attempt:   WHERE status='RUNNING' AND heartbeat_at < NOW(3)-grace → 置 DEAD,节点重入队
