# T10.1 沙箱运行时基线安全评审(P1 安全评审落地,首日交付物)

- 状态:GATE 1 拍板(用户 2026-09-28)——路线 = 合成案(C 骨架 + A/B 嫁接:docker CLI 直 exec + 泛型 SandboxCarrier + 同模块 helper);token 传输面 = stdin 信封;CI 门控 = `BRAID_TEST_REQUIRE_SANDBOX` 继承 `BRAID_TEST_REQUIRE_STACK`;资源超限分类 = 业务 `tool_failed`。决策记录见 `docs/DECISIONS.md` D-2026-09-28-t101。
- 本文是 ARCHITECTURE.md L74「沙箱运行时基线(非 runc 默认即安全)在 P1 安全评审时定义」的落地:评审不是关于默认的论文,而是使 T10.1 实现 diff 可审的规格——每项控制给出机制、对抗的威胁、与「失控即红」的 e2e 证明。

## 1. 「非 runc 默认即安全」的解读

唯一合法读法是**否定式**:非[runc 默认即安全]——"runc/容器默认配置本身不构成安全基线"。三重依据:

1. L74 全句中括注是「基线」的定语:若默认即安全,则无可定义之基线,「在 P1 安全评审时定义」退化为空话;
2. ROADMAP T10.1 验收逐项列出的恰是非默认物(只读根 fs、资源上限、无特权)——验收清单是锁文语义的最强内证;
3. 句法上「非…即…」无并列备选项时,「非」辖制其后整个命题。

实质判断为真:runc 默认 = 可写 rootfs、无内存/pids 上限、默认 14 cap 集(含 NET_RAW)、root 用户、无 no-new-privileges、默认桥网全量出网——对执行 LLM 工作流可控输入的工具执行器,每一项都是具名威胁。

**操作含义(基线义务)**:(a) 每项控制必须在容器层显式下发(CLI flag / HostConfig 字段),应用层守卫(SSRF 受防客户端、file_read 双门)必要非充分,不抵扣容器层义务;(b) 每项必须携带「失控即红」的 e2e 证明——配置漂移被测试击穿,不默默生效;(c) 全矩阵以本文为档。

## 2. 基线硬化矩阵(本任务落地的控制)

| # | 控制 | 机制(docker CLI) | 对抗的威胁 | e2e 证明 |
|---|------|-------------------|-----------|---------|
| 1 | 只读根 fs | `--read-only` | 输入向镜像 fs 写入:替换工具逻辑、植入铺垫、填盘 | probe 写 `/usr/local/bin/.probe-w` 与 `/etc/.probe-w` → EROFS;写 `/tmp/.probe-w` 成功(tmpfs 在场,防全盘只读假绿);容器 A 落 marker、容器 B 读不到(每调用一次性) |
| 2 | tmpfs scratch | `--tmpfs /tmp:rw,noexec,nosuid,size=16m` | ro 下合法 scratch 供给;/tmp 落盘执行点:size 封顶、noexec 防落盘二进制、nosuid 防 setuid | probe 写 /tmp 至 ENOSPC 断言 ≈16MiB;复制自身到 /tmp exec → EACCES |
| 3 | 白名单目录 ro bind(同路径) | `--mount type=bind,src=<dir>,dst=<同绝对路径>,readonly` | file_read 白名单的宿主面暴露;rw 挂载 = 容器内写宿主;路径漂移破坏双门与词形回显 | 读白名单内成功且 output.path 回显词形与宿主一致;`/etc/passwd` → `tool_path_blocked`;白名单内 symlink → 指外部 → `tool_path_blocked` |
| 4 | 内存上限(零 swap) | `--memory 256m --memory-swap 256m` | 输入诱发内存放大拖同宿主服务入 OOM;swap 默认 2×memory 软化限额 | probe 按 8MiB 步长分配至被杀;宿主断言失败关闭(非成功、非截断成功)、worker 存活、后续调用正常 |
| 5 | CPU 上限 | `--cpus 0.5` | 输入诱发自旋饿死同宿主 goroutine/服务 | 自旋 probe + cgroup cpu 记账窗口计量(窗外 docker exec 双采样;v2 cpu.stat 主路径 + v1 cpuacct 回退,两代布局都不可读 = 环境门控 skip;速率 ∈ [0.15,0.75] 分离配额执法 0.5 与漂移形态 ~1.0,nr_throttled 增长为 CFS 节流佐证;零时序敏感断言) |
| 6 | 进程数上限 | `--pids-limit 64` | fork 炸弹/无界派生耗尽宿主 PID 空间 | `/proc/self/exe` 自再执行链(非真炸弹):链尾上报单行 depth/stop 报告(budget 收口附 pids 计数;预算 margin 防止「fork 抢到最后槽位后子代 runtime 建线程 EAGAIN = fatal throw」竞态)、EAGAIN 兜底如实上报,断言 2 ≤ depth ≤ 64(结构上界);限额漂移缺失时 pids.max=max → 链走满硬上界 stop=cap 即红。宿主无漂移以链自终止 + 零残留差分覆盖(宿主进程计数在并行环境噪声主导,不背书) |
| 7 | FD 上限 | `--ulimit nofile=1024:1024` | 输入诱发 FD 耗尽挤压宿主预算 | probe 循环 open 至 EMFILE(软失败正常退出),断言 512 ≤ 计数 ≤ 1024(下限分离「限额远低于预期」形态);旗标在场性由 inspect 矩阵 Ulimits 全值比对独立钉死(分层依赖,单用例不背书 daemon 缺省贴界形态) |
| 8 | cap-drop ALL | `--cap-drop ALL` | runc 默认 14 cap(NET_RAW = 桥网 ARP 伪造/内网侦察;MKNOD/SETUID 等放大任何缺陷后果) | probe 读 `/proc/self/status` CapEff/CapBnd == 0;mknod/chown → EPERM(配置+功能双锚) |
| 9 | no-new-privileges | `--security-opt no-new-privileges` | 镜像/挂载内 setuid 位或 file caps 的 exec 链提权(与 ro 互补:ro 防写入铺垫,NNP 防执行期提权) | `/proc/self/status` NoNewPrivs == 1(标志级断言) |
| 10 | 非 root 用户 | `--user 65532:65532` | root-in-container 对 bind mount 按宿主 uid 判权;逃逸以 root 起跑半径远大于 65532 | probe 断言 Geteuid()==65532;写挂载目录 → EACCES;读白名单内文件成功(权限矩阵健全) |
| 11 | 网络按载体拆分 | file_read:`--network none`;http:`--network bridge`(过渡,t10.2 单点换装) | file_read 零出网需求——none 是零成本纵深;http 出网执法完全依赖守卫随行(见 §4) | file_read 容器内拨任意地址 → unreachable;http 容器内 `https://127.0.0.1` / `https://169.254.169.254` → `tool_ssrf_blocked`(守卫在容器内执法的证明) |
| 12 | env 从零构造 | 容器 env 仅 `PATH` / `LANG=C.UTF-8`;token、attempt_key、工具 config、inputs 全走 stdin 信封(GATE 1 拍板) | worker ambient env(DSN/LLM key/HMAC secret/代理变量)经 env 通道泄漏;`-e` 旗标把值写进 docker argv(`/proc/<pid>/cmdline` 宿主全局可读);docker inspect 对 daemon 可达主体可读 | command_test.go:14-46 双向断言模式上移:t.Setenv 假主凭证,容器内序列化全量 env,正半边(两成员在场)+ 负半边(假凭证键名与值双缺席);结构性防再入:所有 `BRAID_TOOL_SANDBOX*` 键永不进容器 env(构造式白名单天然成立 + 断言) |
| 13 | 生命周期绑定 ctx | `--rm` + ctx.Done → `docker stop -t 3 <name>`(5s 有界)+ helper 内 watchdog(信封携相对剩余毫秒,与宿主 ctx 同单调钟锚;期限后 +grace 硬退兜底)+ worker 启动按 label 清扫(T15.3/D-t151 #5 起 owner-scoped:`braid.sandbox.owner=<WorkerID>` label + 双 filter 只清自家;kill -9 孤儿全局回收降级 runbook 原语,见 deploy/RUNBOOK.md §1.2) | cancel 窗僵尸容器占资源;worker kill -9 孤儿;时钟步进把 helper 期限提前致宿主误分类(绝对秒锚缝,T5 审计收口) | 短超时触发 cancel → 15s 内 label 查询容器清零;kill -9 残余窗 = watchdog 有界自灭,登记残余 |
| 14 | daemon 日志封顶 | `--log-driver json-file --log-opt max-size=5m --log-opt max-file=1` | 工具无界 stderr 经 daemon 日志通道填宿主盘(stdout 已 16MiB 信封封顶,此为独立防线) | probe 写 52MiB stderr(非 5m 整倍数——整倍数轮转可清空驻留);inspect LogConfig 配置断言 + docker logs 双流合并截留 ≤ 5m+128KiB(容器 stderr 流走 CLI stderr,单收 stdout 是假绿;slack 依据 = daemon stdio copier 读块粒度)+ 尾部哨兵行在场(轮转删头留尾,哨兵必驻留 = 通道承载证明;宿主侧文件路径需 root,非测试可达面) |
| 15 | tini 作 PID 1 | `--init` | 工具进程派生子进程后退出 → 孤儿僵尸堆积顶满 pids limit(与 #6 的复合失败) | inspect `HostConfig.Init`;probe 双 fork 孤儿派生,沉降轮询扫描 `/proc/*/stat` Z=0(瞬态收割窗容忍);负向对照 = 剥 `--init`(PID 1 = helper 自身不收割)断言孤儿僵尸滞留 >0——漂移红路径 + 探针检测力自证,并兑现复合形态半边(孤儿僵尸 × PID 1 不收割;堆积顶满 pids 的完整形态由 #6 链自终止 + 零残留分解覆盖) |

资源档位(256m / 0.5 / 64 / 1024)是基线提案值,生产终值按真实工作负载在 P1 评审定档;全部经 env 可配(`BRAID_TOOL_SANDBOX_MEMORY/CPUS/PIDS`,构造期正则校验响亮败;键家族统一 `BRAID_TOOL_SANDBOX*`,防再入不变量 = 该家族结构性不进容器 env)。

## 3. 镜像政策

- **基座**:`alpine:3.22` 按 digest 钉死(多架构 index digest)。沿用根 Dockerfile 既有裁决:「曾评估 distroless static(零 shell 零包管理器),gcr.io 在受限网络不可达且无稳定镜像代理,取舍 alpine」;基座自带 ca-certificates-bundle(https 出站必需);uid 65532 同根 Dockerfile adduser 惯例。digest 钉法先例 = dev.docker-compose.yml asynqmon 条目。
- **busybox shell 残余(披露)**:alpine 自带 busybox;在 ro rootfs + noexec tmpfs + cap-drop ALL + non-root 下,shell 只能运行镜像内既有 applet,无法落盘执行新二进制——可接受残余,登记 §9。
- **构建**:多阶段 `deploy/sandbox.Dockerfile`:golang:1.27-alpine builder(digest 钉,CGO_ENABLED=0 -trimpath -ldflags='-s -w',与根 Dockerfile 同纪律)→ alpine:3.22 运行层,prod 目标单文件 COPY `braid-sandbox-exec`(e2e 目标另有 CA 注入成员,见下条),USER 65532。测试路径 = 宿主 `go build` + COPY-only docker build(免拉 golang 镜像,CI 20min 预算友好)。
- **私 CA 注入契约(T7,构建期单通道)**:信任集扩张只经镜像构建,运行期零通道——env(§5 白名单恰 PATH/LANG;Go 侧信任源覆盖变量 `SSL_CERT_FILE`/`SSL_CERT_DIR` 同被白名单结构性挡在外)、stdin 信封(装的是工具原料,非信任锚)、挂载(白名单挂载不检视内容,承载的是不可当信任锚消费的字节)均不开 CA 通道,是 §9-7「束内容随镜像构建固定,运行期不改」的落地;将来企业私 CA 端点同通道重建镜像(deploy/README 登记,T8)。要点:
  - **材料**:context 文件 `sandbox-e2e-ca.crt`(仅 CA 证书 PEM;材料每次测试运行临时生成)。hermetic e2e 的 TLS server 落**宿主侧**——bridge 网容器 IP 全落禁段,可达的 hermetic 段地址必为宿主持有(T18.1 重选:198.51.100.0/24 TEST-NET-2;旧段 198.18.0.0/15 含 198.19.x 已随 D-2026-10-05-ssrf-five-ranges 五段扩禁入禁;地址装配方式与所需特权 T10 定);叶证书与全部私钥不进 build context、不入镜像、不经任何运行期通道进容器。
  - **Dockerfile 拓扑(T8 实现约束)**:`sandbox-e2e` 与 prod 平行 FROM 基座、不继承 prod;prod 必须是 Dockerfile 最后一个 stage(缺省 target,零注入点——SandboxProbe 提示命令不带 `--target` 且 context 是 repo 根,按缺省构建必须直接可用);运行层二进制从 build context COPY(宿主构建,builder stage 不是 prod 祖先,保 COPY-only 测试路径);CA COPY 与 append 只存在于 sandbox-e2e stage、先于 USER 65532(bundle 属 root,USER 后 append 即 Permission denied)。
  - **append 语义**:append 进 `/etc/ssl/certs/ca-certificates.crt` **只增不减**(基座束保留——clobber 式整体替换会静默断掉全部公网 https 出站,且失败形态不指向根因);helper 的 Go 系统池即按「文件字节 → AppendCertsFromPEM」读取(root.go loadOnDiskRoots,该文件是 alpine 上的探测首项),hash symlink 农场是 OpenSSL 消费者需求,不需要。
  - **e2e 镜像引用**:独立 tag,不得覆写 prod 缺省引用 `braid-tool-sandbox:1`(防 ephemeral CA 镜像滞留 daemon 被 prod Probe 放行),测试收尾清理;e2e 选段 198.51.100.x(T18.1 随 D-2026-10-05-ssrf-five-ranges 五段扩禁重选:RFC 5737 TEST-NET-2,与禁段及 fake-ip TUN 常用池零交集),段与守卫禁段的联动由 TestSandboxHermeticCIDRAllowed 锚死——将来扩禁再触段先红、重选段同条款跟进。
  - **机械锚**:构建期半边(context 唯一成员 + append ⇒ 信任)由 internal/tools/sandbox_ca_test.go 钉死;env 半边由 argv 全序列 golden(结构性无 `-e`/`--env-file`/`--env`)钉死;容器内 env 实证(dump-env e2e)T10 落地。
- **离线口径(D-t72 同纪律)**:运行时 daemon **永不 pull**——daemon 本地无镜像 = 响亮失败,错误消息携带构建命令;镜像引用经 env 配置,构造期校验。残余(PR review 登记):「永不 pull」由 worker 启动 Probe 保证;启动后镜像被删(prune / 并行 lane rmi)的运维窗内,docker run 缺省 pull policy=missing 会先对 registry 发起 pull 尝试再失败——`--pull never` 需 CLI ≥20.10,高于声明的 daemon 下限,随 §9-9 一并留待下限抬升时收口。
- **helper 生产二进制含 selftest 子命令家族**(dump-env / alloc / serve / spin / forkchain / fds / spam-stderr / zombies 及内部子模式 zombie-child、exit0;全枚举单点真源 = `sandbox_helper.go` 文件头):argv 词表可达但 carrier 永不发出;容器内攻击者本可自行分配内存/起服务/自旋/派生进程/开 fd/写自身 stderr,无新能力面(T11 审计复核成立:无探针做网络 I/O 或消费 cap,资源执法面是容器配置、由 inspect 矩阵落盘钉死)——以 argv 构造器单测(词表外值构造期拒)为回归锚。

## 4. 网络姿态与 t10.2 合成面

- **t10.1 拆分口径**:file_read 载体恒 `--network none`(零出网需求,零成本纵深);http 载体过渡 = 显式 `bridge`(不发布端口),SSRF/白名单执法完全依赖**守卫客户端随行入容器在拨号层强制**(Dialer.Control 拨号瞬间 IP 复核、ipForbidden 禁段、逐跳 https-only、CheckRedirect 宿主门+剥凭证、跳数 10、Proxy 恒不设)——执法点必须与拨号同进程;host-side-only 检查 = DNS rebinding TOCTOU 重开的假保证。
- **t10.2(egress 默认拒绝代理)必须合成**:(a) http 载体网络位换装为代理网是 SandboxSpec 单字段,t10.2 不触 Tool 接口;(b) 代理跳接组合在 transport/网络层,**不得经 env 代理变量旁路**(ssrf.go Proxy 恒不设是反旁路红线);(c) t10.2 的 DNS/IP 白名单须与 BRAID_HTTP_ENDPOINTS 别名单源合并,防双真源;(d) 本任务的 SSRF e2e(容器内断言)在换装后必须仍绿——它们是网络姿态换装的回归锚。

## 5. env 与凭证边界(基线法则)

- 容器环境**从零构造,仅 `PATH`(容器内标准值,非 worker 继承)与 `LANG=C.UTF-8` 两项**;`BRAID_TOOL_TOKEN`(scoped HMAC,60s TTL,宿主 dispatch 前已验)、`BRAID_ATTEMPT_KEY`(幂等键 `att:<node_exec_id>:<attempt_no>`)、工具 config(endpoints/dirs)、inputs 全部经 **stdin JSON 信封**(GATE 1 拍板;对 D-t32 env 四键契约的 delta,D-t101 登记,command.go 注释交叉引用)。
- **结构性禁令**:`--env-file`、`os.Environ()` 合并、docker secrets/config 挂载、docker socket 挂载、`BRAID_TOOL_TOKEN_SECRET` 入容器、任何 env 代理变量。
- 主凭证(MySQL DSN / LLM key / HMAC secret / 云凭证)**结构性不可达**:白名单构造而非黑名单过滤——不是「过滤掉了」,是「从未放入」。

## 6. 失败分类(含 GATE 1 拍板)

- **资源超限被杀(exit 137 / OOMKilled,无信封)→ 业务 `tool_failed`,不重试**(GATE 1 拍板:确定性超限重试同损,T9.1 fan-out 下烧 asynq 预算);沿用现有码,零新词表、零 runner.go 改动;错误消息携带 OOMKilled 归因(`docker inspect --format '{{.State.OOMKilled}}'` 单字段机器可读)。
- 信封 `ok=false` + 码 ∈ {ssrf_blocked, host_blocked, path_blocked} → 宿主重抛真哨兵 → `tool_ssrf_blocked` / `tool_host_blocked` / `tool_path_blocked`(业务 FAILED);`tool_failed` → 业务。
- **分类边界 = runner.go 现行契约,零改动**:哨兵先于 ctx 门(既有排序,专项竞争单测锚);ctx 取消/deadline(含容器 stop/watchdog 收尾)→ 裸 error 透传 = infra,attempt 留 RUNNING,asynq 重试;其余一切(daemon 不可达、CLI exit 125、信封缺失/畸形/未知码、exit≠0 无信封且 ctx 存活)→ 默认 `tool_failed` 业务——与 CommandTool 子进程失败先例同款语义;daemon 挂起类故障由 ctx deadline 兜进 infra。部署性故障(docker bin 缺失/daemon 不可达/镜像缺失)由 worker 启动 Probe 前移为拒启。

## 7. 降级开关与零静默降级

- `BRAID_TOOL_SANDBOX` = `off`(未设/空 = 缺省,行为逐字节不变,dev 栈零 docker 照跑)| `docker`;其他值构造期响亮败。
- `mode=docker` 下:docker bin 缺失 / daemon 不可达 / 镜像缺失 = worker 启动 Probe 响亮败(`docker info` + image inspect + daemon 版本下限;装配缺陷启动即现,preflight 哲学)。**绝不静默回落 local 执行**。
- CI 门控:`BRAID_TEST_REQUIRE_SANDBOX` 未设时继承 `BRAID_TEST_REQUIRE_STACK`(ci.yml:46 已设 → CI 内 docker 不可达即 Fatalf,零 ci.yml 触碰;门控矩阵本身表驱动单测)。

## 8. P1 安全评审停车场(显式不做,待议)

1. seccomp 定制 profile(本任务保持 daemon 默认;收窄需先做 syscall 审计)
2. cap 白名单粒度(drop-ALL 零加回;任何加回逐项具名论证,NET_RAW 永不在候选)
3. user namespace remap / rootless daemon(改变 bind mount 权限映射与网络语义,独立评估)
4. AppArmor/SELinux LSM 层
5. 镜像签名/attestation(cosign)与 T10.3 工具包签名的组合信任锚
6. 镜像 commit-label 与 worker commit 偏斜检查(本地同源构建下冗余;prod 流水线集成是部署面,deploy/README 登记义务)
7. http 载体网络终态时点(bridge 过渡 vs 提前并入 t10.2)
8. cgroup v2 细粒度调优(memory.high 软限、cpu.weight、限额×扇出吞吐平衡)
9. 宿主侧泄漏容器 watchdog(daemon 层带外监控,超越 per-call 结构化清理)
10. 资源限额生产档位定档(当前为基线提案值)
11. ~~启动清扫的实例身份维度~~(**T15.3 消除**,D-t151 #5 定案:沙箱容器打 `braid.sandbox.owner=<WorkerID>` label,启动清扫双 filter 只清自家——多 worker 副本/滚动重启共存不互杀,「owner 唯一且跨重启稳定」升格部署契约(hostname 显式钉死 + 停机宽限 ≥35s,见 deploy/README §7.1);kill -9 死实例身份可知(owner label 即归属),孤儿回收 = runbook 显式按-owner 原语(deploy/RUNBOOK.md §1.2),不再依赖全局 label 误伤。残余边界 = 活体同 owner 冲突,D-t151 #8-③ 登记,选型「部署契约承载」)

## 9. 残余风险(已知晓、接受)

1. **共享内核**:容器与宿主同内核,seccomp 允许集内任一内核可达缺陷即可逃逸;gVisor/Kata 升级路径明确在范围外(架构级残余)。
2. **容器内 /proc、/sys 暴露**:pid ns 只隔离可见进程,不隐藏内核接口;cap-drop 后收窄但不为零。
3. **worker↔daemon 通道 root 等价**:worker 进程内任何 RCE 即宿主 root 级;沙箱保护的是工作流轴,不是 worker 轴。
4. **http 载体桥网出网在 t10.2 前不受限**(过渡窗已命名;守卫被容器内漏洞击穿的场景拥有全量出网——窗口期长度是调度决定)。
5. **alpine busybox shell 残余**(§3 披露)。
6. **worker kill -9 后在飞容器存活至 watchdog deadline**(有界非零;owner label 已落,T15.3 起启动清扫按家自收 + runbook 按-owner 原语兜底)。
7. **CA 信任束内容**:alpine ca-certificates-bundle 内任何 CA 静默扩大容器内中间人能力;束内容随镜像构建固定,运行期不改,入镜像评审。
8. **daemon 版本漂移**:CI 与 prod daemon 版本不对齐;启动 Probe 含版本下限检查,不满足响亮失败。
9. **post-Probe 镜像丢失运维窗**(PR review 登记,§3 离线口径残余):镜像在 worker 启动后被删时,后续每次工具调用按 docker run 缺省 policy=missing 先发起 registry pull 尝试再失败(offline 环境下是拨号失败,online 环境下是意外拉取);`--pull never` 关闭该窗但需 CLI ≥20.10,高于 Probe 声明的 daemon 下限——待 CLI 下限抬升时随 argv 构造器一并收口。
