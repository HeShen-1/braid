#!/usr/bin/env bash
# T21.5 漂移门判据本体(单一实现:CI 步骤、本地复跑、门自证共用同一个脚本)。
#
# 判据(全部必须过):
#   ① 入库产物与 HEAD 一致(旧 git-status 门的那一半信号;本地跑才有区分度)
#   ② 入库产物逐字节等于从入库源码重建的结果(核心;与构建写到哪里无关)
#   ③ 构建配置仍把产物指向门对象(vite build.outDir 被甩开 = 门对象冻结恒绿)
#   ④ 重建结果确实就位,且两侧不同源(否则比对恒判等 = 自比)
#
# 为什么不是旧的「就地重建 + git status」:那条判据依赖「构建写到门所看的
# 路径」。把 vite 的 build.outDir 改走而不同步 dist 时,就地重建落到新路径、
# 旧 dist 原样,git status 对门对象干净 = 门恒绿(真机复现见 issue #205)。
# 本脚本比对的是两个目录,与构建写到哪里无关;outDir 漂移本身由 ③ 直接断言。
#
# 用法:web/canvas/scripts/dist-drift-gate.sh [dist] [verify]
#   dist   入库产物目录(缺省 internal/api/web,= go:embed web 的对象)
#   verify 临时目录重建结果(缺省 web/canvas/.dist-verify)
#   两条路径按仓库根解析(脚本自己 cd 到 toplevel,cwd 无关)。
# 退出码:0 = 一致;1 = 漂移或门失效(两种都必须红)。
#
# 判据边界(有意不覆盖,给对抗审计留档;均无运行时后果):
#   - 权限位:不比 exec/读写位。旧 git-status 判据会附带报 ` M`,但 go:embed
#     不看模式,而全模式比对会被 umask 差异打成败红。
#   - git 的 clean/smudge 属性过滤器与 CRLF 归一化:仓内 .gitattributes 为空,
#     今天无差;本判据比原始字节,比旧判据(比经属性过滤的 blob)更严。
#     注意:全仓文本都会被 Tailwind v4 自动扫描取材,本脚本自身也是取材面 ——
#     注释里混进 Tailwind 能识别的工具词就会改写入库 CSS(T21.6 / issue #207)。
#   - 子模块 gitlink:dist 面不含子模块。
#   符号链接**不在**边界内:go:embed 静默跳过软链,故 diff 必须
#   --no-dereference,否则「真文件换成同内容软链」是假绿(审计 F1)。
set -euo pipefail

# 门对象/入库面一律按仓库根解析,避免 cwd 不同导致假红/裸 fatal(审计 F7)。
if ! repo=$(git rev-parse --show-toplevel 2>/dev/null); then
  echo "::error::漂移门失效:不在 git 工作区内(cwd=$(pwd)),无从判定入库产物"
  exit 1
fi
cd "$repo"

dist=${1:-internal/api/web}
verify=${2:-web/canvas/.dist-verify}
default_dist=internal/api/web

# 守卫 1:门对象必须存在 —— 对不存在的路径 diff 恒判等 = 恒绿。
if [ ! -d "$dist" ]; then
  echo "::error::漂移门失效:$dist 不存在,门将恒绿"
  exit 1
fi

# 守卫 2:门对象必须有入库文件 —— T21.2 把 outDir 上提后,门曾对旧路径恒绿三天。
# 数整个 $dist 而非硬编码 assets/:assetsDir 改名不该把门变成永久假红(审计 F6)。
tracked=$(git ls-files -- "$dist" | wc -l | tr -d ' ')
if [ "$tracked" -eq 0 ]; then
  echo "::error::漂移门失效:$dist 无入库文件,门将恒绿"
  exit 1
fi

# 守卫 3:重建结果必须先就位(q.v. 判据 ④)。
if [ ! -d "$verify" ]; then
  echo "::error::漂移门失效:$verify 不存在(先跑 npm --prefix web/canvas run build:verify)"
  exit 1
fi

# 守卫 4:两侧不能同源 —— 同目录/软链同源时 diff 恒判等 = 门变自比(审计 F2;
# build:verify 里的 rm -rf 已让软链写穿不可达,此处兜住手工构造)。
if [ "$dist" -ef "$verify" ]; then
  echo "::error::漂移门失效:$dist 与 $verify 同源,门将恒绿(检查 web/canvas/.dist-verify 是否被软链替换)"
  exit 1
fi

# 判据 ③:构建配置仍指向门对象。outDir 漂移过一次(T21.2 → 门对旧路径恒绿三天),
# 而 CLI --outDir 覆盖会让比对仍绿、掩盖「dist 已不是构建目标」——此断言让漂移
# 当场红(审计 F11)。改 outDir = 同改本行 + go:embed 面(同 .nvmrc/playwright 纪律)。
if [ "$dist" = "$default_dist" ]; then
  if ! grep -Eq "outDir:[[:space:]]*'\.\./\.\./internal/api/web'" web/canvas/vite.config.ts; then
    echo "::error::漂移门失效:vite build.outDir 不再指向 $default_dist(改 outDir 必须同改门对象与 go:embed 面)"
    exit 1
  fi
fi

# 判据 ②:diff -r --brief 逐文件比对,差异行本身即修复清单
# (内容不同 / 只在一侧 / 二进制 / 软链类型 都会列出)。
# --no-dereference 必须有:go:embed 静默跳过软链,解引用会把「真文件换同内容软链」
# 判成绿(审计 F1;GNU diffutils ≥3.3,CI 与 dev 机均 GNU diff)。
if ! diff -r --brief --no-dereference -- "$dist" "$verify"; then
  echo "::error::dist 漂移:入库产物 ≠ 从入库源码重建的结果"
  echo "::error::修复:npm --prefix web/canvas run build 重建入库产物并同 commit 提交;若刚动过 vite build.outDir,先确认它仍指向 $default_dist"
  exit 1
fi

# 判据 ①:入库产物必须与 HEAD 一致。CI 里 checkout 干净且本步不改工作树 → 恒真;
# 本地跑时它接住「重建了但没同 commit 提交」那一半信号(审计 F5)。
if [ -n "$(git status --porcelain -- "$dist")" ]; then
  git status --porcelain -- "$dist"
  echo "::error::入库产物与 HEAD 不一致:重建结果未同 commit 提交"
  exit 1
fi

echo "dist drift gate: clean ($(find "$dist" -type f | wc -l | tr -d ' ') files)"
