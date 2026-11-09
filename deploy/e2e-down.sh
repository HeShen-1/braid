#!/usr/bin/env bash
# T16.6 e2e 栈停止:只收本栈进程组(e2e-up.sh 的 setsid pgid = pid 文件,
# 组内含 go run 与其编译产物)→ compose down(数据面 tmpfs 随容器退场,
# 栈可重复)。刻意不做按名 pkill 清扫——那会误伤并行 dev 栈(同二进制
# 名;活进程联调仅主仓公约下两栈同宿主是常态)。组杀前做归属校验
# (组长 cmdline、pgid、boot id 和启动时间须匹配):pid 回绕复用时防误杀无关进程组
# (AI review LOW)。
set -uo pipefail
cd "$(dirname "$0")/.."
source deploy/e2e-process.sh

LOGDIR="log/e2e"

# 归属校验:pid 文件按组件命名(api/relay/worker/reaper.pid),组长
# 进程 cmdline、组长身份和启动时保存的 boot id/starttime 须匹配;
# 不符或旧 PID 文件缺身份信息时跳过并提示,不杀。
own_pid() { # $1=组件 → echo 校验通过的 pid,失败输出空串
  local f="$LOGDIR/$1.pid"
  [ -f "$f" ] || return 1
  local pid saved_boot saved_start extra boot starttime group_status
  pid=$(cat "$f")
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]]; then
    if e2e_group_running "$pid"; then
      : # 组仍存活,继续校验组长身份。
    else
      group_status=$?
      if [ "$group_status" -eq 1 ]; then
        echo "$1(pid $pid) 已停止" >&2
        return 2
      fi
    fi
  fi
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && [ -r "$LOGDIR/$1.identity" ] &&
     read -r saved_boot saved_start extra < "$LOGDIR/$1.identity" &&
     [[ -n "$saved_boot" && "$saved_start" =~ ^[0-9]+$ && -z "$extra" ]] &&
     boot=$(cat /proc/sys/kernel/random/boot_id) &&
     starttime=$(e2e_starttime "$pid") &&
     [[ "$boot" == "$saved_boot" && "$starttime" == "$saved_start" ]] &&
     [ -r "/proc/$pid/cmdline" ] &&
     tr '\0' ' ' < "/proc/$pid/cmdline" | grep -Fq "cmd/$1" &&
     [ "$(ps -o pgid= -p "$pid" | tr -d '[:space:]')" = "$pid" ]; then
    echo "$pid"
    return 0
  fi
  echo "跳过 $1(pid $pid 归属不符——残留 pid 文件或 pid 复用,防误杀)" >&2
  return 1
}

if [ -d "$LOGDIR" ]; then
  pids=""
  skipped=0
  for comp in api relay worker reaper; do
    if p=$(own_pid "$comp"); then
      pids="$pids $p"
    else
      result=$?
      if [ -f "$LOGDIR/$comp.pid" ] && [ "$result" -ne 2 ]; then
        skipped=1
      fi
    fi
  done
  for pid in $pids; do
    kill -TERM -- -"$pid" 2>/dev/null || true
  done
  for _ in $(seq 1 10); do
    alive=0
    for pid in $pids; do
      if e2e_group_running "$pid"; then
        alive=1
      else
        result=$?
        [ "$result" -eq 1 ] || alive=1
      fi
    done
    [ "$alive" -eq 0 ] && break
    sleep 1
  done
  for pid in $pids; do
    if e2e_group_running "$pid"; then
      kill -KILL -- -"$pid" 2>/dev/null || true
    fi
  done
  cleanup_failed=0
  for _ in $(seq 1 5); do
    cleanup_failed=0
    for pid in $pids; do
      if e2e_group_running "$pid"; then
        cleanup_failed=1
      else
        result=$?
        [ "$result" -eq 1 ] || cleanup_failed=1
      fi
    done
    [ "$cleanup_failed" -eq 0 ] && break
    sleep 1
  done
  [ "$cleanup_failed" -eq 0 ] || echo "e2e 进程组仍活动或无法检查,请人工核查" >&2
fi

compose_status=0
docker compose -f deploy/e2e.docker-compose.yml -p braid-e2e down -v || compose_status=$?
if [ "$compose_status" -ne 0 ]; then
  echo "e2e 数据面停止失败(退出码 $compose_status)" >&2
fi
if [ "${skipped:-0}" -ne 0 ]; then
  echo "身份未确认的 e2e 进程已跳过,请人工核查" >&2
elif [ "$compose_status" -eq 0 ] && [ "${cleanup_failed:-0}" -eq 0 ]; then
  echo "e2e 栈已停(数据面 tmpfs 随 down 清空)"
fi
[ "$compose_status" -eq 0 ] || exit "$compose_status"
[ "${skipped:-0}" -eq 0 ] || exit 1
[ "${cleanup_failed:-0}" -eq 0 ] || exit 1
