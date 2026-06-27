package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

func TestParseAsOf(t *testing.T) {
	if v, err := parseAsOf(""); v != nil || err != nil {
		t.Errorf("empty: got (%v, %v), want (nil, nil)", v, err)
	}

	rfc, err := parseAsOf("2026-03-01T12:30:00Z")
	if err != nil || rfc == nil || !rfc.Equal(time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("rfc3339: got (%v, %v)", rfc, err)
	}

	// A bare date projects to the start of the UTC day.
	day, err := parseAsOf("2026-03-01")
	if err != nil || day == nil || !day.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date-only: got (%v, %v), want 2026-03-01T00:00:00Z", day, err)
	}

	// A zoned RFC3339 instant is normalized to UTC.
	off, err := parseAsOf("2026-03-01T12:30:00+02:00")
	if err != nil || off == nil || !off.Equal(time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)) {
		t.Errorf("offset: got (%v, %v), want 2026-03-01T10:30:00Z", off, err)
	}
	if off != nil && off.Location() != time.UTC {
		t.Errorf("offset location = %v, want UTC", off.Location())
	}

	if _, err := parseAsOf("not-a-time"); err == nil {
		t.Error("expected an error for a malformed as_of")
	}
	if _, err := parseAsOf("2026-13-99"); err == nil {
		t.Error("expected an error for an out-of-range date")
	}
}

func TestParseSystemAsOf(t *testing.T) {
	if v, err := parseSystemAsOf(""); v != nil || err != nil {
		t.Errorf("empty: got (%v, %v), want (nil, nil)", v, err)
	}

	rfc, err := parseSystemAsOf("2026-06-19T12:30:00Z")
	if err != nil || rfc == nil || !rfc.Equal(time.Date(2026, 6, 19, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("rfc3339: got (%v, %v)", rfc, err)
	}

	// A bare date projects to the start of the UTC day.
	day, err := parseSystemAsOf("2026-06-19")
	if err != nil || day == nil || !day.Equal(time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date-only: got (%v, %v), want 2026-06-19T00:00:00Z", day, err)
	}

	// A malformed value is the distinct invalid_system_as_of error.
	_, err = parseSystemAsOf("not-a-time")
	if err == nil {
		t.Fatal("expected an error for a malformed system_as_of")
	}
	if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidSystemAsOf {
		t.Errorf("err = %v, want invalid_system_as_of", err)
	}
}

// TestAnchorsSystemAsOf drives the system-time (transaction-time) axis end to
// end over HTTP: the default view is current-only, and a past system_as_of
// reveals a corrected anchor's pre-correction value.
func TestAnchorsSystemAsOf(t *testing.T) {
	s := newFixtureServer()

	type anchorsBody struct {
		Items []struct {
			ID         string         `json:"id"`
			Properties map[string]any `json:"properties"`
			SystemTo   *string        `json:"system_to"`
		} `json:"items"`
		Page struct {
			TotalEstimate int `json:"total_estimate"`
		} `json:"page"`
	}
	get := func(target string) anchorsBody {
		rec := do(t, s, http.MethodGet, target, "mock-admin-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var body anchorsBody
		decode(t, rec, &body)
		return body
	}
	titleOf := func(b anchorsBody, id string) string {
		for _, a := range b.Items {
			if a.ID == id {
				title, _ := a.Properties["title"].(string)
				return title
			}
		}
		t.Fatalf("anchor %s not found", id)
		return ""
	}

	// Default: current-only (142), corrected employee shows its current title,
	// and no superseded row (system_to != null) leaks through.
	cur := get("/api/v1/anchors?page_size=200")
	if cur.Page.TotalEstimate != 142 {
		t.Fatalf("default total = %d, want 142", cur.Page.TotalEstimate)
	}
	for _, a := range cur.Items {
		if a.SystemTo != nil {
			t.Errorf("default view leaked superseded row %s", a.ID)
		}
	}
	if got := titleOf(cur, "anchor_employee_0009"); got != "Engineering Manager" {
		t.Errorf("default title = %q, want Engineering Manager", got)
	}

	// system_as_of just before the 2026-06-20 correction: the same employee
	// projects to its prior title.
	past := get("/api/v1/anchors?system_as_of=2026-06-19&page_size=200")
	if past.Page.TotalEstimate != 142 {
		t.Fatalf("system_as_of=2026-06-19 total = %d, want 142", past.Page.TotalEstimate)
	}
	if got := titleOf(past, "anchor_employee_0009"); got != "Principal Engineer" {
		t.Errorf("pre-correction title = %q, want Principal Engineer", got)
	}

	// A system_as_of before any anchor existed is an empty (not error) view.
	ancient := get("/api/v1/anchors?system_as_of=2020-01-01&page_size=200")
	if ancient.Page.TotalEstimate != 0 {
		t.Errorf("system_as_of=2020 total = %d, want 0", ancient.Page.TotalEstimate)
	}
}

func TestAnchorsAsOfDateOnly(t *testing.T) {
	s := newFixtureServer()

	// A bare YYYY-MM-DD date is accepted (not a 400) and projected: 2020 is before
	// every anchor's valid_from, so the projected view is empty.
	rec := do(t, s, http.MethodGet, "/api/v1/anchors?as_of=2020-01-01&page_size=200", "mock-admin-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Page struct {
			TotalEstimate int `json:"total_estimate"`
		} `json:"page"`
	}
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 0 {
		t.Errorf("as_of=2020-01-01 total = %d, want 0", body.Page.TotalEstimate)
	}

	// A mid-2026 date projects to a non-empty subset smaller than the current view.
	rec = do(t, s, http.MethodGet, "/api/v1/anchors?as_of=2026-02-01&page_size=200", "mock-admin-token")
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 || body.Page.TotalEstimate >= 142 {
		t.Errorf("as_of=2026-02-01 total = %d, want in (0,142)", body.Page.TotalEstimate)
	}
}

func TestLedgerEntriesAsOfDateOnly(t *testing.T) {
	s := newFixtureServer()
	total := func(target string) int {
		rec := do(t, s, http.MethodGet, target, "mock-admin-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Page struct {
				TotalEstimate int `json:"total_estimate"`
			} `json:"page"`
		}
		decode(t, rec, &body)
		return body.Page.TotalEstimate
	}

	// The ledger-entries surface shares the same parser; a bare date is accepted
	// (no 400) and projected: 2020 is before every entry's effective_from, so the
	// projected view is empty and strictly smaller than the unprojected view.
	unprojected := total("/api/v1/ledgers/GeneralLedger/entries?page_size=200")
	projected := total("/api/v1/ledgers/GeneralLedger/entries?as_of=2020-01-01&page_size=200")
	if unprojected == 0 {
		t.Fatal("expected GeneralLedger to have entries")
	}
	if projected != 0 {
		t.Errorf("as_of=2020-01-01 entries = %d, want 0 (< unprojected %d)", projected, unprojected)
	}
}
