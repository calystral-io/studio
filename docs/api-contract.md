# Calystral Studio - BFF <-> UI API Contract (PR1)

Canonical seam between `calystral-io/studio` (Go BFF) and `calystral-io/studio-ui`
(React). The BFF OWNS this contract; the UI consumes it. This file is committed
to `studio/docs/api-contract.md` and mirrored as the integration reference the
UI builds to. ASCII only.

## 0. Conventions

- Base path: `/api/v1`.
- All responses are JSON (`application/json; charset=utf-8`) unless noted.
- All timestamps are RFC 3339 / ISO 8601 UTC strings (`2026-06-27T15:04:05Z`).
- Bitemporal fields are explicit and never collapsed: `valid_*` (business time)
  and `system_*` (transaction/decision time, carrying the LSN).
- The UI NEVER hardcodes a user-visible error string. Errors carry a JSON-pointer
  `code` into the i18n tree plus `params`; the UI looks up the localized string.

## 1. Error envelope (every non-2xx response)

```json
{
  "error": {
    "code": "/errors/validation/page_size_out_of_range",
    "params": { "min": 1, "max": 200, "got": 999 },
    "message": "page_size 999 is out of range [1,200]",
    "request_id": "req_01J..."
  }
}
```

- `code` (string, REQUIRED): JSON Pointer (RFC 6901) into the i18n `errors`
  subtree. The UI resolves `i18n.t(jsonPointerToKey(code), params)`. NEVER
  pre-translated.
- `params` (object, REQUIRED, may be `{}`): interpolation values for the
  localized template.
- `message` (string, REQUIRED): developer-facing English fallback, NON-localized.
  Used only for logs / when a translation key is missing. The UI must not show it
  to end users when a translation exists.
- `request_id` (string, REQUIRED): correlation id, echoed in the `X-Request-Id`
  response header too.

Canonical error codes shipped in PR1 (each MUST have an `en` translation):

| HTTP | code | params |
|---|---|---|
| 400 | `/errors/validation/page_size_out_of_range` | `min,max,got` |
| 400 | `/errors/validation/invalid_cursor` | `cursor` |
| 400 | `/errors/validation/invalid_as_of` | `value` |
| 401 | `/errors/auth/missing_token` | `{}` |
| 401 | `/errors/auth/invalid_token` | `{}` |
| 403 | `/errors/auth/forbidden` | `{}` |
| 404 | `/errors/not_found` | `resource` |
| 501 | `/errors/upstream/unimplemented` | `surface` |
| 502 | `/errors/upstream/unavailable` | `surface` |
| 500 | `/errors/internal` | `{}` |

`code` strings are stable identifiers; never reuse a code for a different
meaning. The mapping table above is duplicated as a typed enum/const in BOTH
repos (Go `errors` package; TS `src/lib/errors.ts`) and a test in each repo
asserts every code has an `en` translation.

## 2. Auth (mock, pluggable)

PR1 auth is MOCK behind a stable interface; Nexus swaps the implementation later
without changing this contract.

UI -> BFF: every `/api/v1/*` request carries `Authorization: Bearer <token>`.

Mock tokens recognized by the BFF in mock mode (config: `STUDIO_AUTH_MODE=mock`,
the PR1 default):

| token | tenant_id | user_id | roles |
|---|---|---|---|
| `mock-admin-token` | `demo-tenant` | `admin@demo` | `["admin","reader"]` |
| `mock-reader-token` | `demo-tenant` | `reader@demo` | `["reader"]` |

- Missing `Authorization` header -> 401 `/errors/auth/missing_token`.
- Unrecognized token -> 401 `/errors/auth/invalid_token`.
- The BFF exposes the resolved principal at `GET /api/v1/me` (see 5.3) so the UI
  can render who it is and gate UI affordances by role.

BFF -> Core (real adapter only): the BFF forwards/mints the principal as the
`x-calystral-principal` header carrying an EdDSA (Ed25519) JWT with claims
`iss, exp, tenant_id, user_id, roles, audit_session_id` (Core's contract). In
mock mode the BFF mints this dev JWT from a local dev keypair (clearly dev-only,
config `STUDIO_CORE_DEV_SIGNING_KEY`); when Nexus lands the BFF forwards the real
inbound Nexus JWT unchanged. The fixture source (PR1 default) does not call Core,
so no JWT is minted there; the principal still scopes the fixture by `tenant_id`.

Go interface (BFF):
```go
type Authenticator interface {
    // Authenticate resolves the request principal or returns a *APIError
    // (mapped to 401). Implementations: mockAuthenticator (PR1), nexusForwarder (later).
    Authenticate(r *http.Request) (*Principal, error)
}
type Principal struct {
    TenantID       string
    UserID         string
    Roles          []string
    AuditSessionID string
}
```

TS interface (UI):
```ts
interface AuthSession { token: string; principal: Principal | null; }
interface AuthContextValue {
  session: AuthSession;
  signInAs: (token: string) => void;   // mock: pick a token
  signOut: () => void;
  hasRole: (role: string) => boolean;
}
// useAuth() reads it; <AuthProvider> holds it; mock impl persists token in localStorage.
```

## 3. Anchor DTO

A node anchor as the UI renders it. (Core models a node anchor; today Core's read
path returns 501, so PR1's default data source is the BFF fixture. The DTO is the
SAME whether sourced from fixture or, later, decoded from Core's cybr rows.)

```json
{
  "id": "anchor_01J8Z9...",
  "type": "Employee",
  "label": "Ada Lovelace",
  "tenant_id": "demo-tenant",
  "properties": {
    "email": "ada@demo",
    "title": "Principal Engineer",
    "department": "Platform"
  },
  "valid_from": "2026-01-04T00:00:00Z",
  "valid_to": null,
  "system_from": "2026-01-04T09:12:30Z",
  "system_to": null,
  "lsn": 4821,
  "txn_id": 4821,
  "closed": false
}
```

- `id` string, opaque, stable, sortable.
- `type` string (node type / label class), `label` human-readable display name.
- `properties` object: string->scalar (string|number|boolean|null).
- Bitemporal: `valid_from`/`valid_to` (business time, `valid_to=null` => open),
  `system_from`/`system_to` (system time, `system_to=null` => current),
  `lsn`/`txn_id` integers.
- `closed` bool: logically deleted (Core `MUTATION_KIND_CLOSE`).

## 4. GET /api/v1/anchors (paginated anchors browser - PR1 headline feature)

Query parameters:

| param | type | default | notes |
|---|---|---|---|
| `page_size` | int | 25 | 1..200 inclusive; out of range -> 400 |
| `cursor` | string | (none) | opaque forward cursor from a prior `next_cursor`; invalid -> 400 |
| `type` | string | (none) | optional filter by node type |
| `q` | string | (none) | optional case-insensitive substring over `label`+`properties` |
| `as_of` | RFC3339 | (none) | optional bitemporal valid-time projection; malformed -> 400 |

Cursor pagination (NOT offset): the cursor is an opaque base64url token the BFF
mints; the UI treats it as an opaque blob. First page omits `cursor`.

Response 200:
```json
{
  "items": [ /* AnchorDTO ... */ ],
  "page": {
    "page_size": 25,
    "next_cursor": "eyJvIjoyNX0",
    "has_more": true,
    "total_estimate": 142
  },
  "source": "fixture"
}
```

- `page.next_cursor`: null when `has_more` is false.
- `page.total_estimate`: best-effort count for UI display; may be approximate.
- `source`: `"fixture" | "core"` - which data source served this response, so the
  UI can surface an honest "demo data" badge in PR1. Driven by BFF config
  `STUDIO_CORE_SOURCE` (`fixture` default in PR1; `grpc` once Core's row path lands).

Behavior when `STUDIO_CORE_SOURCE=grpc` and Core returns UNIMPLEMENTED (the honest
gap today): the BFF returns 501 `/errors/upstream/unimplemented` with
`params.surface="anchors"`. The UI renders a localized "not yet available
upstream" empty state. (PR1 default is `fixture`, so the happy path shows real
paginated data; the 501 path is covered by an integration test against a stub
Core.)

Auth: requires a valid token (any role with `reader`). Results scoped to the
principal's `tenant_id`.

## 5. Infra + identity endpoints

### 5.1 GET /healthz  (unauthenticated, liveness)
`200 {"status":"ok"}`. No auth. Never depends on Core.

### 5.2 GET /readyz  (unauthenticated, readiness)
`200 {"status":"ready","checks":{"core":"skip"}}` when source=fixture;
when source=grpc, `checks.core` is `"ok"|"unavailable"` from a Core health ping.
`503` with the same body shape if not ready.

### 5.3 GET /api/v1/version  (unauthenticated)
```json
{ "service": "studio", "version": "0.1.0", "commit": "abc1234", "go": "go1.26.4", "build_time": "2026-06-27T15:00:00Z" }
```
`version/commit/build_time` injected at build via -ldflags; safe zero-values in dev.

### 5.4 GET /api/v1/me  (authenticated)
Returns the resolved principal:
```json
{ "tenant_id": "demo-tenant", "user_id": "admin@demo", "roles": ["admin","reader"] }
```

## 6. WebSocket (scaffold only in PR1)

`GET /api/v1/ws` upgrades to WebSocket. PR1 ships the endpoint + auth handshake
(token via `Sec-WebSocket-Protocol: bearer,<token>` or `?access_token=`) + a
heartbeat ping/pong + a typed envelope `{ "type": "...", "payload": {...} }`, and
ONE real message type `{"type":"hello","payload":{"principal":{...},"server_time":"..."}}`
sent on connect. Live data streams (ledger tail, etc.) land in later PRs; the
framing + auth + tests ship now so the seam is real, not stubbed.

## 7. CORS / dev proxy

The UI dev server (Vite, port 5173) proxies `/api` -> BFF (default
`http://localhost:8080`) so the browser is same-origin in dev. The BFF also
honors `STUDIO_CORS_ORIGINS` (comma list) for non-proxy setups, defaulting to
`http://localhost:5173`. Credentials: bearer header, not cookies.

## 8. Config (BFF env vars, all with safe defaults)

| var | default | meaning |
|---|---|---|
| `STUDIO_HTTP_ADDR` | `:8080` | listen address |
| `STUDIO_AUTH_MODE` | `mock` | `mock` (PR1) \| `nexus` (later) |
| `STUDIO_CORE_SOURCE` | `fixture` | `fixture` \| `grpc` |
| `STUDIO_CORE_GRPC_ADDR` | `localhost:50051` | Core gRPC endpoint (source=grpc) |
| `STUDIO_CORE_DEV_SIGNING_KEY` | (dev key path/inline) | dev-only EdDSA key to mint x-calystral-principal |
| `STUDIO_CORS_ORIGINS` | `http://localhost:5173` | allowed browser origins |
| `STUDIO_LOG_LEVEL` | `info` | structured JSON log level |

CLI (cobra): `studio serve` (run the HTTP server), `studio version`. Flags mirror
env vars; env wins per 12-factor unless flag explicitly set.
</content>
