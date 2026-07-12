package coreclient

import (
	"context"
	"errors"
	"net"
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
		{"unavailable_is_502", status.Error(codes.Unavailable, "core down"), apierr.CodeUnavailable, ""},
		{"deadline_is_502", status.Error(codes.DeadlineExceeded, "slow"), apierr.CodeUnavailable, ""},
		{"non_status_is_502", errors.New("raw transport failure"), apierr.CodeUnavailable, ""},
		{"unexpected_code_does_not_leak", status.Error(codes.PermissionDenied, "secret upstream detail"), apierr.CodeInternal, "secret upstream detail"},
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
