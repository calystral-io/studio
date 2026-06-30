package httpapi

import (
	"net/http"
	"testing"

	"github.com/calystral-io/studio/internal/coreclient"
)

type clusterSummaryBody struct {
	NodeCount         int    `json:"node_count"`
	ShardCount        int    `json:"shard_count"`
	RegionCount       int    `json:"region_count"`
	ReplicationFactor int    `json:"replication_factor"`
	Health            string `json:"health"`
	ShardHealth       struct {
		Healthy         int `json:"healthy"`
		Degraded        int `json:"degraded"`
		UnderReplicated int `json:"under_replicated"`
	} `json:"shard_health"`
	Regions []struct {
		Name       string `json:"name"`
		NodeCount  int    `json:"node_count"`
		ShardCount int    `json:"shard_count"`
		Health     string `json:"health"`
	} `json:"regions"`
	ObservedAt string `json:"observed_at"`
	Source     string `json:"source"`
}

type nodesBody struct {
	Items  []coreclient.NodeDTO `json:"items"`
	Page   coreclient.Page      `json:"page"`
	Source string               `json:"source"`
}

type shardsBody struct {
	Items  []coreclient.ShardDTO `json:"items"`
	Page   coreclient.Page       `json:"page"`
	Source string                `json:"source"`
}

func TestClusterSummaryHappyPath(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/cluster", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body clusterSummaryBody
	decode(t, rec, &body)

	if body.Source != "fixture" {
		t.Errorf("source = %q, want fixture", body.Source)
	}
	if body.NodeCount != 9 || body.ShardCount != 144 || body.RegionCount != 3 {
		t.Errorf("counts = nodes %d shards %d regions %d", body.NodeCount, body.ShardCount, body.RegionCount)
	}
	if body.ReplicationFactor != 3 {
		t.Errorf("replication_factor = %d, want 3", body.ReplicationFactor)
	}
	// The seed is deliberately non-healthy.
	if body.Health != "degraded" {
		t.Errorf("health = %q, want degraded", body.Health)
	}
	// All three shard_health keys present and summing to the shard count.
	sum := body.ShardHealth.Healthy + body.ShardHealth.Degraded + body.ShardHealth.UnderReplicated
	if sum != body.ShardCount {
		t.Errorf("shard_health sum %d != shard_count %d", sum, body.ShardCount)
	}
	if body.ShardHealth.Degraded == 0 || body.ShardHealth.UnderReplicated == 0 {
		t.Errorf("seed must expose degraded + under_replicated shards, got %+v", body.ShardHealth)
	}
	if len(body.Regions) != 3 {
		t.Fatalf("regions = %d, want 3", len(body.Regions))
	}
	if body.ObservedAt == "" {
		t.Error("observed_at must be present on the summary")
	}
}

func TestClusterNodesHappyPathAndCursorWalk(t *testing.T) {
	s := newFixtureServer()

	cursor := ""
	total := 0
	pages := 0
	seen := map[string]bool{}
	var prevID string
	for {
		target := "/api/v1/cluster/nodes?page_size=4"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body nodesBody
		decode(t, rec, &body)
		pages++
		if body.Source != "fixture" {
			t.Errorf("source = %q", body.Source)
		}
		if body.Page.TotalEstimate != 9 {
			t.Errorf("total_estimate = %d, want 9", body.Page.TotalEstimate)
		}
		for _, n := range body.Items {
			if seen[n.ID] {
				t.Fatalf("duplicate node %s", n.ID)
			}
			seen[n.ID] = true
			if prevID != "" && n.ID <= prevID {
				t.Fatalf("nodes not ascending: %s after %s", n.ID, prevID)
			}
			prevID = n.ID
		}
		total += len(body.Items)
		if !body.Page.HasMore {
			if body.Page.NextCursor != nil {
				t.Error("next_cursor must be null on last page")
			}
			break
		}
		if body.Page.NextCursor == nil {
			t.Fatal("has_more true but next_cursor nil")
		}
		cursor = *body.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if total != 9 {
		t.Fatalf("walked %d nodes, want 9", total)
	}
}

func TestClusterShardsHappyPathAndCursorWalk(t *testing.T) {
	s := newFixtureServer()

	cursor := ""
	total := 0
	pages := 0
	seen := map[string]bool{}
	var prevID string
	for {
		target := "/api/v1/cluster/shards?page_size=40"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body shardsBody
		decode(t, rec, &body)
		pages++
		if body.Source != "fixture" {
			t.Errorf("source = %q", body.Source)
		}
		if body.Page.TotalEstimate != 144 {
			t.Errorf("total_estimate = %d, want 144", body.Page.TotalEstimate)
		}
		for _, sh := range body.Items {
			if seen[sh.ID] {
				t.Fatalf("duplicate shard %s", sh.ID)
			}
			seen[sh.ID] = true
			if prevID != "" && sh.ID <= prevID {
				t.Fatalf("shards not ascending: %s after %s", sh.ID, prevID)
			}
			prevID = sh.ID
			if sh.Lag != sh.CommitIndex-sh.AppliedIndex || sh.Lag < 0 {
				t.Errorf("shard %s lag %d inconsistent with commit/applied", sh.ID, sh.Lag)
			}
		}
		total += len(body.Items)
		if !body.Page.HasMore {
			if body.Page.NextCursor != nil {
				t.Error("next_cursor must be null on last page")
			}
			break
		}
		if body.Page.NextCursor == nil {
			t.Fatal("has_more true but next_cursor nil")
		}
		cursor = *body.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if total != 144 {
		t.Fatalf("walked %d shards, want 144", total)
	}
}

func TestClusterNodesFilters(t *testing.T) {
	s := newFixtureServer()

	rec := do(t, s, http.MethodGet, "/api/v1/cluster/nodes?region=us-east&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body nodesBody
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 3 {
		t.Errorf("us-east nodes = %d, want 3", body.Page.TotalEstimate)
	}
	for _, n := range body.Items {
		if n.Region != "us-east" {
			t.Errorf("node %s region = %q", n.ID, n.Region)
		}
	}

	rec = do(t, s, http.MethodGet, "/api/v1/cluster/nodes?status=draining&page_size=200", "mock-reader-token")
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 1 {
		t.Errorf("draining nodes = %d, want 1", body.Page.TotalEstimate)
	}

	// Unknown status matches nothing (no 400).
	rec = do(t, s, http.MethodGet, "/api/v1/cluster/nodes?status=bogus", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown status status = %d", rec.Code)
	}
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 0 {
		t.Errorf("unknown status matched %d nodes", body.Page.TotalEstimate)
	}
}

func TestClusterShardsFilters(t *testing.T) {
	s := newFixtureServer()

	rec := do(t, s, http.MethodGet, "/api/v1/cluster/shards?status=under_replicated&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body shardsBody
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected some under_replicated shards")
	}
	for _, sh := range body.Items {
		if sh.Status != "under_replicated" {
			t.Errorf("shard %s status = %q", sh.ID, sh.Status)
		}
		if len(sh.ReplicaNodeIDs) >= sh.ReplicationFactor {
			t.Errorf("shard %s under_replicated but has full replica set", sh.ID)
		}
	}

	// node filter: every returned shard references node-0001.
	rec = do(t, s, http.MethodGet, "/api/v1/cluster/shards?node=node-0001&page_size=200", "mock-reader-token")
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected node-0001 shards")
	}
	for _, sh := range body.Items {
		involved := sh.LeaderNodeID == "node-0001"
		for _, r := range sh.ReplicaNodeIDs {
			if r == "node-0001" {
				involved = true
			}
		}
		if !involved {
			t.Errorf("shard %s does not involve node-0001", sh.ID)
		}
	}

	// region filter.
	rec = do(t, s, http.MethodGet, "/api/v1/cluster/shards?region=eu-central&page_size=200", "mock-reader-token")
	decode(t, rec, &body)
	for _, sh := range body.Items {
		if sh.Region != "eu-central" {
			t.Errorf("shard %s region = %q", sh.ID, sh.Region)
		}
	}
}

func TestClusterValidation(t *testing.T) {
	s := newFixtureServer()
	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"nodes page_size too large", "/api/v1/cluster/nodes?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"nodes page_size zero", "/api/v1/cluster/nodes?page_size=0", "/errors/validation/page_size_out_of_range"},
		{"nodes bad cursor", "/api/v1/cluster/nodes?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
		{"shards page_size too large", "/api/v1/cluster/shards?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"shards bad cursor", "/api/v1/cluster/shards?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tc.target, "mock-reader-token")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var env errEnvelope
			decode(t, rec, &env)
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestClusterRequiresAuth(t *testing.T) {
	s := newFixtureServer()
	for _, target := range []string{"/api/v1/cluster", "/api/v1/cluster/nodes", "/api/v1/cluster/shards"} {
		rec := do(t, s, http.MethodGet, target, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
	}
}

func TestClusterForbiddenWithoutReader(t *testing.T) {
	s := New(rolelessAuth{}, coreclient.NewFixture(), quietLogger(), Options{})
	for _, target := range []string{"/api/v1/cluster", "/api/v1/cluster/nodes", "/api/v1/cluster/shards", "/api/v1/cluster/topology"} {
		rec := do(t, s, http.MethodGet, target, "any")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
	}
}

type clusterTopologyBody struct {
	Cluster bool                  `json:"cluster"`
	Summary *clusterSummaryBody   `json:"summary"`
	Nodes   []coreclient.NodeDTO  `json:"nodes"`
	Shards  []coreclient.ShardDTO `json:"shards"`
	Source  string                `json:"source"`
}

func TestClusterTopologyFixtureHappyPath(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/cluster/topology", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body clusterTopologyBody
	decode(t, rec, &body)

	if body.Source != "fixture" {
		t.Errorf("source = %q, want fixture", body.Source)
	}
	if !body.Cluster {
		t.Error("fixture is a multi-node cluster; cluster must be true")
	}
	if body.Summary == nil {
		t.Fatal("summary must be present for a populated cluster")
	}
	if body.Summary.NodeCount != 9 || body.Summary.ShardCount != 144 {
		t.Errorf("summary counts = nodes %d shards %d", body.Summary.NodeCount, body.Summary.ShardCount)
	}
	if len(body.Nodes) != 9 || len(body.Shards) != 144 {
		t.Errorf("rows = %d nodes / %d shards", len(body.Nodes), len(body.Shards))
	}
}

func TestClusterTopologyGRPCReturnsNoClusterInfoNot501(t *testing.T) {
	// The KEY D1 behavior: against today's Core (UNIMPLEMENTED cluster reads), the
	// topology endpoint returns 200 with the honest no-cluster-info shape - NOT the
	// 501 the paginated cluster endpoints return. Empty IS the correct state until
	// Core's cluster topology (RaftTransport + read path) lands.
	s := newGRPCFixtureServer(t)
	rec := do(t, s, http.MethodGet, "/api/v1/cluster/topology", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 (no-cluster-info, not 501)", rec.Code, rec.Body.String())
	}
	var body clusterTopologyBody
	decode(t, rec, &body)
	if body.Source != "core" {
		t.Errorf("source = %q, want core", body.Source)
	}
	if body.Cluster {
		t.Error("cluster must be false with no topology")
	}
	if body.Summary != nil {
		t.Errorf("summary = %+v, want null", body.Summary)
	}
	// Sets must marshal as [] not null.
	if body.Nodes == nil || len(body.Nodes) != 0 || body.Shards == nil || len(body.Shards) != 0 {
		t.Errorf("nodes/shards must be empty arrays, got %v / %v", body.Nodes, body.Shards)
	}
}

func TestClusterGRPCSourceReturns501(t *testing.T) {
	s := newGRPCFixtureServer(t)
	cases := []struct {
		target  string
		surface string
	}{
		{"/api/v1/cluster", "cluster_summary"},
		{"/api/v1/cluster/nodes", "cluster_nodes"},
		{"/api/v1/cluster/shards", "cluster_shards"},
	}
	for _, tc := range cases {
		t.Run(tc.surface, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tc.target, "mock-reader-token")
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var env errEnvelope
			decode(t, rec, &env)
			if env.Error.Code != "/errors/upstream/unimplemented" {
				t.Errorf("code = %q", env.Error.Code)
			}
			if env.Error.Params["surface"] != tc.surface {
				t.Errorf("surface = %v, want %q", env.Error.Params["surface"], tc.surface)
			}
		})
	}
}
