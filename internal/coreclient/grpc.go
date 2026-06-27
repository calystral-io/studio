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

// anchorsSurface is the contract surface tag for anchor-listing errors.
const anchorsSurface = "anchors"

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

// mapCoreError translates a gRPC status into a contract error. UNIMPLEMENTED is
// the honest upstream gap (501); transport failures are 502 unavailable.
func mapCoreError(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return apierr.Unavailable(anchorsSurface)
	}
	switch st.Code() {
	case codes.Unimplemented:
		return apierr.Unimplemented(anchorsSurface)
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return apierr.Unavailable(anchorsSurface)
	default:
		return apierr.Internal(fmt.Sprintf("core query: %s", st.Message()))
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
