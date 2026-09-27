# braid 交付镜像(T7.4):一个镜像四个入口(api/worker/relay/reaper),
# compose 交付栈按 service 各自覆盖 entrypoint。
# 构建上下文 = 仓库根:docker build -t braid . (或经 deploy/prod.docker-compose.yml
# 的 build 段;迁移文件不在镜像内——生产迁移走 golang-migrate CLI 挂
# internal/db/migrations,与 deploy/README.md §1 口径一致)。
#
# 阶段 1:构建。alpine 工具链 + CGO_ENABLED=0(纯静态,go-sql-driver 纯 Go);
# -trimpath -s -w 去路径与符号表。分层缓存:go.mod/go.sum 先拷先下,源码
# 变更不重下依赖。
FROM golang:1.27-alpine AS build
# GOPROXY 经 build-arg 可覆盖(默认不改变官方镜像行为;受限网络环境
# --build-arg GOPROXY=https://goproxy.cn,direct):
ARG GOPROXY=
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/ ./cmd/...

# 阶段 2:运行时。alpine(基础镜像自带 ca-certificates-bundle——LLM/SMTP
# 出站 TLS 需要;tzdata 不需要:DB 时钟契约是会话 UTC,代码侧 time.UTC
# 处理)。非 root:进程以 uid 65532 跑(distroless nonroot 惯例同值),容器
# 内落盘面(日志/临时)不需要 root。曾评估 distroless static(零 shell 零
# 包管理器),gcr.io 在受限网络不可达且无稳定镜像代理,取舍 alpine——
# 附带 busybox wget 可做存活探针(交付 compose 仍不配容器级 healthcheck,
# 存活面 = /healthz + metrics exposition 由外部监控探,UPGRADE.md 登记)。
FROM alpine:3.22
RUN adduser -D -u 65532 braid
COPY --from=build /out/ /usr/local/bin/
# api(HTTP)/ metrics exposition;EXPOSE 仅文档性,实际监听由 BRAID_*_ADDR
# 决定(容器内须绑 0.0.0.0,见 compose environment)。
EXPOSE 8081 9090
USER 65532
ENTRYPOINT ["/usr/local/bin/api"]
