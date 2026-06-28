# studio

Calystral Studio - Go BFF service powering Studio-UI. It is the REST/WebSocket
edge the React UI builds to, and (later) a gRPC client to Core. The BFF owns the
UI seam; that contract is `docs/api-contract.md` and is the single source of
truth for every endpoint, the error envelope, and config.

## Layout

```
cmd/studio/          cobra CLI: `studio serve`, `studio version`
internal/apierr/     typed error registry + envelope writer (contract section 1)
internal/auth/       Authenticator interface, mock token map, EdDSA principal JWT
internal/config/     env + flag config loading (contract section 8)
internal/coreclient/ CoreClient port; fixture + gRPC implementations
internal/corepb/     generated gRPC stubs (committed)
internal/httpapi/    chi server, middleware, handlers, WebSocket
internal/version/    build identity injected via -ldflags
api/proto/           protos vendored from calystral-io/core/core-api/proto
```

## Quick start

```
make run                  # serve on :8080, fixture data, mock auth
# or
go run ./cmd/studio serve
```

Try it (mock tokens from contract section 2):

```
curl localhost:8080/healthz
curl localhost:8080/api/v1/version
curl -H 'Authorization: Bearer mock-admin-token' localhost:8080/api/v1/me
curl -H 'Authorization: Bearer mock-reader-token' \
  'localhost:8080/api/v1/nodes?page_size=25&type=Employee'
```

The nodes browser serves ~142 seeded bitemporal nodes (Employee, Department,
Project, Customer) with cursor pagination and `type` / `q` / `as_of` filters,
honestly tagged `"source":"fixture"`.

## Configuration

All vars have safe defaults (contract section 8); env wins over defaults, an
explicitly-set CLI flag wins over env.

| var | default | meaning |
|---|---|---|
| `STUDIO_HTTP_ADDR` | `:8080` | listen address |
| `STUDIO_AUTH_MODE` | `mock` | `mock` (PR1) or `nexus` (later) |
| `STUDIO_CORE_SOURCE` | `fixture` | `fixture` or `grpc` |
| `STUDIO_CORE_GRPC_ADDR` | `localhost:50051` | Core gRPC endpoint (source=grpc) |
| `STUDIO_CORE_DEV_SIGNING_KEY` | (generated) | dev-only EdDSA key to mint x-calystral-principal |
| `STUDIO_CORS_ORIGINS` | `http://localhost:5173` | allowed browser origins |
| `STUDIO_LOG_LEVEL` | `info` | structured JSON log level |

With `STUDIO_CORE_SOURCE=grpc` the BFF dials Core's `QueryService.Query`, mints
and forwards an EdDSA `x-calystral-principal` JWT, and maps Core's current
`UNIMPLEMENTED` gap to a 501 `/errors/upstream/unimplemented` (surface=nodes).
Decoding Core's opaque cybr rows lands in a later PR (see the TODO in
`internal/coreclient/grpc.go`).

## Build, test, lint

```
make build      # -> bin/studio with version ldflags
make test       # unit + integration, race detector
make lint       # gofmt-check + go vet
```

If your `/tmp` is a small-quota tmpfs, point the Go build temp dir at a roomier
filesystem: `export GOTMPDIR=$PWD/.gotmp` (gitignored) before `make test`.

## Protos

The four Core protos are vendored under `api/proto/` from the canonical source
`calystral-io/core/core-api/proto` (header-stamped, with a `go_package` option
added). Generated stubs live in `internal/corepb/` and are committed.

```
make proto-tools   # install protoc-gen-go + protoc-gen-go-grpc
make proto         # regenerate stubs from api/proto/*.proto
make proto-sync    # re-copy from ../core, re-stamp header + go_package, regenerate
```

Sync note: only `query.proto` is exercised today (the nodes read path);
`schema/mutate/proc` are vendored for forward use. If Core changes a proto, run
`make proto-sync` (requires the core checkout at `../core`), review the diff, and
`make test`.
