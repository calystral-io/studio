// Package coreclient is the BFF's port to Core's read path. It exposes a
// CoreClient interface for listing node anchors with cursor pagination and
// filters, plus two implementations selected by STUDIO_CORE_SOURCE: a seeded
// in-memory fixture (PR1 default) and a gRPC adapter against Core's
// QueryService (which returns UNIMPLEMENTED today). The AnchorDTO is identical
// regardless of source so the UI renders both the same.
package coreclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

// Source tags identify which backend served a response.
const (
	SourceFixture = "fixture"
	SourceCore    = "core"
)

// Core readiness check states surfaced on /readyz.
const (
	CheckSkip        = "skip"
	CheckOK          = "ok"
	CheckUnavailable = "unavailable"
)

// AnchorDTO is a node anchor as the UI renders it (contract section 3). Times
// are UTC; nil *time.Time marshals to JSON null per the bitemporal "open"/
// "current" conventions.
type AnchorDTO struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Label      string         `json:"label"`
	TenantID   string         `json:"tenant_id"`
	Properties map[string]any `json:"properties"`
	ValidFrom  time.Time      `json:"valid_from"`
	ValidTo    *time.Time     `json:"valid_to"`
	SystemFrom time.Time      `json:"system_from"`
	SystemTo   *time.Time     `json:"system_to"`
	LSN        int64          `json:"lsn"`
	TxnID      int64          `json:"txn_id"`
	Closed     bool           `json:"closed"`
}

// Page is the cursor-pagination envelope (contract section 4).
type Page struct {
	PageSize      int     `json:"page_size"`
	NextCursor    *string `json:"next_cursor"`
	HasMore       bool    `json:"has_more"`
	TotalEstimate int     `json:"total_estimate"`
}

// ListAnchorsParams carries the validated request inputs. Cursor is the opaque
// token from a prior next_cursor; decoding/validation is the client's concern.
type ListAnchorsParams struct {
	TenantID string
	PageSize int
	Cursor   string
	Type     string
	Q        string
	AsOf     *time.Time
	// Principal is the resolved caller. The gRPC adapter mints the
	// x-calystral-principal JWT from it; the fixture only needs TenantID.
	Principal *auth.Principal
}

// ListAnchorsResult is one page of anchors plus the source tag.
type ListAnchorsResult struct {
	Items  []AnchorDTO
	Page   Page
	Source string
}

// CoreClient is the read-path port. CheckCore reports the readiness status the
// /readyz endpoint surfaces.
type CoreClient interface {
	ListAnchors(ctx context.Context, p ListAnchorsParams) (*ListAnchorsResult, error)
	CheckCore(ctx context.Context) string
	Source() string
	Close() error
}

// cursorPayload is the BFF-minted opaque cursor (offset-based internally; the
// UI treats the encoded token as an opaque blob).
type cursorPayload struct {
	Offset int `json:"o"`
}

// encodeCursor mints a base64url cursor for the given offset.
func encodeCursor(offset int) string {
	b, _ := json.Marshal(cursorPayload{Offset: offset})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses a cursor token into its offset. An empty token is offset
// 0 (first page). A malformed or negative token is an invalid_cursor error.
func decodeCursor(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, apierr.InvalidCursor(token)
	}
	var c cursorPayload
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, apierr.InvalidCursor(token)
	}
	if c.Offset < 0 {
		return 0, apierr.InvalidCursor(token)
	}
	return c.Offset, nil
}
