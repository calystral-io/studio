package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
)

func wsTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	s := New(auth.MockAuthenticator{}, coreclient.NewFixture(), quietLogger(), Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
	return ts, wsURL
}

func TestWSHelloAndHeartbeat(t *testing.T) {
	_, wsURL := wsTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"bearer", "mock-admin-token"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "test done")

	if c.Subprotocol() != "bearer" {
		t.Errorf("negotiated subprotocol = %q, want bearer", c.Subprotocol())
	}

	var hello struct {
		Type    string `json:"type"`
		Payload struct {
			Principal struct {
				TenantID string   `json:"tenant_id"`
				UserID   string   `json:"user_id"`
				Roles    []string `json:"roles"`
			} `json:"principal"`
			ServerTime string `json:"server_time"`
		} `json:"payload"`
	}
	if err := wsjson.Read(ctx, c, &hello); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if hello.Type != "hello" {
		t.Fatalf("first message type = %q, want hello", hello.Type)
	}
	if hello.Payload.Principal.UserID != "admin@demo" {
		t.Errorf("hello user = %q", hello.Payload.Principal.UserID)
	}
	if hello.Payload.ServerTime == "" {
		t.Error("hello missing server_time")
	}

	// Drive the client read loop so the incoming pong is processed, then a
	// client-initiated ping must be auto-ponged by the server read loop.
	readCtx := c.CloseRead(ctx)
	pingCtx, pcancel := context.WithTimeout(readCtx, 3*time.Second)
	defer pcancel()
	if err := c.Ping(pingCtx); err != nil {
		t.Fatalf("ping/pong failed: %v", err)
	}
}

func TestWSRejectsMissingToken(t *testing.T) {
	_, wsURL := wsTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected dial failure without token")
	}
	if resp == nil || resp.StatusCode != 401 {
		t.Fatalf("expected 401, got resp = %v", resp)
	}
}

func TestWSRejectsInvalidToken(t *testing.T) {
	_, wsURL := wsTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"bearer", "garbage-token"},
	})
	if err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("expected dial failure with invalid token")
	}
	if resp == nil || resp.StatusCode != 401 {
		t.Fatalf("expected 401, got resp = %v", resp)
	}
}

func TestWSTokenViaQueryParam(t *testing.T) {
	_, wsURL := wsTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL+"?access_token=mock-reader-token", nil)
	if err != nil {
		t.Fatalf("dial with query token: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	var hello struct {
		Type    string `json:"type"`
		Payload struct {
			Principal struct {
				UserID string `json:"user_id"`
			} `json:"principal"`
		} `json:"payload"`
	}
	if err := wsjson.Read(ctx, c, &hello); err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if hello.Type != "hello" || hello.Payload.Principal.UserID != "reader@demo" {
		t.Fatalf("unexpected hello: %+v", hello)
	}
}
