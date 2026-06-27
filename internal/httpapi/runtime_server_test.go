package httpapi

import (
	"net/http"
	"testing"

	"github.com/calystral-io/studio/internal/coreclient"
)

type runtimeSummaryBody struct {
	UptimeSeconds        int64  `json:"uptime_seconds"`
	InstructionsExecuted uint64 `json:"instructions_executed"`
	ActiveTransactions   int    `json:"active_transactions"`
	OpcodeCount          int    `json:"opcode_count"`
	MetricSeriesCount    int    `json:"metric_series_count"`
	PlanCache            struct {
		Hits          uint64 `json:"hits"`
		Misses        uint64 `json:"misses"`
		Entries       int    `json:"entries"`
		ResidentBytes uint64 `json:"resident_bytes"`
		CapacityBytes uint64 `json:"capacity_bytes"`
		HitRateMilli  int    `json:"hit_rate_milli"`
	} `json:"plan_cache"`
	MetricGroups []struct {
		Subsystem string                    `json:"subsystem"`
		Series    []coreclient.MetricSeries `json:"series"`
	} `json:"metric_groups"`
	ObservedAt string `json:"observed_at"`
	Source     string `json:"source"`
}

type opcodesBody struct {
	Items  []coreclient.OpcodeDTO `json:"items"`
	Page   coreclient.Page        `json:"page"`
	Source string                 `json:"source"`
}

type planCacheBody struct {
	Items  []coreclient.PlanCacheEntryDTO `json:"items"`
	Page   coreclient.Page                `json:"page"`
	Source string                         `json:"source"`
}

func TestRuntimeSummaryHappyPath(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/runtime", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body runtimeSummaryBody
	decode(t, rec, &body)

	if body.Source != "fixture" {
		t.Errorf("source = %q, want fixture", body.Source)
	}
	if body.MetricSeriesCount != 18 || len(body.MetricGroups) != 4 {
		t.Errorf("series = %d groups = %d, want 18 / 4", body.MetricSeriesCount, len(body.MetricGroups))
	}
	if body.OpcodeCount < 100 {
		t.Errorf("opcode_count = %d, want a near-complete instruction set", body.OpcodeCount)
	}
	if body.InstructionsExecuted == 0 {
		t.Error("instructions_executed must be > 0")
	}
	if body.PlanCache.Entries == 0 || body.PlanCache.CapacityBytes == 0 {
		t.Errorf("plan_cache rollup incomplete: %+v", body.PlanCache)
	}
	if body.PlanCache.HitRateMilli < 0 || body.PlanCache.HitRateMilli > 1000 {
		t.Errorf("hit_rate_milli = %d out of range", body.PlanCache.HitRateMilli)
	}
	if body.ObservedAt == "" {
		t.Error("observed_at must be present on the summary")
	}
}

func TestRuntimeOpcodesCursorWalk(t *testing.T) {
	s := newFixtureServer()

	cursor := ""
	total := 0
	pages := 0
	seen := map[int]bool{}
	prevCode := -1
	var grandTotal int
	for {
		target := "/api/v1/runtime/opcodes?page_size=20"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body opcodesBody
		decode(t, rec, &body)
		pages++
		grandTotal = body.Page.TotalEstimate
		for _, op := range body.Items {
			if seen[op.Code] {
				t.Fatalf("duplicate opcode %d", op.Code)
			}
			seen[op.Code] = true
			if op.Code <= prevCode {
				t.Fatalf("opcodes not ascending: %d after %d", op.Code, prevCode)
			}
			prevCode = op.Code
		}
		total += len(body.Items)
		if !body.Page.HasMore {
			if body.Page.NextCursor != nil {
				t.Error("next_cursor must be null on last page")
			}
			break
		}
		cursor = *body.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if total != grandTotal {
		t.Fatalf("walked %d opcodes, want %d", total, grandTotal)
	}
}

func TestRuntimePlanCacheCursorWalk(t *testing.T) {
	s := newFixtureServer()

	cursor := ""
	total := 0
	pages := 0
	seen := map[string]bool{}
	var prevKey string
	grandTotal := 0
	for {
		target := "/api/v1/runtime/plan-cache?page_size=15"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body planCacheBody
		decode(t, rec, &body)
		pages++
		grandTotal = body.Page.TotalEstimate
		for _, e := range body.Items {
			if seen[e.Key] {
				t.Fatalf("duplicate entry %s", e.Key)
			}
			seen[e.Key] = true
			if prevKey != "" && e.Key <= prevKey {
				t.Fatalf("entries not ascending: %s after %s", e.Key, prevKey)
			}
			prevKey = e.Key
		}
		total += len(body.Items)
		if !body.Page.HasMore {
			break
		}
		cursor = *body.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if total != grandTotal {
		t.Fatalf("walked %d entries, want %d", total, grandTotal)
	}
}

func TestRuntimeOpcodesFilters(t *testing.T) {
	s := newFixtureServer()

	rec := do(t, s, http.MethodGet, "/api/v1/runtime/opcodes?category=control_flow&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body opcodesBody
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected control_flow opcodes")
	}
	for _, op := range body.Items {
		if op.Category != "control_flow" {
			t.Errorf("opcode %s category = %q", op.Mnemonic, op.Category)
		}
	}

	// q filter on mnemonic.
	rec = do(t, s, http.MethodGet, "/api/v1/runtime/opcodes?q=jmp&page_size=200", "mock-reader-token")
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected jmp opcodes")
	}

	// Unknown category matches nothing (no 400).
	rec = do(t, s, http.MethodGet, "/api/v1/runtime/opcodes?category=bogus", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown category status = %d", rec.Code)
	}
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 0 {
		t.Errorf("unknown category matched %d", body.Page.TotalEstimate)
	}
}

func TestRuntimePlanCacheFilters(t *testing.T) {
	s := newFixtureServer()

	pinned := do(t, s, http.MethodGet, "/api/v1/runtime/plan-cache?pinned=true&page_size=200", "mock-reader-token")
	if pinned.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", pinned.Code, pinned.Body.String())
	}
	var body planCacheBody
	decode(t, pinned, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected pinned entries")
	}
	for _, e := range body.Items {
		if !e.Pinned {
			t.Errorf("pinned=true returned unpinned entry %s", e.Key)
		}
	}

	// Unknown pinned value matches nothing (no 400).
	rec := do(t, s, http.MethodGet, "/api/v1/runtime/plan-cache?pinned=maybe", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown pinned status = %d", rec.Code)
	}
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 0 {
		t.Errorf("pinned=maybe matched %d", body.Page.TotalEstimate)
	}
}

func TestRuntimeValidation(t *testing.T) {
	s := newFixtureServer()
	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"opcodes page_size too large", "/api/v1/runtime/opcodes?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"opcodes page_size zero", "/api/v1/runtime/opcodes?page_size=0", "/errors/validation/page_size_out_of_range"},
		{"opcodes bad cursor", "/api/v1/runtime/opcodes?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
		{"plan-cache page_size too large", "/api/v1/runtime/plan-cache?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"plan-cache bad cursor", "/api/v1/runtime/plan-cache?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
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

func TestRuntimeRequiresAuth(t *testing.T) {
	s := newFixtureServer()
	for _, target := range []string{"/api/v1/runtime", "/api/v1/runtime/opcodes", "/api/v1/runtime/plan-cache"} {
		rec := do(t, s, http.MethodGet, target, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
	}
}

func TestRuntimeForbiddenWithoutReader(t *testing.T) {
	s := New(rolelessAuth{}, coreclient.NewFixture(), quietLogger(), Options{})
	for _, target := range []string{"/api/v1/runtime", "/api/v1/runtime/opcodes", "/api/v1/runtime/plan-cache"} {
		rec := do(t, s, http.MethodGet, target, "any")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
	}
}

func TestRuntimeGRPCSourceReturns501(t *testing.T) {
	s := newGRPCFixtureServer(t)
	cases := []struct {
		target  string
		surface string
	}{
		{"/api/v1/runtime", "runtime_summary"},
		{"/api/v1/runtime/opcodes", "runtime_opcodes"},
		{"/api/v1/runtime/plan-cache", "runtime_plan_cache"},
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
