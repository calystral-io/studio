package coreclient

import (
	"testing"

	"github.com/calystral-io/studio/internal/apierr"
)

func TestFixtureLedgerSeed(t *testing.T) {
	f := NewFixture()
	if got := f.LedgerCount(); got != 3 {
		t.Fatalf("ledger count = %d, want 3", got)
	}
	// Round-robin seeding gives a balanced 120 entries per ledger (360 total).
	for _, name := range []string{"GeneralLedger", "AuditLog", "DomainEvents"} {
		if got := f.EntryCount(name); got != 120 {
			t.Errorf("ledger %q entry count = %d, want 120", name, got)
		}
	}
}

func TestFixtureLedgerSeedChainAndSummary(t *testing.T) {
	f := NewFixture()
	for _, l := range f.ledgers {
		es := f.entries[l.Name]
		if len(es) == 0 {
			t.Fatalf("ledger %q has no entries", l.Name)
		}
		// Append chain: first entry has nil prev_lsn; each subsequent entry links
		// to the previous one's lsn; seq is 1-based monotonic; lsn ascending.
		var prev *LedgerEntry
		for i := range es {
			e := es[i]
			if e.Ledger != l.Name {
				t.Errorf("entry %s ledger = %q, want %q", e.ID, e.Ledger, l.Name)
			}
			if e.Seq != int64(i+1) {
				t.Errorf("entry %s seq = %d, want %d", e.ID, e.Seq, i+1)
			}
			if e.TxnID != e.LSN {
				t.Errorf("entry %s txn_id %d != lsn %d", e.ID, e.TxnID, e.LSN)
			}
			if i == 0 {
				if e.PrevLSN != nil {
					t.Errorf("first entry %s prev_lsn = %v, want nil", e.ID, *e.PrevLSN)
				}
			} else {
				if e.PrevLSN == nil || *e.PrevLSN != prev.LSN {
					t.Errorf("entry %s prev_lsn = %v, want %d", e.ID, e.PrevLSN, prev.LSN)
				}
				if e.LSN <= prev.LSN {
					t.Errorf("entry %s lsn %d not ascending after %d", e.ID, e.LSN, prev.LSN)
				}
			}
			prev = &es[i]
		}
		// Summary mirrors the last (most recent) entry.
		last := es[len(es)-1]
		if l.EntryCountEstimate != len(es) {
			t.Errorf("ledger %q entry_count_estimate = %d, want %d", l.Name, l.EntryCountEstimate, len(es))
		}
		if l.LastLSN != last.LSN {
			t.Errorf("ledger %q last_lsn = %d, want %d", l.Name, l.LastLSN, last.LSN)
		}
		if l.LastRecordedAt == nil || !l.LastRecordedAt.Equal(last.RecordedAt) {
			t.Errorf("ledger %q last_recorded_at = %v, want %v", l.Name, l.LastRecordedAt, last.RecordedAt)
		}
	}
}

func TestFixtureLedgersListAndQuery(t *testing.T) {
	f := NewFixture()
	res, err := f.ListLedgers(ctx(), ListLedgersParams{TenantID: FixtureTenant, PageSize: 25})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != 3 || len(res.Items) != 3 {
		t.Fatalf("ledgers total = %d items = %d, want 3/3", res.Page.TotalEstimate, len(res.Items))
	}
	if res.Source != SourceFixture {
		t.Errorf("source = %q", res.Source)
	}
	// Sorted by name (stable opaque order).
	if res.Items[0].Name != "AuditLog" {
		t.Errorf("first ledger = %q, want AuditLog (name sort)", res.Items[0].Name)
	}

	// q substring over name+description.
	qres, err := f.ListLedgers(ctx(), ListLedgersParams{TenantID: FixtureTenant, PageSize: 25, Q: "accounting"})
	if err != nil {
		t.Fatal(err)
	}
	if qres.Page.TotalEstimate != 1 || qres.Items[0].Name != "GeneralLedger" {
		t.Fatalf("q=accounting matched %d (%v), want only GeneralLedger", qres.Page.TotalEstimate, qres.Items)
	}
}

func TestFixtureLedgersTenantScoping(t *testing.T) {
	f := NewFixture()
	res, err := f.ListLedgers(ctx(), ListLedgersParams{TenantID: "other-tenant", PageSize: 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.Page.TotalEstimate != 0 {
		t.Fatalf("foreign tenant saw %d ledgers", len(res.Items))
	}
}

func TestFixtureLedgerEntriesUnknownLedger(t *testing.T) {
	f := NewFixture()
	_, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{TenantID: FixtureTenant, Name: "NoSuchLedger", PageSize: 25})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeNotFound {
		t.Fatalf("err = %v, want not_found", err)
	}
	if ae.Params["resource"] != "ledger:NoSuchLedger" {
		t.Fatalf("resource = %v, want ledger:NoSuchLedger", ae.Params["resource"])
	}
}

func TestFixtureLedgerEntriesForeignTenantIsNotFound(t *testing.T) {
	f := NewFixture()
	_, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{TenantID: "other", Name: "GeneralLedger", PageSize: 25})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeNotFound {
		t.Fatalf("err = %v, want not_found for foreign tenant", err)
	}
}

func TestFixtureLedgerEntriesDescendingAndPagination(t *testing.T) {
	f := NewFixture()
	const ledger = "GeneralLedger"
	const pageSize = 25
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	var lastTotal int
	var prevLSN int64 = 1 << 62 // strictly-decreasing guard, start high

	for {
		res, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{
			TenantID: FixtureTenant, Name: ledger, PageSize: pageSize, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		lastTotal = res.Page.TotalEstimate
		for _, e := range res.Items {
			if seen[e.ID] {
				t.Fatalf("duplicate entry across pages: %s", e.ID)
			}
			seen[e.ID] = true
			if e.LSN >= prevLSN {
				t.Fatalf("entries not strictly descending: %d after %d", e.LSN, prevLSN)
			}
			prevLSN = e.LSN
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

	if len(seen) != 120 || lastTotal != 120 {
		t.Fatalf("walked %d unique (total %d), want 120", len(seen), lastTotal)
	}
	wantPages := (120 + pageSize - 1) / pageSize
	if pages != wantPages {
		t.Fatalf("walked %d pages, want %d", pages, wantPages)
	}
}

func TestFixtureLedgerEntriesKindFilter(t *testing.T) {
	f := NewFixture()
	res, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{
		TenantID: FixtureTenant, Name: "GeneralLedger", PageSize: 200, Kind: "reversal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Fatal("expected some reversal entries")
	}
	for _, e := range res.Items {
		if e.Kind != "reversal" {
			t.Errorf("entry %s kind = %q, want reversal", e.ID, e.Kind)
		}
	}
}

func TestFixtureLedgerEntriesQuerySubstring(t *testing.T) {
	f := NewFixture()
	res, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{
		TenantID: FixtureTenant, Name: "GeneralLedger", PageSize: 200, Q: "4000-revenue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 {
		t.Fatal("expected substring matches over summary+payload for '4000-revenue'")
	}
	for _, e := range res.Items {
		if !entryMatchesQuery(e, "4000-revenue") {
			t.Errorf("entry %s does not contain query term", e.ID)
		}
	}
}

func TestFixtureLedgerEntriesAsOfProjection(t *testing.T) {
	f := NewFixture()
	const ledger = "GeneralLedger"
	early := mustUTC("2026-01-10T00:00:00Z")
	late := mustUTC("2026-06-30T00:00:00Z")

	resEarly, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{TenantID: FixtureTenant, Name: ledger, PageSize: 200, AsOf: &early})
	if err != nil {
		t.Fatal(err)
	}
	resLate, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{TenantID: FixtureTenant, Name: ledger, PageSize: 200, AsOf: &late})
	if err != nil {
		t.Fatal(err)
	}
	if resEarly.Page.TotalEstimate == 0 {
		t.Fatal("expected some entries valid at early as_of")
	}
	if resEarly.Page.TotalEstimate >= resLate.Page.TotalEstimate {
		t.Fatalf("expected early(%d) < late(%d) as_of counts", resEarly.Page.TotalEstimate, resLate.Page.TotalEstimate)
	}
	for _, e := range resEarly.Items {
		if !entryValidAt(e, early) {
			t.Errorf("entry %s not valid at early as_of", e.ID)
		}
	}
}

func TestFixtureLedgerEntriesLSNRange(t *testing.T) {
	f := NewFixture()
	const ledger = "AuditLog"
	// Establish the ledger's lsn span.
	all, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{TenantID: FixtureTenant, Name: ledger, PageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) < 10 {
		t.Fatalf("need entries to bound, got %d", len(all.Items))
	}
	// Items are descending; pick an interior window.
	hi := all.Items[2].LSN
	lo := all.Items[len(all.Items)-3].LSN

	res, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{
		TenantID: FixtureTenant, Name: ledger, PageSize: 200, FromLSN: &lo, ToLSN: &hi,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate == 0 || res.Page.TotalEstimate >= all.Page.TotalEstimate {
		t.Fatalf("bounded total %d should be in (0,%d)", res.Page.TotalEstimate, all.Page.TotalEstimate)
	}
	for _, e := range res.Items {
		if e.LSN < lo || e.LSN > hi {
			t.Errorf("entry lsn %d outside [%d,%d]", e.LSN, lo, hi)
		}
	}
}

func TestFixtureLedgerEntriesInvalidLSNRange(t *testing.T) {
	f := NewFixture()
	from := int64(100)
	to := int64(50)
	_, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{
		TenantID: FixtureTenant, Name: "GeneralLedger", PageSize: 25, FromLSN: &from, ToLSN: &to,
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInvalidLSNRange {
		t.Fatalf("err = %v, want invalid_lsn_range", err)
	}
	if ae.Params["from"] != int64(100) || ae.Params["to"] != int64(50) {
		t.Fatalf("params = %v, want from=100 to=50", ae.Params)
	}
}

func TestFixtureLedgerEntriesInvalidCursor(t *testing.T) {
	f := NewFixture()
	_, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{
		TenantID: FixtureTenant, Name: "GeneralLedger", PageSize: 25, Cursor: "!!!not-base64!!!",
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestLedgerCursorRoundTrip(t *testing.T) {
	for _, off := range []int{0, 1, 25, 119, 360} {
		tok := encodeCursor(off)
		got, err := decodeCursor(tok)
		if err != nil {
			t.Fatalf("decode(%q): %v", tok, err)
		}
		if got != off {
			t.Errorf("roundtrip offset = %d, want %d", got, off)
		}
	}
	// A cursor beyond the result set yields an empty terminal page (no error).
	f := NewFixture()
	res, err := f.ListLedgerEntries(ctx(), ListLedgerEntriesParams{
		TenantID: FixtureTenant, Name: "DomainEvents", PageSize: 25, Cursor: encodeCursor(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 0 || res.Page.HasMore {
		t.Fatalf("expected empty terminal page, got %d items has_more=%v", len(res.Items), res.Page.HasMore)
	}
}
