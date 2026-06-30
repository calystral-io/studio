// Fixture cluster source: a seeded, deterministic in-memory snapshot of a cvm
// cluster's live topology and health - 9 nodes across 3 regions and 144
// replication-factor-3 shards (per-shard Raft groups, key-range sharding,
// replicas spread one-per-region, Hot/Warm/Cold/Archive tiers). Honestly tagged
// source:"fixture", it gives the operator UI real paginated, filterable data in
// PR3 without a live Core. Unlike anchors/ledgers this is live state, not
// bitemporal: each DTO carries an `observed_at` snapshot instant. The summary is
// derived from the seeded nodes+shards so the rollup is always consistent with
// the rows, and the seed deliberately includes degraded/under-replicated shards
// and a draining node so the cluster health is non-"healthy".
package coreclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Cluster health values (summary + per-region).
const (
	HealthHealthy  = "healthy"
	HealthDegraded = "degraded"
)

// Node status values.
const (
	NodeUp       = "up"
	NodeDown     = "down"
	NodeDraining = "draining"
)

// Shard status values.
const (
	ShardHealthy         = "healthy"
	ShardDegraded        = "degraded"
	ShardUnderReplicated = "under_replicated"
)

// Storage tier values (weighted Hot > Warm > Cold > Archive in the seed).
const (
	TierHot     = "Hot"
	TierWarm    = "Warm"
	TierCold    = "Cold"
	TierArchive = "Archive"
)

// NodeCount returns the number of seeded nodes (test/diagnostic helper).
func (f *Fixture) NodeCount() int { return len(f.nodes) }

// ShardCount returns the number of seeded shards (test/diagnostic helper).
func (f *Fixture) ShardCount() int { return len(f.shards) }

// ClusterSummary returns the precomputed cluster rollup. The cluster is shared
// operator infrastructure, so it is not tenant-scoped.
func (f *Fixture) ClusterSummary(_ context.Context, _ ClusterSummaryParams) (*ClusterSummaryResult, error) {
	return &ClusterSummaryResult{Summary: f.summary, Source: SourceFixture}, nil
}

// ClusterTopology returns the seeded cluster as a single aggregate payload (the
// fixture is a fully-populated multi-node cluster, so Cluster is true). Copies
// of the node and shard slices are returned so callers cannot mutate the seed.
func (f *Fixture) ClusterTopology(_ context.Context, _ ClusterTopologyParams) (*ClusterTopologyResult, error) {
	summary := f.summary
	return &ClusterTopologyResult{
		Summary: &summary,
		Nodes:   append([]NodeDTO(nil), f.nodes...),
		Shards:  append([]ShardDTO(nil), f.shards...),
		Cluster: f.summary.NodeCount > 1,
		Source:  SourceFixture,
	}, nil
}

// ListNodes applies region/status/q filters, then cursor-paginates a stable id
// sort. The cluster is shared infrastructure, so results are not tenant-scoped.
func (f *Fixture) ListNodes(_ context.Context, p ListNodesParams) (*ListNodesResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]NodeDTO, 0, len(f.nodes))
	for _, n := range f.nodes {
		if p.Region != "" && n.Region != p.Region {
			continue
		}
		if p.Status != "" && n.Status != p.Status {
			continue
		}
		if q != "" && !nodeMatchesQuery(n, q) {
			continue
		}
		filtered = append(filtered, n)
	}

	// Stable, sortable order by opaque id.
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })

	total := len(filtered)
	items := []NodeDTO{}
	if offset < total {
		end := offset + p.PageSize
		if end > total {
			end = total
		}
		items = filtered[offset:end]
	}

	page := Page{PageSize: p.PageSize, TotalEstimate: total}
	if offset+len(items) < total {
		page.HasMore = true
		c := encodeCursor(offset + len(items))
		page.NextCursor = &c
	}

	return &ListNodesResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// ListShards applies region/status/node/q filters, then cursor-paginates a
// stable id sort. The `node` filter matches shards where the node is the leader
// OR appears in the replica set. Not tenant-scoped (shared infrastructure).
func (f *Fixture) ListShards(_ context.Context, p ListShardsParams) (*ListShardsResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]ShardDTO, 0, len(f.shards))
	for _, sh := range f.shards {
		if p.Region != "" && sh.Region != p.Region {
			continue
		}
		if p.Status != "" && sh.Status != p.Status {
			continue
		}
		if p.Node != "" && !shardInvolvesNode(sh, p.Node) {
			continue
		}
		if q != "" && !shardMatchesQuery(sh, q) {
			continue
		}
		filtered = append(filtered, sh)
	}

	// Stable, sortable order by opaque id.
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })

	total := len(filtered)
	items := []ShardDTO{}
	if offset < total {
		end := offset + p.PageSize
		if end > total {
			end = total
		}
		items = filtered[offset:end]
	}

	page := Page{PageSize: p.PageSize, TotalEstimate: total}
	if offset+len(items) < total {
		page.HasMore = true
		c := encodeCursor(offset + len(items))
		page.NextCursor = &c
	}

	return &ListShardsResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// shardInvolvesNode reports whether node is the shard's leader or one of its
// replicas (the contract `node` filter semantics).
func shardInvolvesNode(sh ShardDTO, node string) bool {
	if sh.LeaderNodeID == node {
		return true
	}
	for _, r := range sh.ReplicaNodeIDs {
		if r == node {
			return true
		}
	}
	return false
}

// nodeMatchesQuery reports whether q occurs in the node id, address, or region
// (case-insensitive substring).
func nodeMatchesQuery(n NodeDTO, q string) bool {
	return strings.Contains(strings.ToLower(n.ID), q) ||
		strings.Contains(strings.ToLower(n.Address), q) ||
		strings.Contains(strings.ToLower(n.Region), q)
}

// shardMatchesQuery reports whether q occurs in the shard id, raft group id, or
// either edge of the key range (case-insensitive substring).
func shardMatchesQuery(sh ShardDTO, q string) bool {
	if strings.Contains(strings.ToLower(sh.ID), q) ||
		strings.Contains(strings.ToLower(sh.RaftGroupID), q) ||
		strings.Contains(strings.ToLower(sh.KeyRange.Start), q) {
		return true
	}
	if sh.KeyRange.End != nil && strings.Contains(strings.ToLower(*sh.KeyRange.End), q) {
		return true
	}
	return false
}

// --- Seed data -------------------------------------------------------------

// clusterReplicationFactor is the target replica count for every shard. A shard
// holding fewer replicas than this is under_replicated.
const clusterReplicationFactor = 3

// clusterRegions are the three regions, listed in stable display order.
var clusterRegions = []string{"eu-central", "us-east", "ap-south"}

// seedCluster builds 9 nodes (3 per region) and 144 replication-factor-3 shards
// with leaders spread across nodes, one replica per region, weighted tiers, and
// a deliberate handful of degraded/under-replicated shards plus one draining
// node. The summary is then derived from the seeded rows so it always agrees
// with them. Node shard_count/leader_count are computed FROM the shards, so the
// node<->shard cross-references are consistent by construction.
func seedCluster() (nodes []NodeDTO, shards []ShardDTO, summary ClusterSummary) {
	observedAt := mustUTC("2026-06-27T09:00:00Z")

	// 3 nodes per region: node-0001..node-0003 eu-central, 0004..0006 us-east,
	// 0007..0009 ap-south. regionNodeIDs[r] holds the ids in that region.
	regionNodeIDs := make([][]string, len(clusterRegions))
	nodeRegion := map[string]int{}
	nodeIndex := map[string]int{}
	id := 0
	for r := range clusterRegions {
		for k := 0; k < 3; k++ {
			id++
			nid := nodeID(id)
			regionNodeIDs[r] = append(regionNodeIDs[r], nid)
			nodeRegion[nid] = r
			nodeIndex[nid] = id
		}
	}

	// Build the 144 shards first; node rollups are derived from them afterwards.
	const shardTotal = 144
	keyStep := 1 << 24 // even, gap-free key partition over a 32-bit-ish space
	for i := 0; i < shardTotal; i++ {
		// One candidate replica per region; the within-region pick varies so
		// leaders and replicas spread across all nodes.
		euNode := regionNodeIDs[0][i%3]
		usNode := regionNodeIDs[1][(i/3)%3]
		apNode := regionNodeIDs[2][(i/9)%3]
		byRegion := []string{euNode, usNode, apNode}

		// Leader region rotates so leadership is balanced across regions.
		leaderRegion := i % 3
		leader := byRegion[leaderRegion]

		replicas := []string{euNode, usNode, apNode}
		status := ShardHealthy

		switch {
		case i > 0 && i%29 == 0:
			// Under-replicated: drop one non-leader replica from the set.
			status = ShardUnderReplicated
			dropRegion := (leaderRegion + 1) % 3
			replicas = make([]string, 0, 2)
			for r, nid := range byRegion {
				if r == dropRegion {
					continue
				}
				replicas = append(replicas, nid)
			}
		case i > 0 && i%17 == 0:
			// Degraded: full replica set, but a follower is lagging/unhealthy.
			status = ShardDegraded
		}

		// Realistic raft term + commit/applied indices; lag is commit-applied and
		// is non-zero mainly on degraded shards (a few), with the occasional small
		// lag elsewhere.
		raftTerm := 4 + i%5
		applied := int64(1_000_000 + i*811)
		var lag int64
		switch {
		case status == ShardDegraded:
			lag = int64(1 + i%4)
		case i%13 == 0:
			lag = int64(i % 3)
		}
		commit := applied + lag

		// Key range: gap-free half-open [start,end); the final shard is unbounded.
		start := keyToken(i * keyStep)
		var end *string
		if i < shardTotal-1 {
			e := keyToken((i + 1) * keyStep)
			end = &e
		}

		tier := tierFor(i)
		shards = append(shards, ShardDTO{
			ID:                shardID(i),
			RaftGroupID:       raftGroupID(i),
			KeyRange:          KeyRange{Start: start, End: end},
			Region:            clusterRegions[leaderRegion],
			LeaderNodeID:      leader,
			ReplicaNodeIDs:    replicas,
			ReplicationFactor: clusterReplicationFactor,
			Status:            status,
			RaftTerm:          raftTerm,
			CommitIndex:       commit,
			AppliedIndex:      applied,
			Lag:               lag,
			SizeBytes:         sizeForTier(tier, i),
			Tier:              tier,
			ObservedAt:        observedAt,
		})
	}

	// Derive per-node rollups from the shards so they always agree.
	shardCountByNode := map[string]int{}
	leaderCountByNode := map[string]int{}
	usedByNode := map[string]int64{}
	maxTermByNode := map[string]int{}
	for _, sh := range shards {
		if sh.RaftTerm > maxTermByNode[sh.LeaderNodeID] {
			maxTermByNode[sh.LeaderNodeID] = sh.RaftTerm
		}
		leaderCountByNode[sh.LeaderNodeID]++
		for _, nid := range sh.ReplicaNodeIDs {
			shardCountByNode[nid]++
			usedByNode[nid] += sh.SizeBytes
			if sh.RaftTerm > maxTermByNode[nid] {
				maxTermByNode[nid] = sh.RaftTerm
			}
		}
	}

	const capacityBytes = int64(4) << 40 // 4 TiB per node
	versions := []string{"1.4.2", "1.4.2", "1.4.1"}
	// One node is draining; everything else is up.
	drainingNode := nodeID(5)
	for r := range clusterRegions {
		for _, nid := range regionNodeIDs[r] {
			idx := nodeIndex[nid]
			status := NodeUp
			if nid == drainingNode {
				status = NodeDraining
			}
			nodes = append(nodes, NodeDTO{
				ID:            nid,
				Address:       nodeAddress(r, idx),
				Region:        clusterRegions[r],
				Status:        status,
				ShardCount:    shardCountByNode[nid],
				LeaderCount:   leaderCountByNode[nid],
				RaftTerm:      maxTermByNode[nid],
				UsedBytes:     usedByNode[nid],
				CapacityBytes: capacityBytes,
				Version:       versions[idx%len(versions)],
				LastHeartbeat: observedAt.Add(-time.Duration(idx) * time.Second),
			})
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	summary = deriveSummary(nodes, shards, observedAt)
	return nodes, shards, summary
}

// deriveSummary computes the cluster rollup from the seeded nodes and shards.
// Region/cluster health is "degraded" if any shard there is non-healthy or any
// node there is non-"up"; otherwise "healthy".
func deriveSummary(nodes []NodeDTO, shards []ShardDTO, observedAt time.Time) ClusterSummary {
	var counts ShardHealthCounts
	shardByRegion := map[string]int{}
	regionUnhealthy := map[string]bool{}
	for _, sh := range shards {
		switch sh.Status {
		case ShardDegraded:
			counts.Degraded++
			regionUnhealthy[sh.Region] = true
		case ShardUnderReplicated:
			counts.UnderReplicated++
			regionUnhealthy[sh.Region] = true
		default:
			counts.Healthy++
		}
		shardByRegion[sh.Region]++
	}

	nodeByRegion := map[string]int{}
	for _, n := range nodes {
		nodeByRegion[n.Region]++
		if n.Status != NodeUp {
			regionUnhealthy[n.Region] = true
		}
	}

	regions := make([]RegionSummary, 0, len(clusterRegions))
	clusterHealthy := true
	for _, name := range clusterRegions {
		health := HealthHealthy
		if regionUnhealthy[name] {
			health = HealthDegraded
			clusterHealthy = false
		}
		regions = append(regions, RegionSummary{
			Name:       name,
			NodeCount:  nodeByRegion[name],
			ShardCount: shardByRegion[name],
			Health:     health,
		})
	}

	health := HealthHealthy
	if !clusterHealthy {
		health = HealthDegraded
	}

	return ClusterSummary{
		NodeCount:         len(nodes),
		ShardCount:        len(shards),
		RegionCount:       len(clusterRegions),
		ReplicationFactor: clusterReplicationFactor,
		Health:            health,
		ShardHealth:       counts,
		Regions:           regions,
		ObservedAt:        observedAt,
	}
}

func nodeID(n int) string      { return fmt.Sprintf("node-%04d", n) }
func shardID(i int) string     { return fmt.Sprintf("shard-%04d", i) }
func raftGroupID(i int) string { return fmt.Sprintf("rg_%05d", i) }
func keyToken(v int) string    { return fmt.Sprintf("key_%08x", v) }

func nodeAddress(region, idx int) string {
	return fmt.Sprintf("10.%d.0.%d:7400", region+1, idx)
}

// tierFor weights tiers Hot > Warm > Cold > Archive (50/30/10/10) over i%10.
func tierFor(i int) string {
	switch m := i % 10; {
	case m < 5:
		return TierHot
	case m < 8:
		return TierWarm
	case m < 9:
		return TierCold
	default:
		return TierArchive
	}
}

// sizeForTier gives a varied, tier-correlated shard size (hot shards are larger,
// archived shards smaller), with a deterministic per-shard spread.
func sizeForTier(tier string, i int) int64 {
	base := map[string]int64{
		TierHot:     8 << 30,   // ~8 GiB
		TierWarm:    3 << 30,   // ~3 GiB
		TierCold:    768 << 20, // ~768 MiB
		TierArchive: 192 << 20, // ~192 MiB
	}[tier]
	return base + int64(i%37)*(64<<20)
}
