#!/usr/bin/env bash
# /proc stat's comm can contain spaces or parentheses; field 22 is starttime.
e2e_starttime() {
  local stat
  local -a fields
  IFS= read -r stat < "/proc/$1/stat" || return 1
  stat=${stat##*) }
  read -ra fields <<< "$stat"
  [[ "${fields[19]:-}" =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${fields[19]}"
}

# Return 0 for live members, 1 when none remain, 2 when inspection failed.
e2e_group_running() {
  local processes
  processes=$(ps -eo pgid=,stat=) || return 2
  awk -v pgid="$1" '$1 == pgid && $2 !~ /^Z/ { found=1 } END { exit !found }' <<< "$processes"
}

# Refuse to start if an untracked relay can connect to either data-plane port.
# The optional proc root exists for tests; e2e-up always uses the real /proc.
e2e_guard_relays() {
  local mysql_dsn="$1" redis_addr="$2" proc_root="${3:-/proc}"
  local mysql_port redis_port self_ns dir pid comm netns result
  local -a argv
  local loopback='(127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|localhost|\[::1\])'
  [[ "$mysql_dsn" =~ tcp\(127\.0\.0\.1:([0-9]+)\) ]] || return 2
  mysql_port=${BASH_REMATCH[1]}
  [[ "$redis_addr" =~ ^127\.0\.0\.1:([0-9]+)$ ]] || return 2
  redis_port=${BASH_REMATCH[1]}
  self_ns=$(readlink "$proc_root/self/ns/net") || {
    echo '无法检查本进程的网络命名空间，拒绝启动 e2e 栈' >&2
    return 2
  }
  for dir in "$proc_root"/[0-9]*; do
    [[ -d "$dir" ]] || continue
    pid=${dir##*/}
    if ! IFS= read -r comm < "$dir/comm" 2>/dev/null; then
      [[ ! -d "$dir" ]] && continue
      echo "无法检查进程 $pid 的身份，拒绝启动 e2e 栈" >&2
      return 2
    fi
    [[ "$comm" == relay || "$comm" == go ]] || continue
    if ! netns=$(readlink "$dir/ns/net" 2>/dev/null); then
      [[ ! -d "$dir" ]] && continue
      echo "无法检查 relay 进程 $pid 的网络命名空间，拒绝启动 e2e 栈" >&2
      return 2
    fi
    [[ "$netns" == "$self_ns" ]] || continue
    if [[ "$comm" == go ]]; then
      if ! mapfile -d '' -t argv 2>/dev/null < "$dir/cmdline"; then
        [[ ! -d "$dir" ]] && continue
        echo "无法检查 go 进程 $pid 的命令，拒绝启动 e2e 栈" >&2
        return 2
      fi
      if [[ ${#argv[@]} -eq 0 ]]; then
        [[ ! -d "$dir" ]] && continue
        echo "无法确认 go 进程 $pid 的命令，拒绝启动 e2e 栈" >&2
        return 2
      fi
      [[ "${argv[1]:-}" == run && "${argv[2]:-}" == ./cmd/relay ]] || continue
    fi
    if grep -azqE "^BRAID_MYSQL_DSN=.*tcp\\(${loopback}:${mysql_port}\\)|^BRAID_REDIS_QUEUE_ADDR=${loopback}:${redis_port}$" "$dir/environ" 2>/dev/null; then
      echo "发现可能连接 e2e 数据面的 relay 进程 $pid，请人工核查后再启动" >&2
      return 1
    else
      result=$?
      if [[ "$result" -ne 1 ]]; then
        [[ ! -d "$dir" ]] && continue
        echo "无法读取 relay 进程 $pid 的配置，拒绝启动 e2e 栈" >&2
        return 2
      fi
    fi
    # A process named relay with unknown configuration cannot be ruled out.
    if ! grep -azq '^BRAID_MYSQL_DSN=' "$dir/environ" 2>/dev/null ||
       ! grep -azq '^BRAID_REDIS_QUEUE_ADDR=' "$dir/environ" 2>/dev/null; then
      [[ ! -d "$dir" ]] && continue
      echo "无法确认 relay 进程 $pid 的配置，拒绝启动 e2e 栈" >&2
      return 2
    fi
  done
}
