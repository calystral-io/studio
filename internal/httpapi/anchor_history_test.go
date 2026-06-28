package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/coreclient"
)

// mustTime parses an RFC3339 instant or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return parsed
}

// anchorHistoryBody mirrors the GET /nodes/{id}/history envelope for tests.
type anchorHistoryBody struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenant_id"`
	Versions []coreclient.AnchorDTO `json:"versions"`
	Summary  struct {
		VersionCount      int `json:"version_count"`
		CurrentCount      int `json:"current_count"`
		SupersededCount   int `json:"superseded_count"`
		ValidSegmentCount int `json:"valid_segment_count"`
	} `json:"summary"`
	Source string `json:"source"`
}

func TestAnchorHistory(t *testing.T) {
	s := newFixtureServer()

	rec := do(t, s, http.MethodGet, "/api/v1/nodes/node_employee_0018/history", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body anchorHistoryBody
	decode(t, rec, &body)
	if body.ID != "node_employee_0018" || body.Type != "Employee" {
		t.Errorf("id/type = %q/%q", body.ID, body.Type)
	}
	if body.Source != "fixture" {
		t.Errorf("source = %q, want fixture", body.Source)
	}
	if len(body.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(body.Versions))
	}
	if body.Summary.VersionCount != 2 || body.Summary.CurrentCount != 1 || body.Summary.SupersededCount != 1 {
		t.Errorf("summary = %+v, want 2/1/1", body.Summary)
	}

	// Unknown id -> 404 not_found with resource="anchor:<id>".
	rec = do(t, s, http.MethodGet, "/api/v1/nodes/node_nope/history", "mock-reader-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d", rec.Code)
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/not_found" {
		t.Errorf("code = %q, want not_found", env.Error.Code)
	}
}

func TestAnchorHistoryForbiddenWithoutReader(t *testing.T) {
	s := New(rolelessAuth{}, coreclient.NewFixture(), quietLogger(), Options{})
	rec := do(t, s, http.MethodGet, "/api/v1/nodes/node_employee_0018/history", "any")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/auth/forbidden" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

// anchorDiffBody mirrors the GET /nodes/{id}/diff envelope for tests.
type anchorDiffBody struct {
	ID   string `json:"id"`
	From struct {
		Version *coreclient.AnchorDTO `json:"version"`
	} `json:"from"`
	To struct {
		Version *coreclient.AnchorDTO `json:"version"`
	} `json:"to"`
	Deltas []struct {
		Field string `json:"field"`
		Op    string `json:"op"`
	} `json:"deltas"`
	Source string `json:"source"`
}

func TestAnchorDiff(t *testing.T) {
	s := newFixtureServer()

	// from = pre-correction (system 2026-06-19), to = current (system omitted ->
	// current/open), both at the same explicit valid instant so the test is
	// deterministic (independent of the wall clock).
	target := "/api/v1/nodes/node_employee_0018/diff?as_of=2026-05-01&system_as_of=2026-06-19&to_as_of=2026-05-01"
	rec := do(t, s, http.MethodGet, target, "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body anchorDiffBody
	decode(t, rec, &body)
	if body.From.Version == nil || body.To.Version == nil {
		t.Fatalf("expected both sides resolved, got from=%v to=%v", body.From.Version, body.To.Version)
	}
	ops := map[string]string{}
	for _, d := range body.Deltas {
		ops[d.Field] = d.Op
	}
	for _, want := range []string{"closed", "valid_to", "properties.title"} {
		if ops[want] != "changed" {
			t.Errorf("delta %q = %q, want changed (deltas=%+v)", want, ops[want], body.Deltas)
		}
	}
	if _, ok := ops["valid_from"]; ok {
		t.Errorf("valid_from should be unchanged between the two versions")
	}

	// A from-coordinate before the anchor existed yields a null from-version and
	// an "added" diff (every present field of the to-version).
	target = "/api/v1/nodes/node_employee_0018/diff?as_of=2020-01-01&to_as_of=2026-05-01"
	rec = do(t, s, http.MethodGet, target, "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	decode(t, rec, &body)
	if body.From.Version != nil {
		t.Error("from at 2020 should be null")
	}
	if body.To.Version == nil {
		t.Error("to at 2026-05-01 should resolve")
	}
	sawAdded := false
	for _, d := range body.Deltas {
		if d.Op == "added" {
			sawAdded = true
		}
	}
	if !sawAdded {
		t.Error("expected added deltas when from-version is null")
	}
}

func TestAnchorDiffValidation(t *testing.T) {
	s := newFixtureServer()
	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"bad as_of", "/api/v1/nodes/node_employee_0018/diff?as_of=nope", "/errors/validation/invalid_as_of"},
		{"bad to_as_of", "/api/v1/nodes/node_employee_0018/diff?to_as_of=nope", "/errors/validation/invalid_as_of"},
		{"bad system_as_of", "/api/v1/nodes/node_employee_0018/diff?system_as_of=nope", "/errors/validation/invalid_system_as_of"},
		{"bad to_system_as_of", "/api/v1/nodes/node_employee_0018/diff?to_system_as_of=nope", "/errors/validation/invalid_system_as_of"},
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

	// Unknown id -> 404.
	rec := do(t, s, http.MethodGet, "/api/v1/nodes/node_nope/diff", "mock-reader-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d", rec.Code)
	}
}

func TestAnchorHistoryDiffGRPCSourceReturns501(t *testing.T) {
	s := newGRPCFixtureServer(t)
	cases := []struct {
		target  string
		surface string
	}{
		{"/api/v1/nodes/node_employee_0018/history", "node_history"},
		{"/api/v1/nodes/node_employee_0018/diff", "node_diff"},
	}
	for _, c := range cases {
		rec := do(t, s, http.MethodGet, c.target, "mock-reader-token")
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s status = %d body=%s", c.target, rec.Code, rec.Body.String())
		}
		var env errEnvelope
		decode(t, rec, &env)
		if env.Error.Code != "/errors/upstream/unimplemented" {
			t.Errorf("%s code = %q", c.target, env.Error.Code)
		}
		if env.Error.Params["surface"] != c.surface {
			t.Errorf("%s surface = %v, want %q", c.target, env.Error.Params["surface"], c.surface)
		}
	}
}

// TestDiffAnchorsUnit exercises the field-delta algorithm directly, including the
// valid_from delta and property add/remove that the fixture's close-correction
// does not cover.
func TestDiffAnchorsUnit(t *testing.T) {
	mk := func(label string, closed bool, vf string, vt *string, props map[string]any) *coreclient.AnchorDTO {
		a := &coreclient.AnchorDTO{Label: label, Closed: closed, Properties: props}
		a.ValidFrom = mustTime(t, vf)
		if vt != nil {
			tt := mustTime(t, *vt)
			a.ValidTo = &tt
		}
		return a
	}
	delta := func(ds []fieldDelta, field string) *fieldDelta {
		for i := range ds {
			if ds[i].Field == field {
				return &ds[i]
			}
		}
		return nil
	}

	// Two non-nil versions differing in label, valid_from, and properties.
	jul := "2026-07-01T00:00:00Z"
	from := mk("Alpha", false, "2026-01-01T00:00:00Z", nil, map[string]any{"tier": "gold", "region": "EU"})
	to := mk("Beta", true, "2026-03-01T00:00:00Z", &jul, map[string]any{"tier": "platinum", "team": "x"})
	ds := diffAnchors(from, to)

	checks := map[string]string{
		"label":             "changed",
		"closed":            "changed",
		"valid_from":        "changed",
		"valid_to":          "changed", // nil -> set
		"properties.tier":   "changed",
		"properties.region": "removed",
		"properties.team":   "added",
	}
	for field, op := range checks {
		d := delta(ds, field)
		if d == nil || d.Op != op {
			t.Errorf("delta %q = %v, want op %q", field, d, op)
		}
	}

	// Both nil -> empty.
	if got := diffAnchors(nil, nil); len(got) != 0 {
		t.Errorf("nil/nil deltas = %v, want empty", got)
	}

	// from nil -> all of `to` added.
	added := diffAnchors(nil, to)
	if d := delta(added, "label"); d == nil || d.Op != "added" {
		t.Errorf("label op = %v, want added", d)
	}
	if d := delta(added, "properties.tier"); d == nil || d.Op != "added" {
		t.Errorf("properties.tier op = %v, want added", d)
	}

	// to nil -> all of `from` removed.
	removed := diffAnchors(from, nil)
	if d := delta(removed, "label"); d == nil || d.Op != "removed" {
		t.Errorf("label op = %v, want removed", d)
	}
}
