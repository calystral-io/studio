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
	if got := f.Count(); got != 142 {
		t.Fatalf("seed count = %d, want 142", got)
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
