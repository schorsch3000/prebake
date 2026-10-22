# prebake

Go tool that builds and refreshes base images for dev containers on a schedule,
serves them from an embedded read-only registry and validates its config repo.

Design: `docs/design.md` (source of truth; update it when decisions change).

## Conventions

- Language: Go, single binary `prebake` (`cmd/prebake`), internal packages under `internal/`.
- Config format: YAML; unknown keys are errors.
- `internal/validate` is pure and offline (no Docker, no network); keep it that way.
- Builds use the host Docker via the `docker` CLI; amd64 only.
- Registry: embedded `distribution/distribution`; read-only external listener, write listener on loopback only.
- Secrets (Pushover) come from env or Docker secrets, never from the config repo.
- Tests: schedule math, validator and retention selection need unit tests and must not need Docker.
- Run `go vet ./...` and `go test ./...` before declaring work done.

## Workflow

- Work on feature branches, never on `main`/`master`.
- Tasks live in git-bug, grouped by milestone label (`milestone:M0` ... `milestone:M7`, see design §17).
- Reference the git-bug ID in commit messages.
- `.az/`, `Dockerfile.az`, `Dockerfile.az.dockerignore`, `aicatraz.yaml` configure the dev container; ignore them.

## Environment notes

- Go and the Go module proxy may not be available in the dev container; ask the human before working around it.
