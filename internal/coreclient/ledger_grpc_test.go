package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

func TestGRPCListLedgersMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "admin@demo", Roles: []string{"admin", "reader"}, AuditSessionID: "as_x"}
	_, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 25, Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != ledgersSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], ledgersSurface)
	}

	select {
	case tok := <-principalCh:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe forwarded x-calystral-principal metadata")
	}
}

func TestGRPCListLedgerEntriesMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "admin@demo", Roles: []string{"reader"}, AuditSessionID: "as_y"}
	_, err := c.ListLedgerEntries(context.Background(), ListLedgerEntriesParams{
		TenantID: "demo-tenant", Name: "GeneralLedger", PageSize: 25, Principal: p,
	})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != ledgerEntriesSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], ledgerEntriesSurface)
	}

	select {
	case tok := <-principalCh:
		if tok == "" {
			t.Error("forwarded principal token was empty")
		}
	case <-time.After(2 * time.Second):
		t.Error("did not observe forwarded x-calystral-principal metadata")
	}
}

func TestGRPCLedgersRejectBadCursor(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	p := &auth.Principal{TenantID: "demo-tenant"}

	if _, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListLedgers")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}

	if _, err := c.ListLedgerEntries(context.Background(), ListLedgerEntriesParams{TenantID: "demo-tenant", Name: "GeneralLedger", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListLedgerEntries")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestGRPCLedgerEntriesRejectInvalidLSNRange(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	from := int64(900)
	to := int64(100)
	_, err := c.ListLedgerEntries(context.Background(), ListLedgerEntriesParams{
		TenantID: "demo-tenant", Name: "GeneralLedger", PageSize: 25,
		FromLSN: &from, ToLSN: &to, Principal: &auth.Principal{TenantID: "demo-tenant"},
	})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInvalidLSNRange {
		t.Fatalf("err = %v, want invalid_lsn_range", err)
	}
}

func TestBuildLedgerCyQL(t *testing.T) {
	// The catalog projects the NAME and sorts/filters/limits client-side (Core
	// cannot yet ORDER BY / filter a projected field), so the CyQL is a bare
	// projection with no ORDER BY / WHERE / LIMIT.
	got := buildListLedgersCyQL()
	for _, want := range []string{"MATCH", "Ledger", "RETURN l.name"} {
		if !contains(got, want) {
			t.Errorf("ledgers cyql %q missing %q", got, want)
		}
	}
	for _, unwanted := range []string{"ORDER BY", "LIMIT", "WHERE"} {
		if contains(got, unwanted) {
			t.Errorf("ledgers cyql %q must not contain %q (handled client-side)", got, unwanted)
		}
	}

	from := int64(5)
	to := int64(50)
	gotE := buildListLedgerEntriesCyQL(ListLedgerEntriesParams{
		PageSize: 25, Name: "GeneralLedger", Kind: "posting", Q: "revenue", FromLSN: &from, ToLSN: &to,
	})
	for _, want := range []string{"GeneralLedger", "posting", "revenue", "e.lsn >= 5", "e.lsn <= 50", "ORDER BY e.lsn DESC", "LIMIT 25"} {
		if !contains(gotE, want) {
			t.Errorf("entries cyql %q missing %q", gotE, want)
		}
	}
}
