package coreclient

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/corepb/querypb"
)

// hangingQueryServer accepts the connection but never answers until its context
// is cancelled - a black-hole replica (partition / dropped packets), the case a
// connection-refused stub does NOT exercise.
type hangingQueryServer struct {
	querypb.UnimplementedQueryServiceServer
}

func (s *hangingQueryServer) Query(ctx context.Context, _ *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	<-ctx.Done()
	return nil, status.Error(codes.DeadlineExceeded, "hung")
}

func startHangingCore(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(srv, &hangingQueryServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func newTestFanout(t *testing.T, addrs []string) *FanoutClient {
	t.Helper()
	signer, err := auth.NewPrincipalSigner("")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fc, err := NewFanoutClient(addrs, signer, Options{})
	if err != nil {
		t.Fatalf("new fanout: %v", err)
	}
	t.Cleanup(func() { _ = fc.Close() })
	return fc
}

func readerPrincipal() *auth.Principal {
	return &auth.Principal{TenantID: "demo-tenant", Roles: []string{"reader"}}
}

func TestFanoutClusterTopologyFansOutToAllReplicas(t *testing.T) {
	// Three replicas, all returning UNIMPLEMENTED today. The fan-out queries every
	// one (asserted via the per-stub principal channel) and folds the union to the
	// honest no-cluster-info shape.
	addr1, ch1 := startStubCore(t)
	addr2, ch2 := startStubCore(t)
	addr3, ch3 := startStubCore(t)
	fc := newTestFanout(t, []string{addr1, addr2, addr3})

	res, err := fc.ClusterTopology(context.Background(), ClusterTopologyParams{
		TenantID:  "demo-tenant",
		Principal: readerPrincipal(),
	})
	if err != nil {
		t.Fatalf("topology err = %v", err)
	}
	if res.Summary != nil || res.Cluster || len(res.Nodes) != 0 || len(res.Shards) != 0 {
		t.Errorf("want no-cluster-info shape, got %+v", res)
	}
	if res.Source != SourceCore {
		t.Errorf("source = %q", res.Source)
	}

	// Every replica must have received a forwarded principal (proof of fan-out).
	for i, ch := range []chan string{ch1, ch2, ch3} {
		select {
		case tok := <-ch:
			if tok == "" {
				t.Errorf("replica %d got empty principal", i)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("replica %d was not queried (no forwarded principal)", i)
		}
	}
}

func TestFanoutClusterTopologyToleratesOneReplicaDown(t *testing.T) {
	// Two healthy stubs + one address with nothing listening. The unreachable
	// replica is skipped; the read still succeeds with the no-cluster-info shape
	// (>=1 replica reachable).
	addr1, _ := startStubCore(t)
	addr2, _ := startStubCore(t)
	fc := newTestFanout(t, []string{addr1, "127.0.0.1:1", addr2})

	res, err := fc.ClusterTopology(context.Background(), ClusterTopologyParams{
		TenantID:  "demo-tenant",
		Principal: readerPrincipal(),
	})
	if err != nil {
		t.Fatalf("one replica down must not fail the read, got %v", err)
	}
	if res.Cluster || res.Summary != nil {
		t.Errorf("want no-cluster-info shape, got %+v", res)
	}
}

func TestFanoutClusterTopologyAllReplicasDown(t *testing.T) {
	// No replica reachable -> 502 unavailable (an honest "can't tell you"), never a
	// fabricated empty-but-OK cluster.
	fc := newTestFanout(t, []string{"127.0.0.1:1", "127.0.0.1:2"})
	_, err := fc.ClusterTopology(context.Background(), ClusterTopologyParams{
		TenantID:  "demo-tenant",
		Principal: readerPrincipal(),
	})
	if err == nil {
		t.Fatal("expected unavailable when all replicas are down")
	}
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
	if ae.Params["surface"] != clusterTopologySurface {
		t.Errorf("surface = %v, want %q", ae.Params["surface"], clusterTopologySurface)
	}
}

func TestFanoutDelegatesNonClusterReadsToPrimary(t *testing.T) {
	// Embedding the primary means non-cluster reads (e.g. anchors) still flow to a
	// single replica and map UNIMPLEMENTED as before.
	addr1, _ := startStubCore(t)
	addr2, _ := startStubCore(t)
	fc := newTestFanout(t, []string{addr1, addr2})
	_, err := fc.ListAnchors(context.Background(), ListAnchorsParams{
		TenantID:  "demo-tenant",
		PageSize:  25,
		Principal: readerPrincipal(),
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("err = %v, want unimplemented from primary", err)
	}
}

func TestNewFanoutClientRejectsEmptyAddrs(t *testing.T) {
	signer, _ := auth.NewPrincipalSigner("")
	if _, err := NewFanoutClient(nil, signer, Options{}); err == nil {
		t.Fatal("expected error with no replica addresses")
	}
}

func TestFanoutClusterTopologySkipsHungReplica(t *testing.T) {
	// A black-hole replica (accepts the connection, never answers) must be bounded
	// by the per-replica deadline and skipped - not block the whole fan-out. With
	// a healthy replica present, the read still succeeds.
	old := replicaTopologyTimeout
	replicaTopologyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { replicaTopologyTimeout = old })

	good, _ := startStubCore(t)
	hung := startHangingCore(t)
	fc := newTestFanout(t, []string{good, hung})

	start := time.Now()
	res, err := fc.ClusterTopology(context.Background(), ClusterTopologyParams{
		TenantID:  "demo-tenant",
		Principal: readerPrincipal(),
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("hung replica must be skipped (healthy one reachable), got %v", err)
	}
	if res.Cluster || res.Summary != nil {
		t.Errorf("want no-cluster-info shape, got %+v", res)
	}
	// Bounded by the deadline, not hung indefinitely (generous ceiling for CI).
	if elapsed > 3*time.Second {
		t.Errorf("fan-out took %v - a hung replica blocked the request", elapsed)
	}
}

func TestFanoutAllReplicasHungReturns502(t *testing.T) {
	// Every replica black-holed -> bounded, then 502 (not an infinite hang).
	old := replicaTopologyTimeout
	replicaTopologyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { replicaTopologyTimeout = old })

	fc := newTestFanout(t, []string{startHangingCore(t), startHangingCore(t)})
	_, err := fc.ClusterTopology(context.Background(), ClusterTopologyParams{
		TenantID:  "demo-tenant",
		Principal: readerPrincipal(),
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeUnavailable {
		t.Fatalf("err = %v, want unavailable", err)
	}
}
