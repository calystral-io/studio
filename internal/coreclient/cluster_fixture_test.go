package coreclient

import (
	"context"
	"testing"

	"github.com/calystral-io/studio/internal/apierr"
)

func TestFixtureClusterSeedCounts(t *testing.T) {
	f := NewFixture()
	if got := f.NodeCount(); got != 9 {
		t.Errorf("node count = %d, want 9", got)
	}
	if got := f.ShardCount(); got != 144 {
		t.Errorf("shard count = %d, want 144", got)
	}
}

func TestFixtureClusterSummaryRollup(t *testing.T) {
	f := NewFixture()
	res, err := f.ClusterSummary(ctx(), ClusterSummaryParams{TenantID: FixtureTenant})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceFixture {
		t.Errorf("source = %q", res.Source)
	}
	s := res.Summary

	// Top-level counts match the seed exactly.
	if s.NodeCount != len(f.nodes) || s.NodeCount != 9 {
		t.Errorf("node_count = %d, want 9", s.NodeCount)
	}
	if s.ShardCount != len(f.shards) || s.ShardCount != 144 {
		t.Errorf("shard_count = %d, want 144", s.ShardCount)
	}
	if s.RegionCount != 3 {
		t.Errorf("region_count = %d, want 3", s.RegionCount)
	}
	if s.ReplicationFactor != clusterReplicationFactor {
		t.Errorf("replication_factor = %d, want %d", s.ReplicationFactor, clusterReplicationFactor)
	}

	// shard_health has all three keys and sums to the shard total; recompute from
	// the seeded shards so the rollup is verified against the rows.
	var healthy, degraded, under int
	for _, sh := range f.shards {
		switch sh.Status {
		case ShardHealthy:
			healthy++
		case ShardDegraded:
			degraded++
		case ShardUnderReplicated:
			under++
		default:
			t.Fatalf("shard %s has unexpected status %q", sh.ID, sh.Status)
		}
	}
	if s.ShardHealth.Healthy != healthy {
		t.Errorf("shard_health.healthy = %d, want %d", s.ShardHealth.Healthy, healthy)
	}
	if s.ShardHealth.Degraded != degraded {
		t.Errorf("shard_health.degraded = %d, want %d", s.ShardHealth.Degraded, degraded)
	}
	if s.ShardHealth.UnderReplicated != under {
		t.Errorf("shard_health.under_replicated = %d, want %d", s.ShardHealth.UnderReplicated, under)
	}
	if healthy+degraded+under != s.ShardCount {
		t.Errorf("shard_health sum %d != shard_count %d", healthy+degraded+under, s.ShardCount)
	}

	// The seed deliberately has unhealthy shards + a draining node, so the
	// cluster is degraded (non-healthy).
	if degraded == 0 || under == 0 {
		t.Fatalf("seed must include degraded(%d) and under_replicated(%d) shards", degraded, under)
	}
	if s.Health != HealthDegraded {
		t.Errorf("health = %q, want degraded", s.Health)
	}

	// Regions roll up consistently: one entry per region, node/shard counts sum
	// to the totals, and the names are the seeded set in order.
	if len(s.Regions) != 3 {
		t.Fatalf("regions = %d, want 3", len(s.Regions))
	}
	var rNodes, rShards int
	for i, r := range s.Regions {
		if r.Name != clusterRegions[i] {
			t.Errorf("region[%d] = %q, want %q", i, r.Name, clusterRegions[i])
		}
		if r.Health != HealthHealthy && r.Health != HealthDegraded {
			t.Errorf("region %s health = %q", r.Name, r.Health)
		}
		rNodes += r.NodeCount
		rShards += r.ShardCount
	}
	if rNodes != s.NodeCount {
		t.Errorf("region node_count sum %d != %d", rNodes, s.NodeCount)
	}
	if rShards != s.ShardCount {
		t.Errorf("region shard_count sum %d != %d", rShards, s.ShardCount)
	}
	if s.ObservedAt.IsZero() {
		t.Error("summary observed_at must be set")
	}
}

func TestFixtureClusterNodeShardCrossReference(t *testing.T) {
	f := NewFixture()
	nodeByID := map[string]NodeDTO{}
	for _, n := range f.nodes {
		nodeByID[n.ID] = n
	}

	// Recompute node rollups from the shards and assert they match the seeded
	// node fields, and that every leader/replica references a real node.
	wantShardCount := map[string]int{}
	wantLeaderCount := map[string]int{}
	for _, sh := range f.shards {
		if _, ok := nodeByID[sh.LeaderNodeID]; !ok {
			t.Errorf("shard %s leader %q is not a real node", sh.ID, sh.LeaderNodeID)
		}
		wantLeaderCount[sh.LeaderNodeID]++

		// The leader must be a member of the replica set.
		if !shardInvolvesNode(sh, sh.LeaderNodeID) {
			t.Errorf("shard %s leader %q not in replica set %v", sh.ID, sh.LeaderNodeID, sh.ReplicaNodeIDs)
		}
		// Replica set size relates to status; lag is non-negative and consistent.
		if sh.Status == ShardUnderReplicated {
			if len(sh.ReplicaNodeIDs) >= sh.ReplicationFactor {
				t.Errorf("shard %s under_replicated but has %d replicas (rf %d)", sh.ID, len(sh.ReplicaNodeIDs), sh.ReplicationFactor)
			}
		} else if len(sh.ReplicaNodeIDs) != sh.ReplicationFactor {
			t.Errorf("shard %s has %d replicas, want rf %d", sh.ID, len(sh.ReplicaNodeIDs), sh.ReplicationFactor)
		}
		if sh.Lag < 0 || sh.Lag != sh.CommitIndex-sh.AppliedIndex {
			t.Errorf("shard %s lag %d != commit %d - applied %d", sh.ID, sh.Lag, sh.CommitIndex, sh.AppliedIndex)
		}
		seen := map[string]bool{}
		for _, nid := range sh.ReplicaNodeIDs {
			if _, ok := nodeByID[nid]; !ok {
				t.Errorf("shard %s replica %q is not a real node", sh.ID, nid)
			}
			if seen[nid] {
				t.Errorf("shard %s has duplicate replica %q", sh.ID, nid)
			}
			seen[nid] = true
			wantShardCount[nid]++
		}
	}

	for _, n := range f.nodes {
		if n.ShardCount != wantShardCount[n.ID] {
			t.Errorf("node %s shard_count = %d, want %d (from shards)", n.ID, n.ShardCount, wantShardCount[n.ID])
		}
		if n.LeaderCount != wantLeaderCount[n.ID] {
			t.Errorf("node %s leader_count = %d, want %d (from shards)", n.ID, n.LeaderCount, wantLeaderCount[n.ID])
		}
		if n.UsedBytes > n.CapacityBytes {
			t.Errorf("node %s used %d exceeds capacity %d", n.ID, n.UsedBytes, n.CapacityBytes)
		}
	}
}

func TestFixtureClusterOneNodeDraining(t *testing.T) {
	f := NewFixture()
	draining := 0
	for _, n := range f.nodes {
		switch n.Status {
		case NodeUp, NodeDown, NodeDraining:
		default:
			t.Errorf("node %s has invalid status %q", n.ID, n.Status)
		}
		if n.Status == NodeDraining {
			draining++
		}
	}
	if draining != 1 {
		t.Errorf("draining nodes = %d, want 1", draining)
	}
}

func TestFixtureNodesPaginationWalksAll(t *testing.T) {
	f := NewFixture()
	const pageSize = 4
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	var lastTotal int
	var prevID string

	for {
		res, err := f.ListNodes(ctx(), ListNodesParams{PageSize: pageSize, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		lastTotal = res.Page.TotalEstimate
		for _, n := range res.Items {
			if seen[n.ID] {
				t.Fatalf("duplicate node across pages: %s", n.ID)
			}
			seen[n.ID] = true
			if prevID != "" && n.ID <= prevID {
				t.Fatalf("nodes not ascending: %s after %s", n.ID, prevID)
			}
			prevID = n.ID
		}
		if !res.Page.HasMore {
			if res.Page.NextCursor != nil {
				t.Error("next_cursor must be null when has_more is false")
			}
			break
		}
		if res.Page.NextCursor == nil {
			t.Fatal("has_more true but next_cursor nil")
		}
		cursor = *res.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 9 || lastTotal != 9 {
		t.Fatalf("walked %d unique (total %d), want 9", len(seen), lastTotal)
	}
	wantPages := (9 + pageSize - 1) / pageSize
	if pages != wantPages {
		t.Fatalf("walked %d pages, want %d", pages, wantPages)
	}
}

func TestFixtureShardsPaginationWalksAll(t *testing.T) {
	f := NewFixture()
	const pageSize = 25
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	var lastTotal int
	var prevID string

	for {
		res, err := f.ListShards(ctx(), ListShardsParams{PageSize: pageSize, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		lastTotal = res.Page.TotalEstimate
		if res.Page.PageSize != pageSize {
			t.Errorf("page_size echoed = %d", res.Page.PageSize)
		}
		for _, sh := range res.Items {
			if seen[sh.ID] {
				t.Fatalf("duplicate shard across pages: %s", sh.ID)
			}
			seen[sh.ID] = true
			if prevID != "" && sh.ID <= prevID {
				t.Fatalf("shards not ascending: %s after %s", sh.ID, prevID)
			}
			prevID = sh.ID
		}
		if !res.Page.HasMore {
			if res.Page.NextCursor != nil {
				t.Error("next_cursor must be null when has_more is false")
			}
			break
		}
		if res.Page.NextCursor == nil {
			t.Fatal("has_more true but next_cursor nil")
		}
		cursor = *res.Page.NextCursor
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 144 || lastTotal != 144 {
		t.Fatalf("walked %d unique (total %d), want 144", len(seen), lastTotal)
	}
	wantPages := (144 + pageSize - 1) / pageSize
	if pages != wantPages {
		t.Fatalf("walked %d pages, want %d", pages, wantPages)
	}
}

func TestFixtureNodesRegionAndStatusFilters(t *testing.T) {
	f := NewFixture()

	// Region filter: every node is in the requested region; counts sum to total.
	totalByRegion := 0
	for _, region := range clusterRegions {
		res, err := f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Region: region})
		if err != nil {
			t.Fatal(err)
		}
		if res.Page.TotalEstimate != 3 {
			t.Errorf("region %s node total = %d, want 3", region, res.Page.TotalEstimate)
		}
		for _, n := range res.Items {
			if n.Region != region {
				t.Errorf("node %s region = %q, want %q", n.ID, n.Region, region)
			}
		}
		totalByRegion += res.Page.TotalEstimate
	}
	if totalByRegion != 9 {
		t.Errorf("region node totals sum %d, want 9", totalByRegion)
	}

	// Status filter: exactly one draining node.
	res, err := f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Status: NodeDraining})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 1 {
		t.Errorf("draining node total = %d, want 1", res.Page.TotalEstimate)
	}

	// Unknown status matches nothing (no error).
	res, err = f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Status: "nonsense"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 0 || len(res.Items) != 0 {
		t.Errorf("unknown status matched %d, want 0", res.Page.TotalEstimate)
	}

	// Unknown region matches nothing.
	res, err = f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Region: "mars"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 0 {
		t.Errorf("unknown region matched %d, want 0", res.Page.TotalEstimate)
	}
}

func TestFixtureNodesQuerySubstring(t *testing.T) {
	f := NewFixture()
	// Query over id+address+region: the region name is a known substring.
	res, err := f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Q: "us-east"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 3 {
		t.Errorf("q=us-east matched %d nodes, want 3", res.Page.TotalEstimate)
	}
	for _, n := range res.Items {
		if !nodeMatchesQuery(n, "us-east") {
			t.Errorf("node %s does not contain query term", n.ID)
		}
	}

	// Query over node id.
	res, err = f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Q: "node-0001"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 1 || res.Items[0].ID != "node-0001" {
		t.Errorf("q=node-0001 matched %d, want exactly node-0001", res.Page.TotalEstimate)
	}

	// Query over the address branch: an address octet matches neither id nor
	// region, so this exercises the nodeMatchesQuery address term specifically.
	res, err = f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Q: "10.2.0.4"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 1 || res.Items[0].ID != "node-0004" {
		t.Errorf("q=10.2.0.4 matched %d, want exactly node-0004 (by address)", res.Page.TotalEstimate)
	}
}

func TestFixtureShardsRegionStatusFilters(t *testing.T) {
	f := NewFixture()

	totalByRegion := 0
	for _, region := range clusterRegions {
		res, err := f.ListShards(ctx(), ListShardsParams{PageSize: 200, Region: region})
		if err != nil {
			t.Fatal(err)
		}
		for _, sh := range res.Items {
			if sh.Region != region {
				t.Errorf("shard %s region = %q, want %q", sh.ID, sh.Region, region)
			}
		}
		totalByRegion += res.Page.TotalEstimate
	}
	if totalByRegion != 144 {
		t.Errorf("region shard totals sum %d, want 144", totalByRegion)
	}

	for _, status := range []string{ShardDegraded, ShardUnderReplicated} {
		res, err := f.ListShards(ctx(), ListShardsParams{PageSize: 200, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if res.Page.TotalEstimate == 0 {
			t.Errorf("expected some %s shards", status)
		}
		for _, sh := range res.Items {
			if sh.Status != status {
				t.Errorf("shard %s status = %q, want %q", sh.ID, sh.Status, status)
			}
		}
	}

	// Unknown status matches nothing.
	res, err := f.ListShards(ctx(), ListShardsParams{PageSize: 200, Status: "exploded"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 0 {
		t.Errorf("unknown status matched %d shards, want 0", res.Page.TotalEstimate)
	}
}

func TestFixtureShardsNodeFilter(t *testing.T) {
	f := NewFixture()
	const node = "node-0001"
	res, err := f.ListShards(ctx(), ListShardsParams{PageSize: 200, Node: node})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Fatal("expected node-0001 to lead or replicate some shards")
	}
	for _, sh := range res.Items {
		if !shardInvolvesNode(sh, node) {
			t.Errorf("shard %s does not involve %s", sh.ID, node)
		}
	}

	// The node filter total equals the node's seeded shard_count (leader is a
	// member of the replica set, so leader-OR-replica == replica membership).
	var nodeShardCount int
	for _, n := range f.nodes {
		if n.ID == node {
			nodeShardCount = n.ShardCount
		}
	}
	if res.Page.TotalEstimate != nodeShardCount {
		t.Errorf("node filter total = %d, want node shard_count %d", res.Page.TotalEstimate, nodeShardCount)
	}

	// Unknown node matches nothing.
	res, err = f.ListShards(ctx(), ListShardsParams{PageSize: 200, Node: "node-9999"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 0 {
		t.Errorf("unknown node matched %d shards, want 0", res.Page.TotalEstimate)
	}
}

func TestFixtureShardsQuerySubstring(t *testing.T) {
	f := NewFixture()
	// Query over the raft group id of a specific shard.
	res, err := f.ListShards(ctx(), ListShardsParams{PageSize: 200, Q: "rg_00007"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 1 || res.Items[0].ID != "shard-0007" {
		t.Errorf("q=rg_00007 matched %d, want exactly shard-0007", res.Page.TotalEstimate)
	}

	// Query over the key range edge of the first shard (start key_00000000).
	res, err = f.ListShards(ctx(), ListShardsParams{PageSize: 200, Q: "key_00000000"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Fatal("expected a key-range substring match for key_00000000")
	}

	// key_01000000 is shard-0000's END edge (and shard-0001's start). Asserting
	// shard-0000 is matched exercises shardMatchesQuery's End branch, which the
	// start-only query above does not reach.
	res, err = f.ListShards(ctx(), ListShardsParams{PageSize: 200, Q: "key_01000000"})
	if err != nil {
		t.Fatal(err)
	}
	var matchedFirst bool
	for _, sh := range res.Items {
		if sh.ID == shardID(0) {
			matchedFirst = true
		}
	}
	if !matchedFirst {
		t.Errorf("q=key_01000000 did not match shard-0000 via its End edge")
	}
}

func TestFixtureShardKeyRangeUnboundedTail(t *testing.T) {
	f := NewFixture()
	res, err := f.ListShards(ctx(), ListShardsParams{PageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one shard (the last by id) has an unbounded (null) upper edge.
	unbounded := 0
	var tail ShardDTO
	for _, sh := range res.Items {
		if sh.KeyRange.End == nil {
			unbounded++
			tail = sh
		}
	}
	if unbounded != 1 {
		t.Fatalf("unbounded-end shards = %d, want 1", unbounded)
	}
	if tail.ID != shardID(143) {
		t.Errorf("unbounded shard = %s, want %s", tail.ID, shardID(143))
	}
}

func TestFixtureClusterInvalidCursor(t *testing.T) {
	f := NewFixture()
	if _, err := f.ListNodes(ctx(), ListNodesParams{PageSize: 25, Cursor: "!!!bad!!!"}); err == nil {
		t.Fatal("expected invalid cursor from ListNodes")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
	if _, err := f.ListShards(ctx(), ListShardsParams{PageSize: 25, Cursor: "!!!bad!!!"}); err == nil {
		t.Fatal("expected invalid cursor from ListShards")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestFixtureClusterCursorBeyondEnd(t *testing.T) {
	f := NewFixture()
	res, err := f.ListShards(ctx(), ListShardsParams{PageSize: 25, Cursor: encodeCursor(1000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.Page.HasMore {
		t.Fatalf("expected empty terminal page, got %d items has_more=%v", len(res.Items), res.Page.HasMore)
	}

	// Same terminal-page behaviour on the nodes list.
	nres, err := f.ListNodes(ctx(), ListNodesParams{PageSize: 25, Cursor: encodeCursor(1000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(nres.Items) != 0 || nres.Page.HasMore {
		t.Fatalf("expected empty terminal node page, got %d items has_more=%v", len(nres.Items), nres.Page.HasMore)
	}
}

// Combined filters must AND together (region AND status), on both lists.
func TestFixtureClusterCombinedFilters(t *testing.T) {
	f := NewFixture()

	// The sole draining node (node-0005) lives in us-east, so us-east+draining
	// matches it and eu-central+draining matches nothing.
	res, err := f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Region: "us-east", Status: NodeDraining})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 1 || res.Items[0].ID != nodeID(5) {
		t.Errorf("us-east+draining matched %d, want exactly node-0005", res.Page.TotalEstimate)
	}
	res, err = f.ListNodes(ctx(), ListNodesParams{PageSize: 200, Region: "eu-central", Status: NodeDraining})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 0 {
		t.Errorf("eu-central+draining matched %d, want 0", res.Page.TotalEstimate)
	}

	// Region AND status on shards: every match satisfies both predicates, and the
	// count never exceeds the region-only count for that status.
	sres, err := f.ListShards(ctx(), ListShardsParams{PageSize: 200, Region: "eu-central", Status: ShardDegraded})
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range sres.Items {
		if sh.Region != "eu-central" || sh.Status != ShardDegraded {
			t.Errorf("shard %s = (%s,%s), want (eu-central,degraded)", sh.ID, sh.Region, sh.Status)
		}
	}
}

func TestFixtureClusterTopology(t *testing.T) {
	f := NewFixture()
	res, err := f.ClusterTopology(context.Background(), ClusterTopologyParams{})
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if res.Source != SourceFixture {
		t.Errorf("source = %q, want fixture", res.Source)
	}
	if !res.Cluster {
		t.Error("fixture is a multi-node cluster; Cluster must be true")
	}
	if res.Summary == nil || res.Summary.NodeCount != f.NodeCount() || res.Summary.ShardCount != f.ShardCount() {
		t.Fatalf("summary = %+v, want node/shard counts %d/%d", res.Summary, f.NodeCount(), f.ShardCount())
	}
	if len(res.Nodes) != f.NodeCount() || len(res.Shards) != f.ShardCount() {
		t.Errorf("rows = %d nodes / %d shards, want %d / %d", len(res.Nodes), len(res.Shards), f.NodeCount(), f.ShardCount())
	}
	// Returned slices are copies: mutating them must not corrupt the fixture seed.
	if len(res.Nodes) > 0 {
		res.Nodes[0].ID = "tampered"
		again, _ := f.ListNodes(context.Background(), ListNodesParams{PageSize: 1})
		if len(again.Items) > 0 && again.Items[0].ID == "tampered" {
			t.Error("ClusterTopology must return a copy, not the live seed slice")
		}
	}
}

func TestFixtureClusterTopologyDeepCopiesRegions(t *testing.T) {
	// The contract advertises a copy: mutating the returned summary's Regions must
	// not corrupt the seed (a shallow struct copy would still alias the backing
	// array).
	f := NewFixture()
	res, err := f.ClusterTopology(context.Background(), ClusterTopologyParams{})
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	if res.Summary == nil || len(res.Summary.Regions) == 0 {
		t.Fatal("expected a populated summary with regions")
	}
	res.Summary.Regions[0].Name = "tampered"

	again, _ := f.ClusterTopology(context.Background(), ClusterTopologyParams{})
	if again.Summary.Regions[0].Name == "tampered" {
		t.Error("summary.Regions must be a copy, not the live seed slice")
	}
	// The paginated summary endpoint must be untouched too.
	sum, _ := f.ClusterSummary(context.Background(), ClusterSummaryParams{})
	if sum.Summary.Regions[0].Name == "tampered" {
		t.Error("ClusterSummary seed corrupted via ClusterTopology's shared slice")
	}
}

func TestFixtureClusterSummaryDeepCopiesRegions(t *testing.T) {
	// ClusterSummary returns the same struct by value; mutating its Regions must
	// not corrupt the long-lived seed (guards the sibling of the ClusterTopology
	// copy fix).
	f := NewFixture()
	res, err := f.ClusterSummary(context.Background(), ClusterSummaryParams{})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(res.Summary.Regions) == 0 {
		t.Fatal("expected seeded regions")
	}
	res.Summary.Regions[0].Name = "tampered"

	again, _ := f.ClusterSummary(context.Background(), ClusterSummaryParams{})
	if again.Summary.Regions[0].Name == "tampered" {
		t.Error("ClusterSummary.Regions must be a copy, not the live seed slice")
	}
}

func TestFixtureClusterTopologyDeepCopiesReplicaIDs(t *testing.T) {
	// Each returned shard's ReplicaNodeIDs must be a copy too - the whole result
	// payload is mutation-safe, not just its top-level slices.
	f := NewFixture()
	res, err := f.ClusterTopology(context.Background(), ClusterTopologyParams{})
	if err != nil {
		t.Fatalf("topology: %v", err)
	}
	var idx int = -1
	for i, sh := range res.Shards {
		if len(sh.ReplicaNodeIDs) > 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("expected a shard with replicas")
	}
	res.Shards[idx].ReplicaNodeIDs[0] = "tampered"

	again, _ := f.ClusterTopology(context.Background(), ClusterTopologyParams{})
	if again.Shards[idx].ReplicaNodeIDs[0] == "tampered" {
		t.Error("shard ReplicaNodeIDs must be a copy, not the live seed slice")
	}
}
