// Fixture messaging source: a seeded, deterministic in-memory snapshot of the
// cvm-channels runtime - durable channels (kind "stream" or "queue"), their live
// queue/ephemeral state, and the live subscriptions (stream cursors). Honestly
// tagged source:"fixture", it gives the operator UI real paginated, filterable
// data in PR5 without a live Core. Like the cluster/runtime views this is live
// state, not bitemporal: each DTO carries an `observed_at` snapshot instant.
//
// cvm-channels exposes no enumeration accessor or gRPC surface today (only
// per-id getters and a Prometheus text render), so the live set is fabricated
// here behind the demo-data tag; the gRPC path maps to 501 until Core grows a
// list accessor. The summary's counts and aggregates are DERIVED from the seeded
// rows so they always agree with them.
package coreclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Channel kinds and statuses (mirrors cvm-channels ChannelKind/ChannelStatus).
const (
	ChannelStream = "stream"
	ChannelQueue  = "queue"

	ChannelOpen   = "open"
	ChannelClosed = "closed"
)

// Subscription enum values (mirrors cvm-channels StartAt/Ordering/OverflowPolicy).
const (
	StartTail   = "tail"
	StartOffset = "offset"
	StartAsOf   = "as_of"

	OrderingPerPartition    = "per_partition"
	OrderingStrictlyOrdered = "strictly_ordered"

	OverflowDropOldest = "drop_oldest"
	OverflowDropNewest = "drop_newest"
	OverflowPause      = "pause"
)

// MessagingSummary implements CoreClient. Returns the precomputed rollup. The
// messaging runtime is shared operator infrastructure, so it is not tenant-scoped.
func (f *Fixture) MessagingSummary(_ context.Context, _ MessagingSummaryParams) (*MessagingSummaryResult, error) {
	return &MessagingSummaryResult{Summary: f.messaging, Source: SourceFixture}, nil
}

// ListChannels applies kind/status/q filters, then cursor-paginates a stable id
// sort. Q matches the name, carried type, or placement (case-insensitive).
func (f *Fixture) ListChannels(_ context.Context, p ListChannelsParams) (*ListChannelsResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]ChannelDTO, 0, len(f.channels))
	for _, c := range f.channels {
		if p.Kind != "" && c.Kind != p.Kind {
			continue
		}
		if p.Status != "" && c.Status != p.Status {
			continue
		}
		if q != "" && !channelMatchesQuery(c, q) {
			continue
		}
		filtered = append(filtered, c)
	}

	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })

	total := len(filtered)
	items := []ChannelDTO{}
	if offset < total {
		end := offset + p.PageSize
		if end > total {
			end = total
		}
		items = filtered[offset:end]
	}

	page := Page{PageSize: p.PageSize, TotalEstimate: total}
	if offset+len(items) < total {
		page.HasMore = true
		c := encodeCursor(offset + len(items))
		page.NextCursor = &c
	}

	return &ListChannelsResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// ListSubscriptions applies channel/ordering/overflow/q filters, then
// cursor-paginates a stable id sort. Q matches the subscription id or channel
// name (case-insensitive substring).
func (f *Fixture) ListSubscriptions(_ context.Context, p ListSubscriptionsParams) (*ListSubscriptionsResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]SubscriptionDTO, 0, len(f.subscriptions))
	for _, s := range f.subscriptions {
		if p.Channel != "" && s.ChannelID != p.Channel {
			continue
		}
		if p.Ordering != "" && s.Ordering != p.Ordering {
			continue
		}
		if p.Overflow != "" && s.Overflow != p.Overflow {
			continue
		}
		if q != "" && !subscriptionMatchesQuery(s, q) {
			continue
		}
		filtered = append(filtered, s)
	}

	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })

	total := len(filtered)
	items := []SubscriptionDTO{}
	if offset < total {
		end := offset + p.PageSize
		if end > total {
			end = total
		}
		items = filtered[offset:end]
	}

	page := Page{PageSize: p.PageSize, TotalEstimate: total}
	if offset+len(items) < total {
		page.HasMore = true
		c := encodeCursor(offset + len(items))
		page.NextCursor = &c
	}

	return &ListSubscriptionsResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// channelMatchesQuery reports whether q occurs in the channel name, carried type,
// or placement (case-insensitive substring).
func channelMatchesQuery(c ChannelDTO, q string) bool {
	return strings.Contains(strings.ToLower(c.Name), q) ||
		strings.Contains(strings.ToLower(c.Carries), q) ||
		strings.Contains(strings.ToLower(c.Placement), q)
}

// subscriptionMatchesQuery reports whether q occurs in the subscription id or its
// channel name (case-insensitive substring).
func subscriptionMatchesQuery(s SubscriptionDTO, q string) bool {
	return strings.Contains(strings.ToLower(s.ID), q) ||
		strings.Contains(strings.ToLower(s.ChannelName), q)
}

// --- Seed data -------------------------------------------------------------

const (
	channelTotal      = 48
	subscriptionTotal = 96
)

var (
	carriedTypes  = []string{"OrderEvent", "AuditRecord", "Telemetry", "Command", "Receipt"}
	placements    = []string{"us-east", "us-west", "eu-central"}
	retentionSecs = []int64{3600, 86_400, 604_800, 2_592_000}
)

// sptr returns a pointer to s (for optional string DTO fields).
func sptr(s string) *string { return &s }

// seedMessaging builds the channels and subscriptions, then derives the rollup
// (counts, aggregates, and the live cvm_channels_* metric series) from the rows
// so the summary always agrees with them.
func seedMessaging() (channels []ChannelDTO, subscriptions []SubscriptionDTO, summary MessagingSummary) {
	observedAt := mustUTC("2026-06-27T09:00:00Z")

	for i := 0; i < channelTotal; i++ {
		seq := i + 1
		// 3-of-5 channels are streams; the rest queues.
		kind := ChannelStream
		if i%5 >= 3 {
			kind = ChannelQueue
		}
		status := ChannelOpen
		if i%11 == 0 {
			status = ChannelClosed
		}
		ephemeral := i%7 == 0
		partitionCount := 1 + i%8
		tenant := FixtureTenant
		if i%5 == 0 {
			tenant = "ops-tenant"
		}

		c := ChannelDTO{
			ID:             channelID(i),
			Name:           fmt.Sprintf("%s-%04d", strings.ToLower(carriedTypes[i%len(carriedTypes)]), seq),
			Tenant:         tenant,
			Kind:           kind,
			Status:         status,
			Carries:        carriedTypes[i%len(carriedTypes)],
			Placement:      placements[i%len(placements)],
			PartitionCount: partitionCount,
			RetentionSecs:  retentionSecs[i%len(retentionSecs)],
			EmitLSN:        int64(10_000 + i*137),
			ObservedAt:     observedAt,
		}
		if partitionCount > 1 {
			c.PartitionedBy = sptr("tenant_id")
		}
		if ephemeral {
			c.Ephemeral = true
			c.TTLSecs = i64(int64(300 + i%600))
		}
		if kind == ChannelQueue {
			if i%2 == 0 {
				c.AckMode = sptr("manual")
			} else {
				c.AckMode = sptr("auto")
			}
			c.VisibilityTimeoutSecs = i64(int64(30 + i%60))
			c.InFlight = (i * 7) % 40
			c.Redelivery = (i * 3) % 8
		}
		channels = append(channels, c)
	}

	// Stream channels carry subscriptions; collect them for round-robin assignment.
	streamIdx := make([]int, 0, len(channels))
	for idx, c := range channels {
		if c.Kind == ChannelStream {
			streamIdx = append(streamIdx, idx)
		}
	}

	subCountByChannel := map[string]int{}
	for n := 0; n < subscriptionTotal; n++ {
		ch := &channels[streamIdx[n%len(streamIdx)]]
		capacity := 128 << (n % 4) // 128, 256, 512, 1024
		overflow := []string{OverflowDropOldest, OverflowDropNewest, OverflowPause}[n%3]
		var dropped int64
		if overflow != OverflowPause {
			dropped = int64((n * 7) % 50)
		}
		var outOfSpan int64
		if n%9 == 0 {
			outOfSpan = int64(n % 20)
		}
		ordering := OrderingPerPartition
		if n%4 == 0 {
			ordering = OrderingStrictlyOrdered
		}
		subscriptions = append(subscriptions, SubscriptionDTO{
			ID:               subscriptionID(n),
			ChannelID:        ch.ID,
			ChannelName:      ch.Name,
			Tenant:           ch.Tenant,
			Start:            []string{StartTail, StartOffset, StartAsOf}[n%3],
			Ordering:         ordering,
			Overflow:         overflow,
			BufferCapacity:   capacity,
			Buffered:         (n * 13) % capacity,
			PartitionSpan:    ch.PartitionCount,
			LiveFromLSN:      ch.EmitLSN - int64(n%500),
			Lag:              int64((n * 11) % 300),
			Dropped:          dropped,
			OutOfSpanDropped: outOfSpan,
			ObservedAt:       observedAt,
		})
		subCountByChannel[ch.ID]++
	}

	// Fold the per-channel subscription counts back onto the channels.
	for idx := range channels {
		channels[idx].SubscriptionCount = subCountByChannel[channels[idx].ID]
	}

	summary = deriveMessagingSummary(channels, subscriptions, observedAt)
	return channels, subscriptions, summary
}

// deriveMessagingSummary computes the rollup from the seeded channels and
// subscriptions. Counts by kind/status, ephemeral, and the buffered/in-flight/
// dropped aggregates are exact tallies; the metric series reuse the runtime
// metric builders and ground the aggregates in the cvm_channels_* names.
func deriveMessagingSummary(channels []ChannelDTO, subs []SubscriptionDTO, observedAt time.Time) MessagingSummary {
	var kinds ChannelKindCounts
	var statuses ChannelStatusCounts
	ephemeral := 0
	totalInFlight := 0
	for _, c := range channels {
		switch c.Kind {
		case ChannelStream:
			kinds.Stream++
		case ChannelQueue:
			kinds.Queue++
		}
		switch c.Status {
		case ChannelOpen:
			statuses.Open++
		case ChannelClosed:
			statuses.Closed++
		}
		if c.Ephemeral {
			ephemeral++
		}
		totalInFlight += c.InFlight
	}

	totalBuffered := 0
	var totalDropped int64
	for _, s := range subs {
		totalBuffered += s.Buffered
		totalDropped += s.Dropped + s.OutOfSpanDropped
	}

	metrics := []MetricSeries{
		histogram("cvm_channels_emit_latency_nanos", "Channel emit latency (ns).",
			nanoHistogram(2_118_551_000,
				[]uint64{10_000, 50_000, 100_000, 500_000, 1_000_000},
				[]uint64{40_118, 188_551, 410_204, 498_900, 511_551}, 512_044)),
		counter("cvm_channels_partition_routes_total", "Total messages routed to a partition.", 18_551_204),
		counter("cvm_channels_partition_history_bumps_total", "Total repartition bumps.", 142),
		gauge("cvm_channels_subscriber_buffer_depth", "Aggregate live subscriber buffer depth.", int64(totalBuffered)),
		counter("cvm_channels_overflow_drops_total", "Total subscriber overflow/out-of-span drops.", totalDropped),
	}

	return MessagingSummary{
		ChannelCount:      len(channels),
		ByKind:            kinds,
		ByStatus:          statuses,
		EphemeralCount:    ephemeral,
		SubscriptionCount: len(subs),
		TotalBuffered:     totalBuffered,
		TotalInFlight:     totalInFlight,
		TotalDropped:      totalDropped,
		Metrics:           metrics,
		ObservedAt:        observedAt,
	}
}

func channelID(i int) string      { return fmt.Sprintf("chan_%04d", i+1) }
func subscriptionID(n int) string { return fmt.Sprintf("sub_%04d", n+1) }
