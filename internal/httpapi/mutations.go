package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
)

// maxMutationBody bounds a mutation request body (defensive; bodies are tiny).
const maxMutationBody = 1 << 20 // 1 MiB

// anchorMutationResponse is the create/correct/close envelope (contract 4.3):
// the resulting current version, plus the superseded prior version for
// correct/close.
type anchorMutationResponse struct {
	Anchor     coreclient.AnchorDTO  `json:"node"`
	Superseded *coreclient.AnchorDTO `json:"superseded,omitempty"`
	Source     string                `json:"source"`
}

type createAnchorRequest struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Label      string         `json:"label"`
	Properties map[string]any `json:"properties"`
	ValidFrom  string         `json:"valid_from"`
}

type correctAnchorRequest struct {
	Label       *string        `json:"label"`
	Properties  map[string]any `json:"properties"`
	ExpectedLSN *int64         `json:"expected_lsn"`
}

type closeAnchorRequest struct {
	ValidTo     string `json:"valid_to"`
	ExpectedLSN *int64 `json:"expected_lsn"`
}

// decodeMutationBody decodes a JSON request body into dst, rejecting malformed
// or oversized input as a 400 invalid_request. An empty body decodes to the zero
// value (so all-optional bodies like close are allowed with no body).
func decodeMutationBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxMutationBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return apierr.InvalidRequest("body", "malformed JSON: "+err.Error())
	}
	if dec.More() {
		return apierr.InvalidRequest("body", "unexpected trailing data after JSON")
	}
	return nil
}

// parseMutationTime parses an optional RFC3339 or bare-date field; empty -> nil.
// A malformed value is a 400 invalid_request naming the field.
func parseMutationTime(field, raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		t = t.UTC()
		return &t, nil
	}
	if t, err := time.Parse(time.DateOnly, raw); err == nil {
		t = t.UTC()
		return &t, nil
	}
	return nil, apierr.InvalidRequest(field, "want RFC3339 or YYYY-MM-DD")
}

// mutationPrincipal resolves the caller and enforces the writer role, writing
// the typed error and returning nil when the request must not proceed.
func (s *Server) mutationPrincipal(w http.ResponseWriter, r *http.Request, reqID string) *auth.Principal {
	p := principalFrom(r.Context())
	if p == nil {
		apierr.Write(w, reqID, apierr.Internal("missing principal on authenticated route"))
		return nil
	}
	if !p.HasRole("writer") {
		apierr.Write(w, reqID, apierr.Forbidden())
		return nil
	}
	return p
}

// handleCreateAnchor creates a brand-new anchor. Requires the writer role.
func (s *Server) handleCreateAnchor(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDOf(r)
	p := s.mutationPrincipal(w, r, reqID)
	if p == nil {
		return
	}

	var body createAnchorRequest
	if err := decodeMutationBody(r, &body); err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	for _, f := range []struct{ name, value string }{
		{"id", body.ID}, {"type", body.Type}, {"label", body.Label},
	} {
		if f.value == "" {
			apierr.Write(w, reqID, apierr.InvalidRequest(f.name, "required"))
			return
		}
	}
	validFrom, err := parseMutationTime("valid_from", body.ValidFrom)
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	res, err := s.core.CreateAnchor(r.Context(), coreclient.CreateAnchorParams{
		TenantID:   p.TenantID,
		ID:         body.ID,
		Type:       body.Type,
		Label:      body.Label,
		Properties: body.Properties,
		ValidFrom:  validFrom,
		Principal:  p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	writeJSON(w, http.StatusCreated, anchorMutationResponse{Anchor: res.Anchor, Superseded: res.Superseded, Source: res.Source})
}

// handleCorrectAnchor records a system-time correction. Requires writer.
func (s *Server) handleCorrectAnchor(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDOf(r)
	p := s.mutationPrincipal(w, r, reqID)
	if p == nil {
		return
	}

	var body correctAnchorRequest
	if err := decodeMutationBody(r, &body); err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	if body.Label == nil && body.Properties == nil {
		apierr.Write(w, reqID, apierr.InvalidRequest("body", "nothing to correct: provide label and/or properties"))
		return
	}

	res, err := s.core.CorrectAnchor(r.Context(), coreclient.CorrectAnchorParams{
		TenantID:    p.TenantID,
		ID:          chi.URLParam(r, "id"),
		Label:       body.Label,
		Properties:  body.Properties,
		ExpectedLSN: body.ExpectedLSN,
		Principal:   p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	writeJSON(w, http.StatusOK, anchorMutationResponse{Anchor: res.Anchor, Superseded: res.Superseded, Source: res.Source})
}

// handleCloseAnchor logically closes an anchor in valid-time. Requires writer.
func (s *Server) handleCloseAnchor(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDOf(r)
	p := s.mutationPrincipal(w, r, reqID)
	if p == nil {
		return
	}

	var body closeAnchorRequest
	if err := decodeMutationBody(r, &body); err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	validTo, err := parseMutationTime("valid_to", body.ValidTo)
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	res, err := s.core.CloseAnchor(r.Context(), coreclient.CloseAnchorParams{
		TenantID:    p.TenantID,
		ID:          chi.URLParam(r, "id"),
		ValidTo:     validTo,
		ExpectedLSN: body.ExpectedLSN,
		Principal:   p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	writeJSON(w, http.StatusOK, anchorMutationResponse{Anchor: res.Anchor, Superseded: res.Superseded, Source: res.Source})
}
