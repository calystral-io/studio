// Fan-out CoreClient for a multi-replica Core cluster. It embeds the primary
// replica's *GRPCClient, so every non-cluster read (anchors, ledgers, runtime,
// messaging, and the paginated cluster listings) is served by the primary - those
// are consensus-replicated tenant/observability reads, identical from any node.
// Only ClusterTopology is overridden to fan out across ALL replicas and union
// what each reports, which is the one read whose answer is assembled from
// per-node contributions.
package coreclient

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

// FanoutClient aggregates cluster topology across a set of Core replicas.
type FanoutClient struct {
	*GRPCClient               // primary replica (replicas[0]); serves all non-topology reads
	replicas    []*GRPCClient // all configured replicas, including the primary
}

// Compile-time assertion that the fan-out client is a full CoreClient.
var _ CoreClient = (*FanoutClient)(nil)

// NewFanoutClient dials every replica address and returns a CoreClient that fans
// cluster topology out across them. addrs must be non-empty; addrs[0] is the
// primary. Every replica is dialed with the same opts (TLS + logger). On any
// dial failure it closes the replicas already opened.
func NewFanoutClient(addrs []string, signer *auth.PrincipalSigner, opts Options) (*FanoutClient, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("fanout core client: no replica addresses")
	}
	replicas := make([]*GRPCClient, 0, len(addrs))
	for _, addr := range addrs {
		c, err := NewGRPCClient(addr, signer, opts)
		if err != nil {
			for _, opened := range replicas {
				_ = opened.Close()
			}
			return nil, err
		}
		replicas = append(replicas, c)
	}
	return &FanoutClient{GRPCClient: replicas[0], replicas: replicas}, nil
}

// Close releases every replica connection, returning the first error.
func (f *FanoutClient) Close() error {
	var firstErr error
	for _, r := range f.replicas {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ClusterTopology queries every replica concurrently and unions what each
// reports. An unreachable replica is skipped (not fatal) so the cluster view
// degrades gracefully as nodes come and go; only when NO replica is reachable
// does the read fail (502). When every replica is reachable but reports nothing
// (Core's topology gap today), the union is empty and buildTopology returns the
// honest no-cluster-info shape.
func (f *FanoutClient) ClusterTopology(ctx context.Context, p ClusterTopologyParams) (*ClusterTopologyResult, error) {
	if p.Principal == nil {
		return nil, apierr.Internal("fanout core client: missing principal")
	}

	type replicaResult struct {
		nodes  []NodeDTO
		shards []ShardDTO
		err    error
	}
	results := make([]replicaResult, len(f.replicas))
	var wg sync.WaitGroup
	for i, r := range f.replicas {
		wg.Add(1)
		go func(i int, r *GRPCClient) {
			defer wg.Done()
			nodes, shards, err := fetchReplicaTopology(ctx, r, p)
			results[i] = replicaResult{nodes: nodes, shards: shards, err: err}
		}(i, r)
	}
	wg.Wait()

	var allNodes []NodeDTO
	var allShards []ShardDTO
	reachable := 0
	for i, res := range results {
		if res.err != nil {
			slog.Warn("cluster topology: replica unreachable, skipping",
				"replica", f.replicas[i].conn.Target(), "err", res.err)
			continue
		}
		reachable++
		allNodes = append(allNodes, res.nodes...)
		allShards = append(allShards, res.shards...)
	}
	if reachable == 0 {
		return nil, apierr.Unavailable(clusterTopologySurface)
	}
	return buildTopology(allNodes, allShards, SourceCore), nil
}
