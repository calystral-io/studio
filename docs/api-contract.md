# Calystral Studio - BFF <-> UI API Contract (PR1 - PR6)

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
| 400 | `/errors/validation/invalid_system_as_of` | `value` |
| 400 | `/errors/validation/invalid_lsn_range` | `from,to` |
| 400 | `/errors/validation/invalid_request` | `field` |
| 409 | `/errors/conflict/already_exists` | `resource` |
| 409 | `/errors/conflict/precondition_failed` | `expected,actual` |
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
| `mock-admin-token` | `demo-tenant` | `admin@demo` | `["admin","reader","writer"]` |
| `mock-reader-token` | `demo-tenant` | `reader@demo` | `["reader"]` |
| `mock-writer-token` | `demo-tenant` | `writer@demo` | `["writer","reader"]` |

The `writer` role gates the node mutation surface (section 4.3); reads require
`reader`. `admin` is a superset that carries `writer`.

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

## 3. Node DTO

A node as the UI renders it. (Core models a node; today Core's read
path returns 501, so PR1's default data source is the BFF fixture. The DTO is the
SAME whether sourced from fixture or, later, decoded from Core's cybr rows.)

```json
{
  "id": "node_01J8Z9...",
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

## 4. GET /api/v1/nodes (paginated nodes browser - PR1 headline feature)

Query parameters:

| param | type | default | notes |
|---|---|---|---|
| `page_size` | int | 25 | 1..200 inclusive; out of range -> 400 |
| `cursor` | string | (none) | opaque forward cursor from a prior `next_cursor`; invalid -> 400 |
| `type` | string | (none) | optional filter by node type |
| `q` | string | (none) | optional case-insensitive substring over `label`+`properties` |
| `as_of` | RFC3339 or `YYYY-MM-DD` | (none) | optional bitemporal valid-time projection (a bare date = start-of-UTC-day); malformed -> 400 `/errors/validation/invalid_as_of` |
| `system_as_of` | RFC3339 or `YYYY-MM-DD` | (none) | optional system-time (transaction-time) projection: the node versions the store knew at this instant (a bare date = start-of-UTC-day); malformed -> 400 `/errors/validation/invalid_system_as_of` |

The two time axes are independent and compose (logical AND): `as_of` selects the
business-time slice, `system_as_of` selects the decision-time slice. With
`system_as_of` omitted the response is **current-only** - rows whose system
interval is still open (`system_to=null`), hiding superseded versions. Supplying
a past `system_as_of` reveals the value as originally recorded, before any later
correction (a different version of the same `id`). The system axis is
nodes-only; ledger entries (section 10.2) carry no system-time columns and
accept only `as_of`.

Cursor pagination (NOT offset): the cursor is an opaque base64url token the BFF
mints; the UI treats it as an opaque blob. First page omits `cursor`.

Response 200:
```json
{
  "items": [ /* GraphNodeDTO ... */ ],
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
`params.surface="nodes"`. The UI renders a localized "not yet available
upstream" empty state. (PR1 default is `fixture`, so the happy path shows real
paginated data; the 501 path is covered by an integration test against a stub
Core.)

Auth: requires a valid token (any role with `reader`). Results scoped to the
principal's `tenant_id`.

## 4.1 GET /api/v1/nodes/{id}/history (bitemporal version timeline)

Returns the full set of stored versions of one node id (every valid- and
system-time version), ordered by `(valid_from, system_from, lsn)` ascending. The
version set is small and bounded, so the response is not paginated. 404
`/errors/not_found` (`resource="node:<id>"`) when the id has no versions in the
tenant.

Response 200:
```json
{
  "id": "node_employee_0018",
  "type": "Employee",
  "tenant_id": "demo-tenant",
  "versions": [ /* GraphNodeDTO ... ordered oldest-first */ ],
  "summary": {
    "version_count": 2,
    "current_count": 1,
    "superseded_count": 1,
    "valid_segment_count": 1
  },
  "source": "fixture"
}
```

- `versions`: each is a full `GraphNodeDTO` (section 3) carrying both intervals.
- `summary.current_count`: versions whose system interval is open (`system_to=null`).
- `summary.superseded_count`: versions corrected away (`system_to` set).
- `summary.valid_segment_count`: distinct valid-time windows among the current versions.

Auth: `reader`, tenant-scoped. gRPC source returns 501
`/errors/upstream/unimplemented` with `params.surface="node_history"` (see
section 0 - the read pipeline + node-row wire format are not in Core yet).

## 4.2 GET /api/v1/nodes/{id}/diff (as-of field-level diff)

Resolves the node at TWO bitemporal coordinates and returns the field-level
delta between them - "what changed about this node between belief-state A and
belief-state B".

Query parameters:

| param | type | default | notes |
|---|---|---|---|
| `as_of` | RFC3339 or `YYYY-MM-DD` | now | "from" valid-time coordinate |
| `system_as_of` | RFC3339 or `YYYY-MM-DD` | (current/open) | "from" system-time coordinate |
| `to_as_of` | RFC3339 or `YYYY-MM-DD` | now | "to" valid-time coordinate |
| `to_system_as_of` | RFC3339 or `YYYY-MM-DD` | (current/open) | "to" system-time coordinate |

A coordinate resolves (via the half-open `[from,to)` rule, `null` upper = open)
to at most one version; a coordinate that matches no version yields a `null`
`version` on that side (NOT a 404). Malformed `as_of`/`to_as_of` -> 400
`invalid_as_of`; malformed `system_as_of`/`to_system_as_of` -> 400
`invalid_system_as_of`. 404 `/errors/not_found` only when the id has no versions
at all in the tenant.

Response 200:
```json
{
  "id": "node_employee_0018",
  "from": {
    "coordinate": { "as_of": "2026-05-01T00:00:00Z", "system_as_of": "2026-06-19T00:00:00Z" },
    "version": { /* GraphNodeDTO or null */ }
  },
  "to": {
    "coordinate": { "as_of": "2026-05-01T00:00:00Z", "system_as_of": null },
    "version": { /* GraphNodeDTO or null */ }
  },
  "deltas": [
    { "field": "closed", "op": "changed", "before": false, "after": true },
    { "field": "valid_to", "op": "changed", "before": null, "after": "2026-08-06T00:00:00Z" },
    { "field": "properties.title", "op": "changed", "before": "Engineering Manager", "after": "Principal Engineer" }
  ],
  "source": "fixture"
}
```

- `deltas`: never null (`[]` when the two sides are equal). `op` is
  `added | removed | changed`. `field` is `label | closed | valid_from |
  valid_to | properties.<key>`. A nil side reports every present field of the
  other as added/removed. Recording metadata (`system_from/to`, `lsn`, `txn_id`)
  is intentionally NOT diffed - only business content.
- Order is stable: `label`, `closed`, `valid_from`, `valid_to`, then properties
  by key ascending.

Auth: `reader`, tenant-scoped. gRPC source returns 501 with
`params.surface="node_diff"`.

## 4.3 Node mutations (PR10 — create / correct / close)

Studio's write surface. All three require the **`writer`** role (reads require
`reader`; `admin` carries both). They are backed by a **stateful fixture** in
mock mode — a write is immediately reflected in the read surfaces (list, history,
diff), honestly tagged `source:"fixture"`. Each mutation produces the same
bitemporal versions the history/diff surfaces render.

| method | path | role | success | semantics |
|---|---|---|---|---|
| `POST` | `/api/v1/nodes` | writer | `201` | create a new open node version |
| `POST` | `/api/v1/nodes/{id}/corrections` | writer | `200` | system-time correction (supersede + new current) |
| `POST` | `/api/v1/nodes/{id}/close` | writer | `200` | logical close in valid-time (`valid_to` + `closed`) |

Request bodies:
```json
// POST /nodes
{ "id": "node_x", "type": "Service", "label": "Billing",
  "properties": { "tier": "gold" }, "valid_from": "2026-01-01" }   // valid_from optional (RFC3339 or YYYY-MM-DD), default now
// POST /nodes/{id}/corrections
{ "label": "New label", "properties": { ... }, "expected_lsn": 4203 }  // label/properties each optional (>=1 required); properties is FULL replace; expected_lsn optional
// POST /nodes/{id}/close
{ "valid_to": "2026-12-31", "expected_lsn": 4203 }                  // both optional; valid_to default now
```

Response (all three): the resulting **current** `GraphNodeDTO`, plus the prior
version for correct/close:
```json
{ "node": { /* GraphNodeDTO ... current */ },
  "superseded": { /* GraphNodeDTO ... prior, system_to set */ },   // omitted on create
  "source": "fixture" }
```

**Bitemporal transitions:**
- CREATE — appends one open version (`system_from=now`, `system_to=null`,
  `valid_to=null`, `closed=false`).
- CORRECT — closes the current version's system interval at the mutation instant
  and appends a new current version with the corrected `label`/`properties`; the
  valid window is unchanged. This is exactly the supersession history/diff render.
- CLOSE — same supersession, the new current version carries `valid_to` + `closed=true`.

**Optimistic concurrency:** `expected_lsn` (optional) on correct/close is checked
against the current version's `lsn` under the write lock; a mismatch is `409`
`/errors/conflict/precondition_failed` (`params.expected`, `params.actual`).
Omitting it is last-writer-wins.

**Errors:** `400` `/errors/validation/invalid_request` (`params.field`) for a
malformed body, a missing `id`/`type`/`label` on create, a bad
`valid_from`/`valid_to`, an empty correction, or closing an already-closed
node; `409` `/errors/conflict/already_exists` (`params.resource`) for a
duplicate create id (id uniqueness is tenant-scoped); `404` `/errors/not_found`
for correct/close of an unknown id; `403` for a non-writer. gRPC source returns
`501` with `params.surface` ∈ {`node_create`,`node_correct`,`node_close`}
(Core's mutate path + a cybr encoder are not implemented — the write-side analogue
of the read decoder gap).

## 4.4 GET /api/v1/nodes/{id}/neighborhood (graph view — seeded expansion)

The graph view's data source: a ONE-HOP neighborhood around a seed node,
projected to a bitemporal coordinate. The whole graph is never returned —
expansion is seeded (start at a node) and capped + sampled server-side; the UI
re-seeds from a clicked neighbor to walk further.

Query parameters:

| param | type | default | notes |
|---|---|---|---|
| `as_of` | RFC3339 or `YYYY-MM-DD` | (none) | valid-time projection; malformed -> 400 `/errors/validation/invalid_as_of` |
| `system_as_of` | RFC3339 or `YYYY-MM-DD` | (none) | system-time projection; omitted => current-only; malformed -> 400 `/errors/validation/invalid_system_as_of` |
| `limit` | int | 50 | neighbor cap (1..200; clamped). Non-integer or negative -> 400 `/errors/validation/invalid_request` (`field=limit`) |

An **Edge** is a typed, directed, bitemporal relationship — the SAME bitemporal
shape as a node version:

```json
{
  "id": "edge_900123",
  "type": "MEMBER_OF",
  "source_id": "node_employee_0001",
  "target_id": "node_department_0001",
  "label": "member of",
  "properties": {},
  "valid_from": "2026-01-04T00:00:00Z",
  "valid_to": null,
  "system_from": "2026-01-04T09:00:00Z",
  "system_to": null,
  "lsn": 900123,
  "txn_id": 900123
}
```

Response 200:

```json
{
  "root": { /* NodeDTO (section 3) or null */ },
  "neighbors": [ /* NodeDTO ... capped + sampled */ ],
  "edges": [ /* Edge ... both endpoints inside {root} ∪ neighbors */ ],
  "neighbor_total": 11,
  "sampled": true,
  "bounds": {
    "valid_from": "2026-01-04T00:00:00Z", "valid_to": null,
    "system_from": "2026-01-04T09:00:00Z", "system_to": null
  },
  "source": "fixture"
}
```

- An edge is included only when it is present AND both endpoints are present at
  the coordinate; the returned `edges` is every present edge with both endpoints
  in the (root + kept neighbors) set, so neighbor-to-neighbor edges render too.
- `root` is `null` when the id exists but is not present at the coordinate (e.g.
  before it was created or after it was closed) — an empty graph, NOT an error.
- `neighbor_total` is the distinct neighbor count before the cap; `sampled` is
  true when neighbors were dropped to fit `limit` (evenly sampled, deterministic).
- `bounds` is the bitemporal span over which the seed's whole neighborhood evolves,
  computed over every ever-connected node + edge and UNFILTERED by `as_of` /
  `system_as_of` — so the UI timeline (valid-time) and the "as recorded at" axis
  (system-time) each have a stable scrub range. `valid_from`/`valid_to` is the
  business-time span (`valid_to=null` => still open, scrub up to now);
  `system_from`/`system_to` is the decision-time span (`system_to=null` => still
  current, "as recorded today"). Rolling `system_as_of` back before a recorded
  correction reveals the neighborhood as it was originally recorded (a fact
  recorded later is absent at the earlier system-time).
- `404` `/errors/not_found` (`resource="node:<id>"`) only when the id has no
  versions at all in the tenant. Reader role required. gRPC source returns `501`
  with `params.surface="node_neighborhood"`.

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

## 6. WebSocket live stream (PR6)

`GET /api/v1/ws` upgrades to a WebSocket. Auth is in-handshake: the token arrives
via `Sec-WebSocket-Protocol: bearer,<token>` (negotiated subprotocol `bearer`) or
`?access_token=<token>` (browsers cannot set `Authorization` on a WS handshake).
Missing/invalid token → 401 (typed error envelope written over plain HTTP before
the upgrade). Heartbeat ping/pong keeps idle connections alive; same-origin is
enforced against `STUDIO_CORS_ORIGINS`.

Every message is the typed envelope `{ "type": "...", "payload": {...} }`.

**Server → client** types:
- `hello` (on connect): `{ principal:{tenant_id,user_id,roles}, server_time, topics:["cluster","runtime","messaging"] }`.
- `subscribed` / `unsubscribed`: `{ topic }` acks.
- `snapshot`: `{ topic, data }` — `data` is the SAME body the matching REST summary
  endpoint serves (for `cluster`, the §11 ClusterSummary + `source`; `runtime` →
  §13; `messaging` → §16), with `observed_at` stamped to the push instant. Emitted
  immediately on subscribe and then every push interval (default 2s, env-free).
- `error`: `{ topic?, code, message }` — `/errors/validation/unknown_topic`,
  `/errors/auth/forbidden` (reader role required), `/errors/validation/unknown_message`,
  or an upstream gap (`/errors/upstream/unimplemented` under `STUDIO_CORE_SOURCE=grpc`)
  surfaced IN-BAND rather than closing the socket.

**Client → server** types:
- `subscribe`: `{ type:"subscribe", topic }` — topic ∈ {cluster, runtime, messaging};
  requires the `reader` role; re-subscribe is idempotent.
- `unsubscribe`: `{ type:"unsubscribe", topic }`.

The topics are the three live (non-bitemporal) summary surfaces. Stamping
`observed_at` to "now" makes the live view tick; the BFF never fabricates metric
deltas — under `fixture` each tick re-serves the same honest snapshot (only the
timestamp advances), and under `grpc` it emits the in-band 501 error event.

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

## 9. Ledger DTOs (PR2)

A ledger is a named, append-only, bitemporal entry log in the tenant catalog
(e.g. `GeneralLedger`). Like nodes, the DTO is the SAME whether sourced from the
BFF fixture (PR2 default) or, later, decoded from Core's cybr rows; today Core's
read path returns 501, so the default data source is the fixture.

### 9.1 LedgerSummary

A catalog entry describing one ledger (returned by the list endpoint).

```json
{
  "name": "GeneralLedger",
  "kind": "accounting",
  "description": "Double-entry accounting postings for the demo tenant",
  "entry_count_estimate": 120,
  "last_lsn": 7359,
  "last_recorded_at": "2026-06-15T20:51:00Z"
}
```

- `name` string, opaque, path-safe, stable, sortable (also the `{name}` path segment).
- `kind` free string describing the ledger's nature (e.g. `accounting|audit|event`).
- `description` human-readable summary.
- `entry_count_estimate` int: best-effort entry count for UI display; may be approximate.
- `last_lsn` int: the global LSN of the most recent entry in this ledger.
- `last_recorded_at` RFC3339: system time of that most recent entry.

### 9.2 LedgerEntry

One append-only, bitemporal entry in a ledger.

```json
{
  "id": "entry_0007001",
  "ledger": "GeneralLedger",
  "seq": 1,
  "lsn": 7001,
  "txn_id": 7001,
  "kind": "posting",
  "summary": "Posting #1 to account 4000-Revenue",
  "actor": "admin@demo",
  "node_id": "node_employee_0003",
  "recorded_at": "2026-01-02T08:00:00Z",
  "effective_from": "2026-01-04T00:00:00Z",
  "effective_to": null,
  "prev_lsn": null,
  "payload": { "account": "4000-Revenue", "amount": 1250, "currency": "EUR" }
}
```

- `id` string, opaque, stable, unique.
- `ledger` string: the owning ledger `name`.
- `seq` int: per-ledger monotonic sequence (1-based, append order within the ledger).
- `lsn` int: global system order across all ledgers (strictly increasing append order).
- `txn_id` int: the originating transaction id.
- `kind` string: entry classification (filterable; e.g. `posting|reversal|login|created`).
- `summary` human-readable one-line description.
- `actor` string: the principal `user_id` that appended the entry.
- `node_id` string|null: optional reference to a related node.
- `recorded_at` RFC3339: system (decision/transaction) time the entry was appended.
- Bitemporal valid time: `effective_from`/`effective_to` (business time;
  `effective_to=null` => still in effect / open).
- `prev_lsn` int|null: the LSN of the previous entry in THIS ledger - the append-chain
  link. `null` for the first entry of a ledger.
- `payload` object: string->scalar (string|number|boolean|null), entry-specific data.

## 10. Ledger endpoints (PR2)

### 10.1 GET /api/v1/ledgers (paginated ledger catalog)

Lists the tenant's ledgers. Auth: requires `reader`. Results scoped to the
principal's `tenant_id`.

Query parameters:

| param | type | default | notes |
|---|---|---|---|
| `page_size` | int | 25 | 1..200 inclusive; out of range -> 400 |
| `cursor` | string | (none) | opaque forward cursor from a prior `next_cursor`; invalid -> 400 |
| `q` | string | (none) | optional case-insensitive substring over `name`+`description` |

Response 200 (same envelope as nodes):
```json
{
  "items": [ /* LedgerSummary ... */ ],
  "page": { "page_size": 25, "next_cursor": null, "has_more": false, "total_estimate": 3 },
  "source": "fixture"
}
```

### 10.2 GET /api/v1/ledgers/{name}/entries (paginated ledger entries)

Lists the entries of one ledger, NEWEST FIRST (descending `lsn`). Auth: requires
`reader`. Results scoped to the principal's `tenant_id`. An unknown `{name}` ->
404 `/errors/not_found` with `params.resource = "ledger:<name>"`.

Query parameters:

| param | type | default | notes |
|---|---|---|---|
| `page_size` | int | 25 | 1..200 inclusive; out of range -> 400 |
| `cursor` | string | (none) | opaque forward cursor from a prior `next_cursor`; invalid -> 400 |
| `kind` | string | (none) | optional filter by entry `kind` |
| `q` | string | (none) | optional case-insensitive substring over `summary`+`payload` |
| `as_of` | RFC3339 or `YYYY-MM-DD` | (none) | optional bitemporal valid-time projection (a bare date = start-of-UTC-day); malformed -> 400 `/errors/validation/invalid_as_of` |
| `from_lsn` | int | (none) | optional lower bound (inclusive) on `lsn` |
| `to_lsn` | int | (none) | optional upper bound (inclusive) on `lsn`; if `from_lsn > to_lsn` -> 400 `/errors/validation/invalid_lsn_range` |

Entries are returned in DESCENDING `lsn` order (newest first). The cursor is an
opaque, stable base64url token; the walk never duplicates or skips an entry.

Response 200 (same `{items,page,source}` envelope):
```json
{
  "items": [ /* LedgerEntry ... (descending lsn) */ ],
  "page": { "page_size": 25, "next_cursor": "eyJvIjoyNX0", "has_more": true, "total_estimate": 120 },
  "source": "fixture"
}
```

Behavior when `STUDIO_CORE_SOURCE=grpc` and Core returns UNIMPLEMENTED (the honest
gap today): the BFF returns 501 `/errors/upstream/unimplemented` with
`params.surface="ledgers"` for the list and `params.surface="ledger_entries"` for
the entries endpoint. We never fabricate rows. (PR2 default is `fixture`, so the
happy path shows real paginated data; the 501 path is covered by integration tests
against a stub Core.)

## 11. Cluster DTOs + endpoints (PR3)

The cluster view is an OPERATOR observability surface over the cvm cluster
(per-shard Raft groups, key-range sharding, replicas across regions, storage
tiers). Unlike nodes/ledgers it is LIVE state, NOT bitemporal: every DTO carries
an `observed_at` snapshot instant and no valid/system time. The cluster is shared
operator infrastructure, so it is NOT tenant-scoped. All endpoints require
`reader`.

### 11.1 ClusterSummary / Node DTOs

`ClusterSummary` (the rollup): `node_count`, `shard_count`, `region_count`,
`replication_factor` (int); `health` ("healthy"|"degraded"); `shard_health`
(`{healthy, degraded, under_replicated}` int counts, always all present);
`regions` (array of `{name, node_count, shard_count, health}`); `observed_at`
(RFC3339). Health is derived: any non-healthy shard or any non-"up" node in scope
=> "degraded".

`NodeDTO`: `id`, `address`, `region`, `status` ("up"|"draining"|"down"),
`shard_count`, `leader_count`, `raft_term` (int), `used_bytes`, `capacity_bytes`
(int64), `version`, `last_heartbeat` (RFC3339).

### 11.2 GET /api/v1/cluster (rollup) + GET /api/v1/cluster/nodes (paginated)

`/cluster` returns the `ClusterSummary` object with a top-level `source` tag (the
summary fields are promoted). `/cluster/nodes` returns the standard
`{items,page,source}` envelope (id asc).

Node query parameters: `page_size` (1..200), `cursor` (opaque), `region` (exact),
`status` (exact), `q` (case-insensitive substring over id+address+region). Unknown
`region`/`status` values match nothing (not a 400).

## 12. Shard DTO + GET /api/v1/cluster/shards (PR3)

`ShardDTO`: `id`, `raft_group_id`; `key_range` (`{start, end}`, half-open; `end`
null = unbounded upper edge of the final shard); `region`, `leader_node_id`;
`replica_node_ids` (string array, includes the leader; shorter than
`replication_factor` exactly when `under_replicated`); `replication_factor`,
`status` ("healthy"|"degraded"|"under_replicated"), `raft_term` (int),
`commit_index`, `applied_index`, `lag` (int64, = commit-applied, always >= 0),
`size_bytes` (int64), `tier` ("Hot"|"Warm"|"Cold"|"Archive"), `observed_at`.

`/cluster/shards` returns `{items,page,source}` (id asc). Query parameters:
`page_size`, `cursor`, `region` (exact), `status` (exact), `node` (matches shards
where the node is the leader OR a replica), `q` (substring over
id+raft_group_id+key_range edges).

501 behavior under `STUDIO_CORE_SOURCE=grpc`: `params.surface` is
`cluster_summary` / `cluster_nodes` / `cluster_shards` respectively.

### 12.1 GET /api/v1/cluster/topology (aggregated cluster view)

A single payload aggregating the cluster across all configured Core replicas, for
the cluster overview. Requires `reader`. The BFF fans the read out across every
replica in `STUDIO_CORE_GRPC_ADDRS` (comma-separated; falls back to the single
`STUDIO_CORE_GRPC_ADDR`), unions the reported nodes/shards (deduped by id, id
asc), and derives the rollup from those rows.

Response 200:

```json
{
  "cluster": false,
  "summary": null,
  "nodes": [],
  "shards": [],
  "source": "core"
}
```

- `cluster` (bool): true when more than one node was OBSERVED across the reachable
  replicas; false for a single-node Core, when no topology is available, or when a
  multi-node cluster is partitioned down to one reachable node (the flag reflects
  observed membership, not declared deployment size).
- `summary` (`ClusterSummary` | null): the derived rollup, or `null` when no
  replica has cluster topology to report. `region_count` and `replication_factor`
  are derived from the rows themselves (not seed constants).
- `nodes` / `shards`: the unioned sets (always arrays, `[]` when empty - never
  `null`).
- `source`: `"fixture"` or `"core"`.

NEVER-FABRICATE: a single-node Core - and, today, a cluster whose Core build does
not yet serve cluster topology over gRPC (it returns UNIMPLEMENTED, pending
Core's RaftTransport + read path) - yields the honest no-cluster-info shape above
(`cluster:false`, `summary:null`, empty sets), NOT a synthesized rollup. Unlike
the paginated cluster endpoints this surface does NOT 501 on that gap: an empty
topology is the correct answer until Core can report one. An unreachable replica
is skipped (the read degrades gracefully); only when EVERY replica is unreachable
does it return 502 `params.surface="cluster_topology"`.

## 13. Runtime-state DTOs + GET /api/v1/runtime (PR4)

The runtime view is an OPERATOR observability surface over the cvm execution
engine: VM metrics (the in-process registry), the content-addressed plan cache,
and the cybr opcode instruction set with execution profiling. LIVE state (carries
`observed_at`); NOT tenant-scoped; requires `reader`.

> NOTE: `OpcodeDTO.exec_count` / `exec_share_milli` and
> `RuntimeSummary.instructions_executed` are FORWARD-LOOKING telemetry - the cvm
> interpreter does not tally per-opcode or instruction counts today - so the
> fixture seeds representative values behind the demo-data (`source:"fixture"`)
> tag. Opcode discriminants are assigned within the documented cybr v0.2.8
> category ranges; mnemonics and categories are the real instruction set.

`MetricSeries`: `name` (Prometheus exposition name, e.g. `cvm_txn_commits_total`),
`kind` ("counter"|"gauge"|"histogram"), `help`, `value` (int64 scalar for
counter/gauge - counters >= 0, gauges may be negative - `null` for histograms),
`histogram` (present only for histograms): `{buckets: [{upper_bound, count}],
sum, count}` where `upper_bound` is the inclusive `le` bound (native unit
ns/bytes) and a `null` bound is the +Inf overflow bucket; bucket counts are
cumulative.

`MetricGroup`: `{subsystem, series: [MetricSeries]}`. The registry exposes 18
named series across 4 subsystems (storage, transactions, raft, calvin).

`PlanCacheStats`: `hits`, `misses`, `inserts`, `evictions` (uint64), `entries`
(int), `resident_bytes` (uint64, excludes pinned entries), `capacity_bytes`
(uint64, the byte budget; default 64 MiB), `hit_rate_milli` (int, per-mille
0..1000; 0 when no lookups).

`GET /api/v1/runtime` returns the `RuntimeSummary` object with a top-level
`source` tag (fields promoted): `uptime_seconds` (int64), `instructions_executed`
(uint64), `active_transactions` (int, mirrors the `cvm_txn_active` gauge),
`opcode_count` and `metric_series_count` (int, derived from the rows),
`plan_cache` (PlanCacheStats), `metric_groups` (array), `observed_at` (RFC3339).

## 14. Opcode DTO + GET /api/v1/runtime/opcodes (PR4)

`OpcodeDTO`: `mnemonic`, `code` (int, stable u16 discriminant), `code_hex`
(0x-prefixed, e.g. `"0x00D8"`), `category` (e.g. storage/comparison/control_flow/
arithmetic/load/stream/ledger/...), `short_form` (bool, single-byte encoding =
code < 0x100), `exec_count` (uint64), `exec_share_milli` (int, per-mille of all
executed instructions 0..1000), `observed_at`.

`/runtime/opcodes` returns `{items,page,source}` (code asc). Query parameters:
`page_size` (1..200), `cursor`, `category` (exact; unknown matches nothing), `q`
(case-insensitive substring over the mnemonic).

## 15. Plan-cache entry DTO + GET /api/v1/runtime/plan-cache (PR4)

`PlanCacheEntryDTO`: `key` (content address - BLAKE3 of the bitcode - as a 64-char
hex string), `size_bytes` (uint64, evictable footprint), `cost` (uint64,
deterministic recompute-cost proxy), `freq` (uint64, access frequency),
`ref_count` (int, referencing tenants), `pinned` (bool), `observed_at`.

`/runtime/plan-cache` returns `{items,page,source}` (key asc). Query parameters:
`page_size`, `cursor`, `pinned` ("true"|"false" exact filter; any other non-empty
value matches nothing), `q` (case-insensitive substring over the key, for prefix
lookups).

501 behavior under `STUDIO_CORE_SOURCE=grpc`: `params.surface` is
`runtime_summary` / `runtime_opcodes` / `runtime_plan_cache` respectively.

## 16. Messaging DTOs + GET /api/v1/messaging (PR5)

The messaging view is an OPERATOR observability surface over the cvm-channels
runtime: durable channels (kind "stream" or "queue"), their live queue/ephemeral
state, and the live subscriptions (the stream cursors). LIVE state (carries
`observed_at`); NOT tenant-scoped; requires `reader`.

> NOTE: cvm-channels exposes no enumeration accessor or gRPC surface today (only
> per-id getters + a Prometheus text render), so the BFF fixture seeds a
> representative live set behind the demo-data (`source:"fixture"`) tag. Enum
> values mirror the real cvm-channels types (ChannelKind / ChannelStatus /
> StartAt / Ordering / OverflowPolicy / AckMode).

`MessagingSummary` (the rollup, fields promoted with a top-level `source`):
`channel_count` (int); `by_kind` (`{stream, queue}` int counts); `by_status`
(`{open, closed}` int counts); `ephemeral_count`, `subscription_count`,
`total_buffered`, `total_in_flight` (int); `total_dropped` (int64); `metrics`
(array of `MetricSeries` - the 5 live `cvm_channels_*` series: emit-latency
histogram, partition-routes counter, partition-history-bumps counter,
subscriber-buffer-depth gauge = `total_buffered`, overflow-drops counter =
`total_dropped`; same `MetricSeries` shape as §13); `observed_at` (RFC3339).
Counts and aggregates are derived from the seeded rows.

`GET /api/v1/messaging` returns the `MessagingSummary` object.

## 17. Channel DTO + GET /api/v1/messaging/channels (PR5)

`ChannelDTO`: `id`, `name`, `tenant`; `kind` ("stream"|"queue"), `status`
("open"|"closed"); `carries` (carried type name), `placement`; `partition_count`
(int), `partitioned_by` (string|null); `retention_secs` (int64); `ack_mode`
("auto"|"manual"|null, queue-only), `visibility_timeout_secs` (int64|null,
queue-only); `ephemeral` (bool), `ttl_secs` (int64|null, ephemeral-only);
`emit_lsn` (int64); `in_flight`, `redelivery` (int, queue delivery state, 0 for
streams); `subscription_count` (int, streams); `observed_at`.

`/messaging/channels` returns `{items,page,source}` (id asc). Query parameters:
`page_size` (1..200), `cursor`, `kind` (exact), `status` (exact), `q`
(case-insensitive substring over name+carries+placement). Unknown `kind`/`status`
values match nothing (not a 400).

## 18. Subscription DTO + GET /api/v1/messaging/subscriptions (PR5)

`SubscriptionDTO` (one live stream cursor): `id`, `channel_id`, `channel_name`,
`tenant`; `start` ("tail"|"offset"|"as_of"), `ordering`
("per_partition"|"strictly_ordered"), `overflow`
("drop_oldest"|"drop_newest"|"pause"); `buffer_capacity`, `buffered` (int, running
live-buffer depth), `partition_span` (int); `live_from_lsn` (int64); `lag` (int64,
channel emit-LSN minus cursor head, >= 0); `dropped`, `out_of_span_dropped`
(int64); `observed_at`.

`/messaging/subscriptions` returns `{items,page,source}` (id asc). Query
parameters: `page_size`, `cursor`, `channel` (exact channel id), `ordering`
(exact), `overflow` (exact), `q` (substring over id+channel_name).

501 behavior under `STUDIO_CORE_SOURCE=grpc`: `params.surface` is
`messaging_summary` / `messaging_channels` / `messaging_subscriptions`
respectively.
</content>
