package httpapi

import (
	"context"
	"net"
	"net/http"
	"testing"

	"google.golang.org/grpc"

	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
	"github.com/calystral-io/studio/internal/corepb/querypb"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// clusterNoInfoBody decodes the no-cluster-info shape GET /api/v1/cluster returns
// when Core has no :Cluster node (mirrors /cluster/topology).
type clusterNoInfoBody struct {
	Cluster bool                       `json:"cluster"`
	Summary *coreclient.ClusterSummary `json:"summary"`
	Source  string                     `json:"source"`
}

// summaryRowQuery is a stub Core Query service that returns fixed rows, standing
// in for a live Core whose read/query path executes the summary projection.
type summaryRowQuery struct {
	querypb.UnimplementedQueryServiceServer
	rows []*querypb.QueryRow
}

func (s summaryRowQuery) Query(context.Context, *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	return &querypb.QueryResponse{Rows: s.rows}, nil
}

// newGRPCServerWithQuery builds a Server whose GRPCClient talks to a local Core
// exposing the given Query service — so a request exercises the full
// decode-and-render path, not a fixture.
func newGRPCServerWithQuery(t *testing.T, qs querypb.QueryServiceServer) *Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcSrv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(grpcSrv, qs)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	signer, err := auth.NewPrincipalSigner("")
	if err != nil {
		t.Fatal(err)
	}
	core, err := coreclient.NewGRPCClient(lis.Addr().String(), signer, coreclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	return New(auth.MockAuthenticator{}, core, quietLogger(), Options{})
}

func summaryRow(t *testing.T, jsonText string) *querypb.QueryRow {
	t.Helper()
	payload, err := cybrwire.EncodeValue(cybrwire.Array([]cybrwire.Value{cybrwire.Str(jsonText)}))
	if err != nil {
		t.Fatalf("encode summary row: %v", err)
	}
	return &querypb.QueryRow{Payload: payload}
}

// A real Core row (the cluster node's summary field as JSON) renders through
// GET /api/v1/cluster as 200 with source:core and the rollup fields promoted at
// the top level — the first real query result surfaced by the console.
func TestClusterSummaryGRPCHappyPath(t *testing.T) {
	const rollup = `{"node_count":3,"shard_count":1,"region_count":1,"replication_factor":3,` +
		`"health":"healthy","shard_health":{"healthy":1,"degraded":0,"under_replicated":0},` +
		`"regions":[{"name":"region-a","node_count":3,"shard_count":1,"health":"healthy"}],` +
		`"observed_at":"2026-07-22T09:00:00Z"}`
	s := newGRPCServerWithQuery(t, summaryRowQuery{rows: []*querypb.QueryRow{summaryRow(t, rollup)}})

	rec := do(t, s, http.MethodGet, "/api/v1/cluster", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body clusterSummaryBody
	decode(t, rec, &body)
	if body.Source != "core" {
		t.Errorf("source = %q, want core", body.Source)
	}
	if body.NodeCount != 3 || body.ShardCount != 1 || body.RegionCount != 1 ||
		body.ReplicationFactor != 3 || body.Health != "healthy" {
		t.Fatalf("promoted rollup fields wrong: %+v", body)
	}
	if body.ShardHealth.Healthy != 1 || len(body.Regions) != 1 || body.Regions[0].Name != "region-a" {
		t.Fatalf("nested rollup fields wrong: %+v", body)
	}
	if body.ObservedAt == "" {
		t.Error("observed_at must be present")
	}
}

// No :Cluster node (zero rows) renders as 200 with the no-cluster-info shape
// (cluster:false / summary:null), never a zero-valued rollup or a 501.
func TestClusterSummaryGRPCEmptyIsNoInfo(t *testing.T) {
	s := newGRPCServerWithQuery(t, summaryRowQuery{rows: nil})

	rec := do(t, s, http.MethodGet, "/api/v1/cluster", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 no-info", rec.Code, rec.Body.String())
	}
	var body clusterNoInfoBody
	decode(t, rec, &body)
	if body.Source != "core" || body.Cluster || body.Summary != nil {
		t.Fatalf("want {cluster:false, summary:null, source:core}, got %+v", body)
	}
}
