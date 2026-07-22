package coreclient

import (
	"context"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

func neighborIDs(res *NeighborhoodResult) map[string]bool {
	m := map[string]bool{}
	for _, n := range res.Neighbors {
		m[n.ID] = true
	}
	return m
}

func hasEdgeBetween(res *NeighborhoodResult, a, b string) bool {
	for _, e := range res.Edges {
		if (e.SourceID == a && e.TargetID == b) || (e.SourceID == b && e.TargetID == a) {
			return true
		}
	}
	return false
}

// Every seeded edge must reference existing node ids, never self-loop, keep a
// non-inverted valid interval, carry a dedicated-range unique LSN, and stay in
// stable id order.
func TestSeedEdgesInvariants(t *testing.T) {
	f := NewFixture()
	ids := map[string]bool{}
	for _, a := range f.anchors {
		ids[a.ID] = true
	}
	if len(f.edges) == 0 {
		t.Fatal("no edges seeded")
	}
	seen := map[string]bool{}
	var prev string
	for i, e := range f.edges {
		if !ids[e.SourceID] || !ids[e.TargetID] {
			t.Fatalf("edge %s references unknown node(s) %s->%s", e.ID, e.SourceID, e.TargetID)
		}
		if e.SourceID == e.TargetID {
			t.Fatalf("edge %s is a self-loop", e.ID)
		}
		if e.ValidTo != nil && e.ValidTo.Before(e.ValidFrom) {
			t.Fatalf("edge %s has inverted valid interval", e.ID)
		}
		if e.LSN < edgeLSNBase {
			t.Fatalf("edge %s LSN %d below dedicated base %d", e.ID, e.LSN, edgeLSNBase)
		}
		if seen[e.ID] {
			t.Fatalf("duplicate edge id %s", e.ID)
		}
		seen[e.ID] = true
		if i > 0 && e.ID < prev {
			t.Fatalf("edges not in stable id order at %d", i)
		}
		prev = e.ID
	}
}

// Seeding edges must not disturb the node-count invariants (142 current / 154
// stored / 12 superseded) the rest of the suite relies on.
func TestSeedEdgesDoesNotDisturbNodeCounts(t *testing.T) {
	f := NewFixture()
	current, superseded := 0, 0
	for _, a := range f.anchors {
		if a.SystemTo == nil {
			current++
		} else {
			superseded++
		}
	}
	if current != 142 || superseded != 12 || len(f.anchors) != 154 {
		t.Fatalf("node counts changed: current=%d superseded=%d total=%d (want 142/12/154)", current, superseded, len(f.anchors))
	}
}

func TestNeighborhoodBasic(t *testing.T) {
	f := NewFixture()
	res, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_employee_0001"})
	if err != nil {
		t.Fatalf("neighborhood: %v", err)
	}
	if res.Root == nil || res.Root.ID != "node_employee_0001" {
		t.Fatalf("root = %+v", res.Root)
	}
	if len(res.Neighbors) == 0 {
		t.Fatal("expected neighbors")
	}
	// Every returned edge has BOTH endpoints inside the union of {root} and neighbors.
	set := neighborIDs(res)
	set[res.Root.ID] = true
	for _, e := range res.Edges {
		if !set[e.SourceID] || !set[e.TargetID] {
			t.Fatalf("edge %s endpoint outside node set", e.ID)
		}
	}
	// An employee is MEMBER_OF its department; that edge must be present.
	dept := "node_department_0001" // employee i=1 -> dept i%nDept
	if !set[dept] || !hasEdgeBetween(res, "node_employee_0001", dept) {
		t.Fatalf("expected MEMBER_OF edge to %s; neighbors=%v", dept, set)
	}
}

func TestNeighborhoodCapAndSample(t *testing.T) {
	f := NewFixture()
	// A department is the hub of many MEMBER_OF / OWNED_BY edges.
	full, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_department_0000"})
	if err != nil {
		t.Fatal(err)
	}
	if full.Sampled {
		t.Fatalf("unbounded call should not be sampled (total=%d)", full.NeighborTotal)
	}
	if full.NeighborTotal < 5 {
		t.Fatalf("expected a busy hub, got %d neighbors", full.NeighborTotal)
	}
	capped, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_department_0000", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Neighbors) != 3 || !capped.Sampled {
		t.Fatalf("cap: got %d neighbors sampled=%v", len(capped.Neighbors), capped.Sampled)
	}
	if capped.NeighborTotal != full.NeighborTotal {
		t.Fatalf("cap changed total: %d vs %d", capped.NeighborTotal, full.NeighborTotal)
	}
	// Every edge in the capped result stays inside the kept node set.
	set := neighborIDs(capped)
	set[capped.Root.ID] = true
	for _, e := range capped.Edges {
		if !set[e.SourceID] || !set[e.TargetID] {
			t.Fatalf("capped edge %s endpoint outside kept set", e.ID)
		}
	}
}

func TestNeighborhoodLimitClampedToMax(t *testing.T) {
	f := NewFixture()
	res, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_department_0000", Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Neighbors) > NeighborhoodLimitMax {
		t.Fatalf("limit not clamped: %d > %d", len(res.Neighbors), NeighborhoodLimitMax)
	}
}

func TestNeighborhoodNotFound(t *testing.T) {
	f := NewFixture()
	_, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_nope_9999"})
	assertCode(t, err, apierr.CodeNotFound)
}

func TestNeighborhoodForeignTenant(t *testing.T) {
	f := NewFixture()
	_, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: "other-tenant", ID: "node_employee_0001"})
	assertCode(t, err, apierr.CodeNotFound)
}

// A node closed in valid-time is absent at an as_of after its valid_to: the
// neighborhood is empty (root nil) but it is NOT an error.
func TestNeighborhoodAbsentRootAfterClose(t *testing.T) {
	f := NewFixture()
	// node_employee_0017 is closed (i%17==0) with a bounded valid window.
	base, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_employee_0017"})
	if err != nil {
		t.Fatal(err)
	}
	if base.Root == nil || base.Root.ValidTo == nil {
		t.Fatalf("expected a closed root with valid_to, got %+v", base.Root)
	}
	after := base.Root.ValidTo.Add(48 * time.Hour)
	res, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_employee_0017", AsOf: &after})
	if err != nil {
		t.Fatalf("absent root should not error: %v", err)
	}
	if res.Root != nil || len(res.Neighbors) != 0 || len(res.Edges) != 0 {
		t.Fatalf("expected empty graph for absent root, got root=%v neighbors=%d edges=%d", res.Root, len(res.Neighbors), len(res.Edges))
	}
}

// The system-time axis rematerializes edges: employee_0018's WORKS_ON was
// reassigned at the shared correction instant. The current view shows the new
// target; a system_as_of before the correction shows the superseded target.
func TestNeighborhoodSystemSupersession(t *testing.T) {
	f := NewFixture()
	const newTarget = "node_project_0025" // (18+7)%40
	const oldTarget = "node_project_0019" // (18+1)%40

	cur, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_employee_0018"})
	if err != nil {
		t.Fatal(err)
	}
	curSet := neighborIDs(cur)
	if !curSet[newTarget] || curSet[oldTarget] {
		t.Fatalf("current view: want %s present and %s absent; neighbors=%v", newTarget, oldTarget, curSet)
	}

	before := fixtureCorrectionAt.Add(-24 * time.Hour)
	past, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_employee_0018", SystemAsOf: &before})
	if err != nil {
		t.Fatal(err)
	}
	pastSet := neighborIDs(past)
	if !pastSet[oldTarget] {
		t.Fatalf("system_as_of before correction: want %s present; neighbors=%v", oldTarget, pastSet)
	}
}

// The valid-time bounds span the whole neighborhood (every ever-connected node)
// and are unfiltered by as_of, so the timeline slider has a stable range.
func TestNeighborhoodBounds(t *testing.T) {
	f := NewFixture()
	res, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_department_0000"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ValidFrom.IsZero() {
		t.Fatal("bounds valid_from is zero")
	}
	// A department + its open employees/projects => the upper bound is open.
	if res.ValidTo != nil {
		t.Fatalf("expected open valid_to, got %v", res.ValidTo)
	}
	// The lower bound is the earliest of the whole neighborhood, so it is at or
	// before the seed's own valid_from and before every present neighbor's.
	if res.Root != nil && res.ValidFrom.After(res.Root.ValidFrom) {
		t.Fatalf("bounds.from %v is after root.from %v", res.ValidFrom, res.Root.ValidFrom)
	}
	for _, n := range res.Neighbors {
		if res.ValidFrom.After(n.ValidFrom) {
			t.Fatalf("bounds.from %v after neighbor %s from %v", res.ValidFrom, n.ID, n.ValidFrom)
		}
	}
	// Unfiltered: scrubbing as_of must not move the bounds.
	future := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	res2, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_department_0000", AsOf: &future})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.ValidFrom.Equal(res.ValidFrom) {
		t.Fatalf("bounds.from shifted with as_of: %v vs %v", res2.ValidFrom, res.ValidFrom)
	}
}

// The system-time bounds span the whole neighborhood's recording history and are
// unfiltered by system_as_of, so the "as recorded at" axis has a stable scrub
// range. The lower bound (earliest system_from) sits at or before the recorded
// correction instant, so the slider can reach a coordinate before the correction;
// the span stays open (system_to nil => "as recorded today") because current
// versions coexist with the one supersession.
func TestNeighborhoodSystemBounds(t *testing.T) {
	f := NewFixture()
	res, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_employee_0018"})
	if err != nil {
		t.Fatal(err)
	}
	if res.SystemFrom.IsZero() {
		t.Fatal("bounds system_from is zero")
	}
	// The lower bound is at or before the seed's own system_from.
	if res.Root != nil && res.SystemFrom.After(res.Root.SystemFrom) {
		t.Fatalf("bounds.system_from %v is after root.system_from %v", res.SystemFrom, res.Root.SystemFrom)
	}
	// The range must reach back before the correction so a user can roll system-time
	// to a coordinate where the superseded fact is still the recorded one.
	if !res.SystemFrom.Before(fixtureCorrectionAt) {
		t.Fatalf("bounds.system_from %v is not before the correction instant %v", res.SystemFrom, fixtureCorrectionAt)
	}
	// Current versions coexist, so the span is open (=> scrub up to now).
	if res.SystemTo != nil {
		t.Fatalf("expected an open system span (current data coexists), got %v", res.SystemTo)
	}

	// Unfiltered: scrubbing system_as_of must not move the bounds.
	before := fixtureCorrectionAt.Add(-24 * time.Hour)
	res2, err := f.GetNeighborhood(context.Background(), NeighborhoodParams{TenantID: FixtureTenant, ID: "node_employee_0018", SystemAsOf: &before})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.SystemFrom.Equal(res.SystemFrom) {
		t.Fatalf("bounds.system_from shifted with system_as_of: %v vs %v", res2.SystemFrom, res.SystemFrom)
	}
	if res2.SystemTo != nil {
		t.Fatalf("bounds.system_to shifted to closed with system_as_of: %v", res2.SystemTo)
	}
}

// Unit: edgePresent honors the bounded valid window and the current-only system
// default.
func TestEdgePresentBounded(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 4, 0)
	e := EdgeDTO{ValidFrom: from, ValidTo: &to, SystemFrom: from}
	within := from.AddDate(0, 1, 0)
	after := to.AddDate(0, 1, 0)
	if !edgePresent(e, &within, nil) {
		t.Fatal("edge should be present within its valid window")
	}
	if edgePresent(e, &after, nil) {
		t.Fatal("edge should be absent after valid_to")
	}
	// Superseded (system_to set) edge is hidden in the current (system nil) view.
	st := from.AddDate(0, 2, 0)
	sup := EdgeDTO{ValidFrom: from, SystemFrom: from, SystemTo: &st}
	if edgePresent(sup, nil, nil) {
		t.Fatal("superseded edge should be hidden in current view")
	}
}
