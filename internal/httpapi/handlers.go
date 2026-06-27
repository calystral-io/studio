package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

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

// ledgersResponse is the paginated ledger-catalog envelope (contract 10.1).
type ledgersResponse struct {
	Items  []coreclient.LedgerSummary `json:"items"`
	Page   coreclient.Page            `json:"page"`
	Source string                     `json:"source"`
}

// handleLedgers validates query params and serves a page of ledgers scoped to
// the principal's tenant. Requires the reader role.
func (s *Server) handleLedgers(w http.ResponseWriter, r *http.Request) {
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

	res, err := s.core.ListLedgers(r.Context(), coreclient.ListLedgersParams{
		TenantID:  p.TenantID,
		PageSize:  pageSize,
		Cursor:    q.Get("cursor"),
		Q:         q.Get("q"),
		Principal: p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, ledgersResponse{Items: res.Items, Page: res.Page, Source: res.Source})
}

// ledgerEntriesResponse is the paginated ledger-entries envelope (contract 10.2).
type ledgerEntriesResponse struct {
	Items  []coreclient.LedgerEntry `json:"items"`
	Page   coreclient.Page          `json:"page"`
	Source string                   `json:"source"`
}

// handleLedgerEntries validates query params and serves a page of one ledger's
// entries (newest first) scoped to the principal's tenant. Requires the reader
// role. An unknown ledger name yields 404 not_found ("ledger:<name>").
func (s *Server) handleLedgerEntries(w http.ResponseWriter, r *http.Request) {
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

	name := chi.URLParam(r, "name")
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

	fromLSN, toLSN, err := parseLSNBounds(q.Get("from_lsn"), q.Get("to_lsn"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	res, err := s.core.ListLedgerEntries(r.Context(), coreclient.ListLedgerEntriesParams{
		TenantID:  p.TenantID,
		Name:      name,
		PageSize:  pageSize,
		Cursor:    q.Get("cursor"),
		Kind:      q.Get("kind"),
		Q:         q.Get("q"),
		AsOf:      asOf,
		FromLSN:   fromLSN,
		ToLSN:     toLSN,
		Principal: p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, ledgerEntriesResponse{Items: res.Items, Page: res.Page, Source: res.Source})
}

// clusterSummaryResponse is the cluster rollup rendered as a single object with
// a top-level `source` tag (contract section 11). The embedded ClusterSummary
// fields are promoted to the top level.
type clusterSummaryResponse struct {
	coreclient.ClusterSummary
	Source string `json:"source"`
}

// handleClusterSummary serves the cluster-wide observability rollup. Requires
// the reader role. The cluster is shared operator infrastructure (not
// tenant-scoped data).
func (s *Server) handleClusterSummary(w http.ResponseWriter, r *http.Request) {
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

	res, err := s.core.ClusterSummary(r.Context(), coreclient.ClusterSummaryParams{
		TenantID:  p.TenantID,
		Principal: p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, clusterSummaryResponse{ClusterSummary: res.Summary, Source: res.Source})
}

// nodesResponse is the paginated cluster-nodes envelope (contract section 11).
type nodesResponse struct {
	Items  []coreclient.NodeDTO `json:"items"`
	Page   coreclient.Page      `json:"page"`
	Source string               `json:"source"`
}

// handleClusterNodes validates query params and serves a page of cluster nodes
// (id asc). Requires the reader role. Unknown region/status filter values simply
// match nothing.
func (s *Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
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

	res, err := s.core.ListNodes(r.Context(), coreclient.ListNodesParams{
		TenantID:  p.TenantID,
		PageSize:  pageSize,
		Cursor:    q.Get("cursor"),
		Region:    q.Get("region"),
		Status:    q.Get("status"),
		Q:         q.Get("q"),
		Principal: p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, nodesResponse{Items: res.Items, Page: res.Page, Source: res.Source})
}

// shardsResponse is the paginated cluster-shards envelope (contract section 12).
type shardsResponse struct {
	Items  []coreclient.ShardDTO `json:"items"`
	Page   coreclient.Page       `json:"page"`
	Source string                `json:"source"`
}

// handleClusterShards validates query params and serves a page of cluster shards
// (id asc). Requires the reader role. The `node` filter matches shards where the
// node is the leader or a replica; unknown region/status/node values match
// nothing.
func (s *Server) handleClusterShards(w http.ResponseWriter, r *http.Request) {
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

	res, err := s.core.ListShards(r.Context(), coreclient.ListShardsParams{
		TenantID:  p.TenantID,
		PageSize:  pageSize,
		Cursor:    q.Get("cursor"),
		Region:    q.Get("region"),
		Status:    q.Get("status"),
		Node:      q.Get("node"),
		Q:         q.Get("q"),
		Principal: p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, shardsResponse{Items: res.Items, Page: res.Page, Source: res.Source})
}

// parseLSNBounds resolves the optional from_lsn/to_lsn params. An empty value is
// an unbounded (nil) side; a non-integer value or an inverted window
// (from_lsn > to_lsn) is a 400 invalid_lsn_range error.
func parseLSNBounds(fromRaw, toRaw string) (*int64, *int64, error) {
	from, err := parseOptionalLSN("from_lsn", fromRaw)
	if err != nil {
		return nil, nil, err
	}
	to, err := parseOptionalLSN("to_lsn", toRaw)
	if err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && *from > *to {
		return nil, nil, apierr.InvalidLSNRange(*from, *to)
	}
	return from, to, nil
}

// parseOptionalLSN parses one optional lsn bound: empty -> nil; otherwise a
// signed integer or a 400 invalid_lsn_range error (a malformed bound cannot
// define a valid window).
func parseOptionalLSN(name, raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		ae := apierr.InvalidLSNRange(0, 0)
		ae.Message = name + " " + strconv.Quote(raw) + " is not an integer"
		return nil, ae
	}
	return &n, nil
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
