// Package coreclient is the BFF's port to Core's read path. It exposes a
// CoreClient interface for listing node anchors and bitemporal ledger entries
// with cursor pagination and filters, plus two implementations selected by
// STUDIO_CORE_SOURCE: a seeded in-memory fixture (default) and a gRPC adapter
// against Core's QueryService (which returns UNIMPLEMENTED today). The DTOs are
// identical regardless of source so the UI renders both the same.
package coreclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

// Source tags identify which backend served a response.
const (
	SourceFixture = "fixture"
	SourceCore    = "core"
)

// Core readiness check states surfaced on /readyz.
const (
	CheckSkip        = "skip"
	CheckOK          = "ok"
	CheckUnavailable = "unavailable"
)

// AnchorDTO is a node anchor as the UI renders it (contract section 3). Times
// are UTC; nil *time.Time marshals to JSON null per the bitemporal "open"/
// "current" conventions.
type AnchorDTO struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Label      string         `json:"label"`
	TenantID   string         `json:"tenant_id"`
	Properties map[string]any `json:"properties"`
	ValidFrom  time.Time      `json:"valid_from"`
	ValidTo    *time.Time     `json:"valid_to"`
	SystemFrom time.Time      `json:"system_from"`
	SystemTo   *time.Time     `json:"system_to"`
	Revision   int64          `json:"revision"`
	TxnID      int64          `json:"txn_id"`
	Closed     bool           `json:"closed"`
}

// Page is the cursor-pagination envelope (contract section 4).
type Page struct {
	PageSize      int     `json:"page_size"`
	NextCursor    *string `json:"next_cursor"`
	HasMore       bool    `json:"has_more"`
	TotalEstimate int     `json:"total_estimate"`
}

// ListAnchorsParams carries the validated request inputs. Cursor is the opaque
// token from a prior next_cursor; decoding/validation is the client's concern.
type ListAnchorsParams struct {
	TenantID string
	PageSize int
	Cursor   string
	Type     string
	Q        string
	AsOf     *time.Time
	// SystemAsOf projects the system-time (transaction-time) axis: the view as
	// the store knew it at this instant. nil selects the current view (rows whose
	// system interval is still open). Anchors-only; ledger entries carry no system
	// axis.
	SystemAsOf *time.Time
	// Principal is the resolved caller. The gRPC adapter mints the
	// x-calystral-principal JWT from it; the fixture only needs TenantID.
	Principal *auth.Principal
}

// ListAnchorsResult is one page of anchors plus the source tag.
type ListAnchorsResult struct {
	Items  []AnchorDTO
	Page   Page
	Source string
}

// LedgerSummary is a catalog entry describing one ledger (contract section 9.1).
// Only Name is always known; the rest are omitempty so a source that cannot yet
// supply them (the grpc catalog projects the name only) reports them ABSENT
// rather than present-and-zero. LastRecordedAt is a pointer because omitempty
// does not elide a zero time.Time: a nil pointer marshals to null (unknown), not
// a year-1 timestamp that reads as real data.
type LedgerSummary struct {
	Name               string     `json:"name"`
	Kind               string     `json:"kind,omitempty"`
	Description        string     `json:"description,omitempty"`
	EntryCountEstimate int        `json:"entry_count_estimate,omitempty"`
	LastLSN            int64      `json:"last_lsn,omitempty"`
	LastRecordedAt     *time.Time `json:"last_recorded_at,omitempty"`
}

// LedgerEntry is one append-only, bitemporal ledger entry (contract section
// 9.2). Times are UTC; a nil *time.Time marshals to JSON null per the "open"
// (EffectiveTo) / "first" (PrevLSN) conventions.
type LedgerEntry struct {
	ID            string         `json:"id"`
	Ledger        string         `json:"ledger"`
	Seq           int64          `json:"seq"`
	LSN           int64          `json:"lsn"`
	TxnID         int64          `json:"txn_id"`
	Kind          string         `json:"kind"`
	Summary       string         `json:"summary"`
	Actor         string         `json:"actor"`
	AnchorID      *string        `json:"node_id"`
	RecordedAt    time.Time      `json:"recorded_at"`
	EffectiveFrom time.Time      `json:"effective_from"`
	EffectiveTo   *time.Time     `json:"effective_to"`
	PrevLSN       *int64         `json:"prev_lsn"`
	Payload       map[string]any `json:"payload"`
}

// ListLedgersParams carries the validated inputs for the ledger catalog list.
type ListLedgersParams struct {
	TenantID string
	PageSize int
	Cursor   string
	Q        string
	// Principal is the resolved caller; the gRPC adapter mints the
	// x-calystral-principal JWT from it, the fixture only needs TenantID.
	Principal *auth.Principal
}

// ListLedgersResult is one page of ledger summaries plus the source tag.
type ListLedgersResult struct {
	Items  []LedgerSummary
	Page   Page
	Source string
}

// ListLedgerEntriesParams carries the validated inputs for a ledger's entry
// listing. FromLSN/ToLSN are inclusive bounds (nil => unbounded); when both are
// set, the caller has already enforced FromLSN <= ToLSN.
type ListLedgerEntriesParams struct {
	TenantID  string
	Name      string
	PageSize  int
	Cursor    string
	Kind      string
	Q         string
	AsOf      *time.Time
	FromLSN   *int64
	ToLSN     *int64
	Principal *auth.Principal
}

// ListLedgerEntriesResult is one page of ledger entries (descending lsn) plus
// the source tag.
type ListLedgerEntriesResult struct {
	Items  []LedgerEntry
	Page   Page
	Source string
}

// --- Cluster / shards (contract sections 11/12) ----------------------------
//
// The cluster view is an OPERATOR observability surface over the cvm cluster
// (per-shard Raft groups, key-range sharding, replicas across regions, storage
// tiers). Unlike anchors and ledgers it is LIVE state, NOT bitemporal: each DTO
// carries an `observed_at` snapshot instant and no valid/system time.

// ShardHealthCounts is the per-status shard tally on a ClusterSummary. All three
// keys are always present (zero when none).
type ShardHealthCounts struct {
	Healthy         int `json:"healthy"`
	Degraded        int `json:"degraded"`
	UnderReplicated int `json:"under_replicated"`
}

// RegionSummary is the per-region rollup carried on a ClusterSummary.
type RegionSummary struct {
	Name       string `json:"name"`
	NodeCount  int    `json:"node_count"`
	ShardCount int    `json:"shard_count"`
	Health     string `json:"health"`
}

// ClusterSummary is the cluster-wide observability rollup (contract section 11).
// Health is "healthy" or "degraded"; it is derived from the shard health tally
// and node status (any non-healthy shard or any non-"up" node => "degraded").
type ClusterSummary struct {
	NodeCount         int               `json:"node_count"`
	ShardCount        int               `json:"shard_count"`
	RegionCount       int               `json:"region_count"`
	ReplicationFactor int               `json:"replication_factor"`
	Health            string            `json:"health"`
	ShardHealth       ShardHealthCounts `json:"shard_health"`
	Regions           []RegionSummary   `json:"regions"`
	ObservedAt        time.Time         `json:"observed_at"`
}

// Raft role of a node in the cluster control-plane raft group (contract section
// 11). One of these four; "pre_candidate" is the PreVote phase before a node
// stands for election.
const (
	RaftRoleLeader       = "leader"
	RaftRoleFollower     = "follower"
	RaftRoleCandidate    = "candidate"
	RaftRolePreCandidate = "pre_candidate"
)

// NodeDTO is one cvm cluster node as the operator UI renders it (contract
// section 11). Times are UTC.
type NodeDTO struct {
	ID            string    `json:"id"`
	Address       string    `json:"address"`
	Region        string    `json:"region"`
	Status        string    `json:"status"`
	RaftRole      string    `json:"raft_role"`
	ShardCount    int       `json:"shard_count"`
	LeaderCount   int       `json:"leader_count"`
	RaftTerm      int       `json:"raft_term"`
	UsedBytes     int64     `json:"used_bytes"`
	CapacityBytes int64     `json:"capacity_bytes"`
	Version       string    `json:"version"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// KeyRange is a shard's half-open key span [start, end). A nil *string End is an
// unbounded upper edge (the final shard), marshaled to JSON null.
type KeyRange struct {
	Start string  `json:"start"`
	End   *string `json:"end"`
}

// ShardDTO is one Raft-group shard as the operator UI renders it (contract
// section 12). Lag is commit_index-applied_index and is always >= 0. Times are
// UTC. ReplicaNodeIDs is the full replica set (including the leader); it is
// shorter than ReplicationFactor exactly when the shard is under_replicated.
type ShardDTO struct {
	ID                string    `json:"id"`
	RaftGroupID       string    `json:"raft_group_id"`
	KeyRange          KeyRange  `json:"key_range"`
	Region            string    `json:"region"`
	LeaderNodeID      string    `json:"leader_node_id"`
	ReplicaNodeIDs    []string  `json:"replica_node_ids"`
	ReplicationFactor int       `json:"replication_factor"`
	Status            string    `json:"status"`
	RaftTerm          int       `json:"raft_term"`
	CommitIndex       int64     `json:"commit_index"`
	AppliedIndex      int64     `json:"applied_index"`
	Lag               int64     `json:"lag"`
	SizeBytes         int64     `json:"size_bytes"`
	Tier              string    `json:"tier"`
	ObservedAt        time.Time `json:"observed_at"`
}

// ClusterSummaryParams carries the validated inputs for the cluster rollup. The
// cluster is shared operator infrastructure, so it is not tenant-scoped; the
// Principal is still carried so the gRPC adapter can mint the principal JWT.
type ClusterSummaryParams struct {
	TenantID  string
	Principal *auth.Principal
}

// ClusterSummaryResult is the cluster rollup plus the source tag.
type ClusterSummaryResult struct {
	Summary ClusterSummary
	// Present is false when Core has no :Cluster node (an executed query with
	// zero rows). The handler renders that as the honest no-cluster-info shape
	// (summary:null) rather than a zero-valued rollup. Always true for fixtures.
	Present bool
	Source  string
}

// ListNodesParams carries the validated inputs for the node listing.
type ListNodesParams struct {
	TenantID  string
	PageSize  int
	Cursor    string
	Region    string
	Status    string
	Q         string
	Principal *auth.Principal
}

// ListNodesResult is one page of nodes (id asc) plus the source tag.
type ListNodesResult struct {
	Items  []NodeDTO
	Page   Page
	Source string
}

// ListShardsParams carries the validated inputs for the shard listing. Node, if
// set, matches shards where it is the leader OR appears in the replica set.
type ListShardsParams struct {
	TenantID  string
	PageSize  int
	Cursor    string
	Region    string
	Status    string
	Node      string
	Q         string
	Principal *auth.Principal
}

// ListShardsResult is one page of shards (id asc) plus the source tag.
type ListShardsResult struct {
	Items  []ShardDTO
	Page   Page
	Source string
}

// ClusterTopologyParams carries the validated inputs for the aggregated cluster
// topology read. Like the other cluster surfaces it is not tenant-scoped, but
// the Principal is carried so the gRPC adapter can mint the principal JWT for
// each replica it fans out to.
type ClusterTopologyParams struct {
	TenantID  string
	Principal *auth.Principal
}

// ClusterTopologyResult is the single-payload cluster view the BFF assembles by
// fanning out across all configured Core replicas: the derived rollup plus the
// unioned node and shard sets.
//
// Cluster reports whether more than one node was OBSERVED across the reachable
// replicas. It reflects observed membership, not declared deployment size: a
// multi-node cluster partitioned down to one reachable node reports Cluster=false
// (honest to what was seen). Summary is nil - and Nodes/Shards empty - when no
// replica has any cluster topology to report: a single-node Core, or (today) a
// cluster whose Core build does not yet serve topology over its gRPC surface.
// That nil/empty shape is the honest "no cluster info" state, NEVER a fabricated
// rollup. Source is "fixture" or "core".
type ClusterTopologyResult struct {
	Summary *ClusterSummary
	Nodes   []NodeDTO
	Shards  []ShardDTO
	Cluster bool
	Source  string
}

// --- Runtime state (contract sections 13/14/15) ----------------------------
//
// The runtime view is an OPERATOR observability surface over the cvm execution
// engine: VM metrics (the in-process Prometheus registry), the content-addressed
// plan cache, and the cybr opcode instruction set with execution profiling. Like
// the cluster view this is LIVE state, NOT bitemporal: every snapshot carries an
// `observed_at` instant. NOTE: per-opcode execution counts and the
// instructions-executed total are FORWARD-LOOKING telemetry - the interpreter
// does not tally them today - so the fixture seeds representative values behind
// the demo-data tag.

// HistogramBucket is one cumulative bucket of a histogram snapshot. UpperBound is
// the inclusive `le` bound in the metric's native unit (nanoseconds/bytes); a nil
// UpperBound marshals to JSON null and denotes the +Inf overflow bucket.
type HistogramBucket struct {
	UpperBound *uint64 `json:"upper_bound"`
	Count      uint64  `json:"count"`
}

// HistogramValue is a point-in-time histogram snapshot (cumulative buckets).
type HistogramValue struct {
	Buckets []HistogramBucket `json:"buckets"`
	Sum     uint64            `json:"sum"`
	Count   uint64            `json:"count"`
}

// MetricSeries is one named VM metric series. Kind is "counter", "gauge", or
// "histogram". For counters/gauges Value holds the scalar (counters are >= 0,
// gauges may be negative) and Histogram is nil; for histograms Histogram holds
// the snapshot and Value is nil.
type MetricSeries struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Help      string          `json:"help"`
	Value     *int64          `json:"value"`
	Histogram *HistogramValue `json:"histogram"`
}

// MetricGroup buckets the VM metric series by subsystem (e.g. storage,
// transactions, raft, calvin).
type MetricGroup struct {
	Subsystem string         `json:"subsystem"`
	Series    []MetricSeries `json:"series"`
}

// PlanCacheStats is the plan-cache rollup (mirrors the cvm CacheMetrics plus the
// configured byte budget). HitRateMilli is hits/(hits+misses) in per-mille
// (0..1000), and is 0 when there have been no lookups.
type PlanCacheStats struct {
	Hits          uint64 `json:"hits"`
	Misses        uint64 `json:"misses"`
	Inserts       uint64 `json:"inserts"`
	Evictions     uint64 `json:"evictions"`
	Entries       int    `json:"entries"`
	ResidentBytes uint64 `json:"resident_bytes"`
	CapacityBytes uint64 `json:"capacity_bytes"`
	HitRateMilli  int    `json:"hit_rate_milli"`
}

// RuntimeSummary is the cvm runtime observability rollup (contract section 13):
// grouped VM metric series, the plan-cache rollup, and headline execution
// counters. Live state; ObservedAt is the snapshot instant.
type RuntimeSummary struct {
	UptimeSeconds        int64          `json:"uptime_seconds"`
	InstructionsExecuted uint64         `json:"instructions_executed"`
	ActiveTransactions   int            `json:"active_transactions"`
	OpcodeCount          int            `json:"opcode_count"`
	MetricSeriesCount    int            `json:"metric_series_count"`
	PlanCache            PlanCacheStats `json:"plan_cache"`
	MetricGroups         []MetricGroup  `json:"metric_groups"`
	ObservedAt           time.Time      `json:"observed_at"`
}

// OpcodeDTO is one cybr VM opcode as the instruction-set view renders it
// (contract section 14). Code is the stable u16 discriminant and CodeHex its
// 0x-prefixed form. ExecCount/ExecShareMilli are forward-looking execution
// profiling (see the section note); ExecShareMilli is per-mille of all executed
// instructions (0..1000).
type OpcodeDTO struct {
	Mnemonic       string    `json:"mnemonic"`
	Code           int       `json:"code"`
	CodeHex        string    `json:"code_hex"`
	Category       string    `json:"category"`
	ShortForm      bool      `json:"short_form"`
	ExecCount      uint64    `json:"exec_count"`
	ExecShareMilli int       `json:"exec_share_milli"`
	ObservedAt     time.Time `json:"observed_at"`
}

// PlanCacheEntryDTO is one resident plan-cache entry (contract section 15). Key
// is the content-address (BLAKE3 of the bitcode) rendered as hex. SizeBytes is
// the evictable footprint; Cost is the deterministic recompute-cost proxy; Freq
// is the access frequency; RefCount is the number of referencing tenants.
type PlanCacheEntryDTO struct {
	Key        string    `json:"key"`
	SizeBytes  uint64    `json:"size_bytes"`
	Cost       uint64    `json:"cost"`
	Freq       uint64    `json:"freq"`
	RefCount   int       `json:"ref_count"`
	Pinned     bool      `json:"pinned"`
	ObservedAt time.Time `json:"observed_at"`
}

// RuntimeSummaryParams carries the validated inputs for the runtime rollup. The
// runtime is shared operator infrastructure, so it is not tenant-scoped; the
// Principal is still carried so the gRPC adapter can mint the principal JWT.
type RuntimeSummaryParams struct {
	TenantID  string
	Principal *auth.Principal
}

// RuntimeSummaryResult is the runtime rollup plus the source tag.
type RuntimeSummaryResult struct {
	Summary RuntimeSummary
	Source  string
}

// ListOpcodesParams carries the validated inputs for the opcode listing. Q
// matches the mnemonic (case-insensitive substring); Category, if set, is an
// exact category filter.
type ListOpcodesParams struct {
	TenantID  string
	PageSize  int
	Cursor    string
	Category  string
	Q         string
	Principal *auth.Principal
}

// ListOpcodesResult is one page of opcodes (code asc) plus the source tag.
type ListOpcodesResult struct {
	Items  []OpcodeDTO
	Page   Page
	Source string
}

// ListPlanCacheEntriesParams carries the validated inputs for the plan-cache
// entry listing. Pinned is "", "true", or "false" (an exact filter when set); Q
// matches the entry key (case-insensitive substring, for prefix lookups).
type ListPlanCacheEntriesParams struct {
	TenantID  string
	PageSize  int
	Cursor    string
	Pinned    string
	Q         string
	Principal *auth.Principal
}

// ListPlanCacheEntriesResult is one page of plan-cache entries (key asc) plus the
// source tag.
type ListPlanCacheEntriesResult struct {
	Items  []PlanCacheEntryDTO
	Page   Page
	Source string
}

// --- Streams / channels / subscriptions (contract sections 16/17/18) -------
//
// The messaging view is an OPERATOR observability surface over the cvm-channels
// runtime: durable channels (kind "stream" or "queue"), their live queue/
// ephemeral state, and the live subscriptions (the stream cursors). Like the
// cluster/runtime views this is LIVE state, NOT bitemporal: every snapshot
// carries an `observed_at` instant. cvm-channels exposes no enumeration accessor
// or gRPC surface today, so the fixture seeds a representative live set behind
// the demo-data tag; the gRPC path maps to 501 until Core grows the accessor.

// ChannelDTO is one cvm-channels channel as the operator UI renders it (contract
// section 17). A channel's Kind is "stream" (fan-out, subscription-driven) or
// "queue" (at-least-once, ack/nack). AckMode/VisibilityTimeoutSecs are queue-only
// (nil for streams); TTLSecs is set only for ephemeral channels. InFlight and
// Redelivery are queue delivery state (0 for streams).
type ChannelDTO struct {
	ID                    string    `json:"id"`
	Name                  string    `json:"name"`
	Tenant                string    `json:"tenant"`
	Kind                  string    `json:"kind"`
	Status                string    `json:"status"`
	Carries               string    `json:"carries"`
	Placement             string    `json:"placement"`
	PartitionCount        int       `json:"partition_count"`
	PartitionedBy         *string   `json:"partitioned_by"`
	RetentionSecs         int64     `json:"retention_secs"`
	AckMode               *string   `json:"ack_mode"`
	VisibilityTimeoutSecs *int64    `json:"visibility_timeout_secs"`
	Ephemeral             bool      `json:"ephemeral"`
	TTLSecs               *int64    `json:"ttl_secs"`
	EmitLSN               int64     `json:"emit_lsn"`
	InFlight              int       `json:"in_flight"`
	Redelivery            int       `json:"redelivery"`
	SubscriptionCount     int       `json:"subscription_count"`
	ObservedAt            time.Time `json:"observed_at"`
}

// SubscriptionDTO is one live stream cursor as the operator UI renders it
// (contract section 18). Start is "tail"|"offset"|"as_of"; Ordering is
// "per_partition"|"strictly_ordered"; Overflow is "drop_oldest"|"drop_newest"|
// "pause". Buffered is the running live-buffer depth; Lag is the channel emit-LSN
// minus the cursor head (always >= 0).
type SubscriptionDTO struct {
	ID               string    `json:"id"`
	ChannelID        string    `json:"channel_id"`
	ChannelName      string    `json:"channel_name"`
	Tenant           string    `json:"tenant"`
	Start            string    `json:"start"`
	Ordering         string    `json:"ordering"`
	Overflow         string    `json:"overflow"`
	BufferCapacity   int       `json:"buffer_capacity"`
	Buffered         int       `json:"buffered"`
	PartitionSpan    int       `json:"partition_span"`
	LiveFromLSN      int64     `json:"live_from_lsn"`
	Lag              int64     `json:"lag"`
	Dropped          int64     `json:"dropped"`
	OutOfSpanDropped int64     `json:"out_of_span_dropped"`
	ObservedAt       time.Time `json:"observed_at"`
}

// ChannelKindCounts is the per-kind channel tally on a MessagingSummary.
type ChannelKindCounts struct {
	Stream int `json:"stream"`
	Queue  int `json:"queue"`
}

// ChannelStatusCounts is the per-status channel tally on a MessagingSummary.
type ChannelStatusCounts struct {
	Open   int `json:"open"`
	Closed int `json:"closed"`
}

// MessagingSummary is the cvm-channels observability rollup (contract section
// 16): channel counts by kind/status, subscription aggregates, and the live
// cvm_channels_* metric series. Live state; ObservedAt is the snapshot instant.
type MessagingSummary struct {
	ChannelCount      int                 `json:"channel_count"`
	ByKind            ChannelKindCounts   `json:"by_kind"`
	ByStatus          ChannelStatusCounts `json:"by_status"`
	EphemeralCount    int                 `json:"ephemeral_count"`
	SubscriptionCount int                 `json:"subscription_count"`
	TotalBuffered     int                 `json:"total_buffered"`
	TotalInFlight     int                 `json:"total_in_flight"`
	TotalDropped      int64               `json:"total_dropped"`
	Metrics           []MetricSeries      `json:"metrics"`
	ObservedAt        time.Time           `json:"observed_at"`
}

// MessagingSummaryParams carries the validated inputs for the messaging rollup.
// Messaging is shared operator infrastructure, so it is not tenant-scoped; the
// Principal is still carried so the gRPC adapter can mint the principal JWT.
type MessagingSummaryParams struct {
	TenantID  string
	Principal *auth.Principal
}

// MessagingSummaryResult is the messaging rollup plus the source tag.
type MessagingSummaryResult struct {
	Summary MessagingSummary
	Source  string
}

// ListChannelsParams carries the validated inputs for the channel listing. Kind
// ("stream"/"queue") and Status ("open"/"closed"), if set, are exact filters; Q
// matches name/carries/placement (case-insensitive substring).
type ListChannelsParams struct {
	TenantID  string
	PageSize  int
	Cursor    string
	Kind      string
	Status    string
	Q         string
	Principal *auth.Principal
}

// ListChannelsResult is one page of channels (id asc) plus the source tag.
type ListChannelsResult struct {
	Items  []ChannelDTO
	Page   Page
	Source string
}

// ListSubscriptionsParams carries the validated inputs for the subscription
// listing. Channel (a channel id), Ordering, and Overflow, if set, are exact
// filters; Q matches the subscription id and channel name.
type ListSubscriptionsParams struct {
	TenantID  string
	PageSize  int
	Cursor    string
	Channel   string
	Ordering  string
	Overflow  string
	Q         string
	Principal *auth.Principal
}

// ListSubscriptionsResult is one page of subscriptions (id asc) plus the source
// tag.
type ListSubscriptionsResult struct {
	Items  []SubscriptionDTO
	Page   Page
	Source string
}

// GetAnchorHistoryParams identifies a single anchor whose full bitemporal
// version set is requested, tenant-scoped to the principal.
type GetAnchorHistoryParams struct {
	TenantID  string
	ID        string
	Principal *auth.Principal
}

// GetAnchorHistoryResult is every stored version of one anchor id (all valid-
// and system-time versions), ordered deterministically, plus the source tag.
type GetAnchorHistoryResult struct {
	Versions []AnchorDTO
	Source   string
}

// GetAnchorDiffParams resolves one anchor at two bitemporal coordinates. Each
// coordinate is a (valid-time, system-time) pair; a nil system instant selects
// the current/open version. The handler applies defaults (omitted valid axis =
// now) before calling.
type GetAnchorDiffParams struct {
	TenantID     string
	ID           string
	FromValidAt  time.Time
	FromSystemAt *time.Time
	ToValidAt    time.Time
	ToSystemAt   *time.Time
	Principal    *auth.Principal
}

// GetAnchorDiffResult is the anchor version resolved at each coordinate (nil
// when no version exists there) plus the source tag. The field-level delta is
// computed in the httpapi layer.
type GetAnchorDiffResult struct {
	FromVersion *AnchorDTO
	ToVersion   *AnchorDTO
	Source      string
}

// CreateAnchorParams is a new-anchor mutation. ValidFrom is optional (nil = the
// mutation instant). Properties may be nil (treated as empty).
type CreateAnchorParams struct {
	TenantID   string
	ID         string
	Type       string
	Label      string
	Properties map[string]any
	ValidFrom  *time.Time
	Principal  *auth.Principal
}

// CorrectAnchorParams is a system-time correction of an anchor's content. A nil
// Label/Properties means "unchanged"; a non-nil Properties is a FULL replace.
// ExpectedRevision, when set, is an optimistic-concurrency precondition on the
// current version's lsn.
type CorrectAnchorParams struct {
	TenantID         string
	ID               string
	Label            *string
	Properties       map[string]any
	ExpectedRevision *int64
	Principal        *auth.Principal
}

// CloseAnchorParams logically closes an anchor in valid-time. ValidTo is
// optional (nil = the mutation instant). ExpectedRevision is an optional precondition.
type CloseAnchorParams struct {
	TenantID         string
	ID               string
	ValidTo          *time.Time
	ExpectedRevision *int64
	Principal        *auth.Principal
}

// AnchorMutationResult is the resulting current version plus, for correct/close,
// the prior version that was superseded.
type AnchorMutationResult struct {
	Anchor     AnchorDTO
	Superseded *AnchorDTO
	Source     string
}

// CoreClient is the read-path port. CheckCore reports the readiness status the
// /readyz endpoint surfaces.
type CoreClient interface {
	ListAnchors(ctx context.Context, p ListAnchorsParams) (*ListAnchorsResult, error)
	GetAnchorHistory(ctx context.Context, p GetAnchorHistoryParams) (*GetAnchorHistoryResult, error)
	GetAnchorDiff(ctx context.Context, p GetAnchorDiffParams) (*GetAnchorDiffResult, error)
	GetNeighborhood(ctx context.Context, p NeighborhoodParams) (*NeighborhoodResult, error)
	CreateAnchor(ctx context.Context, p CreateAnchorParams) (*AnchorMutationResult, error)
	CorrectAnchor(ctx context.Context, p CorrectAnchorParams) (*AnchorMutationResult, error)
	CloseAnchor(ctx context.Context, p CloseAnchorParams) (*AnchorMutationResult, error)
	ListLedgers(ctx context.Context, p ListLedgersParams) (*ListLedgersResult, error)
	ListLedgerEntries(ctx context.Context, p ListLedgerEntriesParams) (*ListLedgerEntriesResult, error)
	ClusterSummary(ctx context.Context, p ClusterSummaryParams) (*ClusterSummaryResult, error)
	ListNodes(ctx context.Context, p ListNodesParams) (*ListNodesResult, error)
	ListShards(ctx context.Context, p ListShardsParams) (*ListShardsResult, error)
	ClusterTopology(ctx context.Context, p ClusterTopologyParams) (*ClusterTopologyResult, error)
	RuntimeSummary(ctx context.Context, p RuntimeSummaryParams) (*RuntimeSummaryResult, error)
	ListOpcodes(ctx context.Context, p ListOpcodesParams) (*ListOpcodesResult, error)
	ListPlanCacheEntries(ctx context.Context, p ListPlanCacheEntriesParams) (*ListPlanCacheEntriesResult, error)
	MessagingSummary(ctx context.Context, p MessagingSummaryParams) (*MessagingSummaryResult, error)
	ListChannels(ctx context.Context, p ListChannelsParams) (*ListChannelsResult, error)
	ListSubscriptions(ctx context.Context, p ListSubscriptionsParams) (*ListSubscriptionsResult, error)
	CheckCore(ctx context.Context) string
	Source() string
	Close() error
}

// cursorPayload is the BFF-minted opaque cursor (offset-based internally; the
// UI treats the encoded token as an opaque blob).
type cursorPayload struct {
	Offset int `json:"o"`
}

// encodeCursor mints a base64url cursor for the given offset.
func encodeCursor(offset int) string {
	b, _ := json.Marshal(cursorPayload{Offset: offset})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a cursor token into its offset. An empty token is offset
// 0 (first page). A malformed or negative token is an invalid_cursor error.
func decodeCursor(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, apierr.InvalidCursor(token)
	}
	var c cursorPayload
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, apierr.InvalidCursor(token)
	}
	if c.Offset < 0 {
		return 0, apierr.InvalidCursor(token)
	}
	return c.Offset, nil
}
