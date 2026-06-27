package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

func TestGRPCRuntimeSummaryMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "admin@demo", Roles: []string{"admin", "reader"}, AuditSessionID: "as_rt"}
	_, err := c.RuntimeSummary(context.Background(), RuntimeSummaryParams{TenantID: "demo-tenant", Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != runtimeSummarySurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], runtimeSummarySurface)
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

func TestGRPCListOpcodesMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "reader@demo", Roles: []string{"reader"}, AuditSessionID: "as_op"}
	_, err := c.ListOpcodes(context.Background(), ListOpcodesParams{TenantID: "demo-tenant", PageSize: 25, Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != opcodesSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], opcodesSurface)
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

func TestGRPCListPlanCacheMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "reader@demo", Roles: []string{"reader"}, AuditSessionID: "as_pc"}
	_, err := c.ListPlanCacheEntries(context.Background(), ListPlanCacheEntriesParams{TenantID: "demo-tenant", PageSize: 25, Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != planCacheSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], planCacheSurface)
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

func TestGRPCRuntimeRejectBadCursor(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	p := &auth.Principal{TenantID: "demo-tenant"}

	if _, err := c.ListOpcodes(context.Background(), ListOpcodesParams{TenantID: "demo-tenant", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListOpcodes")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}

	if _, err := c.ListPlanCacheEntries(context.Background(), ListPlanCacheEntriesParams{TenantID: "demo-tenant", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListPlanCacheEntries")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestBuildRuntimeCyQL(t *testing.T) {
	if got := buildRuntimeSummaryCyQL(); !contains(got, "MATCH") || !contains(got, "Runtime") {
		t.Errorf("summary cyql %q missing MATCH/Runtime", got)
	}

	gotO := buildListOpcodesCyQL(ListOpcodesParams{PageSize: 10, Category: catControlFlow, Q: "Jmp"})
	for _, want := range []string{"Opcode", catControlFlow, "Jmp", "ORDER BY o.code", "LIMIT 10"} {
		if !contains(gotO, want) {
			t.Errorf("opcodes cyql %q missing %q", gotO, want)
		}
	}

	gotP := buildListPlanCacheCyQL(ListPlanCacheEntriesParams{PageSize: 25, Pinned: "true", Q: "ab12"})
	for _, want := range []string{"PlanCacheEntry", "pinned = true", "ab12", "ORDER BY e.key", "LIMIT 25"} {
		if !contains(gotP, want) {
			t.Errorf("plan-cache cyql %q missing %q", gotP, want)
		}
	}
}
