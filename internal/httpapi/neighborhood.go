package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/coreclient"
)

// neighborhoodResponse is the GET /nodes/{id}/neighborhood envelope: the seed
// node (null when absent at the coordinate), its capped + sampled neighbors, and
// the edges among that node set, all projected to (as_of, system_as_of).
type neighborhoodResponse struct {
	Root          *coreclient.AnchorDTO  `json:"root"`
	Neighbors     []coreclient.AnchorDTO `json:"neighbors"`
	Edges         []coreclient.EdgeDTO   `json:"edges"`
	NeighborTotal int                    `json:"neighbor_total"`
	Sampled       bool                   `json:"sampled"`
	Source        string                 `json:"source"`
}

// handleNeighborhood serves a one-hop graph neighborhood for the seed node id,
// projected to the optional bitemporal coordinate. The neighbor count is capped
// server-side (limit, default/max bounded) and evenly sampled when it overflows —
// the whole graph is never returned. Requires reader; 404 when the id has no
// versions in the tenant.
func (s *Server) handleNeighborhood(w http.ResponseWriter, r *http.Request) {
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
	asOf, err := parseAsOf(q.Get("as_of"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	systemAsOf, err := parseSystemAsOf(q.Get("system_as_of"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 0 {
			apierr.Write(w, reqID, apierr.InvalidRequest("limit", "limit must be a non-negative integer"))
			return
		}
		limit = n
	}

	res, err := s.core.GetNeighborhood(r.Context(), coreclient.NeighborhoodParams{
		TenantID:   p.TenantID,
		ID:         chi.URLParam(r, "id"),
		AsOf:       asOf,
		SystemAsOf: systemAsOf,
		Limit:      limit,
		Principal:  p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, neighborhoodResponse{
		Root:          res.Root,
		Neighbors:     res.Neighbors,
		Edges:         res.Edges,
		NeighborTotal: res.NeighborTotal,
		Sampled:       res.Sampled,
		Source:        res.Source,
	})
}
