# braid 工具沙箱镜像(T10.1,docs/designs/t101-sandbox-baseline.md §3):
# file_read/http 的容器载体。三个 stage:
#   build        — 从源构建的纪律真源(与根 Dockerfile 同旗标);不是 prod
#                  的祖先(prod 运行层 COPY 宿主产物),缺省构建不执行,
#                  --target build 可单独验证源构建。
#   sandbox-e2e  — hermetic e2e 专用(独立 tag,不覆写 braid-tool-sandbox:1,
#                  测试收尾 rmi):私 CA 构建期注入单通道。
#   prod(最后) — 缺省 target,零注入:单文件 COPY 宿主预构建二进制。
# 缺省构建两步(context = repo 根,先产出二进制成员;同构命令见
# deploy/README.md §7 与 SandboxProbe 缺镜像错误消息)。构建需 BuildKit
# (buildx):legacy builder 线性执行全部 stage,缺省构建在 sandbox-e2e
# 的 CA COPY 处中止。
#   CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
#     -o braid-sandbox-exec ./cmd/braid-sandbox-exec
#   docker build -f deploy/sandbox.Dockerfile -t braid-tool-sandbox:1 .
# 测试路径 = 宿主构建 + COPY-only(免拉 golang 镜像,CI 20min 预算友好;
# internal/tools/sandbox_image_test.go 消费)。

# 阶段 1:build,源构建纪律真源(非 prod 祖先)。digest 钉死;GOPROXY
# build-arg 同根 Dockerfile(受限网络可换国内代理)。
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG GOPROXY=
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o /out/braid-sandbox-exec ./cmd/braid-sandbox-exec

# 阶段 2:sandbox-e2e(--target sandbox-e2e)。平行 FROM 基座、不继承 prod。
# 私 CA 注入契约(基线 §3):仅 CA 证书 PEM(context 成员 sandbox-e2e-ca.crt,
# ephemeral 测试材料)append 进 /etc/ssl/certs/ca-certificates.crt,只增不减
# ——基座束保留(clobber 式整体替换会静默断全部公网 https 出站),Go 系统
# 池按文件字节加载(root.go loadOnDiskRoots,该文件是 alpine 探测首项)。
# CA COPY 与 append 全部先于 USER 65532(bundle 属 root,USER 后 append 即
# Permission denied)。收尾 rm 让临时成员不驻留运行层 PATH(其字节仍在中间
# 层;CA 是公开材料,非保密位)。
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS sandbox-e2e
COPY braid-sandbox-exec /usr/local/bin/braid-sandbox-exec
COPY sandbox-e2e-ca.crt /tmp/sandbox-e2e-ca.crt
RUN cat /tmp/sandbox-e2e-ca.crt >> /etc/ssl/certs/ca-certificates.crt \
    && rm /tmp/sandbox-e2e-ca.crt \
    && adduser -D -u 65532 braid
# env 从零构造的两成员之一(基线 §5:PATH 来自基座,LANG 由镜像 ENV 提供;
# T10 dump-env e2e 实证 T8 缺此行 = 文档与实现的漂移点,红后补)。
ENV LANG=C.UTF-8
USER 65532
ENTRYPOINT ["/usr/local/bin/braid-sandbox-exec"]

# 阶段 3:prod,缺省 target = Dockerfile 最后一个 stage(零注入点——
# SandboxProbe 提示命令不带 --target 且 context 是 repo 根,按缺省构建必须
# 直接可用)。单文件 COPY 宿主产物(builder 非 prod 祖先,保 COPY-only
# 测试路径);uid 65532 同根 Dockerfile adduser 惯例;ENTRYPOINT 与
# internal/tools sandbox_argv.go 的 sandboxEntrypoint 常量一致(carrier 另
# 行显式 --entrypoint,双保险)。
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
COPY braid-sandbox-exec /usr/local/bin/braid-sandbox-exec
RUN adduser -D -u 65532 braid
# env 两成员契约同 sandbox-e2e 段(基线 §5;两平行段零继承,各自声明)。
ENV LANG=C.UTF-8
USER 65532
ENTRYPOINT ["/usr/local/bin/braid-sandbox-exec"]
