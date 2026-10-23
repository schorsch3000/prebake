# prebake — design

prebake builds and maintains **base images** for dev-container use, so that the
`apt install` line in a project Dockerfile only has to add the few packages that
are missing. It keeps those bases fresh on a schedule, serves them from a
built-in read-only registry, and refuses broken configuration.

Status: design / brainstorm result. Nothing is implemented yet.

## 1. Problem

Dev containers carry a huge package list so every dev and debug tool is present.
Adding one tool to the `apt-get install` line invalidates the layer and rebuilds
everything, which takes minutes.

Solution: a long-lived base image that already contains (almost) all packages.
A derived Dockerfile repeats the full `apt install -y <30 packages + 3 new>`;
apt skips what is installed, so only the missing packages cost time.

prebake automates keeping those bases built, fresh and bounded in disk usage.

## 2. Non-goals

- No git server (rejected; validation runs as a pre-commit hook and in the daemon).
- No fold-back of extra packages from derived images into base package lists.
- No registry authentication; this is an internal tool.
- No multi-arch (amd64 only in v1).
- No package-manager awareness. The tool only runs commands the user configures.
- No notification target other than Pushover in v1.
- No parallel builds in v1 (config key reserved).

## 3. Concepts

- **Container**: a directory `containers/<name>/` with a `Dockerfile`, a config
  file and everything the build needs.
- **Flavor**: a named set of build args (env vars) for a container. Each flavor is
  built and tagged separately (`<name>:<flavor>`).
- **Rebuild**: a full build without cache (`--no-cache`, `--pull`).
- **Refresh**: a build `FROM` the previous image, running the configured
  `refresh_cmd` (e.g. `apt-get update && apt-get upgrade -y`). Cheaper than a
  rebuild, but layers accumulate; a rebuild resets them.
- **Slot**: a configured time window (e.g. `02:00-06:00`) in which builds run.
- **Candidate**: a freshly built image not yet promoted.
- **Promote**: retag a candidate as `latest` (and flavor alias) after success.

## 4. Repository layout (the "config repo")

All configuration and data come from one git repository:

```
prebake.yaml                 # global config
containers/
  <name>/
    Dockerfile
    container.yaml           # per-container config
    ...                      # everything else the build needs (COPY sources)
```

The repo is **not** this repo. This repo holds the prebake source code.

## 5. Configuration

### 5.1 Bootstrap config (env / flags)

The global config lives in the repo, so the repo location must come from outside:

| Env var           | Flag            | Default   | Meaning                          |
|-------------------|-----------------|-----------|----------------------------------|
| `REPO_URL`        | `--repo-url`    | required  | git URL of the config repo       |
| `REPO_BRANCH`     | `--repo-branch` | `main`    | branch to follow                 |
| `POLL_INTERVAL`   | `--poll`        | `5m`      | how often to fetch               |
| `DATA_DIR`        | `--data-dir`    | `/data`   | state, keys, checkout, registry  |
| `PUSHOVER_TOKEN`  |                 | unset     | Pushover app token               |
| `PUSHOVER_USER`   |                 | unset     | Pushover user/group key          |
| `TLS_CERT`/`TLS_KEY` |              | unset     | cert/key for the read-only listener |
| `KNOWN_HOSTS`     |                 | unset     | pinned known_hosts file (else TOFU) |

Secrets are never read from the git repo. Docker secrets (`*_FILE`) are supported.

### 5.2 Global config `prebake.yaml` (sketch)

```yaml
slots:                 # preferred build windows, local time of the host/TZ env
  - "02:00-06:00"
defaults:
  rebuild_interval: 7d
  refresh_interval: 1d
retry_delay: 15m
keep: 5                # max images (date tags) per container, global ceiling
registry:
  gc_slot_only: true
notify:
  pushover:
    priority_failure: 1
    priority_recovery: -1
```

### 5.3 Container config `containers/<name>/container.yaml` (sketch)

```yaml
refresh_cmd: "apt-get update && apt-get upgrade -y"
refresh_interval: 1d       # optional, overrides default
rebuild_interval: 7d       # optional, overrides default
keep: 3                    # optional; MUST be <= global keep
depends_on: [base]         # optional; parent container names
extra_files: ["scripts/*"] # optional allowlist for non-referenced files
flavors:
  default: {}
  cuda:
    args: { CUDA: "1" }
tests:                     # optional; none is fine (containers are vessels)
  - "git --version"
```

Unknown keys are validation errors (typo protection).

## 6. Scheduling

### 6.1 Due times

For each (container, flavor): `next_rebuild = last_rebuild + rebuild_interval`,
`next_refresh = last_refresh + refresh_interval`. A rebuild resets both clocks.
When both are due, only the rebuild runs.

### 6.2 Slot rule

Jobs run inside slots even when not yet due, if waiting would overshoot the due
time. At slot start `now`:

```
run job if  now >= due   OR   due <= start_of_next_slot
```

i.e. a job runs in the last slot before it becomes due. Equivalently: skip only
if the job will still not be due at the next slot start. A manual trigger runs
immediately, ignoring slots.

If the host was off during a slot, due jobs run at the next slot (catch-up).
Slots that cross midnight (`22:00-02:00`) are supported. Jitter spreads start
times so jobs do not all begin at slot start.

### 6.3 Queue and retries

- One build at a time (global lock). A queue orders due jobs.
- Dependency order: if a parent and a child are both due, the parent runs first,
  the child after it in the same slot if time remains, else next slot.
- A parent being rebuilt does **not** make its children due; children build on
  their own schedule and use the parent's `latest`.
- If a parent fails, its children are skipped and flagged `skipped (parent failed)`.
- On failure the job is re-queued at the **end** of the queue after `retry_delay`
  (default 15 min), silently. A second consecutive failure sends a Pushover
  notification and the job is **parked** until the next push to the config repo
  or a manual trigger (CLI or API).
- Jobs are only started inside a slot unless manually triggered; a retry that
  falls outside the slot waits for the next one.

## 7. Build pipeline

For each job:

1. Verify the checkout validates (see §8). Never build from an invalid tree.
2. Resolve parent reference: build arg `PARENT` (and `PARENT_<NAME>`) is set to
   `<registry>/<parent>:latest` of the internal registry.
3. Build to a local candidate tag `<registry>/<name>:<flavor>-<YYYYmmdd-HHMMSS>-candidate`
   (UTC) via the host Docker (`docker buildx build --load`, amd64).
   - Rebuild: `--no-cache --pull`.
   - Refresh: `FROM <previous alias>`, then `RUN <refresh_cmd>` using a
     generated Dockerfile layered on the previous image (pulled from the
     registry if not in the local store). Without `refresh_cmd`, or without a
     previous image, the job is upgraded to a rebuild and a note is recorded.
4. Run `tests:` commands in a throwaway container from the candidate (if any).
5. Promote: tag and push the date tag `<flavor>-<YYYYmmdd-HHMMSS>` first; only
   after that succeeded, tag and push the moving alias. The alias is `latest`
   for the `default` flavor and the flavor name otherwise. The candidate is
   always removed locally.
6. Record result, digest and timings in state.
7. Run retention (§10) if the job succeeded.

A failed build or test never touches `latest`.

## 8. Validation

A pure, offline library (`internal/validate`) used in three places:

1. `prebake validate` CLI, including as a **pre-commit hook**.
2. The daemon, after every pull and before every build.
3. Optionally in CI or a server-side `pre-receive` hook via `--rev`.

### 8.1 CLI

```
prebake validate [--repo PATH] [--staged | --rev REV | --worktree]
                 [--format text|json] [--strict]
```

- `--worktree` (default): files on disk.
- `--staged`: exports the git index to a temp dir
  (`git checkout-index -a --prefix=<tmp>/`) and validates that, so a partial
  commit is judged as committed.
- `--rev REV`: validates a commit.
- Exit codes: `0` ok, `1` validation errors, `2` usage/internal error.
  Warnings do not fail unless `--strict`.
- Output: `containers/dev/container.yaml:12: unknown key "refrsh_cmd"`.

`prebake install-hook` writes `.git/hooks/pre-commit`; a `.pre-commit-hooks.yaml`
is provided for the pre-commit framework (`pass_filenames: false`,
`always_run: true`).

### 8.2 Checks

- Global and container configs parse, match schema, no unknown keys.
- Slots valid, intervals parse, `keep` per container `<=` global `keep`.
- Every dir under `containers/` has `Dockerfile` and `container.yaml`; dir name is
  a valid image name; no loose files directly under `containers/`.
- Stray files: allowed are `Dockerfile`, `container.yaml`, files referenced by
  `COPY`/`ADD`, and globs from `extra_files`. Anything else is an error.
- `COPY`/`ADD` sources exist and do not escape the container directory.
- `depends_on` targets exist; no cycles.
- Flavor names valid; tests syntactically non-empty.
- Warn (not error) if there is no `tests:`; optional `hadolint` if on `PATH`.

## 9. Registry

Embedded `distribution/distribution` (chosen over go-containerregistry because
retention needs delete and GC).

- **Read-only listener** (external, default `:5000`): only `GET`/`HEAD` on
  `/v2/...`; everything else returns 405. No auth. Optional TLS from
  `TLS_CERT`/`TLS_KEY`, reloaded on file change (`GetCertificate` with mtime
  check). Without a cert it serves plain HTTP and logs a warning.
- **Write listener** (internal, default `:5001`): plain HTTP, no auth, used only
  by the daemon's builds. In compose it is published as `127.0.0.1:5001:5001`,
  because the **host's** dockerd performs the push and must reach it.
  `localhost` registries are treated as insecure-OK by Docker. Inside the
  container the listener binds all interfaces (a loopback-only bind would be
  unreachable through the port mapping); the loopback restriction is the
  `127.0.0.1:` prefix of the published port, so never publish it without it.
- Storage in `DATA_DIR/registry` (filesystem driver).

## 10. Retention and GC

- Global `keep: N` (ceiling) and optional per-container `keep: M`, validated
  `M <= N`. Effective keep is the container value if set, else the global value.
- An "image" is a date-stamped tag; `latest` and flavor aliases do not count.
- Never pruned: the date tag the alias currently points to (the promoted tag),
  even if it is not among the newest; so a flavor can briefly hold more than
  `keep` tags. A dependent image's parent tag needs no protection: an image
  manifest is self-contained (its layers are blobs referenced by the child
  itself), so deleting the parent's tag does not break the child.
- Selection is per flavor on the date tag's timestamp; aliases and any
  non-date tags are never selected (`internal/retention`).
- After a successful build: delete manifests beyond the effective keep (oldest
  first), per container and flavor.
- Blob GC runs while holding the build lock; external writes are impossible and
  the daemon serializes its own pushes, so it is safe. GC runs in a slot.

## 11. Daemon, git sync and SSH identity

- On first boot generate an ed25519 key in `DATA_DIR/ssh/` (mode 0600). Never
  regenerate an existing key.
- Show the public key (with fingerprint) in the startup log as a ready-to-paste
  deploy key line, at `GET /pubkey`, and via `prebake show-key`.
- Until the key is accepted, retry with backoff; status shows
  `waiting for deploy key`. No crash loop.
- Host key checking stays on: pinned `KNOWN_HOSTS` if given, otherwise
  trust-on-first-use recorded in `DATA_DIR/ssh/known_hosts`, failing on change.
- Implementation shells out to `git` using `GIT_SSH_COMMAND`. Image needs `git`
  and `openssh-client`.
- Poll `POLL_INTERVAL`; a changed HEAD triggers validation. If invalid: keep the
  last good checkout, status `degraded`, Pushover notification. A valid new
  HEAD unparks parked jobs.

## 12. Notifications (Pushover)

`PUSHOVER_TOKEN` and `PUSHOVER_USER` via env/secret. Events: build failed twice
(parked), test failed, repo invalid after pull, prune/GC failure, recovery.
Notify on state change only, never per retry. Egress: `api.pushover.net`.

## 13. API, CLI and status

Status listener (default `:8080`, read-only except trigger, no auth):

- `GET /` HTML overview, `GET /status` JSON (per container/flavor: last
  rebuild/refresh, next due, state: ok / queued / building / retrying / parked /
  skipped / degraded, digest, last error).
- `GET /pubkey`.
- `POST /trigger/<container>[/<flavor>]?kind=rebuild|refresh` queues a job now
  and unparks it.

CLI: `prebake daemon`, `validate`, `install-hook`, `show-key`, `status`,
`trigger`, `dry-run` (what is due when), `gc`.

## 14. Persistence

SQLite in `DATA_DIR/state.db`: job history, last results, digests, next-due,
parked/retry state. Logs go to stdout; build logs are kept per job (bounded).

## 15. Deployment

`docker-compose.yaml` that works out of the box:

- service `prebake` running the daemon; mounts `/var/run/docker.sock`, a
  `prebake-data` volume at `/data`.
- ports: `5000:5000` (read-only registry), `127.0.0.1:5001:5001` (write),
  `8080:8080` (status).
- env: `REPO_URL` (required), optional Pushover and TLS.
- Docker socket access is root-equivalent on the host; an optional compose
  profile puts `docker-socket-proxy` in front, allowing only build/image/push.

## 16. Implementation

- Go, single static binary, `docker` CLI via `exec` for builds.
- Packages: `config`, `validate`, `schedule`, `build`, `registry`, `gitsync`,
  `state`, `notify`, `api`, `cmd/prebake`.
- Unit tests for validator, schedule math and retention selection are required;
  they are pure and do not need Docker.

## 17. Milestones

| M  | Goal                                                              |
|----|-------------------------------------------------------------------|
| M0 | Project skeleton, Go module, CI-less lint/test targets            |
| M1 | Config schema + `validate` CLI + pre-commit hook                  |
| M2 | Scheduler core (due math, slots, queue, retries) with tests       |
| M3 | Build pipeline against a local registry (rebuild/refresh/promote) |
| M4 | Embedded registry: read-only + write listeners, TLS reload        |
| M5 | Retention + GC                                                    |
| M6 | Git sync, SSH identity, daemon wiring, dependency ordering        |
| M7 | Status/API, Pushover, compose + docs                              |

## 18. Open points

- Embedding `distribution/distribution` verified, see docs/spikes/m4-registry-embedding.md; pin the version.
- Timezone for slots: host `TZ`; make explicit in config if needed.
- Disk-pressure-triggered prune: not in v1.
