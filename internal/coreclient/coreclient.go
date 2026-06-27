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
	LSN        int64          `json:"lsn"`
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
type LedgerSummary struct {
	Name               string    `json:"name"`
	Kind               string    `json:"kind"`
	Description        string    `json:"description"`
	EntryCountEstimate int       `json:"entry_count_estimate"`
	LastLSN            int64     `json:"last_lsn"`
	LastRecordedAt     time.Time `json:"last_recorded_at"`
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
	AnchorID      *string        `json:"anchor_id"`
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

// NodeDTO is one cvm cluster node as the operator UI renders it (contract
// section 11). Times are UTC.
type NodeDTO struct {
	ID            string    `json:"id"`
	Address       string    `json:"address"`
	Region        string    `json:"region"`
	Status        string    `json:"status"`
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

// CoreClient is the read-path port. CheckCore reports the readiness status the
// /readyz endpoint surfaces.
type CoreClient interface {
	ListAnchors(ctx context.Context, p ListAnchorsParams) (*ListAnchorsResult, error)
	ListLedgers(ctx context.Context, p ListLedgersParams) (*ListLedgersResult, error)
	ListLedgerEntries(ctx context.Context, p ListLedgerEntriesParams) (*ListLedgerEntriesResult, error)
	ClusterSummary(ctx context.Context, p ClusterSummaryParams) (*ClusterSummaryResult, error)
	ListNodes(ctx context.Context, p ListNodesParams) (*ListNodesResult, error)
	ListShards(ctx context.Context, p ListShardsParams) (*ListShardsResult, error)
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
