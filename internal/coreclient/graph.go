package coreclient

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
)

// edgeLSNBase seeds edge LSNs in a dedicated high range so they never collide
// with or perturb node LSNs (which the 142/154/12 node invariants depend on).
const edgeLSNBase int64 = 900_000

// edgeID mints a stable, sortable edge id from its seed LSN.
func edgeID(lsn int64) string { return fmt.Sprintf("edge_%06d", lsn) }

// EdgeDTO is a typed, directed, bitemporal relationship between two nodes as the
// graph view renders it. It carries the SAME bitemporal shape as a node version:
// valid_* is business time (valid_to == nil => open), system_* is decision time
// (system_to == nil => current). Edges let the graph rematerialize at any
// bitemporal coordinate: a relationship appears, vanishes, or is superseded.
type EdgeDTO struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"` // relationship type, e.g. MEMBER_OF
	SourceID   string         `json:"source_id"`
	TargetID   string         `json:"target_id"`
	Label      string         `json:"label"`
	Properties map[string]any `json:"properties"`
	ValidFrom  time.Time      `json:"valid_from"`
	ValidTo    *time.Time     `json:"valid_to"`
	SystemFrom time.Time      `json:"system_from"`
	SystemTo   *time.Time     `json:"system_to"`
	LSN        int64          `json:"lsn"`
	TxnID      int64          `json:"txn_id"`
}

// NeighborhoodParams seeds a one-hop neighborhood expansion from a node id at a
// bitemporal coordinate. AsOf nil applies no valid-time filter; SystemAsOf nil
// selects the current view (system-open rows only). Limit caps the neighbor
// count (server-side cap + sample — the whole graph is never returned).
type NeighborhoodParams struct {
	TenantID   string
	ID         string
	AsOf       *time.Time
	SystemAsOf *time.Time
	Limit      int
	Principal  *auth.Principal
}

// NeighborhoodResult is the seed node, its (capped + sampled) neighbors, and the
// edges among that node set, all projected to the requested coordinate. Root is
// nil when the id exists but is not present at the coordinate (e.g. before it was
// created or after it was closed) — an empty graph, not an error.
type NeighborhoodResult struct {
	Root          *AnchorDTO
	Neighbors     []AnchorDTO
	Edges         []EdgeDTO
	NeighborTotal int  // distinct neighbors before the cap/sample
	Sampled       bool // true when NeighborTotal > len(Neighbors)
	// ValidFrom / ValidTo are the valid-time BOUNDS of the seed's whole
	// neighborhood across all time (unfiltered by as_of), so the UI timeline knows
	// its scrub range. ValidTo is nil when anything is still open (=> "up to now").
	ValidFrom time.Time
	ValidTo   *time.Time
	Source    string
}

// neighborhoodBounds returns the valid-time span over which the seed's
// neighborhood evolves: the earliest valid_from and the latest valid_to among the
// seed, every node ever connected to it by an edge, and those edges. ValidTo is
// nil (open) when any of them is still open. Caller holds f.mu.RLock.
func (f *Fixture) neighborhoodBounds(tenant, seedID string) (time.Time, *time.Time) {
	ids := map[string]struct{}{seedID: {}}
	var from time.Time
	var to *time.Time
	open := false
	first := true
	consider := func(vf time.Time, vt *time.Time) {
		if first || vf.Before(from) {
			from = vf
			first = false
		}
		if vt == nil {
			open = true
		} else if to == nil || vt.After(*to) {
			to = vt
		}
	}
	for _, e := range f.edges {
		if e.SourceID != seedID && e.TargetID != seedID {
			continue
		}
		ids[e.SourceID] = struct{}{}
		ids[e.TargetID] = struct{}{}
		consider(e.ValidFrom, e.ValidTo)
	}
	for _, a := range f.anchors {
		if a.TenantID != tenant {
			continue
		}
		if _, ok := ids[a.ID]; ok {
			consider(a.ValidFrom, a.ValidTo)
		}
	}
	if open {
		return from, nil
	}
	return from, to
}

// NeighborhoodLimitDefault / Max bound the server-side neighbor cap. The whole
// graph is never materialized; a seed expands at most Max neighbors per hop.
const (
	NeighborhoodLimitDefault = 50
	NeighborhoodLimitMax     = 200
)

// edgeValidAt / edgeSystemAt project an edge by the shared half-open interval
// rule, mirroring validAt / systemAt for nodes.
func edgeValidAt(e EdgeDTO, t time.Time) bool  { return inInterval(e.ValidFrom, e.ValidTo, t) }
func edgeSystemAt(e EdgeDTO, t time.Time) bool { return inInterval(e.SystemFrom, e.SystemTo, t) }

// edgePresent reports whether an edge is visible at (asOf, sysAsOf): asOf nil =>
// no valid filter; sysAsOf nil => current-only (system_to == nil).
func edgePresent(e EdgeDTO, asOf, sysAsOf *time.Time) bool {
	if asOf != nil && !edgeValidAt(e, *asOf) {
		return false
	}
	if sysAsOf == nil {
		return e.SystemTo == nil
	}
	return edgeSystemAt(e, *sysAsOf)
}

// projectNode resolves a single visible version of id at the coordinate, applying
// the same projection as ListAnchors (asOf nil => no valid filter; sysAsOf nil =>
// current/open). When several versions qualify (e.g. asOf nil spanning multiple
// valid segments) the latest-starting one wins, so a node renders once. nil when
// the id has no version present at the coordinate. Caller holds f.mu.RLock.
func (f *Fixture) projectNode(tenant, id string, asOf, sysAsOf *time.Time) *AnchorDTO {
	var best *AnchorDTO
	for i := range f.anchors {
		a := f.anchors[i]
		if a.TenantID != tenant || a.ID != id {
			continue
		}
		if asOf != nil && !validAt(a, *asOf) {
			continue
		}
		if sysAsOf == nil {
			if a.SystemTo != nil {
				continue
			}
		} else if !systemAt(a, *sysAsOf) {
			continue
		}
		if best == nil || a.ValidFrom.After(best.ValidFrom) ||
			(a.ValidFrom.Equal(best.ValidFrom) && a.LSN > best.LSN) {
			v := a
			best = &v
		}
	}
	return best
}

// GetNeighborhood materializes a one-hop neighborhood of the seed node at the
// requested bitemporal coordinate. An edge is included only when it is present
// AND both endpoints are present at the coordinate; the returned edge set is
// every present edge with both endpoints inside the (root + kept neighbors) set,
// so neighbor-to-neighbor relationships render too. Neighbors beyond Limit are
// evenly sampled (deterministic) and Sampled is set. 404 only when the id has no
// versions at all in the tenant.
func (f *Fixture) GetNeighborhood(_ context.Context, p NeighborhoodParams) (*NeighborhoodResult, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = NeighborhoodLimitDefault
	}
	if limit > NeighborhoodLimitMax {
		limit = NeighborhoodLimitMax
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	if len(f.versionsOf(p.TenantID, p.ID)) == 0 {
		return nil, apierr.NotFound("node:" + p.ID)
	}

	boundsFrom, boundsTo := f.neighborhoodBounds(p.TenantID, p.ID)
	root := f.projectNode(p.TenantID, p.ID, p.AsOf, p.SystemAsOf)
	res := &NeighborhoodResult{
		Root:      root,
		Neighbors: []AnchorDTO{},
		Edges:     []EdgeDTO{},
		ValidFrom: boundsFrom,
		ValidTo:   boundsTo,
		Source:    SourceFixture,
	}
	if root == nil {
		return res, nil // exists, but absent at this coordinate -> empty graph
	}

	// Collect present neighbors reachable by a present edge with both endpoints
	// present at the coordinate.
	neighborByID := map[string]AnchorDTO{}
	for _, e := range f.edges {
		if e.SourceID != root.ID && e.TargetID != root.ID {
			continue
		}
		if !edgePresent(e, p.AsOf, p.SystemAsOf) {
			continue
		}
		otherID := e.TargetID
		if otherID == root.ID {
			otherID = e.SourceID
		}
		if otherID == root.ID {
			continue // defensive: no self-loops in the rendered set
		}
		if _, seen := neighborByID[otherID]; seen {
			continue
		}
		nv := f.projectNode(p.TenantID, otherID, p.AsOf, p.SystemAsOf)
		if nv == nil {
			continue // dangling at this coordinate -> drop the edge
		}
		neighborByID[otherID] = *nv
	}

	ids := make([]string, 0, len(neighborByID))
	for id := range neighborByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	total := len(ids)

	kept := sampleIDs(ids, limit)
	keptSet := map[string]struct{}{root.ID: {}}
	neighbors := make([]AnchorDTO, 0, len(kept))
	for _, id := range kept {
		keptSet[id] = struct{}{}
		neighbors = append(neighbors, neighborByID[id])
	}

	// Every present edge whose BOTH endpoints are in the kept node set, in stable
	// id order (covers root-neighbor and neighbor-neighbor edges).
	edges := []EdgeDTO{}
	for _, e := range f.edges {
		if !edgePresent(e, p.AsOf, p.SystemAsOf) {
			continue
		}
		if _, ok := keptSet[e.SourceID]; !ok {
			continue
		}
		if _, ok := keptSet[e.TargetID]; !ok {
			continue
		}
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })

	res.Neighbors = neighbors
	res.Edges = edges
	res.NeighborTotal = total
	res.Sampled = total > len(neighbors)
	return res, nil
}

// sampleIDs returns at most limit ids from the sorted input, evenly spaced so the
// sample spans the whole range (not just the first window). Deterministic.
func sampleIDs(sorted []string, limit int) []string {
	if len(sorted) <= limit {
		return sorted
	}
	out := make([]string, 0, limit)
	// Even stride across [0, len): index = round(k * (len-1) / (limit-1)).
	for k := 0; k < limit; k++ {
		idx := k * (len(sorted) - 1) / (limit - 1)
		out = append(out, sorted[idx])
	}
	return out
}

// seedEdges derives the typed relationship set from the seeded node ids. Edges
// use a dedicated LSN range (edgeLSNBase+) so they never perturb node LSNs or the
// 142/154/12 node-count invariants. A handful carry bounded valid windows or a
// system-time supersession so the bitemporal timeline has real evolution to
// animate.
func seedEdges(anchors []AnchorDTO) []EdgeDTO {
	// Earliest valid/system instant per current node id (for edge interval starts).
	type stamp struct{ vf, sf time.Time }
	byID := map[string]stamp{}
	idsByType := map[string][]string{}
	seenType := map[string]map[string]struct{}{}
	for _, a := range anchors {
		s, ok := byID[a.ID]
		if !ok || a.ValidFrom.Before(s.vf) {
			byID[a.ID] = stamp{vf: a.ValidFrom, sf: a.SystemFrom}
		}
		if seenType[a.Type] == nil {
			seenType[a.Type] = map[string]struct{}{}
		}
		if _, dup := seenType[a.Type][a.ID]; !dup {
			seenType[a.Type][a.ID] = struct{}{}
			idsByType[a.Type] = append(idsByType[a.Type], a.ID)
		}
	}
	for _, ids := range idsByType {
		sort.Strings(ids)
	}
	employees, departments := idsByType["Employee"], idsByType["Department"]
	projects, customers := idsByType["Project"], idsByType["Customer"]

	var lsn int64 = edgeLSNBase
	var edges []EdgeDTO
	add := func(typ, label, src, tgt string, validTo *time.Time, systemTo *time.Time, props map[string]any) {
		if src == "" || tgt == "" || src == tgt {
			return
		}
		lsn++
		ss, ts := byID[src], byID[tgt]
		vf := ss.vf
		if ts.vf.After(vf) {
			vf = ts.vf
		}
		sf := ss.sf
		if ts.sf.After(sf) {
			sf = ts.sf
		}
		// Stagger when each relationship FORMS across the first ~6 months (always
		// >= when both endpoints exist), and end a fraction of them, so a hub's
		// neighborhood visibly evolves as the valid-time timeline is scrubbed
		// (edges + the leaves they reach appear and vanish over time). The default
		// view (no as_of) is unaffected — it applies no valid-time filter.
		seq := lsn - edgeLSNBase
		vf = vf.AddDate(0, 0, int(seq%13)*14)
		if validTo == nil && seq%6 == 0 {
			ended := vf.AddDate(0, 3, 0)
			validTo = &ended
		}
		edges = append(edges, EdgeDTO{
			ID:         edgeID(lsn),
			Type:       typ,
			SourceID:   src,
			TargetID:   tgt,
			Label:      label,
			Properties: props,
			ValidFrom:  vf,
			ValidTo:    validTo,
			SystemFrom: sf,
			SystemTo:   systemTo,
			LSN:        lsn,
			TxnID:      lsn,
		})
	}
	at := func(src string, months int) *time.Time {
		s := byID[src]
		t := s.vf.AddDate(0, months, 0)
		return &t
	}

	nDept, nProj, nCust := len(departments), len(projects), len(customers)
	for i, emp := range employees {
		if nDept > 0 {
			add("MEMBER_OF", "member of", emp, departments[i%nDept], nil, nil, nil)
		}
		if nProj > 0 {
			// Every 13th assignment ended (bounded valid window) so the timeline
			// shows an edge that exists then vanishes.
			var vt *time.Time
			if i%13 == 0 && i > 0 {
				vt = at(emp, 4)
			}
			add("WORKS_ON", "works on", emp, projects[i%nProj], vt, nil, nil)
		}
		// Org tree: the first min(nDept,len) employees are leads; the rest report to
		// the lead of their department bucket.
		if i >= nDept && nDept > 0 {
			add("REPORTS_TO", "reports to", emp, employees[i%nDept], nil, nil, nil)
		}
	}
	// One system-time supersession: employee[18]'s WORKS_ON was reassigned. The
	// prior edge is superseded (system_to set at the shared correction instant) and
	// a current edge points at a different project from that instant on. Exercises
	// the system-time axis on edges (chunk 3) without changing node counts.
	if len(employees) > 18 && nProj > 1 {
		emp := employees[18]
		s := byID[emp]
		lsn++
		edges = append(edges, EdgeDTO{
			ID: edgeID(lsn), Type: "WORKS_ON", Label: "works on",
			SourceID: emp, TargetID: projects[(18+1)%nProj],
			ValidFrom: s.vf, SystemFrom: s.sf, SystemTo: tp(fixtureCorrectionAt),
			LSN: lsn, TxnID: lsn,
		})
		lsn++
		edges = append(edges, EdgeDTO{
			ID: edgeID(lsn), Type: "WORKS_ON", Label: "works on",
			SourceID: emp, TargetID: projects[(18+7)%nProj],
			ValidFrom: s.vf, SystemFrom: fixtureCorrectionAt,
			LSN: lsn, TxnID: lsn,
		})
	}
	for i, proj := range projects {
		if nDept > 0 {
			add("OWNED_BY", "owned by", proj, departments[i%nDept], nil, nil, nil)
		}
		if nCust > 0 {
			add("FOR_CUSTOMER", "for customer", proj, customers[i%nCust], nil, nil, nil)
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	return edges
}
