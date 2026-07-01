// gRPC cluster-topology adapter. A single Core replica reports its cluster view
// through the same ListNodes/ListShards reads the paginated cluster endpoints
// use; ClusterTopology drains those into the unioned, single-payload aggregate.
//
// Today Core returns UNIMPLEMENTED for these reads (its cluster topology is not
// yet served over gRPC - it depends on Core's RaftTransport and read path
// landing). That gap is folded into the honest no-cluster-info shape rather than
// surfaced as an error: an empty topology is the correct answer until Core can
// report one. A transport failure (Core unreachable) is NOT folded away - it
// propagates as 502, because "unreachable" and "reachable but reports nothing"
// are different operator truths.
package coreclient

import (
	"context"
	"log/slog"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

// replicaTopologyTimeout bounds a single replica's whole topology fetch (all its
// node + shard pages). Without it a black-hole replica - one that accepts the TCP
// connection but never answers the RPC - would block the fan-out's WaitGroup
// forever and leak the goroutine; the headline "skip an unreachable replica"
// resilience must cover hung replicas, not just connection-refused ones. On
// expiry the read fails with DeadlineExceeded, which maps to Unavailable: the
// single-node path returns 502, the fan-out path skips the replica. It is a var
// (not a const) only so tests can shorten it.
var replicaTopologyTimeout = 5 * time.Second

// nodeLister and shardLister are the cluster reads ClusterTopology drains. Both
// the gRPC client and the fixture satisfy clusterReader, so the drain + fetch
// helpers are reusable and unit-testable with a fake.
type nodeLister interface {
	ListNodes(ctx context.Context, p ListNodesParams) (*ListNodesResult, error)
}
type shardLister interface {
	ListShards(ctx context.Context, p ListShardsParams) (*ListShardsResult, error)
}
type clusterReader interface {
	nodeLister
	shardLister
}

// drainPages pages a replica's full set of some listing (the cluster view is
// unpaginated to its caller). `fetchPage` returns one page's items plus its
// pagination signal. It stops at clusterTopologyMaxPages so a stuck upstream
// cursor cannot loop forever, logging rather than truncating silently so an
// unexpectedly huge set is visible to operators. `kind` names the listing in
// that log line.
func drainPages[T any](kind string, fetchPage func(cursor string) (items []T, next *string, hasMore bool, err error)) ([]T, error) {
	var out []T
	cursor := ""
	for page := 0; page < clusterTopologyMaxPages; page++ {
		items, next, hasMore, err := fetchPage(cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if !hasMore || next == nil {
			return out, nil
		}
		cursor = *next
	}
	slog.Warn("cluster topology: drain hit page cap, result may be truncated",
		"listing", kind, "max_pages", clusterTopologyMaxPages, "collected", len(out))
	return out, nil
}

// drainNodes pages a replica's full node set.
func drainNodes(ctx context.Context, c nodeLister, p ClusterTopologyParams) ([]NodeDTO, error) {
	return drainPages("nodes", func(cursor string) ([]NodeDTO, *string, bool, error) {
		res, err := c.ListNodes(ctx, ListNodesParams{
			TenantID:  p.TenantID,
			PageSize:  clusterTopologyPageSize,
			Cursor:    cursor,
			Principal: p.Principal,
		})
		if err != nil {
			return nil, nil, false, err
		}
		return res.Items, res.Page.NextCursor, res.Page.HasMore, nil
	})
}

// drainShards pages a replica's full shard set.
func drainShards(ctx context.Context, c shardLister, p ClusterTopologyParams) ([]ShardDTO, error) {
	return drainPages("shards", func(cursor string) ([]ShardDTO, *string, bool, error) {
		res, err := c.ListShards(ctx, ListShardsParams{
			TenantID:  p.TenantID,
			PageSize:  clusterTopologyPageSize,
			Cursor:    cursor,
			Principal: p.Principal,
		})
		if err != nil {
			return nil, nil, false, err
		}
		return res.Items, res.Page.NextCursor, res.Page.HasMore, nil
	})
}

// fetchReplicaTopology reads one replica's nodes and shards under a bounded
// per-replica deadline (so a hung replica cannot stall the fan-out). UNIMPLEMENTED
// on either listing means the replica has no topology to contribute (folded to an
// empty set, not an error); any other error (transport / unavailable / deadline)
// propagates so the caller can decide whether the whole read fails.
func fetchReplicaTopology(ctx context.Context, c clusterReader, p ClusterTopologyParams) ([]NodeDTO, []ShardDTO, error) {
	ctx, cancel := context.WithTimeout(ctx, replicaTopologyTimeout)
	defer cancel()
	nodes, err := drainNodes(ctx, c, p)
	if err != nil && !isUnimplemented(err) {
		return nil, nil, err
	}
	shards, err := drainShards(ctx, c, p)
	if err != nil && !isUnimplemented(err) {
		return nil, nil, err
	}
	return nodes, shards, nil
}

// ClusterTopology assembles this single replica's cluster view. With Core's
// cluster reads UNIMPLEMENTED today, fetchReplicaTopology yields empty sets and
// buildTopology returns the no-cluster-info shape (nil Summary). A transport
// failure propagates as 502.
func (c *GRPCClient) ClusterTopology(ctx context.Context, p ClusterTopologyParams) (*ClusterTopologyResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("grpc core client: missing principal")
	}
	nodes, shards, err := fetchReplicaTopology(ctx, c, p)
	if err != nil {
		return nil, err
	}
	return buildTopology(nodes, shards, SourceCore), nil
}
