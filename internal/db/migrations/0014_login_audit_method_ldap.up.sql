-- ============================================================================
-- braid 0014_login_audit_method_ldap.up.sql — T17.6 LDAP bind 桩实现
-- (issue #152;D-2026-10-04-t176):login_audit.method ENUM 末尾追加 'ldap',
-- 兑现 0013 列注释「词域封闭仅末尾追加(ldap 预留 T17.6)」的预留承诺。
--   ① 尾追加 = metadata-only(ALGORITHM=INSTANT,存量表零重建;0013
--      ENUM 尾追加同款判);DEFAULT 'password' / NULL 语义 /
--      chk_login_audit_method 双向不变量全部不动(ENUM 本体即词域,
--      CHECK 只管 NULL 配对,不枚举词值)。
--   ② 存量行零影响:0014 前无 'ldap' 值可存在(ENUM 拒收),无 backfill
--      面、无新计数口径须知(0013 的「0013 前 password 计数含 OIDC」
--      口径原样有效)。
--   ③ down:先 DELETE method='ldap' 行再回缩双值 ENUM(严格模式下枚举
--      回缩遇存活词值即报错,顺序不可倒置;镜像 0013 down 对签发行的
--      丢弃判——迁移账号回退面不受业务 append-only 契约约束)。
-- 风格契约继承 0001/0005/0013 头注释:InnoDB / utf8mb4 /
--   utf8mb4_0900_ai_ci / ROW_FORMAT=DYNAMIC;api 账号 INSERT/SELECT only
--   授权契约不变;Go 侧闭集单源 = internal/api/loginaudit.go
--   knownLoginMethods(同步锚 = TestKnownLoginMethodsClosedVocabulary,
--   DDL 侧锚 = migrate_test 0013/0014 块)。
-- ============================================================================

SET NAMES utf8mb4;

ALTER TABLE login_audit
  MODIFY COLUMN method ENUM('oidc','password','ldap') NULL DEFAULT 'password'
    COMMENT '登录方法:oidc = OIDC 回调,password = 口令登录,ldap = LDAP bind(T17.6 尾追加,兑现 0013 预留;企业树 entldap 换装凭证核验缝的审计归因词);**存量行(0013 前)两路混流不可辨,一律 backfill password —— 0013 前 password 计数含 OIDC(口径钉死,统计须知)**;词域封闭仅末尾追加;scoped_token_issued 行恒 NULL(chk_login_audit_method)';
