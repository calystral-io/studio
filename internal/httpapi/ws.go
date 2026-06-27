// WebSocket live stream (contract section 6): GET /api/v1/ws upgrades,
// authenticates in-handshake (token via `Sec-WebSocket-Protocol: bearer,<token>`
// or `?access_token=`), heartbeats with ping/pong, and frames messages as a typed
// envelope. On connect it sends one `hello`; clients then `subscribe` to a topic
// ("cluster"/"runtime"/"messaging") and receive an immediate `snapshot` plus a
// fresh snapshot every wsSnapshotInterval. Each snapshot's `data` is the same
// shape the matching REST endpoint serves, with `observed_at` stamped to the push
// instant so the live view ticks; we never fabricate metric deltas.
//
// Library choice: github.com/coder/websocket (the maintained successor to
// nhooyr.io/websocket). Picked for its context-first API, built-in Ping, and
// zero cgo - a clean fit for a Go BFF.
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
)

// wsHeartbeatInterval is how often the server pings idle clients.
const wsHeartbeatInterval = 15 * time.Second

// Live stream topics (the three live, non-bitemporal summary surfaces).
const (
	wsTopicCluster   = "cluster"
	wsTopicRuntime   = "runtime"
	wsTopicMessaging = "messaging"
)

// wsEnvelope is the typed message frame exchanged over the socket. Server->client
// types: hello, subscribed, unsubscribed, snapshot, error.
type wsEnvelope struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// wsClientMessage is an inbound control frame from the client. Type is
// "subscribe" or "unsubscribe"; Topic names the live stream.
type wsClientMessage struct {
	Type  string `json:"type"`
	Topic string `json:"topic"`
}

// helloPayload is the message emitted on connect.
type helloPayload struct {
	Principal  principalView `json:"principal"`
	ServerTime string        `json:"server_time"`
	Topics     []string      `json:"topics"`
}

type principalView struct {
	TenantID string   `json:"tenant_id"`
	UserID   string   `json:"user_id"`
	Roles    []string `json:"roles"`
}

// topicPayload is the ack body for subscribed/unsubscribed.
type topicPayload struct {
	Topic string `json:"topic"`
}

// snapshotPayload carries one live snapshot: Data is the same body the matching
// REST endpoint serves (summary fields + source).
type snapshotPayload struct {
	Topic string `json:"topic"`
	Data  any    `json:"data"`
}

// wsErrorPayload is an in-band error event (e.g. unknown topic, forbidden, or an
// upstream gap surfaced over the stream).
type wsErrorPayload struct {
	Topic   string `json:"topic,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleWS performs the auth handshake, upgrades, sends hello, then serves
// topic subscriptions: snapshots on subscribe and on each tick, heartbeats, and a
// single writer (this loop) so concurrent writes never race.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDOf(r)

	// Resolve the token from the WS-specific transports and reuse the standard
	// Authenticator by presenting it as a bearer header. An empty token yields
	// missing_token; an unrecognized one yields invalid_token.
	if token := resolveWSToken(r); token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	principal, err := s.auth.Authenticate(r)
	if err != nil {
		// Not yet upgraded: render the typed 401 envelope over plain HTTP.
		apierr.Write(w, reqID, err)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{"bearer"},
		OriginPatterns: s.originPatterns,
	})
	if err != nil {
		// Accept already wrote an HTTP error response on failure.
		s.logger.Error("ws accept failed", "request_id", reqID, "error", err.Error())
		return
	}
	defer c.CloseNow()

	// Tie the connection to the server base context so Close() drains the push
	// loop on shutdown; the reader goroutine cancels it on client disconnect.
	ctx, cancel := context.WithCancel(s.baseCtx)
	defer cancel()

	hello := wsEnvelope{Type: "hello", Payload: helloPayload{
		Principal: principalView{
			TenantID: principal.TenantID,
			UserID:   principal.UserID,
			Roles:    principal.Roles,
		},
		ServerTime: time.Now().UTC().Format(time.RFC3339),
		Topics:     []string{wsTopicCluster, wsTopicRuntime, wsTopicMessaging},
	}}
	if err := s.wsWrite(ctx, c, hello); err != nil {
		return
	}

	// Reader goroutine: the sole reader. Inbound control frames (ping/pong) are
	// auto-handled by Read; data frames become commands. A read error (client
	// close) cancels the connection context, unblocking the writer loop.
	cmds := make(chan wsClientMessage, 8)
	go func() {
		for {
			var m wsClientMessage
			if err := wsjson.Read(ctx, c, &m); err != nil {
				cancel()
				return
			}
			select {
			case cmds <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	hasReader := principal.HasRole("reader")
	subscribed := map[string]bool{}

	heartbeat := time.NewTicker(wsHeartbeatInterval)
	defer heartbeat.Stop()
	snapshots := time.NewTicker(s.wsSnapshotInterval)
	defer snapshots.Stop()

	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "bye")
			return

		case m := <-cmds:
			if err := s.handleWSCommand(ctx, c, principal, hasReader, subscribed, m); err != nil {
				return
			}

		case <-snapshots.C:
			for topic := range subscribed {
				if err := s.pushSnapshot(ctx, c, principal, topic); err != nil {
					return
				}
			}

		case <-heartbeat.C:
			pingCtx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pingCtx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}

// handleWSCommand applies one inbound subscribe/unsubscribe frame, writing the
// ack (and, for a fresh subscribe, an immediate snapshot). It returns an error
// only on a write failure that should tear down the connection.
func (s *Server) handleWSCommand(
	ctx context.Context,
	c *websocket.Conn,
	principal *auth.Principal,
	hasReader bool,
	subscribed map[string]bool,
	m wsClientMessage,
) error {
	switch m.Type {
	case "subscribe":
		if !isKnownTopic(m.Topic) {
			return s.wsWrite(ctx, c, wsEnvelope{Type: "error", Payload: wsErrorPayload{
				Topic: m.Topic, Code: "/errors/validation/unknown_topic", Message: "unknown topic",
			}})
		}
		if !hasReader {
			return s.wsWrite(ctx, c, wsEnvelope{Type: "error", Payload: wsErrorPayload{
				Topic: m.Topic, Code: "/errors/auth/forbidden", Message: "reader role required",
			}})
		}
		if subscribed[m.Topic] {
			return nil // idempotent re-subscribe
		}
		subscribed[m.Topic] = true
		if err := s.wsWrite(ctx, c, wsEnvelope{Type: "subscribed", Payload: topicPayload{Topic: m.Topic}}); err != nil {
			return err
		}
		return s.pushSnapshot(ctx, c, principal, m.Topic)

	case "unsubscribe":
		delete(subscribed, m.Topic)
		return s.wsWrite(ctx, c, wsEnvelope{Type: "unsubscribed", Payload: topicPayload{Topic: m.Topic}})

	default:
		return s.wsWrite(ctx, c, wsEnvelope{Type: "error", Payload: wsErrorPayload{
			Code: "/errors/validation/unknown_message", Message: "unknown message type",
		}})
	}
}

// pushSnapshot fetches the topic's summary, stamps observed_at to now, and writes
// a snapshot frame. An upstream error (e.g. source=grpc UNIMPLEMENTED) is surfaced
// in-band as an error event rather than tearing the connection down.
func (s *Server) pushSnapshot(ctx context.Context, c *websocket.Conn, principal *auth.Principal, topic string) error {
	data, apiErr := s.summarySnapshot(ctx, principal, topic)
	if apiErr != nil {
		return s.wsWrite(ctx, c, wsEnvelope{Type: "error", Payload: wsErrorPayload{
			Topic: topic, Code: string(apiErr.Code), Message: apiErr.Message,
		}})
	}
	return s.wsWrite(ctx, c, wsEnvelope{Type: "snapshot", Payload: snapshotPayload{Topic: topic, Data: data}})
}

// summarySnapshot builds the live snapshot body for a topic - the same shape the
// matching REST endpoint serves, with observed_at stamped to the push instant.
func (s *Server) summarySnapshot(ctx context.Context, principal *auth.Principal, topic string) (any, *apierr.APIError) {
	now := time.Now().UTC()
	switch topic {
	case wsTopicCluster:
		res, err := s.core.ClusterSummary(ctx, coreclient.ClusterSummaryParams{TenantID: principal.TenantID, Principal: principal})
		if err != nil {
			return nil, asAPIError(err)
		}
		res.Summary.ObservedAt = now
		return clusterSummaryResponse{ClusterSummary: res.Summary, Source: res.Source}, nil
	case wsTopicRuntime:
		res, err := s.core.RuntimeSummary(ctx, coreclient.RuntimeSummaryParams{TenantID: principal.TenantID, Principal: principal})
		if err != nil {
			return nil, asAPIError(err)
		}
		res.Summary.ObservedAt = now
		return runtimeSummaryResponse{RuntimeSummary: res.Summary, Source: res.Source}, nil
	case wsTopicMessaging:
		res, err := s.core.MessagingSummary(ctx, coreclient.MessagingSummaryParams{TenantID: principal.TenantID, Principal: principal})
		if err != nil {
			return nil, asAPIError(err)
		}
		res.Summary.ObservedAt = now
		return messagingSummaryResponse{MessagingSummary: res.Summary, Source: res.Source}, nil
	default:
		return nil, apierr.Internal("snapshot for unknown topic")
	}
}

// wsWrite writes one envelope with a bounded timeout (the loop is the sole writer).
func (s *Server) wsWrite(ctx context.Context, c *websocket.Conn, env wsEnvelope) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(wctx, c, env)
}

// asAPIError coerces a core error into an *apierr.APIError for in-band reporting.
func asAPIError(err error) *apierr.APIError {
	var ae *apierr.APIError
	if errors.As(err, &ae) {
		return ae
	}
	return apierr.Internal("")
}

// isKnownTopic reports whether topic is one of the live summary streams.
func isKnownTopic(topic string) bool {
	switch topic {
	case wsTopicCluster, wsTopicRuntime, wsTopicMessaging:
		return true
	default:
		return false
	}
}

// resolveWSToken extracts the bearer token from the WS subprotocol header or the
// access_token query parameter. Returns "" when neither carries a token.
func resolveWSToken(r *http.Request) string {
	if proto := r.Header.Get("Sec-WebSocket-Protocol"); proto != "" {
		parts := strings.Split(proto, ",")
		if len(parts) >= 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "bearer") {
			if tok := strings.TrimSpace(parts[1]); tok != "" {
				return tok
			}
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("access_token"))
}
