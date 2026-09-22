// Package auth 实现 ARCHITECTURE Q13 锁定的内置用户体系(T5.1):users /
// user_role 模型(迁移 0002)、bcrypt 密码哈希、无状态 JWT(HS256)的
// 签发与校验。RBAC 中间件(T5.2)与 IdentityProvider 桩(T5.3)叠加在
// 本包接口面之上,不在此实现。
//
// 契约:
//   - 错误面:未知用户与错密码统一 ErrInvalidCredentials(防用户名枚举);
//     未知用户路径补一次 dummy bcrypt 比较均衡时序(索引 miss µs 级 vs
//     ~100ms 比对是枚举 oracle);errors.As *CredentialsError 取内部归因,
//     仅供日志/运维,外泄即破坏防枚举语义。
//   - DISABLED 只在密码验证通过后暴露(持凭证者可诊断;错密码路径恒
//     ErrInvalidCredentials,防账号状态探测——TestAuthenticateDisabledOrdering
//     钉死该顺序)。
//   - token 判定 fail-closed:alg 白名单、iss/aud、exp 必存、roles 词表
//     逐字校验、sub 非空,任一不符 → ErrTokenInvalid;仅过期单列
//     ErrTokenExpired(消费方动作都是重登录,但诊断语义分立)。
//   - 事务纪律:原语永不自管事务边界,Commit/Rollback 只在 WithTx;
//     bcrypt 哈希计算(~100ms CPU)在事务外完成,不进锁窗。
//   - 密钥纪律:JWT 密钥只经 BRAID_AUTH_JWT_SECRET env 注入(≥32 字节、
//     无默认值、缺省/过短启动即败),不进代码/日志/审计(铁律);测试
//     密钥为低熵占位词组,token 一律测试内签发,不硬编码。
package auth
