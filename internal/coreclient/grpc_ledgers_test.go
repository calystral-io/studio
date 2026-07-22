package coreclient

import (
	"context"
	"testing"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/corepb/querypb"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// nameRow encodes a catalog projection row: one string column (the ledger name),
// as Core's `MATCH (l:Ledger) RETURN l.name` emits it.
func nameRow(t *testing.T, name string) *querypb.QueryRow {
	t.Helper()
	payload, err := cybrwire.EncodeValue(cybrwire.Array([]cybrwire.Value{cybrwire.Str(name)}))
	if err != nil {
		t.Fatalf("encode name row: %v", err)
	}
	return &querypb.QueryRow{Payload: payload}
}

func ledgerRows(t *testing.T, names ...string) []*querypb.QueryRow {
	rows := make([]*querypb.QueryRow, len(names))
	for i, n := range names {
		rows[i] = nameRow(t, n)
	}
	return rows
}

// Core returns names in arbitrary order; the BFF sorts them (Core can't ORDER BY
// a projected field yet) and surfaces LedgerSummary catalog entries from core.
func TestListLedgersSortsNamesFromCore(t *testing.T) {
	addr := startStubCoreRows(t, ledgerRows(t, "gamma", "alpha", "delta", "beta"))
	c := newTestGRPCClient(t, addr)

	res, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 25, Principal: reader()})
	if err != nil {
		t.Fatalf("ListLedgers: %v", err)
	}
	if res.Source != SourceCore {
		t.Errorf("source = %q, want core", res.Source)
	}
	got := []string{}
	for _, l := range res.Items {
		got = append(got, l.Name)
	}
	want := []string{"alpha", "beta", "delta", "gamma"}
	if len(got) != 4 || got[0] != want[0] || got[3] != want[3] {
		t.Fatalf("names = %v, want sorted %v", got, want)
	}
	if res.Page.TotalEstimate != 4 || res.Page.HasMore {
		t.Errorf("page = %+v, want total 4 no-more", res.Page)
	}
}

// Client-side pagination walks the sorted catalog with a real next cursor —
// which the node-id surfaces can't do (they rely on Core LIMIT with no cursor).
func TestListLedgersPaginates(t *testing.T) {
	addr := startStubCoreRows(t, ledgerRows(t, "d", "a", "c", "b", "e"))
	c := newTestGRPCClient(t, addr)

	res, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 2, Principal: reader()})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(res.Items) != 2 || res.Items[0].Name != "a" || res.Items[1].Name != "b" || !res.Page.HasMore || res.Page.NextCursor == nil {
		t.Fatalf("page 1 = %+v (has_more=%v)", res.Items, res.Page.HasMore)
	}
	res2, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 2, Cursor: *res.Page.NextCursor, Principal: reader()})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(res2.Items) != 2 || res2.Items[0].Name != "c" || res2.Items[1].Name != "d" {
		t.Fatalf("page 2 = %+v", res2.Items)
	}
}

// The `q` filter runs client-side (Core can't filter a projected field yet).
func TestListLedgersQFilter(t *testing.T) {
	addr := startStubCoreRows(t, ledgerRows(t, "audit-log", "billing", "audit-trail"))
	c := newTestGRPCClient(t, addr)

	res, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 25, Q: "AUDIT", Principal: reader()})
	if err != nil {
		t.Fatalf("ListLedgers q: %v", err)
	}
	if len(res.Items) != 2 || res.Items[0].Name != "audit-log" || res.Items[1].Name != "audit-trail" {
		t.Fatalf("q=audit items = %+v, want the two audit ledgers", res.Items)
	}
}

// A row whose column 0 is an Int (a node id), not a string name, violates the
// catalog projection contract -> internal error (Core's fault), never a 4xx.
func TestListLedgersNonStringColumnIsInternal(t *testing.T) {
	addr := startStubCoreRows(t, []*querypb.QueryRow{nodeIDRow(t, 7)})
	c := newTestGRPCClient(t, addr)

	_, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 25, Principal: reader()})
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != apierr.CodeInternal {
		t.Fatalf("err = %v, want internal", err)
	}
}

// No :Ledger nodes -> empty catalog, source core, not a 501.
func TestListLedgersEmpty(t *testing.T) {
	addr := startStubCoreRows(t, nil)
	c := newTestGRPCClient(t, addr)

	res, err := c.ListLedgers(context.Background(), ListLedgersParams{TenantID: "demo-tenant", PageSize: 25, Principal: reader()})
	if err != nil {
		t.Fatalf("empty ledgers: %v", err)
	}
	if len(res.Items) != 0 || res.Source != SourceCore {
		t.Fatalf("res = %+v, want empty from core", res)
	}
}
