package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

func TestGRPCClusterSummaryMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "admin@demo", Roles: []string{"admin", "reader"}, AuditSessionID: "as_c"}
	_, err := c.ClusterSummary(context.Background(), ClusterSummaryParams{TenantID: "demo-tenant", Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != clusterSummarySurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], clusterSummarySurface)
	}

	select {
	case tok := <-principalCh:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe forwarded x-calystral-principal metadata")
	}
}

func TestGRPCListNodesMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "reader@demo", Roles: []string{"reader"}, AuditSessionID: "as_n"}
	_, err := c.ListNodes(context.Background(), ListNodesParams{TenantID: "demo-tenant", PageSize: 25, Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != clusterNodesSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], clusterNodesSurface)
	}

	select {
	case tok := <-principalCh:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe forwarded x-calystral-principal metadata")
	}
}

func TestGRPCListShardsMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "reader@demo", Roles: []string{"reader"}, AuditSessionID: "as_s"}
	_, err := c.ListShards(context.Background(), ListShardsParams{TenantID: "demo-tenant", PageSize: 25, Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != clusterShardsSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], clusterShardsSurface)
	}

	select {
	case tok := <-principalCh:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe forwarded x-calystral-principal metadata")
	}
}

func TestGRPCClusterRejectBadCursor(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	p := &auth.Principal{TenantID: "demo-tenant"}

	if _, err := c.ListNodes(context.Background(), ListNodesParams{TenantID: "demo-tenant", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListNodes")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}

	if _, err := c.ListShards(context.Background(), ListShardsParams{TenantID: "demo-tenant", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListShards")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestBuildClusterCyQL(t *testing.T) {
	if got := buildClusterSummaryCyQL(); !contains(got, "MATCH") || !contains(got, "Cluster") {
		t.Errorf("summary cyql %q missing MATCH/Cluster", got)
	}

	gotN := buildListNodesCyQL(ListNodesParams{PageSize: 10, Region: "us-east", Status: "draining", Q: "node-0001"})
	for _, want := range []string{"ClusterNode", "us-east", "draining", "node-0001", "ORDER BY n.id", "LIMIT 10"} {
		if !contains(gotN, want) {
			t.Errorf("nodes cyql %q missing %q", gotN, want)
		}
	}

	gotS := buildListShardsCyQL(ListShardsParams{PageSize: 25, Region: "ap-south", Status: "degraded", Node: "node-0002", Q: "rg_00007"})
	for _, want := range []string{"Shard", "ap-south", "degraded", "node-0002", "rg_00007", "ORDER BY s.id", "LIMIT 25"} {
		if !contains(gotS, want) {
			t.Errorf("shards cyql %q missing %q", gotS, want)
		}
	}
}

func TestGRPCClusterTopologyNoClusterInfo(t *testing.T) {
	// Core returns UNIMPLEMENTED for the cluster reads today. ClusterTopology must
	// fold that into the honest no-cluster-info shape (200-able), NOT a 501 error:
	// an empty topology is the correct answer until Core serves one.
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	res, err := c.ClusterTopology(context.Background(), ClusterTopologyParams{
		TenantID:  "demo-tenant",
		Principal: &auth.Principal{TenantID: "demo-tenant", Roles: []string{"reader"}},
	})
	if err != nil {
		t.Fatalf("unexpected error (UNIMPLEMENTED must fold, not surface): %v", err)
	}
	if res.Summary != nil {
		t.Errorf("summary = %+v, want nil", res.Summary)
	}
	if res.Cluster {
		t.Error("Cluster must be false with no topology")
	}
	if len(res.Nodes) != 0 || len(res.Shards) != 0 {
		t.Errorf("want empty sets, got %v / %v", res.Nodes, res.Shards)
	}
	if res.Source != SourceCore {
		t.Errorf("source = %q, want core", res.Source)
	}

	// The principal JWT must still be forwarded to Core.
	select {
	case tok := <-principalCh:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe forwarded x-calystral-principal metadata")
	}
}

func TestGRPCClusterTopologyMissingPrincipal(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	_, err := c.ClusterTopology(context.Background(), ClusterTopologyParams{TenantID: "demo-tenant"})
	if err == nil {
		t.Fatal("expected error with nil principal")
	}
}
