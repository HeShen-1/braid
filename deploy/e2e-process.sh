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
