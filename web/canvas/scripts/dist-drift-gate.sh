#!/usr/bin/env bash
# T21.5 漂移门判据本体(单一实现:CI 步骤与负向自证共用同一个脚本)。
#
# 判据 = 「入库产物必须逐字节等于从入库源码重建的结果」。
#
# 为什么不是旧的「就地重建 + git status」:那条判据依赖「构建写到门所看的
# 路径」。把 vite 的 build.outDir 改走而不同步 dist 时,就地重建落到新路径、
# 旧 dist 原样,git status 对门对象干净 = 门恒绿(PR #200 的 tracked-file
# 守卫只挡「对象整体消失」,挡不住「构建不再写它」;真机复现见 issue #205)。
# 本脚本比对的是两个目录,与构建写到哪里无关。
#
# 用法:web/canvas/scripts/dist-drift-gate.sh [dist] [verify]
#   dist   入库产物目录(缺省 internal/api/web,= go:embed web 的对象)
#   verify 临时目录重建结果(缺省 web/canvas/.dist-verify)
# 退出码:0 = 一致;1 = 漂移或门失效(两种都必须红)。
set -euo pipefail

dist=${1:-internal/api/web}
verify=${2:-web/canvas/.dist-verify}

# 守卫 1:门对象必须存在 —— 对不存在的路径 diff 恒判等 = 恒绿。
if [ ! -d "$dist" ]; then
  echo "::error::漂移门失效:$dist 不存在,门将恒绿"
  exit 1
fi

# 守卫 2:门对象必须有入库文件 —— T21.2 把 outDir 上提后,门曾对旧路径恒绿三天。
tracked=$(git ls-files -- "$dist/assets" | wc -l | tr -d ' ')
if [ "$tracked" -eq 0 ]; then
  echo "::error::漂移门失效:$dist/assets 无入库文件,门将恒绿"
  exit 1
fi

# 守卫 3:重建结果必须先就位,否则比对无意义(CI 里由 build:verify 产出)。
if [ ! -d "$verify" ]; then
  echo "::error::漂移门失效:$verify 不存在(先跑 npm --prefix web/canvas run build:verify)"
  exit 1
fi

# 判据:diff -r --brief 逐文件比对,差异行本身即修复清单
# (内容不同 / 只在一侧的文件都会列出;二进制文件同样拦截)。
if ! diff -r --brief -- "$dist" "$verify"; then
  echo "::error::dist 漂移:入库产物 ≠ 从入库源码重建的结果"
  echo "::error::修复:npm --prefix web/canvas run build 后把 internal/api/web/ 的重建结果同 commit 提交"
  exit 1
fi

echo "dist drift gate: clean ($(find "$dist" -type f | wc -l | tr -d ' ') files)"
