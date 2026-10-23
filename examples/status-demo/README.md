# status demo

A canned `/status` answer with every state the CLI can show (idle, building, queued, retrying,
parked, skipped, a degraded repo, a never-built flavor), to try `prebake status` and
`prebake dry-run` (colors, `--watch`, `--json`) without a daemon:

```sh
cd examples/status-demo
python3 -m http.server 18099 --bind 127.0.0.1 &        # serves ./status as /status

export PREBAKE_URL=http://127.0.0.1:18099
prebake status
prebake status --color always | less -R
prebake dry-run
prebake status --json | jq '.jobs[] | {container, state}'
NO_COLOR=1 prebake status                              # plain
kill %1
```

Times are fixed (the answer says it was generated at 2026-10-25 12:00 UTC, which is what
`dry-run` counts from). `--watch` redraws the same page every interval; stop the server to see
"cannot reach the daemon, retrying".

`go test ./cmd/prebake` renders this file as well, so it stays valid.
