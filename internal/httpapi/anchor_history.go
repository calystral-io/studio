package httpapi

import (
	"net/http"
	"reflect"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/coreclient"
)

// --- history ---------------------------------------------------------------

// anchorHistorySummary is a quick rollup of an anchor's bitemporal versions.
type anchorHistorySummary struct {
	VersionCount      int `json:"version_count"`       // total stored versions
	CurrentCount      int `json:"current_count"`       // system_to == null (live)
	SupersededCount   int `json:"superseded_count"`    // system_to != null (corrected)
	ValidSegmentCount int `json:"valid_segment_count"` // distinct valid-time windows among current versions
}

// anchorHistoryResponse is the GET /anchors/{id}/history envelope (contract 4.1).
type anchorHistoryResponse struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenant_id"`
	Versions []coreclient.AnchorDTO `json:"versions"`
	Summary  anchorHistorySummary   `json:"summary"`
	Source   string                 `json:"source"`
}

// handleAnchorHistory serves one anchor's full bitemporal version timeline.
// Requires the reader role; 404 when the id has no versions in the tenant.
func (s *Server) handleAnchorHistory(w http.ResponseWriter, r *http.Request) {
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

	res, err := s.core.GetAnchorHistory(r.Context(), coreclient.GetAnchorHistoryParams{
		TenantID:  p.TenantID,
		ID:        chi.URLParam(r, "id"),
		Principal: p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, anchorHistoryResponse{
		ID:       res.Versions[0].ID,
		Type:     res.Versions[0].Type,
		TenantID: res.Versions[0].TenantID,
		Versions: res.Versions,
		Summary:  summarizeHistory(res.Versions),
		Source:   res.Source,
	})
}

// summarizeHistory rolls up the version set: current vs superseded counts and
// the number of distinct valid-time windows among the current versions.
func summarizeHistory(vs []coreclient.AnchorDTO) anchorHistorySummary {
	sum := anchorHistorySummary{VersionCount: len(vs)}
	segments := map[string]struct{}{}
	for _, v := range vs {
		if v.SystemTo != nil {
			sum.SupersededCount++
			continue
		}
		sum.CurrentCount++
		key := v.ValidFrom.Format(time.RFC3339) + "/"
		if v.ValidTo != nil {
			key += v.ValidTo.Format(time.RFC3339)
		} else {
			key += "open"
		}
		segments[key] = struct{}{}
	}
	sum.ValidSegmentCount = len(segments)
	return sum
}

// --- diff ------------------------------------------------------------------

// diffCoordinate is a (valid-time, system-time) point. A nil SystemAsOf selects
// the current/open version.
type diffCoordinate struct {
	AsOf       time.Time  `json:"as_of"`
	SystemAsOf *time.Time `json:"system_as_of"`
}

// diffSide is the version resolved at one coordinate (null when none exists).
type diffSide struct {
	Coordinate diffCoordinate        `json:"coordinate"`
	Version    *coreclient.AnchorDTO `json:"version"`
}

// fieldDelta is one field-level change between two anchor versions. Op is one of
// added | removed | changed. Field is "label" | "closed" | "valid_from" |
// "valid_to" | "properties.<key>".
type fieldDelta struct {
	Field  string `json:"field"`
	Op     string `json:"op"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// anchorDiffResponse is the GET /anchors/{id}/diff envelope (contract 4.2).
type anchorDiffResponse struct {
	ID     string       `json:"id"`
	From   diffSide     `json:"from"`
	To     diffSide     `json:"to"`
	Deltas []fieldDelta `json:"deltas"`
	Source string       `json:"source"`
}

// handleAnchorDiff resolves one anchor at two bitemporal coordinates and returns
// the field-level delta between them. Omitted valid axes default to now;
// omitted system axes select the current/open version. Requires reader; 404
// when the id has no versions at all in the tenant.
func (s *Server) handleAnchorDiff(w http.ResponseWriter, r *http.Request) {
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
	fromValid, err := parseAsOf(q.Get("as_of"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	fromSystem, err := parseSystemAsOf(q.Get("system_as_of"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	toValid, err := parseAsOf(q.Get("to_as_of"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}
	toSystem, err := parseSystemAsOf(q.Get("to_system_as_of"))
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	now := time.Now().UTC()
	fromValidAt := now
	if fromValid != nil {
		fromValidAt = *fromValid
	}
	toValidAt := now
	if toValid != nil {
		toValidAt = *toValid
	}

	id := chi.URLParam(r, "id")
	res, err := s.core.GetAnchorDiff(r.Context(), coreclient.GetAnchorDiffParams{
		TenantID:     p.TenantID,
		ID:           id,
		FromValidAt:  fromValidAt,
		FromSystemAt: fromSystem,
		ToValidAt:    toValidAt,
		ToSystemAt:   toSystem,
		Principal:    p,
	})
	if err != nil {
		apierr.Write(w, reqID, err)
		return
	}

	writeJSON(w, http.StatusOK, anchorDiffResponse{
		ID: id,
		From: diffSide{
			Coordinate: diffCoordinate{AsOf: fromValidAt, SystemAsOf: fromSystem},
			Version:    res.FromVersion,
		},
		To: diffSide{
			Coordinate: diffCoordinate{AsOf: toValidAt, SystemAsOf: toSystem},
			Version:    res.ToVersion,
		},
		Deltas: diffAnchors(res.FromVersion, res.ToVersion),
		Source: res.Source,
	})
}

// diffAnchors computes the ordered field-level delta from one anchor version to
// another. A nil side means "no version at that coordinate": every field of the
// present side is reported as added (from nil) or removed (to nil). Order is
// stable: label, closed, valid_from, valid_to, then properties by key ascending.
// Recording metadata (system_from/to, lsn, txn_id) is excluded by design — only
// business content is diffed.
func diffAnchors(from, to *coreclient.AnchorDTO) []fieldDelta {
	deltas := []fieldDelta{}
	if from == nil && to == nil {
		return deltas
	}

	scalar := func(field string, before, after any, equalWhenBoth bool) {
		switch {
		case from == nil:
			// Only report a field the present (to) side actually has — an optional
			// field that is itself nil (e.g. valid_to) is absent, not "added".
			if after != nil {
				deltas = append(deltas, fieldDelta{Field: field, Op: "added", Before: nil, After: after})
			}
		case to == nil:
			if before != nil {
				deltas = append(deltas, fieldDelta{Field: field, Op: "removed", Before: before, After: nil})
			}
		case !equalWhenBoth:
			deltas = append(deltas, fieldDelta{Field: field, Op: "changed", Before: before, After: after})
		}
	}

	var fLabel, tLabel, fClosed, tClosed, fVF, tVF, fVT, tVT any
	labelEq, closedEq, vfEq, vtEq := true, true, true, true
	if from != nil {
		fLabel, fClosed, fVF, fVT = from.Label, from.Closed, from.ValidFrom, timeOrNil(from.ValidTo)
	}
	if to != nil {
		tLabel, tClosed, tVF, tVT = to.Label, to.Closed, to.ValidFrom, timeOrNil(to.ValidTo)
	}
	if from != nil && to != nil {
		labelEq = from.Label == to.Label
		closedEq = from.Closed == to.Closed
		vfEq = from.ValidFrom.Equal(to.ValidFrom)
		vtEq = equalTimePtr(from.ValidTo, to.ValidTo)
	}
	scalar("label", fLabel, tLabel, labelEq)
	scalar("closed", fClosed, tClosed, closedEq)
	scalar("valid_from", fVF, tVF, vfEq)
	scalar("valid_to", fVT, tVT, vtEq)

	// properties: union of keys across both sides (covers whole-side-nil too).
	keys := map[string]struct{}{}
	if from != nil {
		for k := range from.Properties {
			keys[k] = struct{}{}
		}
	}
	if to != nil {
		for k := range to.Properties {
			keys[k] = struct{}{}
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		var bv, av any
		var bok, aok bool
		if from != nil {
			bv, bok = from.Properties[k]
		}
		if to != nil {
			av, aok = to.Properties[k]
		}
		switch {
		case !bok && aok:
			deltas = append(deltas, fieldDelta{Field: "properties." + k, Op: "added", Before: nil, After: av})
		case bok && !aok:
			deltas = append(deltas, fieldDelta{Field: "properties." + k, Op: "removed", Before: bv, After: nil})
		case bok && aok && !reflect.DeepEqual(bv, av):
			deltas = append(deltas, fieldDelta{Field: "properties." + k, Op: "changed", Before: bv, After: av})
		}
	}

	return deltas
}

// timeOrNil renders an optional instant as a JSON value: the time or null.
func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// equalTimePtr reports whether two optional instants are equal (both nil, or
// both set to the same instant).
func equalTimePtr(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
