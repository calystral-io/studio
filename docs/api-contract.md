# Calystral Studio - BFF <-> UI API Contract (PR1 - PR4)

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
| 400 | `/errors/validation/invalid_lsn_range` | `from,to` |
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

## 9. Ledger DTOs (PR2)

A ledger is a named, append-only, bitemporal entry log in the tenant catalog
(e.g. `GeneralLedger`). Like anchors, the DTO is the SAME whether sourced from the
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
  "anchor_id": "anchor_employee_0003",
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
- `anchor_id` string|null: optional reference to a related node anchor.
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

Response 200 (same envelope as anchors):
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
| `as_of` | RFC3339 | (none) | optional bitemporal valid-time projection; malformed -> 400 |
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
tiers). Unlike anchors/ledgers it is LIVE state, NOT bitemporal: every DTO carries
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
</content>
