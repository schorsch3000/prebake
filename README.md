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

You need Docker with Compose on the host and a git repo with the layout below
(start from [examples/config-repo](examples/config-repo)). The image is published
on GitHub, nothing has to be built:

```sh
mkdir prebake && cd prebake
curl -fsSLO https://raw.githubusercontent.com/schorsch3000/prebake/main/docker-compose.yaml
curl -fsSL  https://raw.githubusercontent.com/schorsch3000/prebake/main/example.env -o .env
$EDITOR .env                    # every setting with its default; set at least REPO_URL
docker compose up -d
docker compose logs prebake | grep ssh-ed25519     # the deploy key, or: docker compose exec prebake prebake show-key
```

(For the socket proxy variant fetch `docker-compose.proxy.yaml` instead, see
"Security".)

Add that key as a **read-only deploy key** to the config repo. Until it is
accepted the status page shows `waiting-for-deploy-key`; the daemon retries on
its own. Then:

```sh
docker compose exec prebake prebake status      # what is built, what is parked
docker compose exec prebake prebake dry-run     # what is due when
```

The status page is on <http://localhost:8080> (or the port you set, see below).

Ports: `5000` read-only registry (everyone), `127.0.0.1:5001` write listener
(only the host's Docker daemon, never publish it without the `127.0.0.1:`),
`8080` status page and control API. If those are taken, set `PREBAKE_READ_PORT`,
`PREBAKE_WRITE_PORT` and `PREBAKE_STATUS_PORT` in `.env`; that is all, the daemon
is told where the Docker host pushes (`REGISTRY_HOST`) automatically. For the CLI on
the host use `PREBAKE_URL=http://127.0.0.1:<status port>`.

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
| `root_files` | none | extra files or directories (globs) allowed at the repo root; see below |
| `slots` | `["02:00-06:00"]` | build windows in the daemon's time zone (the host's, see `TZ`); may cross midnight |
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

Unknown keys are errors. Stray files are errors too, in three places:
- in a container directory: files that the Dockerfile does not `COPY`/`ADD` and that are not listed in `extra_files`
  (with `COPY .` everything is copied, so nothing can be stray there, and `validate` warns about that);
- directly under `containers/`: only container directories belong there;
- at the repo root: only `prebake.yaml`, `containers/` and repository furniture (`README*`, `LICENSE*`, `COPYING*`,
  `NOTICE*`, `.gitignore`, `.gitattributes`, `.editorconfig`, `.pre-commit-config.yaml`, `.github/`, `.gitlab-ci.yml`).
  Anything else is listed in the error; if it is meant to be there, add it to `root_files:` in `prebake.yaml`, e.g.
  `root_files: ["docs", "notes.md"]` (a directory allows everything below it).

### Schedule rules

- **Slots are a preference, never a reason to leave an image missing or outdated.**
  What is needed now is built now, inside a slot or not, in this order: first what is
  *absent* (never built, or missing from the registry, e.g. after the registry volume
  was wiped), then what is *stale against git* (the Dockerfile, a copied file, build
  args or `depends_on` of a container changed in the active commit; changes to only
  `tests`, `keep`, intervals or `refresh_cmd` do not count), then what is *due*
  (an interval ran out, a slot was missed, the host was down). So a new container
  pushed to the repo, or a changed Dockerfile, is built immediately, and so is the
  backlog after a restart.
- Only work that is *not yet due* waits for a slot: a job runs early in the last slot
  before it becomes due, so images are never late. Jitter spreads those early starts.
- A rebuild resets the refresh clock; when both are due only the rebuild runs.
- One build at a time. A parent is built before its child when both are due. A
  parent build does not make the child due; children use the parent's `latest`.
- A failed job is retried once after `retry_delay` (silently; anywhere, if it is needed now). A second failure
  sends a notification and parks the job until the next valid push or a manual
  trigger. Children of a parked parent are skipped.

### Validate before you push

```sh
prebake validate                  # files on disk
prebake validate --staged         # what is about to be committed
prebake validate --rev origin/main
prebake install-hook              # runs `validate --staged` as a pre-commit hook
prebake install-hook --sync --url http://prebake.example:8080   # also: tell the daemon to fetch after every push
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
prebake status [--json] [--watch]
prebake logs <container>[:<flavor>] [-f] [--tail N]
prebake dry-run
prebake sync [--commit SHA|HEAD] [--branch NAME]
prebake trigger <container>[:<flavor>] [--kind rebuild|refresh]
prebake gc
prebake show-key
```

The client commands talk to the status API (`--url` or `PREBAKE_URL`, default
`http://127.0.0.1:8080`); in Compose run them with `docker compose exec prebake prebake ...`.
API: `POST /sync[?commit=SHA&branch=NAME]` (fetch now), `GET /status` (JSON), `GET /logs/<container>[/<flavor>][?follow=1&tail=N]` (build output, streamed), `GET /pubkey`, `POST /trigger/<container>[/<flavor>]?kind=...`, `POST /gc`.

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
| `TZ` | host's zone | time zone of the slots. Unset, the compose files mount the host's `/etc/localtime` read-only (`LOCALTIME_FILE` to point elsewhere); `TZ` overrides it. `prebake status` and the status page show the zone in use |
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

## The command line tool

Everything except `daemon` also works standalone on your own machine: validate
the config repo before pushing, install the pre-commit hook, query a running
daemon. Release archives for linux (amd64, arm64), macOS (amd64, arm64) and
windows (amd64) are on the [releases page](https://github.com/schorsch3000/prebake/releases),
with a `checksums.txt`:

```sh
v=0.1.0 os=linux arch=amd64
curl -fsSLO https://github.com/schorsch3000/prebake/releases/download/v$v/prebake_${v}_${os}_${arch}.tar.gz
curl -fsSLO https://github.com/schorsch3000/prebake/releases/download/v$v/checksums.txt
sha256sum -c --ignore-missing checksums.txt
tar -xzf prebake_${v}_${os}_${arch}.tar.gz && sudo install prebake_${v}_${os}_${arch}/prebake /usr/local/bin/
prebake version
```

### Shell completion, color, live view

```sh
source <(prebake completion bash)                       # bash; add it to ~/.bashrc
prebake completion zsh  > "${fpath[1]}/_prebake"        # zsh
prebake completion fish > ~/.config/fish/completions/prebake.fish
```

Subcommands, flags and flag values complete, and `prebake trigger <TAB>` offers the
containers and flavors of the daemon you are talking to (`dev`, `dev:gpu`). The bash script is tested;
the zsh and fish scripts are generated from the same table but only checked for
well-formedness.

`prebake status` and `dry-run` color their output when stdout is a terminal: green is
fine, yellow is in motion or waiting, red needs attention. `NO_COLOR` or `TERM=dumb`
switch it off, `--color always|never|auto` overrides.

`prebake status` shows the version of the daemon it talks to (`daemon: prebake 0.2.1 (4b273f3)`, also in `/status` and the web page footer), and
warns when the CLI and the daemon differ in major or minor version (patch releases never warn; `edge` or development builds cannot be compared).

`status` has a NEEDS column and `dry-run` a WHY column that say why work is pending, in the terms of the
schedule rules: `never built`, `missing from the registry`, `build inputs changed in git`, `refresh interval over` /
`rebuild interval over` (a missed slot or downtime), or `early start: the last slot before it falls due`. Work that is
needed now shows `now` in `dry-run`, whatever the slot says; `on schedule` means nothing is pending and the next run is
just the planned one. The reasons are refreshed by the scheduler pass (every 30 seconds and after each build), not on every request.

`prebake status --watch` (or `-w`, with `-n 1s` for the interval, default 2s) is a live
view that redraws in place until Ctrl-C, shows how long a running build has been going, and
keeps going when the daemon restarts. It needs a terminal; in scripts, loop around plain
`prebake status`.

**Fetch now.** The daemon polls the config repo every `POLL_INTERVAL` (5 minutes by default).
`prebake sync` makes it fetch, validate and activate right now and reports the result (`synced: running
1a2b3c4 on main (new commit activated)`, or why the commit was rejected). Typical use right after a push:
`git push && prebake sync --commit HEAD`: with `--commit` it asks again every 2 seconds until the daemon
has seen that commit (up to `--timeout`, default 2m), so it does not matter that the push is still arriving.
`--branch` makes the daemon ignore pushes of other branches than the one it follows.

`prebake install-hook --sync` automates that with a `pre-push` hook: git has no hook that runs after
the push, so the hook starts `prebake sync --commit <pushed commit>` in the background, which waits for the commit
to show up and then makes the daemon fetch. The push is never delayed and never fails because of it, tags and
deleted branches are skipped, and without `prebake` in the PATH it does nothing. The hook finds the daemon through
`$PREBAKE_URL` or `git config prebake.url` (`--url` sets that for the repo; so does `git config prebake.url ...`).
This also works as a "refresh" on any client command: the URL falls back to `git config prebake.url` in the current
repository.

`prebake logs dev` prints the build output of the build in progress, or of the last one (kept
over restarts). `prebake logs -f dev` follows a running build until it ends and waits for a queued
one to start, so `prebake trigger dev && prebake logs -f dev` just works; Ctrl-C leaves it. `--tail 50`
shows only the last lines. Build output can contain whatever your Dockerfiles print, and the API
has no authentication, like the rest of it.

`prebake daemon` is the service itself, what the container image runs; the other commands are
the client. You normally never run `daemon` on your workstation.

CI only tests the linux build; the macOS and Windows binaries are built but not
exercised (the daemon itself needs Linux and Docker anyway). The same binary is
inside the container image.

## Images and releases

CI publishes `ghcr.io/schorsch3000/prebake` (linux/amd64) once the whole pipeline
is green:

| ref | tags |
|---|---|
| push to `main` | `edge`, `sha-<short commit>` |
| tag `v1.2.3` | `1.2.3`, `1.2`, `latest` |

The compose files use `latest` by default; set `PREBAKE_IMAGE` in `.env` to pin
a version or a digest (`ghcr.io/schorsch3000/prebake:1.2.3`,
`ghcr.io/schorsch3000/prebake@sha256:...`). Upgrade with `docker compose pull && docker compose up -d`.
`prebake version` shows what is running. To cut a release: `git tag v0.1.0 && git push origin v0.1.0`.
That publishes the image tags above and creates a GitHub release with the CLI archives.

The release workflow (`.github/workflows/release.yml`) does not run the tests again. It
requires that the `ci` workflow passed for exactly the tagged commit (it waits for a ci run that is
still going, and stops the release if there is none or it failed), and then builds the image and
the CLI archives in parallel, from the caches of the ci run. So: merge to `main`, tag that commit, and a
release takes a few minutes.

The first publish creates the package as private. Make it public once (GitHub,
your profile, Packages, `prebake`, Package settings, Change visibility) or
`docker compose pull` needs a `docker login ghcr.io`.

To run your own build instead of the published image:

```sh
docker compose -f docker-compose.yaml -f docker-compose.build.yaml up -d --build
```

## Development

```sh
make test                  # go test ./...
make vet lint
make test-integration      # needs Docker and a registry on localhost:5001
scripts/ci-e2e.sh          # full compose run against a local repo, needs Docker
```

CI (`.github/workflows/ci.yml`) runs, in parallel, lint and static build, the unit tests with `-race`,
the Docker integration tests, a dry run of the release archives and the image build, then the
compose end-to-end script in both modes. The Go build caches and the image layers are cached between
runs (`.github/actions/go-cache`, the Docker layer cache), and the image is built once and reused by the
end-to-end jobs and the publish step. Work is
tracked with [git-bug](https://github.com/git-bug/git-bug) in the repo.

## License

prebake is licensed under the [Apache License 2.0](LICENSE). The release archives and the container
image contain the license text; the image also carries it as `org.opencontainers.image.licenses`.
The libraries it is built from have their own licenses (for example the Docker `distribution`
registry is Apache-2.0, `golang.org/x/crypto` and the pure-Go SQLite driver are BSD-3-Clause); the
container image additionally contains Debian packages, the Docker CLI and git.
