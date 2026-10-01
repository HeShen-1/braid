# braid 运维手册(runbook)

> 稳态运维原语;版本升级流程见 deploy/UPGRADE.md,配额与拓扑契约见
> deploy/README.md。只收「交付面之外、运维显式动作」;随任务按需增长。

## 1. 沙箱孤儿容器与泄漏 egress 网(T15.3/T15.4,D-t151 决策 5)

worker 沙箱容器与 egress 网都是 docker daemon 持久对象,worker 进程退场
不自动回收。两类残留同源(进程灭失或身份变更),清扫一节:

### 1.1 泄漏 egress 网(T15.4)

egress 网名 = 基段 + worker_id 实例段(缺省族
`braid-sbx-egress-<8 hex>`;自定基段换前缀)。worker 正常重启同名幂等
复用;**身份变更**(容器 recreate/升级路径——compose 缺省无 `hostname`
键时 `up -d` 必换容器短 ID)即泄漏一网:旧网无人监听,却占 daemon
地址池配额(缺省 ≈31 用户网/daemon,k8s host docker.sock 按节点计)。
**误删活网 = 该 worker 全部 http 沙箱出网即断**,rm 前每步都过:

```sh
# 1. 列出 egress 网(实例段族 + 裸基段网均入列;自定基段换前缀)
docker network ls --filter name='^braid-sbx-egress' --format '{{.Name}}'

# 2. 逐网核活:网关 IP 上代理端口有监听 = 活网,跳过。
#    端口 = 部署实际 BRAID_TOOL_SANDBOX_EGRESS_PORT(缺省 9210;自定端口
#    的部署替换之;PORT=0 内核指派部署本探针不适用,以第 3 步附着判定
#    + owner 核对为准)。端口无关替代:按网关 IP 查任意监听
#    (ss -ltn | grep <gw>)。网关 IP 见 inspect。
docker network inspect -f '{{.Name}} {{(index .IPAM.Config 0).Gateway}}' <net>
nc -z <gw> 9210

# 3. owner 核对:确认无活 worker 持该实例段——查最近 worker 启动日志行
#    `worker: sandbox egress proxy serving ... network=<name>`
#    (cmd/worker/main.go),或核对 node_attempt.worker_id 无同身份活体。
#    有活 owner = 跳过(探活与 owner 判定须紧贴第 4 步执行,窗口内
#    新起 worker 可能复用同名网——rm 前重跑第 2 步)。

# 4. 核无附着容器(非 0 = 仍有沙箱容器在飞:一律不回收,等容器自然
#    退出后重来;孤儿容器的处置原语见 §1.2 注记)
docker network inspect -f '{{.Name}} {{len .Containers}}' <net>

# 5. 无监听、无活 owner、无附着即回收
docker network rm <net>
```

升级一次性泄漏:从 T15.4 前版本升级后,旧裸基段网(缺省
`braid-sbx-egress`,无实例段后缀)不再被新代码创建/复用/删除——按上列
同规则核活核附着后回收一次,让地址池记账从干净基线开始。

边界:同 daemon 双持同名网结构性不可能(幂等创建);列表中每个名字至多
一个活 owner;同 worker_id 双进程 = 后者 bind 拒启(显式端口部署契约,
见 deploy/README.md §9 运维;`EGRESS_PORT=0` dev/test 不执法),不产生
第二张网。

### 1.2 孤儿沙箱容器(T15.3 面)——落位仲裁注记

kill -9 残窗容器按 owner label 清扫的原语、边界与安全前提由 T15.3 承载。
**落位仲裁(两 lane 现状,2026-10-01)**:t15.3 在飞分支当前把该 runbook
写在 `deploy/README.md §7.1`(「沙箱 owner 与孤儿清扫 runbook」),本
文件 §1 只收泄漏网。D-t151 决策 5 的「孤儿/泄漏网清扫合并一节」由后合
者执行并节(任一方向:容器原语迁入本文件 §1.2,或本 §1.1 泄漏网原语
迁入 README §7.1——合并后全仓只留一个 runbook 节,交叉引用同步改)。
在 t15.3 合入前,附着容器非零的泄漏网一律不回收(第 4 步)。
