#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

# Use only dummy files in an owned temporary repository/context. Never copy
# the user's local env, Git history, provider key, or other workspace files.
probe=$(mktemp -d)
trap 'rm -rf "$probe"' EXIT
mkdir -p "$probe/repo/deploy/tests"
cp .gitignore .dockerignore "$probe/repo/"
cp deploy/tests/private-env-guard.sh "$probe/repo/deploy/tests/"
git -C "$probe/repo" init -q
guard="$probe/repo/deploy/tests/private-env-guard.sh"
CI=true bash "$guard"

must_reject() {
  local reason=$1
  if CI=true bash "$guard" >"$probe/rejection.log" 2>&1; then
    echo "private env self-test: accepted $reason" >&2
    exit 1
  fi
  echo "private env self-test: rejected $reason"
}

for suffix in '' .bak .local '~'; do
  printf 'local-only dummy marker\n' > "$probe/repo/deploy/t23.13-glm.env$suffix"
  must_reject "private env$suffix in CI"
  rm "$probe/repo/deploy/t23.13-glm.env$suffix"
done
printf 'local-only dummy marker\n' > "$probe/repo/deploy/.t23.13-glm.env.swp"
must_reject 'private env editor copy in CI'
rm "$probe/repo/deploy/.t23.13-glm.env.swp"
ln -s missing "$probe/repo/deploy/t23.13-glm.env.bak"
must_reject 'private env backup symlink in CI'
rm "$probe/repo/deploy/t23.13-glm.env.bak"
printf 'local-only dummy marker\n' > "$probe/repo/deploy/t23.13-glm.env"
git -C "$probe/repo" add -f deploy/t23.13-glm.env
must_reject 'forced tracked private env'
git -C "$probe/repo" rm -q --cached deploy/t23.13-glm.env
rm "$probe/repo/deploy/t23.13-glm.env"
printf 'local-only dummy marker\n' > "$probe/repo/deploy/t23.13-glm.env"
git -C "$probe/repo" add -f deploy/t23.13-glm.env
git -C "$probe/repo" -c user.name='Guard test' -c user.email='guard@example.invalid' commit -qm 'test: add dummy private path'
git -C "$probe/repo" rm -q deploy/t23.13-glm.env
git -C "$probe/repo" -c user.name='Guard test' -c user.email='guard@example.invalid' commit -qm 'test: remove dummy private path'
must_reject 'private path in add-then-delete history'
rm -rf "$probe/repo/.git"
git -C "$probe/repo" init -q
cp "$probe/repo/.gitignore" "$probe/gitignore"
sed -i '/^deploy\/t23\.13-glm\.env/d; /^deploy\/\.t23\.13-glm\.env/d' "$probe/repo/.gitignore"
must_reject 'missing Git exclusions'
cp "$probe/gitignore" "$probe/repo/.gitignore"
cp "$probe/repo/.dockerignore" "$probe/dockerignore"
sed -i '/^deploy\/t23\.13-glm\.env/d' "$probe/repo/.dockerignore"
must_reject 'missing Docker exclusions'
cp "$probe/dockerignore" "$probe/repo/.dockerignore"
CI=true bash "$guard"

# Exercise Docker's own ignore parser, including rule ordering/negations.
mkdir -p "$probe/context/deploy"
cp .dockerignore "$probe/context/"
printf 'FROM scratch\nCOPY . /\n' > "$probe/context/Dockerfile"
printf 'positive marker\n' > "$probe/context/public-marker"
for suffix in '' .bak .local '~'; do
  printf 'local-only dummy marker\n' > "$probe/context/deploy/t23.13-glm.env$suffix"
done
printf 'local-only dummy marker\n' > "$probe/context/deploy/.t23.13-glm.env.swp"
build_context() {
  docker build --network none --no-cache --provenance=false \
    --output "type=local,dest=$1" "$probe/context" >"$probe/docker.log" 2>&1
}
build_context "$probe/ignored"
test -f "$probe/ignored/public-marker"
if compgen -G "$probe/ignored/deploy/t23.13-glm.env*" >/dev/null ||
   test -e "$probe/ignored/deploy/.t23.13-glm.env.swp"; then
  echo 'private env self-test: private marker entered Docker context' >&2
  exit 1
fi
sed -i '/^deploy\/t23\.13-glm\.env/d; /^deploy\/\.t23\.13-glm\.env/d' "$probe/context/.dockerignore"
build_context "$probe/unprotected"
for suffix in '' .bak .local '~'; do
  test -f "$probe/unprotected/deploy/t23.13-glm.env$suffix"
done
test -f "$probe/unprotected/deploy/.t23.13-glm.env.swp"
echo 'private env self-test: Git and Docker positive/negative controls passed'
