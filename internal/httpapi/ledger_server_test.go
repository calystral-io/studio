package httpapi

import (
	"net"
	"net/http"
	"testing"

	"google.golang.org/grpc"

	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
	"github.com/calystral-io/studio/internal/corepb/querypb"
)

type ledgersBody struct {
	Items  []coreclient.LedgerSummary `json:"items"`
	Page   coreclient.Page            `json:"page"`
	Source string                     `json:"source"`
}

type entriesBody struct {
	Items  []coreclient.LedgerEntry `json:"items"`
	Page   coreclient.Page          `json:"page"`
	Source string                   `json:"source"`
}

func TestLedgersHappyPath(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/ledgers", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body ledgersBody
	decode(t, rec, &body)
	if body.Source != "fixture" {
		t.Errorf("source = %q, want fixture", body.Source)
	}
	if body.Page.TotalEstimate != 3 || len(body.Items) != 3 {
		t.Fatalf("ledgers total = %d items = %d, want 3/3", body.Page.TotalEstimate, len(body.Items))
	}
	for _, l := range body.Items {
		if l.Name == "" || l.Kind == "" || l.EntryCountEstimate == 0 {
			t.Errorf("ledger summary incomplete: %+v", l)
		}
	}
}

func TestLedgersRequiresAuth(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/ledgers", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestLedgerEntriesHappyPathAndCursorWalk(t *testing.T) {
	s := newFixtureServer()

	cursor := ""
	total := 0
	pages := 0
	var prevLSN int64 = 1 << 62
	seen := map[string]bool{}
	for {
		target := "/api/v1/ledgers/GeneralLedger/entries?page_size=40"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body entriesBody
		decode(t, rec, &body)
		pages++
		if body.Source != "fixture" {
			t.Errorf("source = %q", body.Source)
		}
		if body.Page.TotalEstimate != 120 {
			t.Errorf("total_estimate = %d, want 120", body.Page.TotalEstimate)
		}
		for _, e := range body.Items {
			if seen[e.ID] {
				t.Fatalf("duplicate entry %s", e.ID)
			}
			seen[e.ID] = true
			if e.LSN >= prevLSN {
				t.Fatalf("entries not strictly descending: %d after %d", e.LSN, prevLSN)
			}
			prevLSN = e.LSN
			if e.Ledger != "GeneralLedger" {
				t.Errorf("entry %s ledger = %q", e.ID, e.Ledger)
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
	if total != 120 {
		t.Fatalf("walked %d entries, want 120", total)
	}
}

func TestLedgerEntriesKindAndQueryFilters(t *testing.T) {
	s := newFixtureServer()

	rec := do(t, s, http.MethodGet, "/api/v1/ledgers/AuditLog/entries?kind=login&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body entriesBody
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected login entries in AuditLog")
	}
	for _, e := range body.Items {
		if e.Kind != "login" {
			t.Errorf("entry %s kind = %q, want login", e.ID, e.Kind)
		}
	}

	rec = do(t, s, http.MethodGet, "/api/v1/ledgers/DomainEvents/entries?q=invoice&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected substring matches for 'invoice'")
	}
}

func TestLedgerEntriesLSNRangeFilter(t *testing.T) {
	s := newFixtureServer()
	// Bounded window returns a subset; all entries within bounds.
	rec := do(t, s, http.MethodGet, "/api/v1/ledgers/GeneralLedger/entries?from_lsn=7100&to_lsn=7200&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body entriesBody
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected some entries in lsn window [7100,7200]")
	}
	for _, e := range body.Items {
		if e.LSN < 7100 || e.LSN > 7200 {
			t.Errorf("entry lsn %d outside [7100,7200]", e.LSN)
		}
	}
}

func TestLedgerEntriesUnknownLedger404(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/ledgers/NoSuchLedger/entries", "mock-reader-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/not_found" {
		t.Errorf("code = %q", env.Error.Code)
	}
	if env.Error.Params["resource"] != "ledger:NoSuchLedger" {
		t.Errorf("resource = %v, want ledger:NoSuchLedger", env.Error.Params["resource"])
	}
}

func TestLedgerEntriesValidation(t *testing.T) {
	s := newFixtureServer()
	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"page_size too large", "/api/v1/ledgers/GeneralLedger/entries?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"page_size zero", "/api/v1/ledgers/GeneralLedger/entries?page_size=0", "/errors/validation/page_size_out_of_range"},
		{"bad cursor", "/api/v1/ledgers/GeneralLedger/entries?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
		{"bad as_of", "/api/v1/ledgers/GeneralLedger/entries?as_of=not-a-time", "/errors/validation/invalid_as_of"},
		{"inverted lsn range", "/api/v1/ledgers/GeneralLedger/entries?from_lsn=900&to_lsn=100", "/errors/validation/invalid_lsn_range"},
		{"non-integer lsn", "/api/v1/ledgers/GeneralLedger/entries?from_lsn=abc", "/errors/validation/invalid_lsn_range"},
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

func TestLedgersValidationPageSize(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/ledgers?page_size=999", "mock-reader-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/validation/page_size_out_of_range" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestLedgersForbiddenWithoutReader(t *testing.T) {
	s := New(rolelessAuth{}, coreclient.NewFixture(), quietLogger(), Options{})
	for _, target := range []string{"/api/v1/ledgers", "/api/v1/ledgers/GeneralLedger/entries"} {
		rec := do(t, s, http.MethodGet, target, "any")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
	}
}

// --- gRPC source 501 path for both ledger surfaces --------------------------

func newGRPCFixtureServer(t *testing.T) *Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcSrv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(grpcSrv, unimplementedQuery{})
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	signer, err := auth.NewPrincipalSigner("")
	if err != nil {
		t.Fatal(err)
	}
	core, err := coreclient.NewGRPCClient(lis.Addr().String(), signer, coreclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	return New(auth.MockAuthenticator{}, core, quietLogger(), Options{})
}

func TestLedgersGRPCSourceReturns501(t *testing.T) {
	s := newGRPCFixtureServer(t)
	cases := []struct {
		target  string
		surface string
	}{
		{"/api/v1/ledgers", "ledgers"},
		{"/api/v1/ledgers/GeneralLedger/entries", "ledger_entries"},
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
