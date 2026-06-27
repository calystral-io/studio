package coreclient

import (
	"fmt"
	"strings"
	"testing"

	"github.com/calystral-io/studio/internal/apierr"
)

func TestFixtureRuntimeSummaryRollup(t *testing.T) {
	f := NewFixture()
	res, err := f.RuntimeSummary(ctx(), RuntimeSummaryParams{TenantID: FixtureTenant})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceFixture {
		t.Errorf("source = %q", res.Source)
	}
	s := res.Summary

	// opcode_count and metric_series_count are derived from the seeded rows.
	if s.OpcodeCount != len(f.opcodes) {
		t.Errorf("opcode_count = %d, want %d", s.OpcodeCount, len(f.opcodes))
	}
	if s.MetricSeriesCount != countSeries(s.MetricGroups) {
		t.Errorf("metric_series_count = %d, want %d", s.MetricSeriesCount, countSeries(s.MetricGroups))
	}
	// The registry has exactly 18 named cvm_* series across 4 subsystems.
	if s.MetricSeriesCount != 18 {
		t.Errorf("metric_series_count = %d, want 18", s.MetricSeriesCount)
	}
	if len(s.MetricGroups) != 4 {
		t.Errorf("metric groups = %d, want 4", len(s.MetricGroups))
	}

	// instructions_executed equals the sum of per-opcode exec counts.
	var sum uint64
	for _, op := range f.opcodes {
		sum += op.ExecCount
	}
	if s.InstructionsExecuted != sum {
		t.Errorf("instructions_executed = %d, want %d", s.InstructionsExecuted, sum)
	}

	// active_transactions mirrors the cvm_txn_active gauge.
	if got := gaugeValue(s.MetricGroups, "cvm_txn_active"); int64(s.ActiveTransactions) != got {
		t.Errorf("active_transactions = %d, want gauge %d", s.ActiveTransactions, got)
	}

	// Plan-cache rollup agrees with the seeded entries.
	if s.PlanCache.Entries != len(f.planCache) {
		t.Errorf("plan_cache.entries = %d, want %d", s.PlanCache.Entries, len(f.planCache))
	}
	if s.PlanCache.CapacityBytes != planCacheCapacityBytes {
		t.Errorf("plan_cache.capacity = %d, want %d", s.PlanCache.CapacityBytes, planCacheCapacityBytes)
	}
	// resident_bytes excludes pinned entries.
	var resident uint64
	for _, e := range f.planCache {
		if !e.Pinned {
			resident += e.SizeBytes
		}
	}
	if s.PlanCache.ResidentBytes != resident {
		t.Errorf("plan_cache.resident_bytes = %d, want %d (pinned excluded)", s.PlanCache.ResidentBytes, resident)
	}
	if s.PlanCache.HitRateMilli < 0 || s.PlanCache.HitRateMilli > 1000 {
		t.Errorf("hit_rate_milli = %d, out of [0,1000]", s.PlanCache.HitRateMilli)
	}
}

func TestFixtureMetricSeriesKindsConsistent(t *testing.T) {
	f := NewFixture()
	for _, g := range f.runtime.MetricGroups {
		if len(g.Series) == 0 {
			t.Errorf("subsystem %q has no series", g.Subsystem)
		}
		for _, s := range g.Series {
			switch s.Kind {
			case "counter":
				if s.Value == nil || *s.Value < 0 {
					t.Errorf("counter %s must have a non-negative scalar value", s.Name)
				}
				if s.Histogram != nil {
					t.Errorf("counter %s must not carry a histogram", s.Name)
				}
			case "gauge":
				if s.Value == nil {
					t.Errorf("gauge %s must have a scalar value", s.Name)
				}
			case "histogram":
				if s.Value != nil {
					t.Errorf("histogram %s must not carry a scalar value", s.Name)
				}
				if s.Histogram == nil || len(s.Histogram.Buckets) == 0 {
					t.Fatalf("histogram %s must carry buckets", s.Name)
				}
				// The final bucket is the +Inf overflow (nil upper bound), and its
				// count equals the histogram count; buckets are non-decreasing.
				last := s.Histogram.Buckets[len(s.Histogram.Buckets)-1]
				if last.UpperBound != nil {
					t.Errorf("histogram %s last bucket must be +Inf (nil bound)", s.Name)
				}
				if last.Count != s.Histogram.Count {
					t.Errorf("histogram %s +Inf count %d != count %d", s.Name, last.Count, s.Histogram.Count)
				}
				var prev uint64
				for _, b := range s.Histogram.Buckets {
					if b.Count < prev {
						t.Errorf("histogram %s buckets not cumulative", s.Name)
					}
					prev = b.Count
				}
			default:
				t.Errorf("series %s has unknown kind %q", s.Name, s.Kind)
			}
		}
	}
}

func TestFixtureOpcodesSeed(t *testing.T) {
	f := NewFixture()
	if len(f.opcodes) < 100 {
		t.Fatalf("opcode count = %d, want a near-complete instruction set (>=100)", len(f.opcodes))
	}

	res, err := f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 500})
	if err != nil {
		t.Fatal(err)
	}
	// Codes are unique, sorted ascending, hex matches, exec_share in range, and
	// short_form tracks the single-byte (< 0x100) discriminant range.
	seen := map[int]bool{}
	var prev = -1
	var shareSum int
	for _, op := range res.Items {
		if seen[op.Code] {
			t.Errorf("duplicate opcode code %d (%s)", op.Code, op.Mnemonic)
		}
		seen[op.Code] = true
		if op.Code < prev {
			t.Errorf("opcodes not sorted by code: %d after %d", op.Code, prev)
		}
		prev = op.Code
		if want := fmt.Sprintf("0x%04X", op.Code); op.CodeHex != want {
			t.Errorf("opcode %s code_hex = %q, want %q", op.Mnemonic, op.CodeHex, want)
		}
		if op.ShortForm != (op.Code < shortFormMax) {
			t.Errorf("opcode %s short_form = %v for code %#x", op.Mnemonic, op.ShortForm, op.Code)
		}
		if op.ExecShareMilli < 0 || op.ExecShareMilli > 1000 {
			t.Errorf("opcode %s exec_share_milli = %d out of range", op.Mnemonic, op.ExecShareMilli)
		}
		shareSum += op.ExecShareMilli
	}
	// Per-mille shares of a partition sum to <= 1000 (integer truncation).
	if shareSum > 1000 {
		t.Errorf("exec_share_milli sums to %d, want <= 1000", shareSum)
	}
}

func TestFixtureOpcodesFilters(t *testing.T) {
	f := NewFixture()

	// Category filter: every row is in the requested category.
	res, err := f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 500, Category: catControlFlow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Fatal("expected control_flow opcodes")
	}
	for _, op := range res.Items {
		if op.Category != catControlFlow {
			t.Errorf("opcode %s category = %q, want control_flow", op.Mnemonic, op.Category)
		}
	}

	// q matches the mnemonic (case-insensitive). "jmp" hits the Jmp* family.
	res, err = f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 500, Q: "jmp"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Fatal("expected jmp opcodes")
	}
	for _, op := range res.Items {
		if !contains(strings.ToLower(op.Mnemonic), "jmp") {
			t.Errorf("opcode %s does not contain query term", op.Mnemonic)
		}
	}

	// Combined category + q.
	res, err = f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 500, Category: catLedger, Q: "ledger"})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range res.Items {
		if op.Category != catLedger {
			t.Errorf("combined filter leaked category %q", op.Category)
		}
	}

	// Unknown category matches nothing (no error).
	res, err = f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 500, Category: "nonsense"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 0 {
		t.Errorf("unknown category matched %d, want 0", res.Page.TotalEstimate)
	}
}

func TestFixtureOpcodesPaginationWalksAll(t *testing.T) {
	f := NewFixture()
	total := len(f.opcodes)

	seen := map[int]bool{}
	cursor := ""
	pages := 0
	for {
		res, err := f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 20, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range res.Items {
			if seen[op.Code] {
				t.Errorf("opcode %d seen twice across pages", op.Code)
			}
			seen[op.Code] = true
		}
		pages++
		if !res.Page.HasMore {
			break
		}
		if res.Page.NextCursor == nil {
			t.Fatal("has_more set but next_cursor nil")
		}
		cursor = *res.Page.NextCursor
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("walked %d opcodes, want %d", len(seen), total)
	}
}

func TestFixturePlanCacheFiltersAndPagination(t *testing.T) {
	f := NewFixture()

	all, err := f.ListPlanCacheEntries(ctx(), ListPlanCacheEntriesParams{PageSize: 500})
	if err != nil {
		t.Fatal(err)
	}
	if all.Page.TotalEstimate != len(f.planCache) {
		t.Errorf("total = %d, want %d", all.Page.TotalEstimate, len(f.planCache))
	}
	// Keys are sorted ascending and look like 64-hex content addresses.
	var prev string
	for _, e := range all.Items {
		if e.Key < prev {
			t.Errorf("plan-cache keys not sorted: %q after %q", e.Key, prev)
		}
		prev = e.Key
		if len(e.Key) != 64 {
			t.Errorf("plan-cache key %q is not 64 hex chars", e.Key)
		}
	}

	// pinned=true and pinned=false partition the set.
	pinned, err := f.ListPlanCacheEntries(ctx(), ListPlanCacheEntriesParams{PageSize: 500, Pinned: "true"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pinned.Items {
		if !e.Pinned {
			t.Errorf("pinned=true returned unpinned entry %q", e.Key)
		}
	}
	unpinned, err := f.ListPlanCacheEntries(ctx(), ListPlanCacheEntriesParams{PageSize: 500, Pinned: "false"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range unpinned.Items {
		if e.Pinned {
			t.Errorf("pinned=false returned pinned entry %q", e.Key)
		}
	}
	if pinned.Page.TotalEstimate+unpinned.Page.TotalEstimate != all.Page.TotalEstimate {
		t.Errorf("pinned(%d)+unpinned(%d) != total(%d)", pinned.Page.TotalEstimate, unpinned.Page.TotalEstimate, all.Page.TotalEstimate)
	}
	if pinned.Page.TotalEstimate == 0 {
		t.Error("expected at least one pinned entry")
	}

	// An unknown pinned value matches nothing.
	none, err := f.ListPlanCacheEntries(ctx(), ListPlanCacheEntriesParams{PageSize: 500, Pinned: "maybe"})
	if err != nil {
		t.Fatal(err)
	}
	if none.Page.TotalEstimate != 0 {
		t.Errorf("pinned=maybe matched %d, want 0", none.Page.TotalEstimate)
	}

	// q matches a key prefix substring.
	first := all.Items[0].Key
	res, err := f.ListPlanCacheEntries(ctx(), ListPlanCacheEntriesParams{PageSize: 500, Q: first[:8]})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Errorf("q=%q matched nothing", first[:8])
	}
	for _, e := range res.Items {
		if !contains(e.Key, first[:8]) {
			t.Errorf("entry %q does not contain query prefix", e.Key)
		}
	}
}

func TestFixtureRuntimeInvalidAndBeyondEndCursor(t *testing.T) {
	f := NewFixture()

	if _, err := f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 25, Cursor: "!!!bad!!!"}); err == nil {
		t.Fatal("expected invalid cursor from ListOpcodes")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
	if _, err := f.ListPlanCacheEntries(ctx(), ListPlanCacheEntriesParams{PageSize: 25, Cursor: "!!!bad!!!"}); err == nil {
		t.Fatal("expected invalid cursor from ListPlanCacheEntries")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}

	// Cursor beyond the end is an empty terminal page, not an error.
	res, err := f.ListOpcodes(ctx(), ListOpcodesParams{PageSize: 25, Cursor: encodeCursor(10_000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.Page.HasMore {
		t.Fatalf("expected empty terminal page, got %d items has_more=%v", len(res.Items), res.Page.HasMore)
	}
	pres, err := f.ListPlanCacheEntries(ctx(), ListPlanCacheEntriesParams{PageSize: 25, Cursor: encodeCursor(10_000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(pres.Items) != 0 || pres.Page.HasMore {
		t.Fatalf("expected empty terminal page, got %d items has_more=%v", len(pres.Items), pres.Page.HasMore)
	}
}
