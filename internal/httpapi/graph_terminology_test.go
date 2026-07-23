package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The user-facing GRAPH surfaces (Nodes/Edges/Revisions) must speak the
// user-facing vocabulary: a version identifier is a "revision", never the
// engine's "lsn". This asserts the served JSON keys on the node list, the
// neighborhood (which carries both nodes and edges), and the revision history.
func TestGraphSurfacesUseRevisionNotLSN(t *testing.T) {
	s := newFixtureServer()
	cases := []struct{ name, target string }{
		{"nodes", "/api/v1/nodes?page_size=5"},
		{"neighborhood", "/api/v1/nodes/node_employee_0001/neighborhood"},
		{"history", "/api/v1/nodes/node_employee_0001/history"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tc.target, "mock-reader-token")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"revision"`) {
				t.Errorf("%s response must expose the user-facing key \"revision\":\n%s", tc.name, body)
			}
			if strings.Contains(body, `"lsn"`) {
				t.Errorf("%s response leaks the engine key \"lsn\":\n%s", tc.name, body)
			}
		})
	}
}
