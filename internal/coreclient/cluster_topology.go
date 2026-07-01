// Cluster topology aggregation: the BFF assembles a single cluster view by
// fanning out across all configured Core replicas (see fanout.go) and unioning
// what each one reports. This file holds the SOURCE-AGNOSTIC pieces - error
// classification, set union, the derived rollup, and the assembly of the final
// result - so they are exercised by plain unit tests independent of gRPC.
//
// Honesty contract: when no replica reports any nodes or shards (a single-node
// Core, or - today - a Core whose build does not yet serve cluster topology over
// gRPC), the result is the empty "no cluster info" shape with a nil Summary. We
// never synthesize a rollup the cluster did not report.
package coreclient

import (
	"errors"
	"sort"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

// clusterTopologyPageSize is the page size used when draining a replica's node
// and shard listings for the topology aggregate (the cluster view is unpaginated
// to its caller; we page Core internally up to clusterTopologyMaxPages).
const clusterTopologyPageSize = 200

// clusterTopologyMaxPages bounds the internal drain so a misbehaving upstream
// cannot make a single topology request page forever.
const clusterTopologyMaxPages = 64

// apiErrCode extracts the contract Code from an error if it is an *apierr.APIError.
func apiErrCode(err error) (apierr.Code, bool) {
	var ae *apierr.APIError
	if errors.As(err, &ae) {
		return ae.Code, true
	}
	return "", false
}

// isUnimplemented reports whether err is the upstream "Core has no such surface
// yet" gap. For cluster topology that is not a failure: it means the replica has
// no topology to contribute, so it is folded into the no-cluster-info shape.
func isUnimplemented(err error) bool {
	c, ok := apiErrCode(err)
	return ok && c == apierr.CodeUnimplemented
}

// unionByID dedups items by a stable key (first occurrence wins), so a replica
// reporting overlapping membership never double-counts. Order is not defined
// here; callers sort.
func unionByID[T any](in []T, id func(T) string) []T {
	seen := make(map[string]struct{}, len(in))
	out := make([]T, 0, len(in))
	for _, v := range in {
		k := id(v)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, v)
	}
	return out
}

// unionNodes dedups nodes by id and returns them sorted by id.
func unionNodes(in []NodeDTO) []NodeDTO {
	out := unionByID(in, func(n NodeDTO) string { return n.ID })
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// unionShards dedups shards by id and returns them sorted by id.
func unionShards(in []ShardDTO) []ShardDTO {
	out := unionByID(in, func(s ShardDTO) string { return s.ID })
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// summarizeTopology computes the cluster rollup from the unioned nodes and
// shards. Unlike the fixture's seed-coupled deriveSummary, region set and
// replication factor are taken from the rows themselves, so it is correct for an
// arbitrary cluster aggregated across replicas. It is aggregation, not
// fabrication: every field is a function of the rows the cluster actually
// reported. Health is "degraded" if any node is not "up" or any shard is not
// "healthy", else "healthy". ObservedAt is the freshest signal across the rows.
func summarizeTopology(nodes []NodeDTO, shards []ShardDTO) ClusterSummary {
	type regionAcc struct {
		nodeCount  int
		shardCount int
		degraded   bool
	}
	regions := map[string]*regionAcc{}
	regionOf := func(name string) *regionAcc {
		acc := regions[name]
		if acc == nil {
			acc = &regionAcc{}
			regions[name] = acc
		}
		return acc
	}

	var observedAt time.Time
	clusterDegraded := false

	for _, n := range nodes {
		acc := regionOf(n.Region)
		acc.nodeCount++
		if n.Status != NodeUp {
			acc.degraded = true
			clusterDegraded = true
		}
		if n.LastHeartbeat.After(observedAt) {
			observedAt = n.LastHeartbeat
		}
	}

	health := ShardHealthCounts{}
	replicationFactor := 0
	for _, s := range shards {
		acc := regionOf(s.Region)
		acc.shardCount++
		switch s.Status {
		case ShardHealthy:
			health.Healthy++
		case ShardDegraded:
			health.Degraded++
			acc.degraded = true
			clusterDegraded = true
		case ShardUnderReplicated:
			health.UnderReplicated++
			acc.degraded = true
			clusterDegraded = true
		default:
			// An unrecognized status is treated as non-healthy rather than silently
			// counted as healthy (fail safe, not fail open).
			acc.degraded = true
			clusterDegraded = true
		}
		if s.ReplicationFactor > replicationFactor {
			replicationFactor = s.ReplicationFactor
		}
		if s.ObservedAt.After(observedAt) {
			observedAt = s.ObservedAt
		}
	}

	regionNames := make([]string, 0, len(regions))
	for name := range regions {
		regionNames = append(regionNames, name)
	}
	sort.Strings(regionNames)
	regionSummaries := make([]RegionSummary, 0, len(regionNames))
	for _, name := range regionNames {
		acc := regions[name]
		regionHealth := HealthHealthy
		if acc.degraded {
			regionHealth = HealthDegraded
		}
		regionSummaries = append(regionSummaries, RegionSummary{
			Name:       name,
			NodeCount:  acc.nodeCount,
			ShardCount: acc.shardCount,
			Health:     regionHealth,
		})
	}

	clusterHealth := HealthHealthy
	if clusterDegraded {
		clusterHealth = HealthDegraded
	}

	return ClusterSummary{
		NodeCount:         len(nodes),
		ShardCount:        len(shards),
		RegionCount:       len(regions),
		ReplicationFactor: replicationFactor,
		Health:            clusterHealth,
		ShardHealth:       health,
		Regions:           regionSummaries,
		ObservedAt:        observedAt,
	}
}

// buildTopology unions the per-replica rows and assembles the result. With no
// rows at all it returns the honest no-cluster-info shape (nil Summary, empty
// sets, Cluster=false); otherwise it derives the rollup and marks Cluster true
// only when more than one node is present (a genuine multi-node cluster).
func buildTopology(nodes []NodeDTO, shards []ShardDTO, source string) *ClusterTopologyResult {
	nodes = unionNodes(nodes)
	shards = unionShards(shards)
	if len(nodes) == 0 && len(shards) == 0 {
		return &ClusterTopologyResult{
			Summary: nil,
			Nodes:   []NodeDTO{},
			Shards:  []ShardDTO{},
			Cluster: false,
			Source:  source,
		}
	}
	summary := summarizeTopology(nodes, shards)
	return &ClusterTopologyResult{
		Summary: &summary,
		Nodes:   nodes,
		Shards:  shards,
		Cluster: len(nodes) > 1,
		Source:  source,
	}
}
