package coreclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
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

// stubQueryServer mirrors Core today: every Query returns UNIMPLEMENTED. It also
// records the inbound x-calystral-principal metadata so we can assert the BFF
// forwarded a minted JWT.
type stubQueryServer struct {
	querypb.UnimplementedQueryServiceServer
	gotPrincipal chan string
}

func (s *stubQueryServer) Query(ctx context.Context, _ *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		vals := md.Get(principalMetadataKey)
		if len(vals) > 0 && s.gotPrincipal != nil {
			select {
			case s.gotPrincipal <- vals[0]:
			default:
			}
		}
	}
	return nil, status.Error(codes.Unimplemented, "cvm opcode gap")
}

func startStubCore(t *testing.T) (addr string, principalCh chan string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	principalCh = make(chan string, 1)
	srv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(srv, &stubQueryServer{gotPrincipal: principalCh})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), principalCh
}

// errQueryServer fails every Query with a fixed error (to simulate Core codes
// other than UNIMPLEMENTED, e.g. a CyQL parse-reject INVALID_ARGUMENT).
type errQueryServer struct {
	querypb.UnimplementedQueryServiceServer
	err error
}

func (s *errQueryServer) Query(context.Context, *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	return nil, s.err
}

func startStubCoreErr(t *testing.T, err error) string {
	t.Helper()
	lis, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatalf("listen: %v", e)
	}
	srv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(srv, &errQueryServer{err: err})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// TestClusterTopologyParseRejectFoldsToNoClusterInfo proves the Cluster page
// degrades gracefully when Core rejects the topology CyQL with INVALID_ARGUMENT
// (the live cyqlc parse-coverage gap): it must fold to the honest no-cluster-info
// shape (200-able, empty), the SAME fold UNIMPLEMENTED gets - never a 500 or 502.
func TestClusterTopologyParseRejectFoldsToNoClusterInfo(t *testing.T) {
	addr := startStubCoreErr(t, status.Error(codes.InvalidArgument,
		"query parse error: unexpected trailing token after the query: UpOrder"))
	c := newTestGRPCClient(t, addr)

	res, err := c.ClusterTopology(context.Background(), ClusterTopologyParams{
		TenantID:  "demo-tenant",
		Principal: &auth.Principal{TenantID: "demo-tenant", Roles: []string{"reader"}},
	})
	if err != nil {
		t.Fatalf("parse-reject must fold, not surface: %v", err)
	}
	if res.Cluster || res.Summary != nil || len(res.Nodes) != 0 || len(res.Shards) != 0 {
		t.Fatalf("want no-cluster-info shape, got %+v", res)
	}
}

// TestListNodesParseRejectFoldsToUnimplemented proves a data read hitting the
// same parse-reject folds to 501 (not 500, not 502) and does not leak detail.
func TestListNodesParseRejectFoldsToUnimplemented(t *testing.T) {
	addr := startStubCoreErr(t, status.Error(codes.InvalidArgument,
		"query parse error: unexpected trailing token after the query: UpLimit"))
	c := newTestGRPCClient(t, addr)
	_, err := c.ListNodes(context.Background(), ListNodesParams{
		TenantID: "demo-tenant", PageSize: 25,
		Principal: &auth.Principal{TenantID: "demo-tenant", Roles: []string{"reader"}},
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("err = %v, want upstream/unimplemented (501)", err)
	}
	if ae.Params["surface"] != clusterNodesSurface {
		t.Errorf("surface = %v, want %q", ae.Params["surface"], clusterNodesSurface)
	}
	if contains(ae.Message, "UpLimit") {
		t.Errorf("wire message %q leaked parser detail", ae.Message)
	}
}

func newTestGRPCClient(t *testing.T, addr string) *GRPCClient {
	t.Helper()
	signer, err := auth.NewPrincipalSigner("")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := newGRPCClientWithConn(conn, signer, nil)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestGRPCListAnchorsMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "admin@demo", Roles: []string{"admin", "reader"}, AuditSessionID: "as_x"}
	_, err := c.ListAnchors(context.Background(), ListAnchorsParams{
		TenantID:  "demo-tenant",
		PageSize:  25,
		Principal: p,
	})
	if err == nil {
		t.Fatal("expected error from unimplemented core")
	}
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != anchorsSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], anchorsSurface)
	}

	// The BFF must have forwarded a minted principal JWT.
	select {
	case tok := <-principalCh:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe forwarded x-calystral-principal metadata")
	}
}

func TestGRPCNeighborhoodMapsUnimplemented(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	_, err := c.GetNeighborhood(context.Background(), NeighborhoodParams{
		TenantID:  "demo-tenant",
		ID:        "node_employee_0001",
		Principal: &auth.Principal{TenantID: "demo-tenant", Roles: []string{"reader"}},
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("err = %v, want unimplemented", err)
	}
	if ae.Params["surface"] != nodeNeighborhoodSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], nodeNeighborhoodSurface)
	}
}

func TestGRPCListAnchorsRejectsBadCursor(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	_, err := c.ListAnchors(context.Background(), ListAnchorsParams{
		TenantID:  "demo-tenant",
		PageSize:  25,
		Cursor:    "!!!bad!!!",
		Principal: &auth.Principal{TenantID: "demo-tenant"},
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestGRPCCheckCoreOK(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	if got := c.CheckCore(context.Background()); got != CheckOK {
		t.Fatalf("check = %q, want ok (reachable even though unimplemented)", got)
	}
}

func TestGRPCCheckCoreUnavailable(t *testing.T) {
	// Nothing listening on this address.
	signer, _ := auth.NewPrincipalSigner("")
	c, err := NewGRPCClient("127.0.0.1:1", signer, Options{})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if got := c.CheckCore(context.Background()); got != CheckUnavailable {
		t.Fatalf("check = %q, want unavailable", got)
	}
}

// TestCheckCoreLogsOnlyOnTransition asserts a persistent outage logs the "not
// ready" reason once (on the transition), not once per probe - so a long outage
// does not flood the logs.
func TestCheckCoreLogsOnlyOnTransition(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	signer, _ := auth.NewPrincipalSigner("")
	c, err := NewGRPCClient("127.0.0.1:1", signer, Options{Logger: logger})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	for range 3 {
		if got := c.CheckCore(context.Background()); got != CheckUnavailable {
			t.Fatalf("check = %q, want unavailable", got)
		}
	}
	if n := strings.Count(buf.String(), "core not ready"); n != 1 {
		t.Fatalf("not-ready warnings = %d, want exactly 1 (transition only); logs:\n%s", n, buf.String())
	}
}

func TestBuildListAnchorsCyQL(t *testing.T) {
	got := buildListAnchorsCyQL(ListAnchorsParams{PageSize: 10, Type: "Employee", Q: "ada"})
	for _, want := range []string{"MATCH", "Employee", "ada", "LIMIT 10"} {
		if !contains(got, want) {
			t.Errorf("cyql %q missing %q", got, want)
		}
	}
}

func TestMapCoreError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode apierr.Code
		// secret, when set, must NOT appear in the wire message (no upstream leak).
		secret string
	}{
		{"unimplemented_is_501", status.Error(codes.Unimplemented, "cvm opcode gap"), apierr.CodeUnimplemented, ""},
		// A CyQL parse-reject folds to 501 like UNIMPLEMENTED (same "not served
		// yet" gap), never a 500 or a retryable 502, and must not leak parser detail.
		{"invalid_argument_folds_to_501_no_leak", status.Error(codes.InvalidArgument, "query parse error: unexpected trailing token after the query: UpOrder"), apierr.CodeUnimplemented, "UpOrder"},
		{"unavailable_is_502", status.Error(codes.Unavailable, "core down"), apierr.CodeUnavailable, ""},
		{"deadline_is_502", status.Error(codes.DeadlineExceeded, "slow"), apierr.CodeUnavailable, ""},
		{"non_status_is_502", errors.New("raw transport failure"), apierr.CodeUnavailable, ""},
		// Auth denials / not-found / conflicts / Core-internal keep their distinct
		// 500 (not masked as a transient 502); still no upstream-detail leak.
		{"permission_denied_stays_500_no_leak", status.Error(codes.PermissionDenied, "secret upstream detail"), apierr.CodeInternal, "secret upstream detail"},
		{"failed_precondition_stays_500", status.Error(codes.FailedPrecondition, "stale lsn"), apierr.CodeInternal, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ae *apierr.APIError
			if !errors.As(mapCoreError(tc.err), &ae) {
				t.Fatalf("mapCoreError did not return *apierr.APIError")
			}
			if ae.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", ae.Code, tc.wantCode)
			}
			if tc.secret != "" && contains(ae.Message, tc.secret) {
				t.Errorf("wire message %q leaked upstream detail %q", ae.Message, tc.secret)
			}
		})
	}
}

func TestMapCoreMutateError(t *testing.T) {
	// The write path must NOT inherit the read path's INVALID_ARGUMENT -> 501
	// parser-gap fold: a rejected write payload / bad request is not a capability
	// gap. It stays 500 (until the precise per-code mutate mapping lands), and the
	// upstream detail is never leaked to the wire.
	var ae *apierr.APIError
	if !errors.As(mapCoreMutateError(status.Error(codes.InvalidArgument, "bad payload: secret detail"), anchorCreateSurface), &ae) {
		t.Fatalf("mapCoreMutateError did not return *apierr.APIError")
	}
	if ae.Code != apierr.CodeInternal {
		t.Fatalf("mutate INVALID_ARGUMENT code = %q, want internal (500, NOT the read 501 fold)", ae.Code)
	}
	if contains(ae.Message, "secret detail") {
		t.Errorf("wire message %q leaked upstream detail", ae.Message)
	}
	// Non-InvalidArgument codes map like the read path: UNIMPLEMENTED -> 501.
	if !errors.As(mapCoreMutateError(status.Error(codes.Unimplemented, "gap"), anchorCreateSurface), &ae) || ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("mutate UNIMPLEMENTED should map to 501, got %v", ae)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
