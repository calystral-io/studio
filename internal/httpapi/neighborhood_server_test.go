package httpapi

import (
	"net/http"
	"testing"

	"github.com/calystral-io/studio/internal/coreclient"
)

type neighborhoodBody struct {
	Root      *coreclient.AnchorDTO  `json:"root"`
	Neighbors []coreclient.AnchorDTO `json:"neighbors"`
	Edges     []coreclient.EdgeDTO   `json:"edges"`
	Total     int                    `json:"neighbor_total"`
	Sampled   bool                   `json:"sampled"`
	Source    string                 `json:"source"`
}

func TestNeighborhoodOK(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/nodes/node_employee_0001/neighborhood", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body neighborhoodBody
	decode(t, rec, &body)
	if body.Root == nil || body.Root.ID != "node_employee_0001" {
		t.Fatalf("root = %+v", body.Root)
	}
	if len(body.Neighbors) == 0 || len(body.Edges) == 0 {
		t.Fatalf("expected neighbors+edges, got %d/%d", len(body.Neighbors), len(body.Edges))
	}
	if body.Source != "fixture" {
		t.Fatalf("source = %q", body.Source)
	}
	// Edge JSON carries the graph-facing field names (no engine leak).
	e := body.Edges[0]
	if e.SourceID == "" || e.TargetID == "" || e.Type == "" {
		t.Fatalf("edge missing source/target/type: %+v", e)
	}
}

func TestNeighborhoodLimitCaps(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/nodes/node_department_0000/neighborhood?limit=3", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body neighborhoodBody
	decode(t, rec, &body)
	if len(body.Neighbors) != 3 || !body.Sampled || body.Total <= 3 {
		t.Fatalf("cap: neighbors=%d sampled=%v total=%d", len(body.Neighbors), body.Sampled, body.Total)
	}
}

func TestNeighborhoodValidation(t *testing.T) {
	s := newFixtureServer()
	cases := []struct {
		name, target, wantCode string
	}{
		{"bad limit", "/api/v1/nodes/node_employee_0001/neighborhood?limit=abc", "/errors/validation/invalid_request"},
		{"negative limit", "/api/v1/nodes/node_employee_0001/neighborhood?limit=-1", "/errors/validation/invalid_request"},
		{"bad as_of", "/api/v1/nodes/node_employee_0001/neighborhood?as_of=nope", "/errors/validation/invalid_as_of"},
		{"bad system_as_of", "/api/v1/nodes/node_employee_0001/neighborhood?system_as_of=nope", "/errors/validation/invalid_system_as_of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tc.target, "mock-reader-token")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var env errEnvelope
			decode(t, rec, &env)
			if env.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestNeighborhoodNotFound(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/nodes/node_nope_9999/neighborhood", "mock-reader-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestNeighborhoodRequiresAuth(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/nodes/node_employee_0001/neighborhood", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
