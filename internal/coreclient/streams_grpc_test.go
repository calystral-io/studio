package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

func TestGRPCMessagingSummaryMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "admin@demo", Roles: []string{"admin", "reader"}, AuditSessionID: "as_m"}
	_, err := c.MessagingSummary(context.Background(), MessagingSummaryParams{TenantID: "demo-tenant", Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != messagingSummarySurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], messagingSummarySurface)
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

func TestGRPCListChannelsMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "reader@demo", Roles: []string{"reader"}, AuditSessionID: "as_ch"}
	_, err := c.ListChannels(context.Background(), ListChannelsParams{TenantID: "demo-tenant", PageSize: 25, Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != channelsSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], channelsSurface)
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

func TestGRPCListSubscriptionsMapsUnimplemented(t *testing.T) {
	addr, principalCh := startStubCore(t)
	c := newTestGRPCClient(t, addr)

	p := &auth.Principal{TenantID: "demo-tenant", UserID: "reader@demo", Roles: []string{"reader"}, AuditSessionID: "as_sub"}
	_, err := c.ListSubscriptions(context.Background(), ListSubscriptionsParams{TenantID: "demo-tenant", PageSize: 25, Principal: p})
	ae, ok := err.(*apierr.APIError)
	if !ok {
		t.Fatalf("err type %T", err)
	}
	if ae.Code != apierr.CodeUnimplemented {
		t.Fatalf("code = %q, want unimplemented", ae.Code)
	}
	if ae.Params["surface"] != subscriptionsSurface {
		t.Fatalf("surface = %v, want %q", ae.Params["surface"], subscriptionsSurface)
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

func TestGRPCMessagingRejectBadCursor(t *testing.T) {
	addr, _ := startStubCore(t)
	c := newTestGRPCClient(t, addr)
	p := &auth.Principal{TenantID: "demo-tenant"}

	if _, err := c.ListChannels(context.Background(), ListChannelsParams{TenantID: "demo-tenant", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListChannels")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}

	if _, err := c.ListSubscriptions(context.Background(), ListSubscriptionsParams{TenantID: "demo-tenant", PageSize: 25, Cursor: "!!!bad!!!", Principal: p}); err == nil {
		t.Fatal("expected invalid cursor from ListSubscriptions")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
}

func TestBuildMessagingCyQL(t *testing.T) {
	if got := buildMessagingSummaryCyQL(); !contains(got, "MATCH") || !contains(got, "Messaging") {
		t.Errorf("summary cyql %q missing MATCH/Messaging", got)
	}

	gotC := buildListChannelsCyQL(ListChannelsParams{PageSize: 10, Kind: "stream", Status: "open", Q: "telemetry"})
	for _, want := range []string{"Channel", "stream", "open", "telemetry", "ORDER BY c.id", "LIMIT 10"} {
		if !contains(gotC, want) {
			t.Errorf("channels cyql %q missing %q", gotC, want)
		}
	}

	gotS := buildListSubscriptionsCyQL(ListSubscriptionsParams{PageSize: 25, Channel: "chan_0001", Ordering: "strictly_ordered", Overflow: "pause", Q: "sub_0007"})
	for _, want := range []string{"Subscription", "chan_0001", "strictly_ordered", "pause", "sub_0007", "ORDER BY s.id", "LIMIT 25"} {
		if !contains(gotS, want) {
			t.Errorf("subscriptions cyql %q missing %q", gotS, want)
		}
	}
}
