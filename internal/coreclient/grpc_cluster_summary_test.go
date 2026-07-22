package coreclient

import (
	"context"
	"testing"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/corepb/querypb"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// summaryRow encodes a cluster-summary projection row: one column carrying the
// rollup as JSON text, exactly as Core's `RETURN c.summary` emits it.
func summaryRow(t *testing.T, jsonText string) *querypb.QueryRow {
	t.Helper()
	payload, err := cybrwire.EncodeValue(cybrwire.Array([]cybrwire.Value{cybrwire.Str(jsonText)}))
	if err != nil {
		t.Fatalf("encode summary row: %v", err)
	}
	return &querypb.QueryRow{Payload: payload}
}

func TestClusterSummarySurfacesRealRollup(t *testing.T) {
	const rollup = `{"node_count":3,"shard_count":1,"region_count":1,"replication_factor":3,` +
		`"health":"healthy","shard_health":{"healthy":1,"degraded":0,"under_replicated":0},` +
		`"regions":[{"name":"region-a","node_count":3,"shard_count":1,"health":"healthy"}],` +
		`"observed_at":"2026-07-22T09:00:00Z"}`
	addr := startStubCoreRows(t, []*querypb.QueryRow{summaryRow(t, rollup)})
	c := newTestGRPCClient(t, addr)

	res, err := c.ClusterSummary(context.Background(), ClusterSummaryParams{TenantID: "demo-tenant", Principal: reader()})
	if err != nil {
		t.Fatalf("ClusterSummary: %v", err)
	}
	if res.Source != SourceCore || !res.Present {
		t.Errorf("Source/Present = %q/%v, want core/true", res.Source, res.Present)
	}
	s := res.Summary
	if s.NodeCount != 3 || s.ShardCount != 1 || s.RegionCount != 1 || s.ReplicationFactor != 3 || s.Health != "healthy" {
		t.Fatalf("rollup mismatch: %+v", s)
	}
	if s.ShardHealth.Healthy != 1 || len(s.Regions) != 1 || s.Regions[0].Name != "region-a" {
		t.Fatalf("nested rollup mismatch: %+v", s)
	}
}

// No :Cluster node -> an executed query with zero rows -> honest empty rollup
// from core, never a 501.
func TestClusterSummaryEmptyIsEmptyRollup(t *testing.T) {
	addr := startStubCoreRows(t, nil)
	c := newTestGRPCClient(t, addr)

	res, err := c.ClusterSummary(context.Background(), ClusterSummaryParams{TenantID: "demo-tenant", Principal: reader()})
	if err != nil {
		t.Fatalf("empty summary must not error: %v", err)
	}
	if res.Source != SourceCore || res.Present || res.Summary.NodeCount != 0 {
		t.Fatalf("want Present=false empty rollup from core, got %+v", res)
	}
}

// A summary column that is not valid JSON is Core's fault -> internal error.
func TestClusterSummaryBadJSONIsInternal(t *testing.T) {
	addr := startStubCoreRows(t, []*querypb.QueryRow{summaryRow(t, "not json")})
	c := newTestGRPCClient(t, addr)

	_, err := c.ClusterSummary(context.Background(), ClusterSummaryParams{TenantID: "demo-tenant", Principal: reader()})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInternal {
		t.Fatalf("err = %v, want internal", err)
	}
}
