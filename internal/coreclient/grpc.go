// gRPC CoreClient: dials Core's QueryService.Query, mints and forwards the
// x-calystral-principal EdDSA JWT, and issues a "list node anchors" CyQL read.
// Core returns UNIMPLEMENTED for every valid query today (a cvm opcode gap), so
// PR1 maps that honest gap to the contract 501 /errors/upstream/unimplemented
// with surface="anchors". We never fabricate rows - mirroring how Core itself
// reports the gap rather than faking a result.
package coreclient

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/corepb/querypb"
)

// principalMetadataKey is the gRPC metadata key Core reads the principal from.
const principalMetadataKey = "x-calystral-principal"

// Contract surface tags for upstream errors (params.surface).
const (
	anchorsSurface          = "anchors"
	ledgersSurface          = "ledgers"
	ledgerEntriesSurface    = "ledger_entries"
	clusterSummarySurface   = "cluster_summary"
	clusterNodesSurface     = "cluster_nodes"
	clusterShardsSurface    = "cluster_shards"
	runtimeSummarySurface   = "runtime_summary"
	opcodesSurface          = "runtime_opcodes"
	planCacheSurface        = "runtime_plan_cache"
	messagingSummarySurface = "messaging_summary"
	channelsSurface         = "messaging_channels"
	subscriptionsSurface    = "messaging_subscriptions"
)

// GRPCClient is the live Core adapter.
type GRPCClient struct {
	conn   *grpc.ClientConn
	query  querypb.QueryServiceClient
	signer *auth.PrincipalSigner
	dialTO time.Duration
}

// NewGRPCClient dials addr (lazily; the connection is established on first use)
// and returns a CoreClient backed by Core's QueryService.
func NewGRPCClient(addr string, signer *auth.PrincipalSigner) (*GRPCClient, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial core %q: %w", addr, err)
	}
	return &GRPCClient{
		conn:   conn,
		query:  querypb.NewQueryServiceClient(conn),
		signer: signer,
		dialTO: 3 * time.Second,
	}, nil
}

// newGRPCClientWithConn is a test seam binding an existing connection.
func newGRPCClientWithConn(conn *grpc.ClientConn, signer *auth.PrincipalSigner) *GRPCClient {
	return &GRPCClient{conn: conn, query: querypb.NewQueryServiceClient(conn), signer: signer, dialTO: 3 * time.Second}
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
// response (including UNIMPLEMENTED) means Core is reachable, hence "ok".
func (c *GRPCClient) CheckCore(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, c.dialTO)
	defer cancel()
	_, err := c.query.Query(ctx, &querypb.QueryRequest{Cyql: "PING", Tenant: ""})
	if err == nil {
		return CheckOK
	}
	st, ok := status.FromError(err)
	if !ok {
		return CheckUnavailable
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded:
		return CheckUnavailable
	default:
		return CheckOK
	}
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
	// NOTE: p.SystemAsOf (system-time/transaction-time projection) has no field
	// on querypb.QueryRequest yet, so it cannot be honored over gRPC. The anchors
	// surface returns 501 below regardless; wiring the system axis upstream needs
	// a proto field (e.g. system_as_of_unix_ms) plus Core support.
	// TODO(PR-core-decode): thread p.SystemAsOf once the proto carries it.

	resp, err := c.query.Query(ctx, req)
	if err != nil {
		return nil, mapCoreError(err)
	}

	// Core returned rows. PR1 has no cybr decoder, so we cannot honestly
	// surface them yet. Report the gap rather than fabricate anchors.
	// TODO(PR-core-decode): decode resp.Rows[*].Payload (cybr value bytes) into
	// AnchorDTO once the shared cybr decoder lands; then paginate here.
	_ = resp
	return nil, apierr.Unimplemented(anchorsSurface)
}

// ListLedgers mints the principal JWT, issues the list-ledgers CyQL read, and
// maps Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; the
// row-decode path is structured but explicitly TODO (no cybr decoder).
func (c *GRPCClient) ListLedgers(ctx context.Context, p ListLedgersParams) (*ListLedgersResult, error) {
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
		Cyql:   buildListLedgersCyQL(p),
		Tenant: p.Principal.TenantID,
	})
	if err != nil {
		return nil, mapCoreErrorForSurface(err, ledgersSurface)
	}

	// Core returned rows but PR2 has no cybr decoder, so we cannot honestly
	// surface them yet. Report the gap rather than fabricate ledgers.
	// TODO(PR-core-decode): decode resp.Rows[*].Payload into LedgerSummary.
	_ = resp
	return nil, apierr.Unimplemented(ledgersSurface)
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into LedgerEntry.
	_ = resp
	return nil, apierr.Unimplemented(ledgerEntriesSurface)
}

// ClusterSummary mints the principal JWT, issues the cluster-status read, and
// maps Core's response. Today the only mapped path is UNIMPLEMENTED -> 501; we
// never fabricate a rollup.
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into ClusterSummary.
	_ = resp
	return nil, apierr.Unimplemented(clusterSummarySurface)
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into NodeDTO.
	_ = resp
	return nil, apierr.Unimplemented(clusterNodesSurface)
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into ShardDTO.
	_ = resp
	return nil, apierr.Unimplemented(clusterShardsSurface)
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into RuntimeSummary.
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into OpcodeDTO.
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into PlanCacheEntryDTO.
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into MessagingSummary.
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into ChannelDTO.
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

	// TODO(PR-core-decode): decode resp.Rows[*].Payload into SubscriptionDTO.
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
func buildListLedgersCyQL(p ListLedgersParams) string {
	var b strings.Builder
	b.WriteString("MATCH (l:Ledger)")
	if q := strings.TrimSpace(p.Q); q != "" {
		fmt.Fprintf(&b, " WHERE l CONTAINS %q", q)
	}
	b.WriteString(" RETURN l ORDER BY l.name")
	fmt.Fprintf(&b, " LIMIT %d", p.PageSize)
	return b.String()
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

// buildClusterSummaryCyQL renders a plausible CyQL read for the cluster-wide
// status rollup. Core returns UNIMPLEMENTED regardless of the text.
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
// surface. UNIMPLEMENTED is the honest upstream gap (501); transport failures
// are 502 unavailable.
func mapCoreError(err error) error {
	return mapCoreErrorForSurface(err, anchorsSurface)
}

// mapCoreErrorForSurface is mapCoreError parameterized by the contract surface
// tag so every read path reports its own params.surface.
func mapCoreErrorForSurface(err error, surface string) error {
	st, ok := status.FromError(err)
	if !ok {
		return apierr.Unavailable(surface)
	}
	switch st.Code() {
	case codes.Unimplemented:
		return apierr.Unimplemented(surface)
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return apierr.Unavailable(surface)
	default:
		// An unexpected upstream code may carry internal detail in its message;
		// log it and return a generic envelope so nothing leaks on the wire (the
		// same non-leaky posture Core takes for its own unmapped status codes).
		slog.Warn("core query failed with unexpected status",
			"grpc_code", st.Code().String(), "detail", st.Message())
		return apierr.Internal("")
	}
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
