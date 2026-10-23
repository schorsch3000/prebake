# Spike: embedding distribution/distribution (M4)

Result: embedding works. No child process needed.

Tested with `github.com/distribution/distribution/v3 v3.1.2`, Go 1.27.2.

## Findings

- `handlers.NewApp(ctx, *configuration.Configuration)` returns an `http.Handler`
  (the full `/v2/` API). We run our own `http.Server`s around it, so TLS, listeners
  and middleware are ours. No need for `registry.NewRegistry(...).ListenAndServe()`.
- Two listeners over the same handler:
  - write: `http.ListenAndServe("127.0.0.1:5001", app)`
  - read-only: `http.ListenAndServe(":5000", readOnly(app))`, where `readOnly`
    answers 405 (`Allow: GET, HEAD`) for any other method.
  Verified: `GET /v2/` 200, `POST /v2/x/blobs/uploads/` 405 on read-only and 202 on write.
  `/v2/_catalog` works on the read-only side.
- Storage: filesystem driver with `delete.enabled: true` set in
  `configuration.Storage`. Config is built in code, no YAML file needed.
- GC: `storage.MarkAndSweep(ctx, driver, registry, storage.GCOpts{})` is public.
  Driver via `factory.Create(ctx, "filesystem", params)`, namespace via
  `storage.NewRegistry(ctx, drv, storage.EnableDelete)`.
  On a brand-new store it fails with
  `failed to mark: filesystem: Path not found: /docker/registry/v2/repositories`;
  treat that as "nothing to collect".
- Binary size of the spike: about 34 MB. Acceptable.
- Manifest delete goes through the normal registry API (`DELETE /v2/<name>/manifests/<digest>`)
  on the write listener; tags are removed via the tag service / the same API.

## Consequences for the design

- §9: keep as written; TLS is ours (`tls.Config.GetCertificate`), not distribution's.
- §10: run `MarkAndSweep` in-process under the build lock; ignore the empty-store error.
- Pin the distribution version in `go.mod`; the `handlers`/`storage` packages are
  not a documented embedding API, so upgrades need a smoke test (listener + GC).
