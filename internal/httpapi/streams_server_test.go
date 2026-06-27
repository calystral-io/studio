package httpapi

import (
	"net/http"
	"testing"

	"github.com/calystral-io/studio/internal/coreclient"
)

type messagingSummaryBody struct {
	ChannelCount int `json:"channel_count"`
	ByKind       struct {
		Stream int `json:"stream"`
		Queue  int `json:"queue"`
	} `json:"by_kind"`
	ByStatus struct {
		Open   int `json:"open"`
		Closed int `json:"closed"`
	} `json:"by_status"`
	EphemeralCount    int                       `json:"ephemeral_count"`
	SubscriptionCount int                       `json:"subscription_count"`
	TotalBuffered     int                       `json:"total_buffered"`
	TotalInFlight     int                       `json:"total_in_flight"`
	TotalDropped      int64                     `json:"total_dropped"`
	Metrics           []coreclient.MetricSeries `json:"metrics"`
	ObservedAt        string                    `json:"observed_at"`
	Source            string                    `json:"source"`
}

type channelsBody struct {
	Items  []coreclient.ChannelDTO `json:"items"`
	Page   coreclient.Page         `json:"page"`
	Source string                  `json:"source"`
}

type subscriptionsBody struct {
	Items  []coreclient.SubscriptionDTO `json:"items"`
	Page   coreclient.Page              `json:"page"`
	Source string                       `json:"source"`
}

func TestMessagingSummaryHappyPath(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/messaging", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body messagingSummaryBody
	decode(t, rec, &body)

	if body.Source != "fixture" {
		t.Errorf("source = %q, want fixture", body.Source)
	}
	if body.ChannelCount != 48 || body.ByKind.Stream != 30 || body.ByKind.Queue != 18 {
		t.Errorf("counts = %d (stream %d / queue %d)", body.ChannelCount, body.ByKind.Stream, body.ByKind.Queue)
	}
	if body.ByStatus.Open != 43 || body.ByStatus.Closed != 5 {
		t.Errorf("by_status = open %d / closed %d", body.ByStatus.Open, body.ByStatus.Closed)
	}
	if body.SubscriptionCount != 96 {
		t.Errorf("subscription_count = %d, want 96", body.SubscriptionCount)
	}
	if len(body.Metrics) != 5 {
		t.Errorf("metrics = %d, want 5", len(body.Metrics))
	}
	if body.ObservedAt == "" {
		t.Error("observed_at must be present")
	}
}

func TestMessagingChannelsCursorWalk(t *testing.T) {
	s := newFixtureServer()

	cursor := ""
	total := 0
	pages := 0
	seen := map[string]bool{}
	var prevID string
	for {
		target := "/api/v1/messaging/channels?page_size=10"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body channelsBody
		decode(t, rec, &body)
		pages++
		if body.Page.TotalEstimate != 48 {
			t.Errorf("total_estimate = %d, want 48", body.Page.TotalEstimate)
		}
		for _, c := range body.Items {
			if seen[c.ID] {
				t.Fatalf("duplicate channel %s", c.ID)
			}
			seen[c.ID] = true
			if prevID != "" && c.ID <= prevID {
				t.Fatalf("channels not ascending: %s after %s", c.ID, prevID)
			}
			prevID = c.ID
		}
		total += len(body.Items)
		if !body.Page.HasMore {
			if body.Page.NextCursor != nil {
				t.Error("next_cursor must be null on last page")
			}
			break
		}
		cursor = *body.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if total != 48 {
		t.Fatalf("walked %d channels, want 48", total)
	}
}

func TestMessagingSubscriptionsCursorWalk(t *testing.T) {
	s := newFixtureServer()

	cursor := ""
	total := 0
	pages := 0
	seen := map[string]bool{}
	for {
		target := "/api/v1/messaging/subscriptions?page_size=25"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body subscriptionsBody
		decode(t, rec, &body)
		pages++
		if body.Page.TotalEstimate != 96 {
			t.Errorf("total_estimate = %d, want 96", body.Page.TotalEstimate)
		}
		for _, sub := range body.Items {
			if seen[sub.ID] {
				t.Fatalf("duplicate subscription %s", sub.ID)
			}
			seen[sub.ID] = true
			if sub.Lag < 0 {
				t.Errorf("subscription %s negative lag", sub.ID)
			}
		}
		total += len(body.Items)
		if !body.Page.HasMore {
			break
		}
		cursor = *body.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if total != 96 {
		t.Fatalf("walked %d subscriptions, want 96", total)
	}
}

func TestMessagingChannelsFilters(t *testing.T) {
	s := newFixtureServer()

	rec := do(t, s, http.MethodGet, "/api/v1/messaging/channels?kind=queue&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body channelsBody
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 18 {
		t.Errorf("queue channels = %d, want 18", body.Page.TotalEstimate)
	}
	for _, c := range body.Items {
		if c.Kind != "queue" {
			t.Errorf("channel %s kind = %q", c.ID, c.Kind)
		}
		if c.AckMode == nil {
			t.Errorf("queue %s missing ack_mode", c.ID)
		}
	}

	// Unknown status matches nothing (no 400).
	rec = do(t, s, http.MethodGet, "/api/v1/messaging/channels?status=paused", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown status status = %d", rec.Code)
	}
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 0 {
		t.Errorf("unknown status matched %d", body.Page.TotalEstimate)
	}
}

func TestMessagingSubscriptionsFilters(t *testing.T) {
	s := newFixtureServer()

	rec := do(t, s, http.MethodGet, "/api/v1/messaging/subscriptions?overflow=pause&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body subscriptionsBody
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 32 {
		t.Errorf("pause subscriptions = %d, want 32", body.Page.TotalEstimate)
	}
	for _, sub := range body.Items {
		if sub.Overflow != "pause" {
			t.Errorf("subscription %s overflow = %q", sub.ID, sub.Overflow)
		}
	}

	// channel filter: all returned subscriptions reference the channel.
	rec = do(t, s, http.MethodGet, "/api/v1/messaging/subscriptions?channel=chan_0001&page_size=200", "mock-reader-token")
	decode(t, rec, &body)
	if body.Page.TotalEstimate == 0 {
		t.Fatal("expected chan_0001 subscriptions")
	}
	for _, sub := range body.Items {
		if sub.ChannelID != "chan_0001" {
			t.Errorf("subscription %s channel = %q", sub.ID, sub.ChannelID)
		}
	}
}

func TestMessagingValidation(t *testing.T) {
	s := newFixtureServer()
	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"channels page_size too large", "/api/v1/messaging/channels?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"channels page_size zero", "/api/v1/messaging/channels?page_size=0", "/errors/validation/page_size_out_of_range"},
		{"channels bad cursor", "/api/v1/messaging/channels?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
		{"subscriptions page_size too large", "/api/v1/messaging/subscriptions?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"subscriptions bad cursor", "/api/v1/messaging/subscriptions?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
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

func TestMessagingRequiresAuth(t *testing.T) {
	s := newFixtureServer()
	for _, target := range []string{"/api/v1/messaging", "/api/v1/messaging/channels", "/api/v1/messaging/subscriptions"} {
		rec := do(t, s, http.MethodGet, target, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
	}
}

func TestMessagingForbiddenWithoutReader(t *testing.T) {
	s := New(rolelessAuth{}, coreclient.NewFixture(), quietLogger(), Options{})
	for _, target := range []string{"/api/v1/messaging", "/api/v1/messaging/channels", "/api/v1/messaging/subscriptions"} {
		rec := do(t, s, http.MethodGet, target, "any")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d", target, rec.Code)
		}
	}
}

func TestMessagingGRPCSourceReturns501(t *testing.T) {
	s := newGRPCFixtureServer(t)
	cases := []struct {
		target  string
		surface string
	}{
		{"/api/v1/messaging", "messaging_summary"},
		{"/api/v1/messaging/channels", "messaging_channels"},
		{"/api/v1/messaging/subscriptions", "messaging_subscriptions"},
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
