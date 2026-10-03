#!/usr/bin/env bash
# 交付镜像本地导出流水线(T16.5;D-2026-10-03-t165-image-delivery)。
# 零 Secrets 姿态下的发布通道裁定 = 本地导出:build 双 tag → save tar +
# sha256 → 目标机 load / k3s containerd 导入。GHCR 重访触发器见 D- 条目。
#
# tag 口径:braid:<short-sha>(钉版,不可变,随 commit 可追溯)+
# braid:prod(浮动指针,与 prod compose 契约同名);Helm 部署方改写
# chart values image.tag = <short-sha> 即得钉版部署。
#
# 用法:
#   deploy/release.sh build [--allow-dirty]       构建双 tag 镜像
#   deploy/release.sh save [tag] [dir]            导出 tar + sha256(默认当前 sha、dist/)
#   deploy/release.sh load <tar>                  验和后 docker load
#   deploy/release.sh k3s-import <tar> [容器名]   验和后导入 k3s containerd 并 canonical 重标名
#
# build 细节:
#   - --provenance=false:关 BuildKit attestation,否则镜像是 OCI index,
#     docker save/ctr import 会 content digest not found(本机实证坑);
#   - GOPROXY 环境变量透传 build-arg(受限网络出口,与 Dockerfile 默认互补);
#   - 脏树默认拒(导出物须可追溯到 commit),--allow-dirty 豁免时 tag 追加
#     -dirty 后缀保真溯源。
set -euo pipefail
cd "$(dirname "$0")/.."

K3S_CTR=(ctr --address /run/k3s/containerd/containerd.sock --namespace k8s.io)

die() { echo "$1" >&2; exit 1; }

usage() {
	sed -n '2,/^[^#]/p' "$0" | grep '^#' | sed 's/^# \{0,1\}//'
	exit 1
}

cmd_build() {
	local allow_dirty=""
	[ "${1:-}" = "--allow-dirty" ] && allow_dirty=1
	local sha dirty=""
	sha=$(git rev-parse --short HEAD)
	# 判脏口径 = git status --porcelain(覆盖 staged/unstaged/untracked
	# 三态,对抗审计 HIGH 修正:git diff 双查对 untracked 全盲,而
	# COPY . . 会把未跟踪文件烘进镜像——游离迁移文件会经 migrate Job
	# 对生产库真执行,还顶着干净 sha 假溯源;gitignored 的 dist/log
	# 不在 porcelain 内,不误报)。
	if [ -n "$(git status --porcelain)" ]; then
		if [ -n "$allow_dirty" ]; then
			dirty="-dirty"
			echo "警告:脏树构建,tag 追加 -dirty(导出物溯源降级)" >&2
		else
			die "工作树不干净(含未跟踪文件,经 COPY . . 烘进镜像;导出物须可追溯到 commit):先提交,或 --allow-dirty 豁免"
		fi
	fi
	local tag="$sha$dirty"
	local args=(--provenance=false -t "braid:$tag" -t braid:prod)
	# GOPROXY 解析链:shell env 优先,缺省回落 go env(受限网络出口常经
	# `go env -w` 配置,不落 shell env——实证:不透传则容器内打
	# proxy.golang.org,EOF 不可达);容器内模块下载与 host 同源。
	local proxy="${GOPROXY:-}"
	if [ -z "$proxy" ] && command -v go >/dev/null 2>&1; then
		proxy=$(go env GOPROXY)
	fi
	[ -n "$proxy" ] && args+=(--build-arg "GOPROXY=$proxy")
	docker build "${args[@]}" .
	echo "镜像就绪:braid:$tag(钉版)+ braid:prod(浮动)"
	echo "导出:deploy/release.sh save    目标机:deploy/release.sh load <tar>"
}

cmd_save() {
	local tag="${1:-$(git rev-parse --short HEAD)}"
	local dir="${2:-dist}"
	tag="${tag#braid:}" # 容忍 braid:<tag> 全名传参
	local img="braid:$tag"
	docker image inspect "$img" >/dev/null 2>&1 \
		|| die "镜像 $img 不在本地:先 deploy/release.sh build"
	mkdir -p "$dir"
	local tar="$dir/braid-$tag.tar"
	docker save -o "$tar" "$img"
	(cd "$dir" && sha256sum "braid-$tag.tar" > "braid-$tag.tar.sha256")
	echo "导出:$tar(+$dir/braid-$tag.tar.sha256)"
	echo "目标机:cd $(dirname "$tar" 2>/dev/null || echo "$dir") && sha256sum -c braid-$tag.tar.sha256 && deploy/release.sh load <tar>"
}

# load/k3s-import 前置:tar 与 .sha256 同目录在场且匹配才导入
# (防离线搬运中的误传与损坏;防篡改须带外比对,见 deploy/README §12)。
verify_sha256() {
	local tar="$1"
	local dir sum
	dir=$(dirname "$tar")
	sum="$dir/$(basename "$tar").sha256"
	[ -f "$sum" ] || die "缺校验和文件 $sum(由 save 生成;缺失即不可信,拒导入)"
	(cd "$dir" && sha256sum -c "$(basename "$sum")")
}

cmd_load() {
	local tar="${1:?用法:deploy/release.sh load <tar>}"
	verify_sha256 "$tar"
	docker load -i "$tar"
}

# containerd 名归一:K8s/CRI 把 image: braid:<tag> 归一成
# docker.io/library/braid:<tag> 再查本地,名字不在场则 IfNotPresent 视为
# 缺失。实证(k3s v1.31.4 + ctr import):单段名 import 自动归一,
# canonical 名直出;重标名分支是对不归一形态的防御兜底(幂等,双名共存
# 无害),两形态都不在才报错。
cmd_k3s_import() {
	local tar="${1:?用法:deploy/release.sh k3s-import <tar> [容器名]}"
	local box="${2:-k3s}"
	local tag
	tag=$(basename "$tar" .tar)
	tag="${tag#braid-}"
	local want="docker.io/library/braid:$tag"
	verify_sha256 "$tar"
	docker exec -i "$box" "${K3S_CTR[@]}" images import - < "$tar"
	if docker exec "$box" "${K3S_CTR[@]}" images ls "name==$want" 2>/dev/null | tail -n +2 | grep -q .; then
		: # canonical 名已在场(镜像先期导入过或 import 已归一)
	elif docker exec "$box" "${K3S_CTR[@]}" images ls "name==braid:$tag" 2>/dev/null | tail -n +2 | grep -q .; then
		docker exec "$box" "${K3S_CTR[@]}" images tag "braid:$tag" "$want" >/dev/null
	else
		die "导入后未见镜像名(braid:$tag / $want):docker exec $box ${K3S_CTR[*]} images ls 查实际名"
	fi
	echo "k3s 就绪:$want(chart 部署:--set image.tag=$tag)"
}

case "${1:-}" in
build) shift; cmd_build "$@" ;;
save) shift; cmd_save "$@" ;;
load) shift; cmd_load "$@" ;;
k3s-import) shift; cmd_k3s_import "$@" ;;
*) usage ;;
esac
