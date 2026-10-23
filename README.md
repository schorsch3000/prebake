# prebake

Dev containers carry a huge package list so that every dev and debug tool is
there. Add one tool to the `apt-get install` line and the whole image rebuilds,
which takes minutes.

prebake keeps **base images** built and fresh for you, so that the `apt install`
line of a derived image only has to add the few packages that are missing:

```dockerfile
ARG PARENT
FROM ${PARENT}                      # <registry>/base:latest, set by prebake
RUN apt-get update && apt-get install -y <the same 30 packages + the 3 new ones>
```

apt skips what the base already contains, so this takes seconds.

A daemon follows a git repo that describes your containers, rebuilds and refreshes
them on a schedule inside build windows (for example 02:00-06:00), serves the
results from a built-in **read-only** registry, prunes old images, and tells you
through Pushover when something is stuck. It refuses to build from a repo that
does not validate.

Design and rationale: [docs/design.md](docs/design.md).

## Quick start

You need Docker with Compose on the host, and a git repo with the layout below
(start from [examples/config-repo](examples/config-repo)).

```sh
git clone https://github.com/schorsch3000/prebake && cd prebake
export REPO_URL=git@github.com:you/your-prebake-config.git
docker compose up -d --build
docker compose logs prebake | grep ssh-ed25519     # the deploy key, or: docker compose exec prebake prebake show-key
```

Add that key as a **read-only deploy key** to the config repo. Until it is
accepted the status page shows `waiting-for-deploy-key`; the daemon retries on
its own. Then:

```sh
docker compose exec prebake prebake status      # what is built, what is parked
docker compose exec prebake prebake dry-run     # what is due when
```

The status page is on <http://localhost:8080>.

Ports: `5000` read-only registry (everyone), `127.0.0.1:5001` write listener
(only the host's Docker daemon, never publish it without the `127.0.0.1:`),
`8080` status page and control API.

## The config repo

```
prebake.yaml                  # global config
containers/
  <name>/
    Dockerfile
    container.yaml            # per-container config
    ...                       # everything the Dockerfile COPYs
```

`prebake.yaml`

| key | default | meaning |
|---|---|---|
| `slots` | `["02:00-06:00"]` | build windows in the daemon's `TZ`; may cross midnight |
| `defaults.rebuild_interval` | `7d` | full rebuild (`--no-cache --pull`); units `s m h d w` |
| `defaults.refresh_interval` | `1d` | cheap refresh: previous image plus `refresh_cmd` |
| `retry_delay` | `15m` | wait before the single retry of a failed job |
| `keep` | `5` | date-tagged images kept per container and flavor (ceiling) |
| `registry.gc_slot_only` | `true` | garbage-collect only inside a slot |
| `notify.pushover.priority_failure` / `priority_recovery` | `1` / `-1` | Pushover priority, -2..1 |

`containers/<name>/container.yaml`

| key | meaning |
|---|---|
| `refresh_cmd` | run on top of the previous image, e.g. `apt-get update && apt-get upgrade -y`. Without it a refresh becomes a rebuild |
| `rebuild_interval`, `refresh_interval` | override the defaults |
| `keep` | per-container keep, must not exceed the global `keep` |
| `depends_on` | parent containers; they are built first, and `ARG PARENT` receives `<registry>/<parent>:latest` (also `PARENT_<NAME>`) |
| `flavors` | `name: {args: {K: V}}`, one image per flavor, build args from `args` |
| `tests` | shell commands (`sh -c`) run on the candidate image as `RUN` steps of a throwaway build, so no container is started; any failure stops promotion |
| `extra_files` | globs for files the Dockerfile does not `COPY` but that may stay in the directory |

Unknown keys are errors. Stray files in a container directory (not referenced
by the Dockerfile, not in `extra_files`) are errors too.

### Schedule rules

- Jobs start only inside a slot. A job that would become due before the next
  slot starts runs in the current one, so images are never later than due.
- A rebuild resets the refresh clock; when both are due only the rebuild runs.
- One build at a time. A parent is built before its child when both are due. A
  parent build does not make the child due; children use the parent's `latest`.
- A failed job is retried once after `retry_delay` (silently). A second failure
  sends a notification and parks the job until the next valid push or a manual
  trigger. Children of a parked parent are skipped.

### Validate before you push

```sh
prebake validate                  # files on disk
prebake validate --staged         # what is about to be committed
prebake validate --rev origin/main
prebake install-hook              # runs `validate --staged` as a pre-commit hook
```

Exit codes: 0 ok, 1 validation errors, 2 usage error. `--strict` fails on
warnings, `--format json` for tooling. For the pre-commit framework use
`.pre-commit-hooks.yaml` (id `prebake-validate`). The daemon runs the same
validation after every pull: an invalid commit never replaces the last good one.

## Using the images

For a container `dev` the registry has `dev:latest` (default flavor),
`dev:<flavor>` for other flavors, and immutable date tags
`<flavor>-YYYYmmdd-HHMMSS`.

```sh
docker pull yourhost:5000/dev:latest
```

Docker only talks plain HTTP to `localhost`. For other hosts either give the
registry a certificate (`TLS_CERT` and `TLS_KEY`, mounted into the container;
it is reloaded when the files change), add the CA to
`/etc/docker/certs.d/yourhost:5000/ca.crt` on the clients, or list the host in
`insecure-registries` in their `daemon.json`.

## Operating it

```sh
prebake status [--json]
prebake dry-run
prebake trigger <container>[:<flavor>] [--kind rebuild|refresh]
prebake gc
prebake show-key
```

The client commands talk to the status API (`--url` or `PREBAKE_URL`, default
`http://127.0.0.1:8080`); in Compose run them with `docker compose exec prebake prebake ...`.
API: `GET /status` (JSON), `GET /pubkey`, `POST /trigger/<container>[/<flavor>]?kind=...`, `POST /gc`.

### Notifications

Set `PUSHOVER_TOKEN` and `PUSHOVER_USER` (or `PUSHOVER_TOKEN_FILE` /
`PUSHOVER_USER_FILE` for Docker secrets). Events: a job parked after two
failures (or a failing smoke test), a pushed commit that fails validation,
pruning or GC failing, and recovery. Notifications are sent on state changes
only, not for every retry.

## Configuration of the daemon

| variable | default | |
|---|---|---|
| `REPO_URL` | required | config repo URL |
| `REPO_BRANCH` | `main` | |
| `POLL_INTERVAL` | `5m` | how often to fetch |
| `DATA_DIR` | `/data` | state, SSH key, checkouts, registry storage |
| `READ_ADDR` / `WRITE_ADDR` / `STATUS_ADDR` | `:5000` / `:5001` / `:8080` | listeners |
| `REGISTRY_HOST` | `localhost:<write port>` | where the Docker host pushes |
| `TLS_CERT` / `TLS_KEY` | unset | TLS for the read-only listener |
| `KNOWN_HOSTS` | unset | pinned known_hosts; default is trust on first use |
| `JITTER` | `15m` | random start offset after a slot opens, `0` disables |
| `TZ` | `UTC` | time zone of the slots |
| `VERBOSE` | unset | log every registry request |

Flags override variables; see `prebake daemon --help`.

## Security

- The daemon needs Docker to build. With the plain compose file that means the
  Docker socket, which is **root-equivalent on the host**.
- To avoid that, use the socket proxy variant:

  ```sh
  docker compose -f docker-compose.proxy.yaml up -d --build
  docker compose -f docker-compose.proxy.yaml down
  ```

  The daemon then has no socket; it talks to
  [wollomatic/socket-proxy](https://github.com/wollomatic/socket-proxy)
  (pinned to 1.13.1 by digest) over an internal network, and the proxy only lets
  through what building and publishing images needs: build, pull, tag, push,
  remove. It cannot create containers or exec. This only works because smoke
  tests run as build steps: the proxy filters by URL and method, not by request
  body, so allowing container creation would allow privileged containers. The
  popular HAProxy-based Tecnativa proxy does not work: it drops the `h2c` upgrade
  that buildx uses, see the comments in `docker-compose.proxy.yaml`. Needs
  Docker Compose 2.20 or newer. CI runs the end-to-end test in both modes.
  The two compose files are standalone; use one or the other, and put
  `COMPOSE_FILE=docker-compose.proxy.yaml` into `.env` if you want plain
  `docker compose up` and `down` to use it. When you switch from one to the
  other, run `down` with the old file first (the data volume is kept).
  Note that the build itself still runs the Dockerfiles of your config repo, so
  only point prebake at repos you trust.
- The status/control API and the read-only registry have **no authentication**;
  keep them on an internal network. Cross-origin browser POSTs to the API are
  refused. Anyone who can reach it can trigger builds.
- The write listener has no authentication either. That is why compose
  publishes it on the host's loopback only.
- Use a **read-only** deploy key. Secrets (Pushover) come from the environment or
  Docker secrets, never from the config repo.

## Development

```sh
make test                  # go test ./...
make vet lint
make test-integration      # needs Docker and a registry on localhost:5001
scripts/ci-e2e.sh          # full compose run against a local repo, needs Docker
```

CI (`.github/workflows/ci.yml`) runs unit tests with `-race`, the Docker
integration tests, the image build, and the compose end-to-end script. Work is
tracked with [git-bug](https://github.com/git-bug/git-bug) in the repo.
