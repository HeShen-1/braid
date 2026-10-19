#!/usr/bin/env bash
set -euo pipefail

# T23.13: local provider credentials/config never enter a checkout sent to CI.
# gitleaks scans tracked history; this guard protects the exact local filename
# and its backup pattern so a forced add fails before any merge.

readonly local_env='deploy/t23.13-glm.env'
readonly local_backup='deploy/t23.13-glm.env.bak'

for path in "$local_env" "$local_backup"; do
  if ! git check-ignore --no-index -q "$path"; then
    echo "private env guard: $path is not ignored" >&2
    exit 1
  fi
done

if tracked=$(git ls-files 'deploy/t23.13-glm.env*'); [[ -n "$tracked" ]]; then
  echo "private env guard: tracked private env path(s): $tracked" >&2
  exit 1
fi

if [[ "${CI:-}" == 'true' && -e "$local_env" ]]; then
  echo "private env guard: local credentials file exists in CI checkout" >&2
  exit 1
fi

echo "private env guard: ignored and untracked"
