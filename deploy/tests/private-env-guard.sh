#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."

# T23.13: local provider credentials/config never enter a checkout sent to CI.
# gitleaks scans tracked history; this guard protects the exact local filename
# and its backup pattern so a forced add fails before any merge.

readonly local_env='deploy/t23.13-glm.env'
readonly hidden_env='deploy/.t23.13-glm.env'

for path in "$local_env" "$local_env.bak" "$local_env~" "$hidden_env.swp"; do
  if ! git check-ignore --no-index -q "$path"; then
    echo "private env guard: $path is not ignored" >&2
    exit 1
  fi
done

# Docker does not consume .gitignore. Require the exact exclusions in the
# build context too, before a local credential can enter COPY . . cache layers.
for rule in "$local_env*" "$hidden_env*"; do
  if ! grep -Fxq "$rule" .dockerignore; then
    echo "private env guard: Docker context must exclude local env and backups" >&2
    exit 1
  fi
done

tracked=$(git ls-files "$local_env*" "$hidden_env*")
if [[ -n "$tracked" ]]; then
  echo "private env guard: tracked private env path(s): $tracked" >&2
  exit 1
fi

# Also reject add-then-delete commits before push, without reading their content.
history=$(git log --all --format='%H' -- "$local_env*" "$hidden_env*")
if [[ -n "$history" ]]; then
  echo "private env guard: private path found in reachable Git history" >&2
  exit 1
fi

if [[ "${CI:-}" == 'true' ]]; then
  shopt -s nullglob
  private_files=("$local_env"* "$hidden_env"*)
  for path in "${private_files[@]}"; do
    if [[ -e "$path" || -L "$path" ]]; then
      echo "private env guard: local credentials file exists in CI checkout" >&2
      exit 1
    fi
  done
fi

echo "private env guard: Git and Docker exclusions present; private paths untracked"
