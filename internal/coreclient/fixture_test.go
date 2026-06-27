package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

func ctx() context.Context { return context.Background() }

func TestFixtureSeedCount(t *testing.T) {
	f := NewFixture()
	// The seed stores 142 current anchors plus 12 superseded prior system-time
	// versions (6 employees, 3 projects, 3 customers) for the transaction-time
	// axis: 154 rows total, of which 142 are current (system_to == nil).
	if got := f.Count(); got != 154 {
		t.Fatalf("seed count = %d, want 154 (142 current + 12 superseded)", got)
	}

	current, superseded := 0, 0
	for _, a := range f.anchors {
		if a.SystemTo == nil {
			current++
		} else {
			superseded++
		}
	}
	if current != 142 {
		t.Errorf("current anchors = %d, want 142", current)
	}
	if superseded != 12 {
		t.Errorf("superseded anchors = %d, want 12", superseded)
	}
}

func TestFixtureSourceAndCheck(t *testing.T) {
	f := NewFixture()
	if f.Source() != SourceFixture {
		t.Errorf("source = %q", f.Source())
	}
	if c := f.CheckCore(ctx()); c != CheckSkip {
		t.Errorf("check = %q, want skip", c)
	}
}

func TestFixtureTenantScoping(t *testing.T) {
	f := NewFixture()
	res, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: "other-tenant", PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.Page.TotalEstimate != 0 {
		t.Fatalf("expected zero anchors for foreign tenant, got %d (total %d)", len(res.Items), res.Page.TotalEstimate)
	}
}

func TestFixturePaginationWalksAllItems(t *testing.T) {
	f := NewFixture()
	const pageSize = 25
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	var lastTotal int

	for {
		res, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: pageSize, Cursor: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		lastTotal = res.Page.TotalEstimate
		if res.Page.PageSize != pageSize {
			t.Errorf("page_size echoed = %d", res.Page.PageSize)
		}
		for _, a := range res.Items {
			if seen[a.ID] {
				t.Fatalf("duplicate id across pages: %s", a.ID)
			}
			seen[a.ID] = true
		}
		if !res.Page.HasMore {
			if res.Page.NextCursor != nil {
				t.Error("next_cursor must be null when has_more is false")
			}
			break
		}
		if res.Page.NextCursor == nil {
			t.Fatal("has_more true but next_cursor nil")
		}
		cursor = *res.Page.NextCursor
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != 142 {
		t.Fatalf("walked %d unique items, want 142", len(seen))
	}
	if lastTotal != 142 {
		t.Fatalf("total_estimate = %d, want 142", lastTotal)
	}
	wantPages := (142 + pageSize - 1) / pageSize
	if pages != wantPages {
		t.Fatalf("walked %d pages, want %d", pages, wantPages)
	}
}

func TestFixtureTypeFilter(t *testing.T) {
	f := NewFixture()
	res, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200, Type: "Department"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 12 {
		t.Fatalf("Department total = %d, want 12", res.Page.TotalEstimate)
	}
	for _, a := range res.Items {
		if a.Type != "Department" {
			t.Errorf("unexpected type %q", a.Type)
		}
	}
}

func TestFixtureQuerySubstring(t *testing.T) {
	f := NewFixture()
	// Query a known property value (case-insensitive over label+properties).
	res, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200, Q: "lovelace"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Fatal("expected substring matches for 'lovelace'")
	}
	for _, a := range res.Items {
		if !containsFold(a) {
			t.Errorf("anchor %s does not contain query term", a.ID)
		}
	}
}

func containsFold(a AnchorDTO) bool {
	return matchesQuery(a, "lovelace")
}

func TestFixtureAsOfProjection(t *testing.T) {
	f := NewFixture()
	// Early as_of excludes anchors whose valid_from is later in the year.
	early, err := time.Parse(time.RFC3339, "2026-01-10T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	resEarly, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200, AsOf: &early})
	if err != nil {
		t.Fatal(err)
	}
	// A later as_of should include strictly more (later valid_from) anchors.
	late, _ := time.Parse(time.RFC3339, "2026-06-30T00:00:00Z")
	resLate, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200, AsOf: &late})
	if err != nil {
		t.Fatal(err)
	}
	if resEarly.Page.TotalEstimate >= resLate.Page.TotalEstimate {
		t.Fatalf("expected early(%d) < late(%d) as_of counts", resEarly.Page.TotalEstimate, resLate.Page.TotalEstimate)
	}
	// Every returned anchor must actually be valid at the projection instant.
	for _, a := range resEarly.Items {
		if !validAt(a, early) {
			t.Errorf("anchor %s not valid at early as_of", a.ID)
		}
	}
}

func TestFixtureSystemTimeProjection(t *testing.T) {
	f := NewFixture()

	findTitle := func(items []AnchorDTO, id string) (string, bool) {
		for _, a := range items {
			if a.ID == id {
				title, _ := a.Properties["title"].(string)
				return title, true
			}
		}
		return "", false
	}

	// Default (no system_as_of): current-only view. Exactly the 142 open rows,
	// every one with system_to == nil, and the corrected employee shows its
	// current title.
	cur, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Page.TotalEstimate != 142 {
		t.Fatalf("current view total = %d, want 142", cur.Page.TotalEstimate)
	}
	for _, a := range cur.Items {
		if a.SystemTo != nil {
			t.Errorf("current view leaked a superseded row: %s", a.ID)
		}
	}
	curTitle, ok := findTitle(cur.Items, "anchor_employee_0009")
	if !ok {
		t.Fatal("anchor_employee_0009 missing from current view")
	}
	if curTitle != "Engineering Manager" {
		t.Errorf("current title = %q, want Engineering Manager", curTitle)
	}

	// A system_as_of just before the correction instant (2026-06-20) reveals the
	// value as originally recorded. The corrected employee shows its prior title,
	// and no current (open) row is selected for that id.
	before, _ := time.Parse(time.RFC3339, "2026-06-19T00:00:00Z")
	past, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200, SystemAsOf: &before})
	if err != nil {
		t.Fatal(err)
	}
	if past.Page.TotalEstimate != 142 {
		t.Fatalf("system_as_of=2026-06-19 total = %d, want 142 (every id has one version then)", past.Page.TotalEstimate)
	}
	pastTitle, ok := findTitle(past.Items, "anchor_employee_0009")
	if !ok {
		t.Fatal("anchor_employee_0009 missing from pre-correction view")
	}
	if pastTitle != "Principal Engineer" {
		t.Errorf("pre-correction title = %q, want Principal Engineer", pastTitle)
	}
	// Every selected row must actually contain the projection instant.
	for _, a := range past.Items {
		if !systemAt(a, before) {
			t.Errorf("anchor %s not system-valid at 2026-06-19", a.ID)
		}
	}

	// A system_as_of before any anchor was recorded yields an empty view.
	ancient, _ := time.Parse(time.RFC3339, "2020-01-01T00:00:00Z")
	empty, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200, SystemAsOf: &ancient})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Page.TotalEstimate != 0 {
		t.Errorf("system_as_of=2020 total = %d, want 0", empty.Page.TotalEstimate)
	}
}

func TestFixtureSystemTimeBoundary(t *testing.T) {
	f := NewFixture()
	// At exactly the correction instant the half-open intervals flip cleanly: the
	// prior row [orig, corr) excludes t==corr while the current row [corr, nil)
	// includes it, so the corrected anchor resolves to its current value with no
	// double-count.
	corr, _ := time.Parse(time.RFC3339, "2026-06-20T00:00:00Z")
	res, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 200, SystemAsOf: &corr})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 142 {
		t.Fatalf("system_as_of=correction-instant total = %d, want 142", res.Page.TotalEstimate)
	}
	for _, a := range res.Items {
		if a.ID == "anchor_employee_0009" {
			if title, _ := a.Properties["title"].(string); title != "Engineering Manager" {
				t.Errorf("at correction instant title = %q, want current Engineering Manager", title)
			}
			if a.SystemTo != nil {
				t.Errorf("at correction instant expected the current (open) row, got system_to=%v", a.SystemTo)
			}
		}
	}
}

func TestFixtureBothAxesCompose(t *testing.T) {
	f := NewFixture()
	// anchor_employee_0009 has valid_from 2026-05-19 and a system correction at
	// 2026-06-20. The two axes compose as a logical AND.
	const id = "anchor_employee_0009"
	titleOf := func(items []AnchorDTO) (string, bool) {
		for _, a := range items {
			if a.ID == id {
				title, _ := a.Properties["title"].(string)
				return title, true
			}
		}
		return "", false
	}

	// valid-time AFTER its valid_from + system-time BEFORE its correction: the
	// anchor is present with its prior (pre-correction) title.
	validAfter, _ := time.Parse(time.RFC3339, "2026-06-01T00:00:00Z")
	systemBefore, _ := time.Parse(time.RFC3339, "2026-06-19T00:00:00Z")
	both, err := f.ListAnchors(ctx(), ListAnchorsParams{
		TenantID: FixtureTenant, PageSize: 200, AsOf: &validAfter, SystemAsOf: &systemBefore,
	})
	if err != nil {
		t.Fatal(err)
	}
	title, ok := titleOf(both.Items)
	if !ok {
		t.Fatalf("%s should be present (valid at 2026-06-01, recorded by 2026-06-19)", id)
	}
	if title != "Principal Engineer" {
		t.Errorf("composed title = %q, want prior Principal Engineer", title)
	}

	// valid-time BEFORE its valid_from excludes it regardless of the system axis
	// (AND, not OR).
	validBefore, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	excluded, err := f.ListAnchors(ctx(), ListAnchorsParams{
		TenantID: FixtureTenant, PageSize: 200, AsOf: &validBefore, SystemAsOf: &systemBefore,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := titleOf(excluded.Items); present {
		t.Errorf("%s must be excluded when valid-time precedes its valid_from", id)
	}
}

func TestFixtureInvalidCursor(t *testing.T) {
	f := NewFixture()
	_, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 25, Cursor: "!!!not-base64!!!"})
	if err == nil {
		t.Fatal("expected invalid cursor error")
	}
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestFixtureCursorBeyondEnd(t *testing.T) {
	f := NewFixture()
	// A cursor whose offset exceeds the result set yields an empty final page.
	far := encodeCursor(1000)
	res, err := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 25, Cursor: far})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.Page.HasMore {
		t.Fatalf("expected empty terminal page, got %d items has_more=%v", len(res.Items), res.Page.HasMore)
	}
}

func assertNotFound(t *testing.T, err error) {
	t.Helper()
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeNotFound {
		t.Fatalf("err = %v, want not_found", err)
	}
}

func TestFixtureAnchorHistory(t *testing.T) {
	f := NewFixture()

	// employee_0018 was corrected: a prior open version (old title) plus a
	// current closed version (corrected title) — two system-versions of one id.
	res, err := f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: "anchor_employee_0018"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 2 {
		t.Fatalf("history versions = %d, want 2", len(res.Versions))
	}
	// Ordered (valid_from, system_from) ascending: the superseded prior first.
	if res.Versions[0].SystemTo == nil {
		t.Error("first version should be the superseded (system_to != nil) prior")
	}
	if res.Versions[1].SystemTo != nil {
		t.Error("second version should be the current (system_to == nil)")
	}
	if !res.Versions[1].Closed || res.Versions[1].ValidTo == nil {
		t.Error("current version should be closed with a bounded valid window")
	}

	// Unknown id -> 404.
	_, err = f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: "anchor_nope"})
	assertNotFound(t, err)

	// Foreign tenant -> 404 (no cross-tenant existence leak).
	_, err = f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: "other-tenant", ID: "anchor_employee_0018"})
	assertNotFound(t, err)
}

func TestFixtureAnchorDiff(t *testing.T) {
	f := NewFixture()
	may1, _ := time.Parse(time.RFC3339, "2026-05-01T00:00:00Z")
	jun19, _ := time.Parse(time.RFC3339, "2026-06-19T00:00:00Z")
	y2020, _ := time.Parse(time.RFC3339, "2020-01-01T00:00:00Z")

	// from = pre-correction (system 2026-06-19), to = current (system open), both
	// at the same valid instant: the system-time correction is visible.
	res, err := f.GetAnchorDiff(ctx(), GetAnchorDiffParams{
		TenantID: FixtureTenant, ID: "anchor_employee_0018",
		FromValidAt: may1, FromSystemAt: &jun19,
		ToValidAt: may1, ToSystemAt: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FromVersion == nil || res.ToVersion == nil {
		t.Fatalf("expected both versions resolved, got from=%v to=%v", res.FromVersion, res.ToVersion)
	}
	if res.FromVersion.SystemTo == nil {
		t.Error("from should be the superseded (pre-correction) version")
	}
	if !res.ToVersion.Closed {
		t.Error("to should be the current closed version")
	}

	// A valid coordinate before the anchor existed resolves to no version.
	res2, err := f.GetAnchorDiff(ctx(), GetAnchorDiffParams{
		TenantID: FixtureTenant, ID: "anchor_employee_0018",
		FromValidAt: y2020, FromSystemAt: nil,
		ToValidAt: may1, ToSystemAt: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.FromVersion != nil {
		t.Error("from at 2020 should resolve to no version")
	}
	if res2.ToVersion == nil {
		t.Error("to at 2026-05-01 should resolve to the current version")
	}

	// Unknown id -> 404.
	_, err = f.GetAnchorDiff(ctx(), GetAnchorDiffParams{TenantID: FixtureTenant, ID: "anchor_nope", FromValidAt: may1, ToValidAt: may1})
	assertNotFound(t, err)
}

// TestFixtureVersionRectanglesNonOverlapping enforces the bitemporal invariant
// that, per id, the version rectangles [valid] x [system] are pairwise
// disjoint, so any (valid, system) coordinate resolves to at most one version.
func TestFixtureVersionRectanglesNonOverlapping(t *testing.T) {
	f := NewFixture()
	byID := map[string][]AnchorDTO{}
	for _, a := range f.anchors {
		byID[a.ID] = append(byID[a.ID], a)
	}
	for id, vs := range byID {
		for i := 0; i < len(vs); i++ {
			for j := i + 1; j < len(vs); j++ {
				if intervalsOverlap(vs[i].ValidFrom, vs[i].ValidTo, vs[j].ValidFrom, vs[j].ValidTo) &&
					intervalsOverlap(vs[i].SystemFrom, vs[i].SystemTo, vs[j].SystemFrom, vs[j].SystemTo) {
					t.Errorf("anchor %s has overlapping version rectangles (%d,%d)", id, i, j)
				}
			}
		}
	}
}

// intervalsOverlap reports whether two half-open [from,to) intervals share any
// instant (a nil upper bound is open/unbounded).
func intervalsOverlap(aFrom time.Time, aTo *time.Time, bFrom time.Time, bTo *time.Time) bool {
	left := aTo == nil || bFrom.Before(*aTo)
	right := bTo == nil || aFrom.Before(*bTo)
	return left && right
}
