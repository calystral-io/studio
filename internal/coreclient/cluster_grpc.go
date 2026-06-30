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

	"github.com/calystral-io/studio/internal/apierr"
)

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

// drainNodes pages a replica's full node set (the cluster view is unpaginated to
// its caller). It stops at clusterTopologyMaxPages so a stuck upstream cursor
// cannot loop forever.
func drainNodes(ctx context.Context, c nodeLister, p ClusterTopologyParams) ([]NodeDTO, error) {
	var out []NodeDTO
	cursor := ""
	for page := 0; page < clusterTopologyMaxPages; page++ {
		res, err := c.ListNodes(ctx, ListNodesParams{
			TenantID:  p.TenantID,
			PageSize:  clusterTopologyPageSize,
			Cursor:    cursor,
			Principal: p.Principal,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if !res.Page.HasMore || res.Page.NextCursor == nil {
			break
		}
		cursor = *res.Page.NextCursor
	}
	return out, nil
}

// drainShards pages a replica's full shard set, bounded like drainNodes.
func drainShards(ctx context.Context, c shardLister, p ClusterTopologyParams) ([]ShardDTO, error) {
	var out []ShardDTO
	cursor := ""
	for page := 0; page < clusterTopologyMaxPages; page++ {
		res, err := c.ListShards(ctx, ListShardsParams{
			TenantID:  p.TenantID,
			PageSize:  clusterTopologyPageSize,
			Cursor:    cursor,
			Principal: p.Principal,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if !res.Page.HasMore || res.Page.NextCursor == nil {
			break
		}
		cursor = *res.Page.NextCursor
	}
	return out, nil
}

// fetchReplicaTopology reads one replica's nodes and shards. UNIMPLEMENTED on
// either listing means the replica has no topology to contribute (folded to an
// empty set, not an error); any other error (transport / unavailable) propagates
// so the caller can decide whether the whole read fails.
func fetchReplicaTopology(ctx context.Context, c clusterReader, p ClusterTopologyParams) ([]NodeDTO, []ShardDTO, error) {
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
