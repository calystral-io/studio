package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

func newTestFanout(t *testing.T, addrs []string) *FanoutClient {
	t.Helper()
	signer, err := auth.NewPrincipalSigner("")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	fc, err := NewFanoutClient(addrs, signer)
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
	if _, err := NewFanoutClient(nil, signer); err == nil {
		t.Fatal("expected error with no replica addresses")
	}
}
