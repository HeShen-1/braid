// Package drill 承载 T15.6 多副本整体演练(D-t151 决策 6):独立
// multi-replica compose 栈(deploy/drill.docker-compose.yml)× t8.1 故障
// 注入形态。本包只有测试文件——runner 逻辑全部在 drill_test.go,静态
// 锚在 compose_anchor_test.go。
//
// Redis 槽位裁定(决策 6 开工前置,已裁定):演练断言全部走 MySQL 面
// (按槽位发布端口)+ HTTP(单 api 发布端口)+ docker CLI(label 差分/
// 日志 grep/容器操作),零 t13(16379)键空间消费——不构成第 5 个 Redis
// 消费包,无需扩 AGENTS.md 槽位分配表。BRAID_TEST_REDIS_DB 在本包语义
// = 槽位号(镜像 tag / compose 项目名 / 宿主端口命名空间),与 sandbox
// e2e 的 tag 隔离同款;若实现中发现必须连 t13 Redis 断言,停下先扩表
// (决策 6 明文)。
//
// 门控:显式 opt-in——BRAID_TEST_REQUIRE_DRILL=1 时栈/docker 不可达即红;
// 未设时 docker 不在场自动 skip(不继承 BRAID_TEST_REQUIRE_STACK:演练是
// 验收仪式不是每 commit 回归,回归面由闸单测 + 五臂注入套件承载,GATE 1
// 拍板)。
package drill
