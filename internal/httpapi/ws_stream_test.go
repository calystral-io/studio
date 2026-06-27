package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
)

// wsStreamServer starts a real-upgrade test server with the given snapshot
// interval and returns its ws:// URL.
func wsStreamServer(t *testing.T, interval time.Duration) string {
	t.Helper()
	s := New(auth.MockAuthenticator{}, coreclient.NewFixture(), quietLogger(), Options{WSSnapshotInterval: interval})
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
}

// wsMsg is a generic inbound frame; Payload is decoded per type.
type wsMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// dialAndHello dials with the admin token and consumes the hello frame.
func dialAndHello(t *testing.T, ctx context.Context, wsURL string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"bearer", "mock-admin-token"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var hello wsMsg
	if err := wsjson.Read(ctx, c, &hello); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if hello.Type != "hello" {
		t.Fatalf("first frame = %q, want hello", hello.Type)
	}
	// hello advertises the available topics.
	var hp struct {
		Topics []string `json:"topics"`
	}
	_ = json.Unmarshal(hello.Payload, &hp)
	if len(hp.Topics) != 3 {
		t.Errorf("hello topics = %v, want 3", hp.Topics)
	}
	return c
}

func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) wsMsg {
	t.Helper()
	var m wsMsg
	if err := wsjson.Read(ctx, c, &m); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return m
}

func TestWSSubscribeSnapshot(t *testing.T) {
	wsURL := wsStreamServer(t, time.Hour) // long interval: only the immediate snapshot
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	if err := wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "cluster"}); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}

	// First an ack, then an immediate snapshot.
	ack := readFrame(t, ctx, c)
	if ack.Type != "subscribed" {
		t.Fatalf("frame = %q, want subscribed", ack.Type)
	}
	var ackP struct {
		Topic string `json:"topic"`
	}
	_ = json.Unmarshal(ack.Payload, &ackP)
	if ackP.Topic != "cluster" {
		t.Errorf("ack topic = %q", ackP.Topic)
	}

	snap := readFrame(t, ctx, c)
	if snap.Type != "snapshot" {
		t.Fatalf("frame = %q, want snapshot", snap.Type)
	}
	var sp struct {
		Topic string `json:"topic"`
		Data  struct {
			NodeCount  int    `json:"node_count"`
			ShardCount int    `json:"shard_count"`
			Source     string `json:"source"`
			ObservedAt string `json:"observed_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(snap.Payload, &sp); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if sp.Topic != "cluster" {
		t.Errorf("snapshot topic = %q", sp.Topic)
	}
	// data is the same body the REST endpoint serves (cluster fixture: 9 nodes).
	if sp.Data.NodeCount != 9 || sp.Data.ShardCount != 144 {
		t.Errorf("snapshot data = nodes %d shards %d, want 9 / 144", sp.Data.NodeCount, sp.Data.ShardCount)
	}
	if sp.Data.Source != "fixture" {
		t.Errorf("snapshot source = %q", sp.Data.Source)
	}
	if sp.Data.ObservedAt == "" {
		t.Error("snapshot observed_at must be stamped")
	}
}

func TestWSPeriodicSnapshots(t *testing.T) {
	wsURL := wsStreamServer(t, 25*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	if err := wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "runtime"}); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
	if ack := readFrame(t, ctx, c); ack.Type != "subscribed" {
		t.Fatalf("frame = %q, want subscribed", ack.Type)
	}

	// The immediate snapshot plus at least one ticker-driven snapshot.
	snapshots := 0
	for snapshots < 2 {
		m := readFrame(t, ctx, c)
		if m.Type == "snapshot" {
			snapshots++
		}
	}
}

func TestWSMultipleTopics(t *testing.T) {
	wsURL := wsStreamServer(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	for _, topic := range []string{"cluster", "messaging"} {
		if err := wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: topic}); err != nil {
			t.Fatalf("subscribe %s: %v", topic, err)
		}
	}

	// Collect frames until both topics have delivered a snapshot.
	got := map[string]bool{}
	for len(got) < 2 {
		m := readFrame(t, ctx, c)
		if m.Type == "snapshot" {
			var sp struct {
				Topic string `json:"topic"`
			}
			_ = json.Unmarshal(m.Payload, &sp)
			got[sp.Topic] = true
		}
	}
	if !got["cluster"] || !got["messaging"] {
		t.Errorf("topics delivered = %v, want cluster + messaging", got)
	}
}

func TestWSUnknownTopic(t *testing.T) {
	wsURL := wsStreamServer(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	if err := wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "bogus"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := readFrame(t, ctx, c)
	if m.Type != "error" {
		t.Fatalf("frame = %q, want error", m.Type)
	}
	var ep struct {
		Code  string `json:"code"`
		Topic string `json:"topic"`
	}
	_ = json.Unmarshal(m.Payload, &ep)
	if ep.Code != "/errors/validation/unknown_topic" || ep.Topic != "bogus" {
		t.Errorf("error payload = %+v", ep)
	}
}

func TestWSUnsubscribe(t *testing.T) {
	wsURL := wsStreamServer(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	_ = wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "cluster"})
	if ack := readFrame(t, ctx, c); ack.Type != "subscribed" {
		t.Fatalf("frame = %q, want subscribed", ack.Type)
	}
	if snap := readFrame(t, ctx, c); snap.Type != "snapshot" {
		t.Fatalf("frame = %q, want snapshot", snap.Type)
	}

	_ = wsjson.Write(ctx, c, wsClientMessage{Type: "unsubscribe", Topic: "cluster"})
	if ack := readFrame(t, ctx, c); ack.Type != "unsubscribed" {
		t.Fatalf("frame = %q, want unsubscribed", ack.Type)
	}
}

func TestWSGRPCSourceEmitsErrorEvent(t *testing.T) {
	// A grpc-backed server returns UNIMPLEMENTED for every summary, which must be
	// surfaced as an in-band error event rather than tearing the socket down.
	s := newGRPCFixtureServer(t)
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	_ = wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "cluster"})
	if ack := readFrame(t, ctx, c); ack.Type != "subscribed" {
		t.Fatalf("frame = %q, want subscribed", ack.Type)
	}
	m := readFrame(t, ctx, c)
	if m.Type != "error" {
		t.Fatalf("frame = %q, want error", m.Type)
	}
	var ep struct {
		Code  string `json:"code"`
		Topic string `json:"topic"`
	}
	_ = json.Unmarshal(m.Payload, &ep)
	if ep.Code != "/errors/upstream/unimplemented" || ep.Topic != "cluster" {
		t.Errorf("error payload = %+v, want upstream/unimplemented for cluster", ep)
	}
}

func TestWSUnknownMessageType(t *testing.T) {
	wsURL := wsStreamServer(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	_ = wsjson.Write(ctx, c, struct {
		Type string `json:"type"`
	}{Type: "frobnicate"})
	m := readFrame(t, ctx, c)
	if m.Type != "error" {
		t.Fatalf("frame = %q, want error", m.Type)
	}
	var ep struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(m.Payload, &ep)
	if ep.Code != "/errors/validation/unknown_message" {
		t.Errorf("error code = %q, want unknown_message", ep.Code)
	}
}

func TestWSIdempotentResubscribe(t *testing.T) {
	// time.Hour interval: no ticker snapshots, so the only frames are the ones the
	// commands produce - letting us prove a duplicate subscribe emits nothing.
	wsURL := wsStreamServer(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialAndHello(t, ctx, wsURL)
	defer c.Close(websocket.StatusNormalClosure, "done")

	_ = wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "cluster"})
	if ack := readFrame(t, ctx, c); ack.Type != "subscribed" {
		t.Fatalf("frame = %q, want subscribed", ack.Type)
	}
	if snap := readFrame(t, ctx, c); snap.Type != "snapshot" {
		t.Fatalf("frame = %q, want snapshot", snap.Type)
	}

	// A duplicate subscribe is a no-op; an unsubscribe must be the very next frame
	// (proving the duplicate produced neither a second ack nor a second snapshot).
	_ = wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "cluster"})
	_ = wsjson.Write(ctx, c, wsClientMessage{Type: "unsubscribe", Topic: "cluster"})
	if next := readFrame(t, ctx, c); next.Type != "unsubscribed" {
		t.Fatalf("frame after duplicate subscribe = %q, want unsubscribed", next.Type)
	}
}

func TestWSForbiddenWithoutReader(t *testing.T) {
	// rolelessAuth resolves a principal with no roles for any token.
	s := New(rolelessAuth{}, coreclient.NewFixture(), quietLogger(), Options{WSSnapshotInterval: time.Hour})
	t.Cleanup(s.Close)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Subprotocols: []string{"bearer", "any"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "done")
	if hello := readFrame(t, ctx, c); hello.Type != "hello" {
		t.Fatalf("frame = %q, want hello", hello.Type)
	}

	_ = wsjson.Write(ctx, c, wsClientMessage{Type: "subscribe", Topic: "cluster"})
	m := readFrame(t, ctx, c)
	if m.Type != "error" {
		t.Fatalf("frame = %q, want error", m.Type)
	}
	var ep struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(m.Payload, &ep)
	if ep.Code != "/errors/auth/forbidden" {
		t.Errorf("error code = %q, want forbidden", ep.Code)
	}
}
