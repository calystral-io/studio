// gRPC CoreClient: dials Core's QueryService.Query, mints and forwards the
// x-calystral-principal EdDSA JWT, and issues CyQL reads (Core models nodes as
// anchors internally). A surface Core has not yet implemented is mapped to the
// contract 501 /errors/upstream/unimplemented with its surface tag; we never
// fabricate rows - mirroring how Core itself reports the gap rather than faking
// a result.
package coreclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/corepb/mutatepb"
	"github.com/calystral-io/studio/internal/corepb/querypb"
)

// principalMetadataKey is the gRPC metadata key Core reads the principal from.
const principalMetadataKey = "x-calystral-principal"

// Contract surface tags for upstream errors (params.surface).
const (
	anchorsSurface          = "nodes"
	anchorHistorySurface    = "node_history"
	anchorDiffSurface       = "node_diff"
	nodeNeighborhoodSurface = "node_neighborhood"
	anchorCreateSurface     = "node_create"
	anchorCorrectSurface    = "node_correct"
	anchorCloseSurface      = "node_close"
	ledgersSurface          = "ledgers"
	ledgerEntriesSurface    = "ledger_entries"
	clusterSummarySurface   = "cluster_summary"
	clusterNodesSurface     = "cluster_nodes"
	clusterShardsSurface    = "cluster_shards"
	clusterTopologySurface  = "cluster_topology"
	runtimeSummarySurface   = "runtime_summary"
	opcodesSurface          = "runtime_opcodes"
	planCacheSurface        = "runtime_plan_cache"
	messagingSummarySurface = "messaging_summary"
	channelsSurface         = "messaging_channels"
	subscriptionsSurface    = "messaging_subscriptions"
)

// Options configures a GRPCClient. A zero Options dials Core in plaintext with a
// default logger, preserving the fixture/local behaviour; production sets TLS
// (Core's edge mandates mTLS) and a structured Logger.
type Options struct {
	// TLS, when non-nil, dials Core over mutual TLS; nil dials plaintext.
	TLS *TLSConfig
	// Logger receives readiness-check diagnostics; nil uses slog.Default().
	Logger *slog.Logger
}

// GRPCClient is the live Core adapter.
type GRPCClient struct {
	conn   *grpc.ClientConn
	query  querypb.QueryServiceClient
	mutate mutatepb.MutateServiceClient
	signer *auth.PrincipalSigner
	dialTO time.Duration
	logger *slog.Logger

	// readyMu guards lastReady, the previous CheckCore verdict, so readiness is
	// logged only when it transitions (avoiding one Warn per probe during an
	// outage). It starts optimistic (true) so the first failure still logs.
	readyMu   sync.Mutex
	lastReady bool
}

// NewGRPCClient dials addr (lazily; the connection is established on first use)
// and returns a CoreClient backed by Core's Query + Mutate services. When
// opts.TLS is set the dial uses mutual TLS; otherwise it is plaintext.
func NewGRPCClient(addr string, signer *auth.PrincipalSigner, opts Options) (*GRPCClient, error) {
	creds, err := transportCredentials(opts.TLS)
	if err != nil {
		return nil, fmt.Errorf("core transport credentials: %w", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial core %q: %w", addr, err)
	}
	return &GRPCClient{
		conn:      conn,
		query:     querypb.NewQueryServiceClient(conn),
		mutate:    mutatepb.NewMutateServiceClient(conn),
		signer:    signer,
		dialTO:    3 * time.Second,
		logger:    loggerOrDefault(opts.Logger),
		lastReady: true,
	}, nil
}

// newGRPCClientWithConn is a test seam binding an existing connection.
func newGRPCClientWithConn(conn *grpc.ClientConn, signer *auth.PrincipalSigner, logger *slog.Logger) *GRPCClient {
	return &GRPCClient{
		conn:      conn,
		query:     querypb.NewQueryServiceClient(conn),
		mutate:    mutatepb.NewMutateServiceClient(conn),
		signer:    signer,
		dialTO:    3 * time.Second,
		logger:    loggerOrDefault(logger),
		lastReady: true,
	}
}

// loggerOrDefault falls back to the process default logger so a nil never panics.
func loggerOrDefault(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}

// Source implements CoreClient.
func (c *GRPCClient) Source() string { return SourceCore }

// Close releases the gRPC connection.
func (c *GRPCClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CheckCore pings Core. A transport failure is "unavailable"; any application
// response (including UNIMPLEMENTED or UNAUTHENTICATED) means Core is reachable,
// hence "ok". The reason for an "unavailable" verdict (the underlying gRPC error
// - e.g. a TLS/mTLS handshake failure that surfaces here as a fast Unavailable)
// is logged, but only when readiness TRANSITIONS, so a persistent outage does
// not emit one Warn per probe. See recordReadiness.
func (c *GRPCClient) CheckCore(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, c.dialTO)
	defer cancel()
	_, err := c.query.Query(ctx, &querypb.QueryRequest{Cyql: "PING", Tenant: ""})

	ready, reason := true, ""
	switch {
	case err == nil:
		// reachable
	default:
		if st, ok := status.FromError(err); !ok {
			ready, reason = false, "non-status transport error: "+err.Error()
		} else if st.Code() == codes.Unavailable || st.Code() == codes.DeadlineExceeded {
			ready, reason = false, st.Code().String()+": "+st.Message()
		}
		// Any other code is an application-level status (UNIMPLEMENTED,
		// UNAUTHENTICATED, ...) which proves the request reached Core over a healthy
		// transport, so readiness holds.
	}

	c.recordReadiness(ready, reason)
	if ready {
		return CheckOK
	}
	return CheckUnavailable
}

// recordReadiness logs a readiness change once: the first probe that finds Core
// not ready logs a Warn carrying the reason (so an operator can see WHY /readyz
// is failing), recovery logs an Info, and repeats at the same verdict are
// silent - so a multi-day outage does not flood the logs.
func (c *GRPCClient) recordReadiness(ready bool, reason string) {
	c.readyMu.Lock()
	changed := ready != c.lastReady
	c.lastReady = ready
	c.readyMu.Unlock()
	if !changed {
		return
	}
	if ready {
		c.logger.Info("core readiness restored: core reachable again", "target", c.conn.Target())
		return
	}
	c.logger.Warn("core readiness check failed: core not ready", "target", c.conn.Target(), "reason", reason)
}

// ListAnchors mints the principal JWT, issues the list-anchors CyQL read, and
// maps Core's response. Today the only mapped success path is UNIMPLEMENTED ->
// 501; the row-decode path is structured but explicitly TODO (no cybr decoder).
func (c *GRPCClient) ListAnchors(ctx context.Context, p ListAnchorsParams) (*ListAnchorsResult, error) {
	// Validate the cursor up front so source=grpc rejects bad cursors the same
	// way the fixture does (400), independent of the upstream gap.
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	token, err := c.signer.Mint(p.Principal)
	if err != nil {
		return nil, apierr.Internal(fmt.Sprintf("mint principal jwt: %v", err))
	}
	ctx = metadata.AppendToOutgoingContext(ctx, principalMetadataKey, token)

	req := &querypb.QueryRequest{
		Cyql:   buildListAnchorsCyQL(p),
		Tenant: p.Principal.TenantID,
	}
	if p.AsOf != nil {
		req.AsOfUnixMs = uint64(p.AsOf.UnixMilli())
	}
	// p.SystemAsOf (system-time/transaction-time projection) has no field on
	// querypb.QueryRequest yet, so it CANNOT be honored over gRPC. Now that we
	// surface rows, we must refuse it explicitly: silently dropping it would
	// serve the LATEST view mislabeled as the historical system-time projection
	// the user scrubbed to. Report the gap instead. (Valid-time AsOf is safe: it
	// rides AsOfUnixMs and Core rejects a non-zero as_of -> folds to 501.)
	// TODO(system-time): thread p.SystemAsOf once the proto carries it.
	if p.SystemAsOf != nil {
		return nil, apierr.Unimplemented(anchorsSurface)
	}

	resp, err := c.query.Query(ctx, req)
	if err != nil {
		return nil, mapCoreError(err)
	}

	// Surface Core's rows. Core's v1 read wire carries only the node id per row
	// (RETURN n -> Int(node_id)); the richer anchor fields (type/label/
	// properties/bitemporal coords) populate once Core projects typed columns.
	// An empty result is an empty item list, not an error.
	ids, err := decodeNodeIDRows(resp.GetRows(), anchorsSurface)
	if err != nil {
		return nil, err
	}
	items := make([]AnchorDTO, len(ids))
	for i, id := range ids {
		items[i] = AnchorDTO{ID: id}
	}
	return &ListAnchorsResult{Items: items, Page: gRPCPage(p.PageSize, len(items)), Source: SourceCore}, nil
}

// GetAnchorHistory would return one anchor's full bitemporal version set from
// Core. Like every read surface it depends on the cvm read-pipeline + the
// (undesigned) anchor-row wire format, so it reports the gap as 501 today.
func (c *GRPCClient) GetAnchorHistory(ctx context.Context, p GetAnchorHistoryParams) (*GetAnchorHistoryResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}
	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}
	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildAnchorHistoryCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, anchorHistorySurface)
	}
	// TODO(decode): decode resp.Rows[*].Payload (cybr value bytes) into
	// every AnchorDTO version once the shared cybr decoder lands.
	_ = resp
	return nil, apierr.Unimplemented(anchorHistorySurface)
}

// GetAnchorDiff resolves one anchor at two bitemporal coordinates. A single
// query expresses both coordinates; Core resolves them server-side. Blocked on
// the shared row decoder, so it reports 501 after the round-trip today.
func (c *GRPCClient) GetAnchorDiff(ctx context.Context, p GetAnchorDiffParams) (*GetAnchorDiffResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}
	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}
	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildAnchorDiffCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, anchorDiffSurface)
	}
	// TODO(decode): decode both resolved versions + compute field deltas
	// once the shared cybr decoder lands.
	_ = resp
	return nil, apierr.Unimplemented(anchorDiffSurface)
}

// GetNeighborhood expands a seeded node neighborhood at a bitemporal coordinate.
// Wired to Core's query surface; reports the honest 501 gap
// (surface=node_neighborhood) after the round-trip until the row decoder lands.
func (c *GRPCClient) GetNeighborhood(ctx context.Context, p NeighborhoodParams) (*NeighborhoodResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}
	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}
	req := &querypb.QueryRequest{
		Cyql:   buildNeighborhoodCyQL(p),
		Tenant: p.Principal.TenantID,
	}
	if p.AsOf != nil {
		req.AsOfUnixMs = uint64(p.AsOf.UnixMilli())
	}
	// NOTE: like ListAnchors, p.SystemAsOf (system-time projection) has no field on
	// querypb.QueryRequest, so it cannot be honored over gRPC yet; the surface
	// returns 501 below regardless.
	// TODO(system-time): thread p.SystemAsOf once the proto carries it.
	resp, err := c.query.Query(ctx, req)
	if err != nil {
		return nil, mapCoreErrorForSurface(err, nodeNeighborhoodSurface)
	}
	// TODO(decode): decode the seed + neighbor + edge rows once the
	// shared cybr decoder lands.
	_ = resp
	return nil, apierr.Unimplemented(nodeNeighborhoodSurface)
}

// CreateAnchor issues a node-create mutation to Core's MutateService as a
// single-mutation transaction. The committed result cannot be surfaced until the
// shared cybr row decoder lands (symmetric with the reads), so it reports 501
// after the round-trip.
func (c *GRPCClient) CreateAnchor(ctx context.Context, p CreateAnchorParams) (*AnchorMutationResult, error) {
	resp, err := c.applyMutation(ctx, p.Principal,
		mutatepb.MutationKind_MUTATION_KIND_CREATE_NODE, encodeCreateNodePayload(p), anchorCreateSurface)
	if err != nil {
		return nil, err
	}
	return mutationResultTODO(resp, anchorCreateSurface)
}

// CorrectAnchor issues an in-place node-update (correction) mutation to Core.
func (c *GRPCClient) CorrectAnchor(ctx context.Context, p CorrectAnchorParams) (*AnchorMutationResult, error) {
	resp, err := c.applyMutation(ctx, p.Principal,
		mutatepb.MutationKind_MUTATION_KIND_UPDATE, encodeCorrectNodePayload(p), anchorCorrectSurface)
	if err != nil {
		return nil, err
	}
	return mutationResultTODO(resp, anchorCorrectSurface)
}

// CloseAnchor issues a node-close (logical delete) mutation to Core.
func (c *GRPCClient) CloseAnchor(ctx context.Context, p CloseAnchorParams) (*AnchorMutationResult, error) {
	resp, err := c.applyMutation(ctx, p.Principal,
		mutatepb.MutationKind_MUTATION_KIND_CLOSE, encodeCloseNodePayload(p), anchorCloseSurface)
	if err != nil {
		return nil, err
	}
	return mutationResultTODO(resp, anchorCloseSurface)
}

// applyMutation forwards a single-mutation transaction to Core's MutateService,
// minting + attaching the principal JWT and mapping the gRPC status. Core's
// Mutate handler is now live (it decodes the payload through Core's wire
// contract - ported to Go in internal/cybrwire - and commits), so the remaining
// write-path gap is host-side: the BFF cannot yet build a VALID payload because
// binding a tenant's string type/field/anchor names to the numeric ids the wire
// needs (schema id resolution) is unbuilt - Core's schema read returns definition
// source text, not an id map. Until that lands the interim payload below is
// non-cybr, and a live Core rejects it invalid_argument (see wire_contract_test.go),
// so this path is not yet exercised against real Core.
func (c *GRPCClient) applyMutation(
	ctx context.Context,
	principal *auth.Principal,
	kind mutatepb.MutationKind,
	payload []byte,
	surface string,
) (*mutatepb.MutateResponse, error) {
	if principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}
	ctx, err := c.withPrincipal(ctx, principal)
	if err != nil {
		return nil, err
	}
	resp, err := c.mutate.Mutate(ctx, &mutatepb.MutateRequest{
		Tenant:    principal.TenantID,
		Mutations: []*mutatepb.Mutation{{Kind: kind, Payload: payload}},
	})
	if err != nil {
		// NOTE: correct/close carry ExpectedLSN (optimistic-concurrency precondition).
		// The fixture maps a conflict to 412 precondition_failed; mapCoreMutateError
		// currently funnels an unmapped code (e.g. FailedPrecondition) to 500 and an
		// INVALID_ARGUMENT (rejected payload / bad write) also to 500 - deliberately
		// NOT the read path's 501 parser-gap fold. When Core's Mutate handler lands,
		// add codes.FailedPrecondition -> 412 and codes.InvalidArgument -> 400/409
		// (with expected/actual from the error detail) so the two backends agree.
		// TODO(mutate): map FailedPrecondition -> PreconditionFailed.
		return nil, mapCoreMutateError(err, surface)
	}
	return resp, nil
}

// mutationResultTODO handles a committed MutateResponse. The response now carries
// the committed txn id, commit LSN, and the created anchor ids (commit_lsn +
// created were added to the proto), so a create's result could be built from the
// request fields plus resp.Created/resp.CommitLsn without a read-back; a correct/
// close result still needs the superseded prior version, which depends on the read
// pipeline. This is unreachable until the write path can send a valid payload
// (schema id resolution), so it reports the honest gap rather than fabricate the
// committed anchor.
func mutationResultTODO(resp *mutatepb.MutateResponse, surface string) (*AnchorMutationResult, error) {
	// TODO(mutate): build a create's AnchorDTO from the request + resp
	// (Created/CommitLsn); read back the superseded prior for correct/close.
	_ = resp
	return nil, apierr.Unimplemented(surface)
}

// ListLedgers mints the principal JWT, issues the list-ledgers CyQL read, and
// maps Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; the
// row-decode path is structured but explicitly TODO (no cybr decoder).
func (c *GRPCClient) ListLedgers(ctx context.Context, p ListLedgersParams) (*ListLedgersResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err = c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildListLedgersCyQL(),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, ledgersSurface)
	}
	names, err := decodeLedgerNames(resp.GetRows())
	if err != nil {
		return nil, err
	}

	// Core projects the name but cannot yet sort/filter/limit a projected field,
	// so do it here over the full catalog (which is small). Only the name is on
	// the wire today; the other LedgerSummary fields (kind/description/last_lsn/...)
	// populate once Core projects typed columns.
	q := strings.ToLower(strings.TrimSpace(p.Q))
	names = slices.DeleteFunc(names, func(n string) bool {
		return q != "" && !strings.Contains(strings.ToLower(n), q)
	})
	slices.Sort(names)

	total := len(names)
	page := Page{PageSize: p.PageSize, TotalEstimate: total}
	items := []LedgerSummary{}
	if offset < total {
		end := min(offset+p.PageSize, total)
		for _, n := range names[offset:end] {
			items = append(items, LedgerSummary{Name: n})
		}
	}
	if offset+len(items) < total {
		page.HasMore = true
		cur := encodeCursor(offset + len(items))
		page.NextCursor = &cur
	}
	return &ListLedgersResult{Items: items, Page: page, Source: SourceCore}, nil
}

// ListLedgerEntries mints the principal JWT, issues the list-entries CyQL read,
// and maps Core's response. As with ListLedgers, the only mapped path today is
// UNIMPLEMENTED -> 501; we never fabricate entries.
func (c *GRPCClient) ListLedgerEntries(ctx context.Context, p ListLedgerEntriesParams) (*ListLedgerEntriesResult, error) {
	if p.FromLSN != nil && p.ToLSN != nil && *p.FromLSN > *p.ToLSN {
		return nil, apierr.InvalidLSNRange(*p.FromLSN, *p.ToLSN)
	}
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	req := &querypb.QueryRequest{
		Cyql:   buildListLedgerEntriesCyQL(p),
		Tenant: p.Principal.TenantID,
	}
	if p.AsOf != nil {
		req.AsOfUnixMs = uint64(p.AsOf.UnixMilli())
	}

	resp, err := c.query.Query(ctx, req)
	if err != nil {
		return nil, mapCoreErrorForSurface(err, ledgerEntriesSurface)
	}

	// Surface Core's rows. Core's v1 read wire carries only the entry node's id
	// per row (RETURN e -> Int(node_id)); the richer entry fields (lsn/kind/
	// summary/actor/bitemporal coords/payload) populate once Core projects typed
	// columns. Ledger is known from the request. Empty -> empty item list.
	ids, err := decodeNodeIDRows(resp.GetRows(), ledgerEntriesSurface)
	if err != nil {
		return nil, err
	}
	items := make([]LedgerEntry, len(ids))
	for i, id := range ids {
		items[i] = LedgerEntry{ID: id, Ledger: p.Name}
	}
	return &ListLedgerEntriesResult{Items: items, Page: gRPCPage(p.PageSize, len(items)), Source: SourceCore}, nil
}

// ClusterSummary mints the principal JWT, issues the cluster-status read, and
// maps Core's response. Core serves this projection (main #49+): the rollup is
// decoded from the summary column. A Core error still folds to 501; zero rows
// (no :Cluster node) is an honest empty rollup (Present=false), never fabricated.
func (c *GRPCClient) ClusterSummary(ctx context.Context, p ClusterSummaryParams) (*ClusterSummaryResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildClusterSummaryCyQL(),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, clusterSummarySurface)
	}

	// Surface the real rollup. `RETURN c.summary` projects the cluster node's
	// summary field (JSON text); decodeClusterSummary parses it. Zero rows (no
	// :Cluster node) yields Present=false — an honest empty rollup, not an error.
	summary, found, err := decodeClusterSummary(resp.GetRows())
	if err != nil {
		return nil, err
	}
	return &ClusterSummaryResult{Summary: summary, Present: found, Source: SourceCore}, nil
}

// ListNodes mints the principal JWT, issues the list-nodes read, and maps Core's
// response. Today the only mapped path is UNIMPLEMENTED -> 501; we never
// fabricate nodes.
func (c *GRPCClient) ListNodes(ctx context.Context, p ListNodesParams) (*ListNodesResult, error) {
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildListNodesCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, clusterNodesSurface)
	}

	// Surface Core's rows. Core's v1 read wire carries only the node id per row
	// (RETURN n -> Int(node_id)); the operational fields (address/region/status/
	// raft role/capacity/...) populate once Core projects typed columns.
	ids, err := decodeNodeIDRows(resp.GetRows(), clusterNodesSurface)
	if err != nil {
		return nil, err
	}
	items := make([]NodeDTO, len(ids))
	for i, id := range ids {
		items[i] = NodeDTO{ID: id}
	}
	return &ListNodesResult{Items: items, Page: gRPCPage(p.PageSize, len(items)), Source: SourceCore}, nil
}

// ListShards mints the principal JWT, issues the list-shards read, and maps
// Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; we never
// fabricate shards.
func (c *GRPCClient) ListShards(ctx context.Context, p ListShardsParams) (*ListShardsResult, error) {
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildListShardsCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, clusterShardsSurface)
	}

	// Surface Core's rows. Core's v1 read wire carries only the shard node id per
	// row (RETURN s -> Int(node_id)); the shard fields (key range/leader/replicas/
	// raft term/lag/...) populate once Core projects typed columns.
	ids, err := decodeNodeIDRows(resp.GetRows(), clusterShardsSurface)
	if err != nil {
		return nil, err
	}
	items := make([]ShardDTO, len(ids))
	for i, id := range ids {
		items[i] = ShardDTO{ID: id}
	}
	return &ListShardsResult{Items: items, Page: gRPCPage(p.PageSize, len(items)), Source: SourceCore}, nil
}

// RuntimeSummary mints the principal JWT, issues the runtime-state read, and maps
// Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; we never
// fabricate a rollup.
func (c *GRPCClient) RuntimeSummary(ctx context.Context, p RuntimeSummaryParams) (*RuntimeSummaryResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildRuntimeSummaryCyQL(),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, runtimeSummarySurface)
	}

	// TODO(decode): decode resp.Rows[*].Payload into RuntimeSummary.
	_ = resp
	return nil, apierr.Unimplemented(runtimeSummarySurface)
}

// ListOpcodes mints the principal JWT, issues the opcode-profile read, and maps
// Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; we never
// fabricate opcodes.
func (c *GRPCClient) ListOpcodes(ctx context.Context, p ListOpcodesParams) (*ListOpcodesResult, error) {
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildListOpcodesCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, opcodesSurface)
	}

	// TODO(decode): decode resp.Rows[*].Payload into OpcodeDTO.
	_ = resp
	return nil, apierr.Unimplemented(opcodesSurface)
}

// ListPlanCacheEntries mints the principal JWT, issues the plan-cache read, and
// maps Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; we
// never fabricate cache entries.
func (c *GRPCClient) ListPlanCacheEntries(ctx context.Context, p ListPlanCacheEntriesParams) (*ListPlanCacheEntriesResult, error) {
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildListPlanCacheCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, planCacheSurface)
	}

	// TODO(decode): decode resp.Rows[*].Payload into PlanCacheEntryDTO.
	_ = resp
	return nil, apierr.Unimplemented(planCacheSurface)
}

// MessagingSummary mints the principal JWT, issues the messaging-state read, and
// maps Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; we
// never fabricate a rollup.
func (c *GRPCClient) MessagingSummary(ctx context.Context, p MessagingSummaryParams) (*MessagingSummaryResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildMessagingSummaryCyQL(),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, messagingSummarySurface)
	}

	// TODO(decode): decode resp.Rows[*].Payload into MessagingSummary.
	_ = resp
	return nil, apierr.Unimplemented(messagingSummarySurface)
}

// ListChannels mints the principal JWT, issues the list-channels read, and maps
// Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; we never
// fabricate channels.
func (c *GRPCClient) ListChannels(ctx context.Context, p ListChannelsParams) (*ListChannelsResult, error) {
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildListChannelsCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, channelsSurface)
	}

	// TODO(decode): decode resp.Rows[*].Payload into ChannelDTO.
	_ = resp
	return nil, apierr.Unimplemented(channelsSurface)
}

// ListSubscriptions mints the principal JWT, issues the list-subscriptions read,
// and maps Core's response. Today the only mapped path is UNIMPLEMENTED -> 501;
// we never fabricate subscriptions.
func (c *GRPCClient) ListSubscriptions(ctx context.Context, p ListSubscriptionsParams) (*ListSubscriptionsResult, error) {
	if _, err := decodeCursor(p.Cursor); err != nil {
		return nil, err
	}
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}

	ctx, err := c.withPrincipal(ctx, p.Principal)
	if err != nil {
		return nil, err
	}

	resp, err := c.query.Query(ctx, &querypb.QueryRequest{
		Cyql:   buildListSubscriptionsCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, subscriptionsSurface)
	}

	// TODO(decode): decode resp.Rows[*].Payload into SubscriptionDTO.
	_ = resp
	return nil, apierr.Unimplemented(subscriptionsSurface)
}

// withPrincipal mints the dev principal JWT and appends it as the
// x-calystral-principal outgoing metadata Core reads.
func (c *GRPCClient) withPrincipal(ctx context.Context, p *auth.Principal) (context.Context, error) {
	token, err := c.signer.Mint(p)
	if err != nil {
		return nil, apierr.Internal(fmt.Sprintf("mint principal jwt: %v", err))
	}
	return metadata.AppendToOutgoingContext(ctx, principalMetadataKey, token), nil
}

// buildListLedgersCyQL renders a plausible CyQL read for the ledger catalog with
// the requested `q` filter. Core returns UNIMPLEMENTED regardless of the text.
func buildListLedgersCyQL() string {
	// Project the NAME (not the bare node, whose only wire value is a numeric id):
	// LedgerSummary is keyed by name. Core executes this projection today (Ch1),
	// but cannot yet ORDER BY / filter a PROJECTED field (that path traps on a
	// LOAD_PARAM slot), so no ORDER BY / WHERE / LIMIT here — the catalog is sorted,
	// q-filtered, and paginated client-side over the full name list (a catalog is
	// small). Core-side ordering/limit lands when the projection+order path does.
	return "MATCH (l:Ledger) RETURN l.name"
}

// buildListLedgerEntriesCyQL renders a plausible CyQL read for one ledger's
// entries (newest first) with the requested filters. Core returns UNIMPLEMENTED
// regardless of the text.
func buildListLedgerEntriesCyQL(p ListLedgerEntriesParams) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MATCH (e:LedgerEntry)-[:IN]->(l:Ledger {name: %q})", p.Name)
	var wheres []string
	if p.Kind != "" {
		wheres = append(wheres, fmt.Sprintf("e.kind = %q", p.Kind))
	}
	if p.FromLSN != nil {
		wheres = append(wheres, fmt.Sprintf("e.lsn >= %d", *p.FromLSN))
	}
	if p.ToLSN != nil {
		wheres = append(wheres, fmt.Sprintf("e.lsn <= %d", *p.ToLSN))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("e CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN e ORDER BY e.lsn DESC")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// buildClusterSummaryCyQL renders the CyQL read for the cluster-wide status
// rollup. Core serves this bare projection (main #49+): it returns the cluster
// node's summary field, which carries the rollup as JSON text.
func buildClusterSummaryCyQL() string {
	return "MATCH (c:Cluster) RETURN c.summary"
}

// buildListNodesCyQL renders a plausible CyQL read for the cluster nodes with
// the requested region/status/q filters. Core returns UNIMPLEMENTED regardless.
func buildListNodesCyQL(p ListNodesParams) string {
	var b strings.Builder
	b.WriteString("MATCH (n:ClusterNode)")
	var wheres []string
	if p.Region != "" {
		wheres = append(wheres, fmt.Sprintf("n.region = %q", p.Region))
	}
	if p.Status != "" {
		wheres = append(wheres, fmt.Sprintf("n.status = %q", p.Status))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("n CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN n ORDER BY n.id")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// buildListShardsCyQL renders a plausible CyQL read for the cluster shards with
// the requested region/status/node/q filters. Core returns UNIMPLEMENTED
// regardless of the text.
func buildListShardsCyQL(p ListShardsParams) string {
	var b strings.Builder
	b.WriteString("MATCH (s:Shard)")
	var wheres []string
	if p.Region != "" {
		wheres = append(wheres, fmt.Sprintf("s.region = %q", p.Region))
	}
	if p.Status != "" {
		wheres = append(wheres, fmt.Sprintf("s.status = %q", p.Status))
	}
	if p.Node != "" {
		// Contract `node` semantics are leader-OR-replica. This relies on the
		// invariant that a shard's leader is always a member of its replica set
		// (see ShardDTO docs), so replica membership alone is sufficient. If a
		// future Core ever stores the leader outside replica_node_ids, widen this
		// to also match s.leader_node_id.
		wheres = append(wheres, fmt.Sprintf("%q IN s.replica_node_ids", p.Node))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("s CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN s ORDER BY s.id")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// buildRuntimeSummaryCyQL renders a plausible CyQL read for the cvm runtime-state
// rollup (VM metrics + plan-cache stats). Core returns UNIMPLEMENTED regardless.
func buildRuntimeSummaryCyQL() string {
	return "MATCH (r:Runtime) RETURN r.summary"
}

// buildListOpcodesCyQL renders a plausible CyQL read for the opcode execution
// profile with the requested category/q filters. Core returns UNIMPLEMENTED
// regardless of the text.
func buildListOpcodesCyQL(p ListOpcodesParams) string {
	var b strings.Builder
	b.WriteString("MATCH (o:Opcode)")
	var wheres []string
	if p.Category != "" {
		wheres = append(wheres, fmt.Sprintf("o.category = %q", p.Category))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("o.mnemonic CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN o ORDER BY o.code")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// buildListPlanCacheCyQL renders a plausible CyQL read for the plan-cache entries
// with the requested pinned/q filters. Core returns UNIMPLEMENTED regardless.
func buildListPlanCacheCyQL(p ListPlanCacheEntriesParams) string {
	var b strings.Builder
	b.WriteString("MATCH (e:PlanCacheEntry)")
	var wheres []string
	// Only a valid boolean becomes a CyQL clause; any other value is a
	// match-nothing filter handled by the fixture, so we omit it here rather than
	// inject an unquoted, invalid bare token.
	if p.Pinned == "true" || p.Pinned == "false" {
		wheres = append(wheres, fmt.Sprintf("e.pinned = %s", p.Pinned))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("e.key CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN e ORDER BY e.key")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// buildMessagingSummaryCyQL renders a plausible CyQL read for the cvm-channels
// messaging rollup. Core returns UNIMPLEMENTED regardless of the text.
func buildMessagingSummaryCyQL() string {
	return "MATCH (m:Messaging) RETURN m.summary"
}

// buildListChannelsCyQL renders a plausible CyQL read for the channels with the
// requested kind/status/q filters. Core returns UNIMPLEMENTED regardless.
func buildListChannelsCyQL(p ListChannelsParams) string {
	var b strings.Builder
	b.WriteString("MATCH (c:Channel)")
	var wheres []string
	if p.Kind != "" {
		wheres = append(wheres, fmt.Sprintf("c.kind = %q", p.Kind))
	}
	if p.Status != "" {
		wheres = append(wheres, fmt.Sprintf("c.status = %q", p.Status))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("c CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN c ORDER BY c.id")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// buildListSubscriptionsCyQL renders a plausible CyQL read for the subscriptions
// with the requested channel/ordering/overflow/q filters. Core returns
// UNIMPLEMENTED regardless of the text.
func buildListSubscriptionsCyQL(p ListSubscriptionsParams) string {
	var b strings.Builder
	b.WriteString("MATCH (s:Subscription)")
	var wheres []string
	if p.Channel != "" {
		wheres = append(wheres, fmt.Sprintf("s.channel_id = %q", p.Channel))
	}
	if p.Ordering != "" {
		wheres = append(wheres, fmt.Sprintf("s.ordering = %q", p.Ordering))
	}
	if p.Overflow != "" {
		wheres = append(wheres, fmt.Sprintf("s.overflow = %q", p.Overflow))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("s CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN s ORDER BY s.id")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// mapCoreError translates a gRPC status into a contract error for the anchors
// surface. UNIMPLEMENTED and a CyQL parse-reject (INVALID_ARGUMENT) are both the
// honest "Core cannot serve this yet" gap (501); transport failures are 502.
func mapCoreError(err error) error {
	return mapCoreErrorForSurface(err, anchorsSurface)
}

// mapCoreErrorForSurface is mapCoreError parameterized by the contract surface
// tag so every read path reports its own params.surface.
func mapCoreErrorForSurface(err error, surface string) error {
	return mapCoreStatus(err, surface, true)
}

// mapCoreStatus is the shared body of the read and mutate mappers. It parses the
// gRPC status once and folds it to a contract error. foldInvalidArg selects the
// one read/write difference: on reads INVALID_ARGUMENT is Core's CyQL parser
// refusing a clause it does not yet cover, so it folds like UNIMPLEMENTED; on
// writes it is a rejected payload / bad request and stays a 500.
func mapCoreStatus(err error, surface string, foldInvalidArg bool) error {
	st, ok := status.FromError(err)
	if !ok {
		return apierr.Unavailable(surface)
	}
	switch st.Code() {
	case codes.Unimplemented:
		return apierr.Unimplemented(surface)
	case codes.InvalidArgument:
		if foldInvalidArg {
			// A read query rejected with INVALID_ARGUMENT today is Core's CyQL parser
			// refusing a clause it does not yet cover (ORDER BY / LIMIT / property
			// access) - the SAME "Core cannot serve this read yet" gap as
			// UNIMPLEMENTED, not a client error, a transient outage, or an internal
			// fault - so it gets the same fold: 501 on a data read, and (via
			// isUnimplemented) the honest no-cluster-info shape on cluster-topology.
			// Never an opaque 500, and never a retryable 502 that would invite a
			// retry storm against a read that cannot succeed until the parser lands.
			// The detail is logged, never wired.
			slog.Warn("core rejected read query (cyql parser-coverage gap); folding to unimplemented",
				"surface", surface, "detail", st.Message())
			return apierr.Unimplemented(surface)
		}
		// Write path: a rejected payload / bad request, not a capability gap.
		slog.Warn("core mutate rejected the request",
			"surface", surface, "grpc_code", st.Code().String(), "detail", st.Message())
		return apierr.Internal("")
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return apierr.Unavailable(surface)
	default:
		// Every other code keeps its own semantics rather than being masked as a
		// transient outage: auth denials, not-found, and optimistic-concurrency
		// conflicts must not read as "Core is down", and a genuine Core-internal
		// fault must stay distinguishable. An unmapped code stays a 500 (a precise
		// per-code mapping - 403/404/409/412 - lands with the mutate/write surfaces;
		// see the TODO in applyMutation). The detail is logged, never wired.
		slog.Warn("core call failed with unmapped status",
			"surface", surface, "grpc_code", st.Code().String(), "detail", st.Message())
		return apierr.Internal("")
	}
}

// mapCoreMutateError maps a write-path (Mutate) error. Unlike the read mapper it
// does NOT fold INVALID_ARGUMENT to 501: on a mutation an invalid argument is a
// rejected write payload / bad request, not a "surface unimplemented" capability
// gap, so it must never be masked as one (Core's live Mutate handler rejects the
// interim non-cybr payload with INVALID_ARGUMENT today, and a real bad write
// later). A precise 400/409/412 mapping lands with the mutate/write surfaces -
// see the TODO in applyMutation; until then an invalid argument stays a 500.
// Every other code maps exactly as the read mapper does.
// mapCoreMutateError maps a write-path (Mutate) error. Unlike the read mapper it
// does NOT fold INVALID_ARGUMENT to 501: on a mutation an invalid argument is a
// rejected write payload / bad request, not a "surface unimplemented" capability
// gap, so it must never be masked as one (Core's live Mutate handler rejects the
// interim non-cybr payload with INVALID_ARGUMENT today, and a real bad write
// later). A precise 400/409/412 mapping lands with the mutate/write surfaces -
// see the TODO in applyMutation; until then an invalid argument stays a 500.
// Every other code maps exactly as the read mapper does.
func mapCoreMutateError(err error, surface string) error {
	return mapCoreStatus(err, surface, false)
}

// buildListAnchorsCyQL renders a plausible CyQL read for node anchors with the
// requested filters. The exact dialect firms up with Core's execution surface;
// today Core returns UNIMPLEMENTED regardless of the text.
func buildListAnchorsCyQL(p ListAnchorsParams) string {
	var b strings.Builder
	b.WriteString("MATCH (n:Node)")
	var wheres []string
	if p.Type != "" {
		wheres = append(wheres, fmt.Sprintf("n.type = %q", p.Type))
	}
	if q := strings.TrimSpace(p.Q); q != "" {
		wheres = append(wheres, fmt.Sprintf("n CONTAINS %q", q))
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(wheres, " AND "))
	}
	b.WriteString(" RETURN n ORDER BY n.id")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
}

// buildAnchorHistoryCyQL renders a plausible CyQL read for every bitemporal
// version of one node (oldest system-time first). Core returns UNIMPLEMENTED
// regardless of the text; the dialect firms up with Core's execution surface.
func buildAnchorHistoryCyQL(p GetAnchorHistoryParams) string {
	return fmt.Sprintf("MATCH (n:Node {id: %q}) RETURN n ALL VERSIONS ORDER BY n.system_from", p.ID)
}

// buildAnchorDiffCyQL renders a plausible CyQL read expressing both bitemporal
// coordinates of a diff; Core resolves the two versions server-side. A nil
// system coordinate reads the current (open) system-time slice.
func buildAnchorDiffCyQL(p GetAnchorDiffParams) string {
	from := formatCoordinate(p.FromValidAt, p.FromSystemAt)
	to := formatCoordinate(p.ToValidAt, p.ToSystemAt)
	return fmt.Sprintf("MATCH (n:Node {id: %q}) RETURN DIFF(n %s, n %s)", p.ID, from, to)
}

// buildNeighborhoodCyQL renders a plausible one-hop neighborhood read seeded on a
// node, capped by the requested limit. The valid-time coordinate rides the
// QueryRequest.as_of_unix_ms field, not the text.
func buildNeighborhoodCyQL(p NeighborhoodParams) string {
	return fmt.Sprintf("MATCH (n:Node {id: %q})-[e]-(m:Node) RETURN n, e, m LIMIT %d", p.ID, p.Limit)
}

// formatCoordinate renders a bitemporal coordinate clause for the diff read.
func formatCoordinate(validAt time.Time, systemAt *time.Time) string {
	if systemAt != nil {
		return fmt.Sprintf("AS OF %s SYSTEM %s",
			validAt.UTC().Format(time.RFC3339), systemAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("AS OF %s", validAt.UTC().Format(time.RFC3339))
}

// --- Mutation payload encoding ------------------------------------------------
//
// MutateService carries each operation as opaque cybr value bytes. That codec is
// now ported to Go (internal/cybrwire, matching core/src/{proc/wire,mutate}.rs),
// but these encoders still emit deterministic JSON as an INTERIM placeholder
// because building a real cybr payload needs the numeric type/field/anchor ids
// the BFF cannot yet resolve (schema id resolution is unbuilt; Core's schema read
// returns source text, not an id map). Core's Mutate handler is live and rejects
// this non-cybr JSON invalid_argument. Swap these for cybrwire.EncodeMutation
// once name->id resolution + an any->Value conversion land; the encode/dispatch/
// decode path is already proven end-to-end in wire_contract_test.go.

func mutationPayload(op map[string]any) []byte {
	// json.Marshal sorts map keys, so the encoding is deterministic.
	b, _ := json.Marshal(op)
	return b
}

func encodeCreateNodePayload(p CreateAnchorParams) []byte {
	op := map[string]any{"id": p.ID, "type": p.Type, "label": p.Label}
	if len(p.Properties) > 0 {
		op["properties"] = p.Properties
	}
	if p.ValidFrom != nil {
		op["valid_from"] = p.ValidFrom.UTC().Format(time.RFC3339)
	}
	return mutationPayload(op)
}

func encodeCorrectNodePayload(p CorrectAnchorParams) []byte {
	op := map[string]any{"id": p.ID}
	if p.Label != nil {
		op["label"] = *p.Label
	}
	if len(p.Properties) > 0 {
		op["properties"] = p.Properties
	}
	if p.ExpectedLSN != nil {
		op["expected_lsn"] = *p.ExpectedLSN
	}
	return mutationPayload(op)
}

func encodeCloseNodePayload(p CloseAnchorParams) []byte {
	op := map[string]any{"id": p.ID}
	if p.ValidTo != nil {
		op["valid_to"] = p.ValidTo.UTC().Format(time.RFC3339)
	}
	if p.ExpectedLSN != nil {
		op["expected_lsn"] = *p.ExpectedLSN
	}
	return mutationPayload(op)
}
