#!/usr/bin/env bash
# End-to-end test of the compose setup with real Docker: the daemon follows a
# local bare repo, builds a container and a dependent child, serves them from the
# read-only registry, and picks up a later push. Run from the repo root.
set -euo pipefail

origin=${CI_ORIGIN:-/tmp/ci-origin.git}
work=$(mktemp -d)
export CI_ORIGIN=$origin REPO_URL=/origin.git
compose=(docker compose -f docker-compose.yaml -f docker-compose.ci.yaml)
status=http://localhost:8080/status

dump_logs() { "${compose[@]}" logs --no-color prebake | tail -n 150 || true; }
cleanup() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "--- daemon logs ---"
    dump_logs
  fi
  "${compose[@]}" down -v >/dev/null 2>&1 || true
  exit "$rc"
}
trap cleanup EXIT

git() { command git -c user.name=ci -c user.email=ci@example.com "$@"; }

rm -rf "$origin"
git init -q --bare -b main "$origin"
git clone -q "$origin" "$work/w" 2>/dev/null
mkdir -p "$work/w/containers/ci-demo"
cat > "$work/w/prebake.yaml" <<'YAML'
slots: ["00:00-23:59"]
keep: 2
retry_delay: 1m
YAML
printf 'FROM busybox:stable\nRUN echo base > /base\n' > "$work/w/containers/ci-demo/Dockerfile"
printf 'tests:\n  - "test -f /base"\n' > "$work/w/containers/ci-demo/container.yaml"
git -C "$work/w" add -A
git -C "$work/w" commit -q -m "ci-demo"
git -C "$work/w" push -q origin HEAD:main

"${compose[@]}" up -d --build

# wait_for <seconds> <description> <command...>
wait_for() {
  local limit=$1 what=$2
  shift 2
  for _ in $(seq 1 "$limit"); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "timed out waiting for: $what" >&2
  return 1
}

job_built() { curl -fsS "$status" | jq -e --arg c "$1" '.jobs[] | select(.container == $c) | .state == "idle" and .last_rebuild != null'; }

wait_for 60 "status API" curl -fsS "$status"
wait_for 300 "ci-demo to be built" job_built ci-demo

echo "== registry: read-only side serves, refuses writes"
curl -fsS http://localhost:5000/v2/ci-demo/tags/list | jq -e '.tags | index("latest")' >/dev/null
test "$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:5000/v2/x/blobs/uploads/)" = 405
test "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE http://localhost:5000/v2/ci-demo/manifests/latest)" = 405

echo "== the built image can be pulled and run through the read-only listener"
docker image rm -f localhost:5001/ci-demo:latest >/dev/null 2>&1 || true
docker pull -q localhost:5000/ci-demo:latest
test "$(docker run --rm localhost:5000/ci-demo:latest cat /base)" = base

echo "== CLI inside the container"
"${compose[@]}" exec -T prebake prebake status
"${compose[@]}" exec -T prebake prebake dry-run
curl -fsS http://localhost:8080/pubkey | grep -q '^ssh-ed25519 '

echo "== a push adds a child container that is built after its parent via PARENT"
mkdir -p "$work/w/containers/ci-child"
# shellcheck disable=SC2016 # ${PARENT} is meant literally, it is expanded by Docker
printf 'ARG PARENT\nFROM ${PARENT}\nRUN echo child > /child\n' > "$work/w/containers/ci-child/Dockerfile"
printf 'depends_on: [ci-demo]\ntests:\n  - "test -f /base && test -f /child"\n' > "$work/w/containers/ci-child/container.yaml"
git -C "$work/w" add -A
git -C "$work/w" commit -q -m "ci-child"
git -C "$work/w" push -q origin HEAD:main
wait_for 300 "ci-child to be built" job_built ci-child
docker pull -q localhost:5000/ci-child:latest
test "$(docker run --rm localhost:5000/ci-child:latest cat /child)" = child

echo "== an invalid push is rejected and the last good commit stays active"
active=$(curl -fsS "$status" | jq -r .repo.active)
echo stray > "$work/w/containers/ci-demo/stray.txt"
git -C "$work/w" add -A
git -C "$work/w" commit -q -m "stray file"
git -C "$work/w" push -q origin HEAD:main
wait_for 60 "repo to turn degraded" bash -c "curl -fsS '$status' | jq -e '.repo.state == \"degraded\"'"
test "$(curl -fsS "$status" | jq -r .repo.active)" = "$active"

echo "== manual trigger and GC are accepted"
test "$(curl -s -o /dev/null -w '%{http_code}' -X POST 'http://localhost:8080/trigger/ci-demo?kind=refresh')" = 202
test "$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:8080/gc)" = 202
test "$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:8080/trigger/nope)" = 404

echo "e2e ok"
