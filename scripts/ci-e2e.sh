#!/usr/bin/env bash
# End-to-end test of the compose setup with real Docker: the daemon follows a
# local bare repo, builds a container and a dependent child, serves them from the
# read-only registry, and picks up a later push. Run from the repo root.
set -euo pipefail

origin=${CI_ORIGIN:-/tmp/ci-origin.git}
work=$(mktemp -d)
gitconfig=$work/gitconfig
printf '[safe]\n\tdirectory = *\n' > "$gitconfig"
# A host time zone that differs from the runner's (UTC), mounted as /etc/localtime, to prove
# that the daemon takes its zone from the host mount when TZ is not set.
unset TZ
cp /usr/share/zoneinfo/Asia/Tokyo "$work/localtime"
export CI_ORIGIN=$origin CI_GITCONFIG=$gitconfig REPO_URL=/origin.git LOCALTIME_FILE=$work/localtime
# PREBAKE_PROXY=1 runs the daemon behind the filtering docker socket proxy: that is a
# standalone compose file, used instead of docker-compose.yaml. The CI file is merged on top.
if [ "${PREBAKE_PROXY:-}" = 1 ]; then
  compose=(docker compose -f docker-compose.proxy.yaml)
  export PROXY_LOGLEVEL=debug   # log allowed requests too, to see what the daemon asks for
else
  compose=(docker compose -f docker-compose.yaml)
fi
# the image under test is always built from this checkout, never the published one
compose+=(-f docker-compose.build.yaml -f docker-compose.ci.yaml)
status=http://localhost:8080/status

dump_logs() {
  "${compose[@]}" logs --no-color prebake | tail -n 150 || true
  if [ "${PREBAKE_PROXY:-}" = 1 ]; then
    echo "--- docker-proxy: blocked requests ---"
    "${compose[@]}" logs --no-color docker-proxy | grep -i 'block\|forbidden\|denied\|not allowed' | tail -n 40 || true
    echo "--- docker-proxy: log tail ---"
    "${compose[@]}" logs --no-color docker-proxy | tail -n 120 || true
    echo "--- docker as seen by the daemon container ---"
    # shellcheck disable=SC2016 # expanded by the shell inside the container, not here
    "${compose[@]}" exec -T prebake sh -c 'echo DOCKER_HOST=$DOCKER_HOST; docker version 2>&1 | head -20; echo "-- trivial build:"; echo "FROM scratch" | docker buildx build --progress=plain -f - /tmp >/tmp/diag.out 2>&1; echo "exit=$?"; tail -20 /tmp/diag.out; echo "-- docker images:"; docker images 2>&1 | head -5; echo "-- buildx ls:"; docker buildx ls 2>&1 | head -8; echo "-- buildx version:"; docker buildx version 2>&1' || true
  fi
}
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

echo "== time zone comes from the mounted /etc/localtime"
curl -fsS "$status" | jq -e '.time_zone | endswith("+09:00")' >/dev/null
test "$("${compose[@]}" exec -T prebake date +%z)" = +0900
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

if [ "${PREBAKE_PROXY:-}" = 1 ]; then
  echo "== behind the socket proxy: images work, containers are out of reach"
  "${compose[@]}" exec -T prebake docker version --format '{{.Server.Version}}' >/dev/null
  if "${compose[@]}" exec -T prebake docker ps >/dev/null 2>&1; then
    echo "the daemon can list containers through the proxy" >&2
    exit 1
  fi
  if "${compose[@]}" exec -T prebake docker run --rm busybox:stable true >/dev/null 2>&1; then
    echo "the daemon can start containers through the proxy" >&2
    exit 1
  fi
  if "${compose[@]}" exec -T prebake test -S /var/run/docker.sock; then
    echo "the docker socket is mounted into the daemon container" >&2
    exit 1
  fi
fi

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
