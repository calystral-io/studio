package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

func mkNode(id, region, status string) NodeDTO {
	return NodeDTO{ID: id, Region: region, Status: status, LastHeartbeat: time.Unix(1000, 0).UTC()}
}

func mkShard(id, region, status string, rf int) ShardDTO {
	return ShardDTO{ID: id, Region: region, Status: status, ReplicationFactor: rf, ObservedAt: time.Unix(2000, 0).UTC()}
}

func TestSummarizeTopologyDerivesRollupFromRows(t *testing.T) {
	nodes := []NodeDTO{
		mkNode("n2", "us-east", NodeUp),
		mkNode("n1", "eu-central", NodeUp),
		mkNode("n3", "us-east", NodeDraining), // non-up => degraded
	}
	shards := []ShardDTO{
		mkShard("s1", "eu-central", ShardHealthy, 3),
		mkShard("s2", "us-east", ShardUnderReplicated, 3),
	}
	sum := summarizeTopology(nodes, shards)

	if sum.NodeCount != 3 || sum.ShardCount != 2 {
		t.Fatalf("counts = nodes %d shards %d", sum.NodeCount, sum.ShardCount)
	}
	if sum.RegionCount != 2 {
		t.Errorf("region_count = %d, want 2 (derived from rows, not a seed constant)", sum.RegionCount)
	}
	if sum.ReplicationFactor != 3 {
		t.Errorf("replication_factor = %d, want 3 (max over shards)", sum.ReplicationFactor)
	}
	if sum.Health != HealthDegraded {
		t.Errorf("health = %q, want degraded (draining node + under-replicated shard)", sum.Health)
	}
	if sum.ShardHealth.Healthy != 1 || sum.ShardHealth.UnderReplicated != 1 {
		t.Errorf("shard_health = %+v", sum.ShardHealth)
	}
	// Regions are sorted by name and carry their own rollup + health.
	if len(sum.Regions) != 2 || sum.Regions[0].Name != "eu-central" || sum.Regions[1].Name != "us-east" {
		t.Fatalf("regions = %+v, want sorted [eu-central, us-east]", sum.Regions)
	}
	if sum.Regions[0].Health != HealthHealthy {
		t.Errorf("eu-central health = %q, want healthy", sum.Regions[0].Health)
	}
	if sum.Regions[1].Health != HealthDegraded {
		t.Errorf("us-east health = %q, want degraded", sum.Regions[1].Health)
	}
	// ObservedAt is the freshest signal (shard observed_at here).
	if !sum.ObservedAt.Equal(time.Unix(2000, 0).UTC()) {
		t.Errorf("observed_at = %v, want freshest row instant", sum.ObservedAt)
	}
}

func TestUnionDedupsAndSorts(t *testing.T) {
	nodes := unionNodes([]NodeDTO{mkNode("b", "r", NodeUp), mkNode("a", "r", NodeUp), mkNode("b", "r", NodeUp)})
	if len(nodes) != 2 || nodes[0].ID != "a" || nodes[1].ID != "b" {
		t.Fatalf("union nodes = %+v, want deduped + sorted [a,b]", nodes)
	}
	shards := unionShards([]ShardDTO{mkShard("s2", "r", ShardHealthy, 3), mkShard("s1", "r", ShardHealthy, 3), mkShard("s2", "r", ShardHealthy, 3)})
	if len(shards) != 2 || shards[0].ID != "s1" {
		t.Fatalf("union shards = %+v, want deduped + sorted", shards)
	}
}

func TestBuildTopologyNoClusterInfoShape(t *testing.T) {
	// No rows at all (single-node Core, or Core's topology gap today) -> the honest
	// no-cluster-info shape: nil Summary, empty (non-nil) sets, Cluster=false.
	res := buildTopology(nil, nil, SourceCore)
	if res.Summary != nil {
		t.Errorf("summary = %+v, want nil (never a fabricated rollup)", res.Summary)
	}
	if res.Nodes == nil || len(res.Nodes) != 0 || res.Shards == nil || len(res.Shards) != 0 {
		t.Errorf("nodes/shards must be empty non-nil, got %v / %v", res.Nodes, res.Shards)
	}
	if res.Cluster {
		t.Error("Cluster must be false with no nodes")
	}
	if res.Source != SourceCore {
		t.Errorf("source = %q", res.Source)
	}
}

func TestBuildTopologySingleNodeIsNotCluster(t *testing.T) {
	res := buildTopology([]NodeDTO{mkNode("solo", "r", NodeUp)}, nil, SourceCore)
	if res.Cluster {
		t.Error("a single node is not a cluster")
	}
	if res.Summary == nil || res.Summary.NodeCount != 1 {
		t.Fatalf("summary = %+v, want NodeCount 1", res.Summary)
	}
}

func TestBuildTopologyMultiNodeIsCluster(t *testing.T) {
	res := buildTopology([]NodeDTO{mkNode("a", "r", NodeUp), mkNode("b", "r", NodeUp)}, nil, SourceCore)
	if !res.Cluster {
		t.Error("two nodes is a cluster")
	}
	if res.Summary == nil || res.Summary.NodeCount != 2 {
		t.Fatalf("summary = %+v", res.Summary)
	}
}

// fakeReader is a clusterReader that returns canned pages or a canned error,
// used to drive the drain + fold-UNIMPLEMENTED paths without a real Core.
type fakeReader struct {
	nodePages  [][]NodeDTO
	shardPages [][]ShardDTO
	nodeErr    error
	shardErr   error
	nodeCalls  int
	shardCalls int
}

func ptr(s string) *string { return &s }

func (f *fakeReader) ListNodes(_ context.Context, _ ListNodesParams) (*ListNodesResult, error) {
	if f.nodeErr != nil {
		return nil, f.nodeErr
	}
	i := f.nodeCalls
	f.nodeCalls++
	page := f.nodePages[i]
	hasMore := i < len(f.nodePages)-1
	var next *string
	if hasMore {
		next = ptr("c")
	}
	return &ListNodesResult{Items: page, Page: Page{HasMore: hasMore, NextCursor: next}, Source: SourceCore}, nil
}

func (f *fakeReader) ListShards(_ context.Context, _ ListShardsParams) (*ListShardsResult, error) {
	if f.shardErr != nil {
		return nil, f.shardErr
	}
	i := f.shardCalls
	f.shardCalls++
	page := f.shardPages[i]
	hasMore := i < len(f.shardPages)-1
	var next *string
	if hasMore {
		next = ptr("c")
	}
	return &ListShardsResult{Items: page, Page: Page{HasMore: hasMore, NextCursor: next}, Source: SourceCore}, nil
}

func TestFetchReplicaTopologyDrainsAllPages(t *testing.T) {
	fr := &fakeReader{
		nodePages:  [][]NodeDTO{{mkNode("a", "r", NodeUp)}, {mkNode("b", "r", NodeUp)}},
		shardPages: [][]ShardDTO{{mkShard("s1", "r", ShardHealthy, 3)}},
	}
	nodes, shards, err := fetchReplicaTopology(context.Background(), fr, ClusterTopologyParams{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (both pages drained)", len(nodes))
	}
	if fr.nodeCalls != 2 {
		t.Errorf("node list calls = %d, want 2", fr.nodeCalls)
	}
	if len(shards) != 1 {
		t.Errorf("shards = %d, want 1", len(shards))
	}
}

func TestFetchReplicaTopologyFoldsUnimplemented(t *testing.T) {
	// UNIMPLEMENTED is folded to empty (not an error): the replica simply has no
	// topology to contribute.
	fr := &fakeReader{nodeErr: apierr.Unimplemented(clusterNodesSurface), shardErr: apierr.Unimplemented(clusterShardsSurface)}
	nodes, shards, err := fetchReplicaTopology(context.Background(), fr, ClusterTopologyParams{})
	if err != nil {
		t.Fatalf("UNIMPLEMENTED must fold to empty, got err %v", err)
	}
	if len(nodes) != 0 || len(shards) != 0 {
		t.Errorf("want empty, got %v / %v", nodes, shards)
	}
}

func TestFetchReplicaTopologyPropagatesUnavailable(t *testing.T) {
	// A real transport failure must NOT be folded away - it propagates.
	fr := &fakeReader{nodeErr: apierr.Unavailable(clusterNodesSurface)}
	_, _, err := fetchReplicaTopology(context.Background(), fr, ClusterTopologyParams{})
	if err == nil {
		t.Fatal("expected unavailable to propagate")
	}
	if c, _ := apiErrCode(err); c != apierr.CodeUnavailable {
		t.Errorf("code = %q, want unavailable", c)
	}
}

func TestFetchReplicaTopologyFoldsShardUnimplemented(t *testing.T) {
	// Node listing succeeds; shard listing is UNIMPLEMENTED. The shard side must
	// fold to empty (not error), and the node rows are kept.
	fr := &fakeReader{
		nodePages: [][]NodeDTO{{mkNode("a", "r", NodeUp)}},
		shardErr:  apierr.Unimplemented(clusterShardsSurface),
	}
	nodes, shards, err := fetchReplicaTopology(context.Background(), fr, ClusterTopologyParams{})
	if err != nil {
		t.Fatalf("shard UNIMPLEMENTED must fold, got err %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("nodes = %d, want 1 (node side kept)", len(nodes))
	}
	if len(shards) != 0 {
		t.Errorf("shards = %d, want 0 (shard side folded)", len(shards))
	}
}

func TestFetchReplicaTopologyPropagatesShardUnavailable(t *testing.T) {
	// Node listing succeeds; shard listing is Unavailable. That is a real failure
	// on the shard side and must propagate, not fold.
	fr := &fakeReader{
		nodePages: [][]NodeDTO{{mkNode("a", "r", NodeUp)}},
		shardErr:  apierr.Unavailable(clusterShardsSurface),
	}
	_, _, err := fetchReplicaTopology(context.Background(), fr, ClusterTopologyParams{})
	if err == nil {
		t.Fatal("expected shard-side unavailable to propagate")
	}
	if c, _ := apiErrCode(err); c != apierr.CodeUnavailable {
		t.Errorf("code = %q, want unavailable", c)
	}
}

func TestUnionByIDDedupsPreservingFirst(t *testing.T) {
	// The shared generic keeps the first occurrence of a duplicate key.
	in := []NodeDTO{
		{ID: "x", Region: "first"},
		{ID: "x", Region: "second"},
		{ID: "y", Region: "only"},
	}
	out := unionByID(in, func(n NodeDTO) string { return n.ID })
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	for _, n := range out {
		if n.ID == "x" && n.Region != "first" {
			t.Errorf("dup kept %q, want the first occurrence", n.Region)
		}
	}
}

func TestUnionByIDShardInstantiation(t *testing.T) {
	// Exercise the generic on the ShardDTO type too (nodes covered above).
	out := unionShards([]ShardDTO{
		mkShard("s2", "r", ShardHealthy, 3),
		mkShard("s1", "r", ShardHealthy, 3),
		mkShard("s2", "r", ShardDegraded, 3), // dup id -> dropped (first wins)
	})
	if len(out) != 2 || out[0].ID != "s1" || out[1].ID != "s2" {
		t.Fatalf("union shards = %+v, want deduped+sorted [s1,s2]", out)
	}
	if out[1].Status != ShardHealthy {
		t.Errorf("dup kept %q, want first occurrence (healthy)", out[1].Status)
	}
}

func TestDrainPagesLogsAndStopsAtCap(t *testing.T) {
	// A replica that never stops paging must be bounded by the page cap (and not
	// loop forever). fetchPage always reports more, so drainPages hits the cap.
	calls := 0
	out, err := drainPages("nodes", func(_ string) ([]NodeDTO, *string, bool, error) {
		calls++
		next := "c"
		return []NodeDTO{mkNode("n", "r", NodeUp)}, &next, true, nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != clusterTopologyMaxPages {
		t.Errorf("fetched %d pages, want cap %d", calls, clusterTopologyMaxPages)
	}
	if len(out) != clusterTopologyMaxPages {
		t.Errorf("collected %d, want %d", len(out), clusterTopologyMaxPages)
	}
}
