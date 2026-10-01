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

### 1.2 孤儿沙箱容器(T15.3)

沙箱容器打 `braid.sandbox.owner=<WorkerID>` label,worker 启动清扫只清
自家(全局清扫**无代码路径**——kill -9 孤儿、recreate 前身容器等跨
owner 回收是本节显式原语)。死实例身份可知:owner label 与
`node_attempt.worker_id` 对账面同源同词形。

```sh
# ① 枚举 + 归属判读:每只容器的 owner label 即归属身份(与 DB 的
#    node_attempt.worker_id 对账可核)。
docker ps -a --filter label=braid.sandbox=1 \
  --format '{{.ID}} {{.Label "braid.sandbox.owner"}} {{.Status}}'

# ② 按 owner 精确清(推荐:只动目标死者身份的容器)。filter 值整体加
#    引号再替换占位符——worker_id 若取自 DB(node_attempt),其值域在
#    off 模式 worker 下不受字符集约束,不过 ^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$
#    的形态不可能是任何沙箱容器的主人(docker 模式启动闸挡死),按敌意
#    输入对待、勿裸贴进 shell。
docker ps -aq --filter label=braid.sandbox=1 \
  --filter "label=braid.sandbox.owner=<死实例 WorkerID>" | xargs -r docker rm -f

# ③ 全局清扫(最后手段:多实例同 daemon 时先确认无其他活体 worker)。
docker ps -aq --filter label=braid.sandbox=1 | xargs -r docker rm -f
```

升级路径的 recreate 孤儿(自管部署未钉 hostname 的形态:前身容器带旧
短 ID owner)同法回收——② 的 owner 取前身容器短 ID;交付 prod compose
已钉 hostname,recreate 前后身份一致,由新实例启动清扫自收。**首个
T15.3 升级窗特例**:旧二进制所起的前身容器没有 owner label,② 与启动
清扫都结构性够不到,只能用 ③(先确认无其他活体 worker),此后再无此
形态(详见 deploy/UPGRADE.md 注记)。

边界(登记不修,D-t151 #8-③):手工同 hostname ×2 等活体同 owner 冲突
会复活互杀面;「sweep 前租约活性复核」选型 = 部署契约承载(followup
位,依赖 t15.2 租约表)。与 §1.1 泄漏网的耦合:附着容器非零的网一律
不回收(§1.1 第 4 步),先按本节清容器再回收网。
