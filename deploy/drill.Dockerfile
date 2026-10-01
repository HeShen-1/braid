# braid 演练镜像变体(T15.6):在根 Dockerfile 交付镜像之上加 docker-cli
# ——worker 容器经 /var/run/docker.sock 对宿主 daemon 跑沙箱 probe/sweep/
# exec(arm ③ 的 owner-scoped 清扫实路径)。仅演练栈使用,不进 prod 面
#(交付镜像不带 docker CLI 是刻意的攻击面收敛,见根 Dockerfile 注释)。
# 构建分两段(runner 编排,compose 面零 build 段——镜像恒由 runner 构建
# 防测 daemon 滞留旧二进制,sandbox e2e 同款契约):
#   docker build -f Dockerfile -t braid-drill-base:t<slot> .
#   docker build -f deploy/drill.Dockerfile --build-arg BASE=braid-drill-base:t<slot> -t braid-drill:t<slot> .
ARG BASE=braid-drill-base:t0
FROM ${BASE}
USER root
RUN apk add --no-cache docker-cli
USER 65532
