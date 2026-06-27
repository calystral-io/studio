package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/coreclient"
	"github.com/calystral-io/studio/internal/version"
)

const (
	defaultPageSize = 25
	minPageSize     = 1
	maxPageSize     = 200
)

// writeJSON renders v as a JSON 200 (or the given status) response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleHealthz is the unauthenticated liveness probe. Never depends on Core.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is the unauthenticated readiness probe. For source=fixture the
// core check is "skip"; for source=grpc it is "ok"|"unavailable" from a ping.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	check := s.core.CheckCore(r.Context())
	body := map[string]any{
		"status": "ready",
		"checks": map[string]string{"core": check},
	}
	if check == coreclient.CheckUnavailable {
		body["status"] = "not_ready"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// handleVersion returns the build identity (unauthenticated).
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, version.Current())
}

// handleMe returns the resolved principal (authenticated).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		apierr.Write(w, requestIDOf(r), apierr.Internal("missing principal on authenticated route"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id": p.TenantID,
		"user_id":   p.UserID,
		"roles":     p.Roles,
	})
}

// anchorsResponse is the paginated anchors envelope (contract section 4).
type anchorsResponse struct {
	Items  []coreclient.AnchorDTO `json:"items"`
	Page   coreclient.Page        `json:"page"`
	Source string                 `json:"source"`
}

// handleAnchors validates query params and serves a page of anchors scoped to
// the principal's tenant. Requires the reader role.
func (s *Server) handleAnchors(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDOf(r)
	p := principalFrom(r.Context())
	if p == nil {
		apierr.Write(w, reqID, apierr.Internal("missing principal on authenticated route"))
		return
	}
	if !p.HasRole("reader") {
		apierr.Write(w, reqID, apierr.Forbidden())
		return
	}

	q := r.URL.Query()

	pageSize, err := parsePageSize(q.Get("page_size"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	var asOf *time.Time
	if raw := q.Get("as_of"); raw != "" {
		t, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			apierr.Write(w, reqID, apierr.InvalidAsOf(raw))
			return
		}
		t = t.UTC()
		asOf = &t
	}

	params := coreclient.ListAnchorsParams{
		TenantID:  p.TenantID,
		PageSize:  pageSize,
		Cursor:    q.Get("cursor"),
		Type:      q.Get("type"),
		Q:         q.Get("q"),
		AsOf:      asOf,
		Principal: p,
	}

	res, err := s.core.ListAnchors(r.Context(), params)
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, anchorsResponse{
		Items:  res.Items,
		Page:   res.Page,
		Source: res.Source,
	})
}

// parsePageSize resolves the page_size param: empty -> default; otherwise an
// integer in [1,200] or a 400 page_size_out_of_range error.
func parsePageSize(raw string) (int, error) {
	if raw == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		// A non-integer value cannot satisfy the [1,200] range constraint.
		ae := apierr.PageSizeOutOfRange(minPageSize, maxPageSize, 0)
		ae.Message = "page_size " + strconv.Quote(raw) + " is not an integer in range [1,200]"
		return 0, ae
	}
	if n < minPageSize || n > maxPageSize {
		return 0, apierr.PageSizeOutOfRange(minPageSize, maxPageSize, n)
	}
	return n, nil
}
