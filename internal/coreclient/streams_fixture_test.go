package coreclient

import (
	"testing"

	"github.com/calystral-io/studio/internal/apierr"
)

func TestFixtureMessagingSummaryRollup(t *testing.T) {
	f := NewFixture()
	res, err := f.MessagingSummary(ctx(), MessagingSummaryParams{TenantID: FixtureTenant})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceFixture {
		t.Errorf("source = %q", res.Source)
	}
	s := res.Summary

	if s.ChannelCount != len(f.channels) || s.ChannelCount != 48 {
		t.Errorf("channel_count = %d, want 48", s.ChannelCount)
	}
	if s.ByKind.Stream != 30 || s.ByKind.Queue != 18 {
		t.Errorf("by_kind = %+v, want stream 30 / queue 18", s.ByKind)
	}
	if s.ByKind.Stream+s.ByKind.Queue != s.ChannelCount {
		t.Errorf("by_kind sum %d != channel_count %d", s.ByKind.Stream+s.ByKind.Queue, s.ChannelCount)
	}
	if s.ByStatus.Open != 43 || s.ByStatus.Closed != 5 {
		t.Errorf("by_status = %+v, want open 43 / closed 5", s.ByStatus)
	}
	if s.ByStatus.Open+s.ByStatus.Closed != s.ChannelCount {
		t.Errorf("by_status sum != channel_count")
	}
	if s.EphemeralCount != 7 {
		t.Errorf("ephemeral_count = %d, want 7", s.EphemeralCount)
	}
	if s.SubscriptionCount != len(f.subscriptions) || s.SubscriptionCount != 96 {
		t.Errorf("subscription_count = %d, want 96", s.SubscriptionCount)
	}

	// Aggregates recomputed from the rows.
	wantBuffered := 0
	var wantDropped int64
	for _, sub := range f.subscriptions {
		wantBuffered += sub.Buffered
		wantDropped += sub.Dropped + sub.OutOfSpanDropped
	}
	wantInFlight := 0
	for _, c := range f.channels {
		wantInFlight += c.InFlight
	}
	if s.TotalBuffered != wantBuffered {
		t.Errorf("total_buffered = %d, want %d", s.TotalBuffered, wantBuffered)
	}
	if s.TotalDropped != wantDropped {
		t.Errorf("total_dropped = %d, want %d", s.TotalDropped, wantDropped)
	}
	if s.TotalInFlight != wantInFlight {
		t.Errorf("total_in_flight = %d, want %d", s.TotalInFlight, wantInFlight)
	}

	// Five live cvm_channels_* series; the buffer-depth gauge mirrors total_buffered.
	if len(s.Metrics) != 5 {
		t.Fatalf("metrics = %d, want 5", len(s.Metrics))
	}
	if v := gaugeValue([]MetricGroup{{Series: s.Metrics}}, "cvm_channels_subscriber_buffer_depth"); v != int64(s.TotalBuffered) {
		t.Errorf("buffer_depth gauge = %d, want total_buffered %d", v, s.TotalBuffered)
	}
}

func TestFixtureChannelSubscriptionCrossReference(t *testing.T) {
	f := NewFixture()
	// SubscriptionCount is set only on stream channels and sums to the total.
	total := 0
	for _, c := range f.channels {
		if c.Kind != ChannelStream && c.SubscriptionCount != 0 {
			t.Errorf("queue channel %s has subscriptions", c.ID)
		}
		total += c.SubscriptionCount
	}
	if total != len(f.subscriptions) {
		t.Errorf("subscription_count sum %d != %d", total, len(f.subscriptions))
	}
	// Queue channels carry ack_mode + visibility timeout; streams do not.
	for _, c := range f.channels {
		if c.Kind == ChannelQueue {
			if c.AckMode == nil || c.VisibilityTimeoutSecs == nil {
				t.Errorf("queue %s missing ack_mode/visibility_timeout", c.ID)
			}
		} else if c.AckMode != nil || c.VisibilityTimeoutSecs != nil || c.InFlight != 0 || c.Redelivery != 0 {
			t.Errorf("stream %s has queue-only fields", c.ID)
		}
	}
}

func TestFixtureChannelsFilters(t *testing.T) {
	f := NewFixture()

	stream, err := f.ListChannels(ctx(), ListChannelsParams{PageSize: 200, Kind: ChannelStream})
	if err != nil {
		t.Fatal(err)
	}
	if stream.Page.TotalEstimate != 30 {
		t.Errorf("stream channels = %d, want 30", stream.Page.TotalEstimate)
	}
	for _, c := range stream.Items {
		if c.Kind != ChannelStream {
			t.Errorf("channel %s kind = %q", c.ID, c.Kind)
		}
	}

	queue, _ := f.ListChannels(ctx(), ListChannelsParams{PageSize: 200, Kind: ChannelQueue})
	if queue.Page.TotalEstimate != 18 {
		t.Errorf("queue channels = %d, want 18", queue.Page.TotalEstimate)
	}

	closed, _ := f.ListChannels(ctx(), ListChannelsParams{PageSize: 200, Status: ChannelClosed})
	if closed.Page.TotalEstimate != 5 {
		t.Errorf("closed channels = %d, want 5", closed.Page.TotalEstimate)
	}

	// q matches carried type / name (Telemetry channels: i%5==2 → 10).
	tele, _ := f.ListChannels(ctx(), ListChannelsParams{PageSize: 200, Q: "telemetry"})
	if tele.Page.TotalEstimate != 10 {
		t.Errorf("q=telemetry = %d, want 10", tele.Page.TotalEstimate)
	}

	// Combined kind + status: stream AND closed.
	combo, _ := f.ListChannels(ctx(), ListChannelsParams{PageSize: 200, Kind: ChannelStream, Status: ChannelClosed})
	for _, c := range combo.Items {
		if c.Kind != ChannelStream || c.Status != ChannelClosed {
			t.Errorf("combo leaked %s (%s,%s)", c.ID, c.Kind, c.Status)
		}
	}
	if combo.Page.TotalEstimate != 3 {
		t.Errorf("stream+closed = %d, want 3", combo.Page.TotalEstimate)
	}

	// Unknown kind matches nothing (no error).
	none, _ := f.ListChannels(ctx(), ListChannelsParams{PageSize: 200, Kind: "topic"})
	if none.Page.TotalEstimate != 0 {
		t.Errorf("unknown kind matched %d", none.Page.TotalEstimate)
	}
}

func TestFixtureSubscriptionsFilters(t *testing.T) {
	f := NewFixture()

	// channel filter: subscriptions on chan_0001 (== that channel's subscription_count).
	var chanOneCount int
	for _, c := range f.channels {
		if c.ID == channelID(0) {
			chanOneCount = c.SubscriptionCount
		}
	}
	res, err := f.ListSubscriptions(ctx(), ListSubscriptionsParams{PageSize: 200, Channel: channelID(0)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Page.TotalEstimate != chanOneCount || chanOneCount == 0 {
		t.Errorf("channel filter = %d, want chan_0001 subscription_count %d", res.Page.TotalEstimate, chanOneCount)
	}
	for _, s := range res.Items {
		if s.ChannelID != channelID(0) {
			t.Errorf("subscription %s channel = %q", s.ID, s.ChannelID)
		}
	}

	// ordering filter (strictly_ordered: n%4==0 → 24).
	ord, _ := f.ListSubscriptions(ctx(), ListSubscriptionsParams{PageSize: 200, Ordering: OrderingStrictlyOrdered})
	if ord.Page.TotalEstimate != 24 {
		t.Errorf("strictly_ordered = %d, want 24", ord.Page.TotalEstimate)
	}

	// overflow filter (pause: n%3==2 → 32) — and paused subs never drop.
	pause, _ := f.ListSubscriptions(ctx(), ListSubscriptionsParams{PageSize: 200, Overflow: OverflowPause})
	if pause.Page.TotalEstimate != 32 {
		t.Errorf("pause = %d, want 32", pause.Page.TotalEstimate)
	}
	for _, s := range pause.Items {
		if s.Dropped != 0 {
			t.Errorf("paused subscription %s dropped %d, want 0", s.ID, s.Dropped)
		}
		if s.Lag < 0 {
			t.Errorf("subscription %s negative lag", s.ID)
		}
	}

	// Unknown channel matches nothing.
	none, _ := f.ListSubscriptions(ctx(), ListSubscriptionsParams{PageSize: 200, Channel: "chan_9999"})
	if none.Page.TotalEstimate != 0 {
		t.Errorf("unknown channel matched %d", none.Page.TotalEstimate)
	}
}

func TestFixtureMessagingPaginationWalksAll(t *testing.T) {
	f := NewFixture()

	walk := func(name string, total int, page func(cursor string) (Page, []string, error)) {
		seen := map[string]bool{}
		cursor := ""
		var prev string
		pages := 0
		for {
			pg, ids, err := page(cursor)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, id := range ids {
				if seen[id] {
					t.Fatalf("%s: duplicate %s", name, id)
				}
				seen[id] = true
				if prev != "" && id <= prev {
					t.Fatalf("%s: not ascending %s after %s", name, id, prev)
				}
				prev = id
			}
			pages++
			if !pg.HasMore {
				break
			}
			cursor = *pg.NextCursor
			if pages > total {
				t.Fatalf("%s: did not terminate", name)
			}
		}
		if len(seen) != total {
			t.Errorf("%s: walked %d, want %d", name, len(seen), total)
		}
	}

	walk("channels", 48, func(cursor string) (Page, []string, error) {
		r, err := f.ListChannels(ctx(), ListChannelsParams{PageSize: 10, Cursor: cursor})
		if err != nil {
			return Page{}, nil, err
		}
		ids := make([]string, len(r.Items))
		for i, c := range r.Items {
			ids[i] = c.ID
		}
		return r.Page, ids, nil
	})

	walk("subscriptions", 96, func(cursor string) (Page, []string, error) {
		r, err := f.ListSubscriptions(ctx(), ListSubscriptionsParams{PageSize: 10, Cursor: cursor})
		if err != nil {
			return Page{}, nil, err
		}
		ids := make([]string, len(r.Items))
		for i, s := range r.Items {
			ids[i] = s.ID
		}
		return r.Page, ids, nil
	})
}

func TestFixtureMessagingInvalidAndBeyondEndCursor(t *testing.T) {
	f := NewFixture()

	if _, err := f.ListChannels(ctx(), ListChannelsParams{PageSize: 25, Cursor: "!!!bad!!!"}); err == nil {
		t.Fatal("expected invalid cursor from ListChannels")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}
	if _, err := f.ListSubscriptions(ctx(), ListSubscriptionsParams{PageSize: 25, Cursor: "!!!bad!!!"}); err == nil {
		t.Fatal("expected invalid cursor from ListSubscriptions")
	} else if ae, ok := err.(*apierr.APIError); !ok || ae.Code != apierr.CodeInvalidCursor {
		t.Fatalf("err = %v, want invalid_cursor", err)
	}

	cres, err := f.ListChannels(ctx(), ListChannelsParams{PageSize: 25, Cursor: encodeCursor(1000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(cres.Items) != 0 || cres.Page.HasMore {
		t.Fatalf("expected empty terminal channel page, got %d has_more=%v", len(cres.Items), cres.Page.HasMore)
	}
	sres, err := f.ListSubscriptions(ctx(), ListSubscriptionsParams{PageSize: 25, Cursor: encodeCursor(1000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(sres.Items) != 0 || sres.Page.HasMore {
		t.Fatalf("expected empty terminal subscription page, got %d has_more=%v", len(sres.Items), sres.Page.HasMore)
	}
}
