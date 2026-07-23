package coreclient

import (
	"context"
	"encoding/json"
	"net"
	"testing"
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

// stubMutateServer mirrors Core's mutate path today: every Mutate returns
// UNIMPLEMENTED. It records the inbound request + principal so tests can assert
// the BFF dispatched a real, well-formed mutation.
type stubMutateServer struct {
	mutatepb.UnimplementedMutateServiceServer
	gotPrincipal chan string
	gotReq       chan *mutatepb.MutateRequest
}

func (s *stubMutateServer) Mutate(ctx context.Context, req *mutatepb.MutateRequest) (*mutatepb.MutateResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(principalMetadataKey); len(v) > 0 && s.gotPrincipal != nil {
			select {
			case s.gotPrincipal <- v[0]:
			default:
			}
		}
	}
	if s.gotReq != nil {
		select {
		case s.gotReq <- req:
		default:
		}
	}
	return nil, status.Error(codes.Unimplemented, "mutate handler not wired")
}

// startQueryMutateCore registers BOTH the query and mutate stubs (both returning
// UNIMPLEMENTED) so a test GRPCClient can exercise the read and write dispatch.
func startQueryMutateCore(t *testing.T) (addr string, principalCh chan string, mutateCh chan *mutatepb.MutateRequest) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	principalCh = make(chan string, 8)
	mutateCh = make(chan *mutatepb.MutateRequest, 8)
	srv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(srv, &stubQueryServer{gotPrincipal: principalCh})
	mutatepb.RegisterMutateServiceServer(srv, &stubMutateServer{gotPrincipal: principalCh, gotReq: mutateCh})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), principalCh, mutateCh
}

func readerPrincipalForDispatch() *auth.Principal {
	return &auth.Principal{TenantID: "demo-tenant", UserID: "admin@demo", Roles: []string{"admin", "reader", "writer"}}
}

func waitPrincipal(t *testing.T, ch chan string) {
	t.Helper()
	select {
	case tok := <-ch:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe a forwarded principal to Core")
	}
}

func TestGRPCAnchorReadsDispatchToCore(t *testing.T) {
	// history / diff / neighborhood now do a real Core round-trip (was a short
	// circuit): the principal is forwarded and UNIMPLEMENTED maps to the surface's
	// 501 rather than returning before ever calling Core.
	addr, principalCh, _ := startQueryMutateCore(t)
	c := newTestGRPCClient(t, addr)
	p := readerPrincipalForDispatch()
	now := time.Unix(1_700_000_000, 0).UTC()

	cases := []struct {
		name    string
		call    func() error
		surface string
	}{
		{"history", func() error {
			_, err := c.GetAnchorHistory(context.Background(), GetAnchorHistoryParams{TenantID: "demo-tenant", ID: "node_1", Principal: p})
			return err
		}, anchorHistorySurface},
		{"diff", func() error {
			_, err := c.GetAnchorDiff(context.Background(), GetAnchorDiffParams{TenantID: "demo-tenant", ID: "node_1", FromValidAt: now, ToValidAt: now, Principal: p})
			return err
		}, anchorDiffSurface},
		{"neighborhood", func() error {
			_, err := c.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: "demo-tenant", ID: "node_1", Limit: 50, AsOf: &now, Principal: p})
			return err
		}, nodeNeighborhoodSurface},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			ae, ok := err.(*apierr.APIError)
			if !ok || ae.Code != apierr.CodeUnimplemented {
				t.Fatalf("err = %v, want unimplemented", err)
			}
			if ae.Params["surface"] != tc.surface {
				t.Errorf("surface = %v, want %q", ae.Params["surface"], tc.surface)
			}
			waitPrincipal(t, principalCh)
		})
	}
}

func TestGRPCMutationsDispatchToCore(t *testing.T) {
	addr, principalCh, mutateCh := startQueryMutateCore(t)
	c := newTestGRPCClient(t, addr)
	p := readerPrincipalForDispatch()
	label := "Ada"
	lsn := int64(42)
	validTo := time.Unix(1_700_000_000, 0).UTC()

	cases := []struct {
		name     string
		call     func() error
		surface  string
		wantKind mutatepb.MutationKind
	}{
		{"create", func() error {
			_, err := c.CreateAnchor(context.Background(), CreateAnchorParams{TenantID: "demo-tenant", ID: "node_1", Type: "Employee", Label: "Ada", Principal: p})
			return err
		}, anchorCreateSurface, mutatepb.MutationKind_MUTATION_KIND_CREATE_NODE},
		{"correct", func() error {
			_, err := c.CorrectAnchor(context.Background(), CorrectAnchorParams{TenantID: "demo-tenant", ID: "node_1", Label: &label, ExpectedRevision: &lsn, Principal: p})
			return err
		}, anchorCorrectSurface, mutatepb.MutationKind_MUTATION_KIND_UPDATE},
		{"close", func() error {
			_, err := c.CloseAnchor(context.Background(), CloseAnchorParams{TenantID: "demo-tenant", ID: "node_1", ValidTo: &validTo, Principal: p})
			return err
		}, anchorCloseSurface, mutatepb.MutationKind_MUTATION_KIND_CLOSE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			ae, ok := err.(*apierr.APIError)
			if !ok || ae.Code != apierr.CodeUnimplemented {
				t.Fatalf("err = %v, want unimplemented", err)
			}
			if ae.Params["surface"] != tc.surface {
				t.Errorf("surface = %v, want %q", ae.Params["surface"], tc.surface)
			}
			waitPrincipal(t, principalCh)

			select {
			case req := <-mutateCh:
				if req.Tenant != "demo-tenant" {
					t.Errorf("tenant = %q", req.Tenant)
				}
				if len(req.Mutations) != 1 {
					t.Fatalf("mutations = %d, want 1 (single-mutation txn)", len(req.Mutations))
				}
				m := req.Mutations[0]
				if m.Kind != tc.wantKind {
					t.Errorf("kind = %v, want %v", m.Kind, tc.wantKind)
				}
				// Payload carries the operation (interim encoding) and includes the id.
				var op map[string]any
				if err := json.Unmarshal(m.Payload, &op); err != nil {
					t.Fatalf("payload not decodable: %v", err)
				}
				if op["id"] != "node_1" {
					t.Errorf("payload id = %v, want node_1", op["id"])
				}
			case <-time.After(2 * time.Second):
				t.Error("Core did not receive a Mutate request (write not dispatched)")
			}
		})
	}
}

func TestGRPCMutationsMissingPrincipal(t *testing.T) {
	addr, _, _ := startQueryMutateCore(t)
	c := newTestGRPCClient(t, addr)
	if _, err := c.CreateAnchor(context.Background(), CreateAnchorParams{ID: "n"}); err == nil {
		t.Fatal("expected error with nil principal")
	}
}

func TestBuildAnchorReadCyQL(t *testing.T) {
	hist := buildAnchorHistoryCyQL(GetAnchorHistoryParams{ID: "node_7"})
	for _, want := range []string{"MATCH", "node_7", "VERSIONS"} {
		if !contains(hist, want) {
			t.Errorf("history cyql %q missing %q", hist, want)
		}
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	sys := now
	diff := buildAnchorDiffCyQL(GetAnchorDiffParams{ID: "node_7", FromValidAt: now, ToValidAt: now, ToSystemAt: &sys})
	for _, want := range []string{"DIFF", "node_7", "AS OF", "SYSTEM"} {
		if !contains(diff, want) {
			t.Errorf("diff cyql %q missing %q", diff, want)
		}
	}
	nbr := buildNeighborhoodCyQL(NeighborhoodParams{ID: "node_7", Limit: 25})
	for _, want := range []string{"node_7", "LIMIT 25"} {
		if !contains(nbr, want) {
			t.Errorf("neighborhood cyql %q missing %q", nbr, want)
		}
	}
}

func TestEncodeMutationPayloadsAreDeterministic(t *testing.T) {
	p := CreateAnchorParams{ID: "n1", Type: "Employee", Label: "Ada", Properties: map[string]any{"z": 1, "a": 2}}
	a := encodeCreateNodePayload(p)
	b := encodeCreateNodePayload(p)
	if string(a) != string(b) {
		t.Errorf("create payload not deterministic:\n%s\n%s", a, b)
	}
	var op map[string]any
	if err := json.Unmarshal(a, &op); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if op["id"] != "n1" || op["type"] != "Employee" {
		t.Errorf("payload = %v", op)
	}
}
