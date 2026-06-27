// Fixture CoreClient: a seeded, deterministic in-memory set of realistic
// bitemporal anchors. It is honestly tagged source:"fixture" and gives the UI
// real paginated, filterable data in PR1 without a live Core. Supports cursor
// pagination, `type` filter, `q` substring, and `as_of` valid-time projection.
package coreclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Fixture is the in-memory anchor, ledger, and cluster source.
type Fixture struct {
	anchors []AnchorDTO
	// ledgers is the catalog in stable name order; entries maps each ledger
	// name to its entries in ascending lsn (append) order.
	ledgers []LedgerSummary
	entries map[string][]LedgerEntry
	// nodes and shards are the live cluster topology in stable id order; summary
	// is the rollup derived from them so the three stay mutually consistent.
	nodes   []NodeDTO
	shards  []ShardDTO
	summary ClusterSummary
	// runtime is the cvm runtime-state snapshot (VM metrics + plan-cache rollup +
	// headline counters); opcodes and planCache are its paginated row sets in
	// stable order. The summary's plan-cache rollup is derived from planCache so
	// they stay mutually consistent.
	runtime   RuntimeSummary
	opcodes   []OpcodeDTO
	planCache []PlanCacheEntryDTO
	// channels and subscriptions are the live cvm-channels messaging set in stable
	// id order; messaging is the rollup derived from them so they stay consistent.
	channels      []ChannelDTO
	subscriptions []SubscriptionDTO
	messaging     MessagingSummary
}

// FixtureTenant is the tenant the seeded fixture data belongs to.
const FixtureTenant = "demo-tenant"

// NewFixture builds a Fixture with the seeded anchor, ledger, and cluster sets.
func NewFixture() *Fixture {
	ledgers, entries := seedLedgers()
	nodes, shards, summary := seedCluster()
	opcodes, planCache, runtime := seedRuntime()
	channels, subscriptions, messaging := seedMessaging()
	return &Fixture{
		anchors:       seedAnchors(),
		ledgers:       ledgers,
		entries:       entries,
		nodes:         nodes,
		shards:        shards,
		summary:       summary,
		opcodes:       opcodes,
		planCache:     planCache,
		runtime:       runtime,
		channels:      channels,
		subscriptions: subscriptions,
		messaging:     messaging,
	}
}

// Source implements CoreClient.
func (f *Fixture) Source() string { return SourceFixture }

// CheckCore reports "skip" - the fixture never depends on Core.
func (f *Fixture) CheckCore(_ context.Context) string { return CheckSkip }

// Close implements CoreClient; the fixture holds no resources.
func (f *Fixture) Close() error { return nil }

// Count returns the total number of seeded anchors (test/diagnostic helper).
func (f *Fixture) Count() int { return len(f.anchors) }

// ListAnchors applies tenant scoping, filters, valid-time projection, then
// cursor pagination over a stable id sort.
func (f *Fixture) ListAnchors(_ context.Context, p ListAnchorsParams) (*ListAnchorsResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]AnchorDTO, 0, len(f.anchors))
	for _, a := range f.anchors {
		if a.TenantID != p.TenantID {
			continue
		}
		if p.Type != "" && a.Type != p.Type {
			continue
		}
		if p.AsOf != nil && !validAt(a, *p.AsOf) {
			continue
		}
		// System-time (transaction-time) projection. With no system_as_of the
		// view is current-only: rows whose system interval is still open
		// (system_to == nil), hiding superseded versions. With a system_as_of the
		// view is projected to that decision instant via the same half-open rule.
		if p.SystemAsOf == nil {
			if a.SystemTo != nil {
				continue
			}
		} else if !systemAt(a, *p.SystemAsOf) {
			continue
		}
		if q != "" && !matchesQuery(a, q) {
			continue
		}
		filtered = append(filtered, a)
	}

	// Stable, sortable order by opaque id.
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })

	total := len(filtered)
	items := []AnchorDTO{}
	if offset < total {
		end := offset + p.PageSize
		if end > total {
			end = total
		}
		items = filtered[offset:end]
	}

	page := Page{PageSize: p.PageSize, TotalEstimate: total}
	if offset+len(items) < total {
		page.HasMore = true
		c := encodeCursor(offset + len(items))
		page.NextCursor = &c
	}

	return &ListAnchorsResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// inInterval reports whether t falls in the half-open interval [from, to): t is
// at or after from and strictly before to. A nil upper bound is open/unbounded.
// This is the shared bitemporal projection used by both axes: valid-time
// (valid_from/to) and system-time (system_from/to).
func inInterval(from time.Time, to *time.Time, t time.Time) bool {
	if t.Before(from) {
		return false
	}
	if to != nil && !t.Before(*to) {
		return false
	}
	return true
}

// validAt reports whether an anchor's valid-time interval contains t.
func validAt(a AnchorDTO, t time.Time) bool {
	return inInterval(a.ValidFrom, a.ValidTo, t)
}

// systemAt reports whether an anchor's system-time (transaction-time) interval
// contains t: the version the store knew at decision instant t.
func systemAt(a AnchorDTO, t time.Time) bool {
	return inInterval(a.SystemFrom, a.SystemTo, t)
}

// matchesQuery reports whether the lowercased q occurs in the label or any
// property value (case-insensitive substring over label+properties).
func matchesQuery(a AnchorDTO, q string) bool {
	if strings.Contains(strings.ToLower(a.Label), q) {
		return true
	}
	for _, v := range a.Properties {
		if strings.Contains(strings.ToLower(fmt.Sprint(v)), q) {
			return true
		}
	}
	return false
}

// --- Seed data -------------------------------------------------------------

func mustUTC(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("fixture: bad seed time " + s)
	}
	return t.UTC()
}

func tp(t time.Time) *time.Time { return &t }

// fixtureCorrectionAt is the shared system-time (transaction time) at which a
// deterministic subset of anchors were superseded by a corrected version. A
// system_as_of before this instant projects those anchors to their
// pre-correction value; the current (default) view shows the corrected one.
var fixtureCorrectionAt = mustUTC("2026-06-20T00:00:00Z")

// propsWith clones base and overrides one key, so a prior system-version can
// carry a different property value without aliasing the current row's map.
func propsWith(base map[string]any, key string, val any) map[string]any {
	m := make(map[string]any, len(base))
	for k, v := range base {
		m[k] = v
	}
	m[key] = val
	return m
}

// supersedeSystem records that anchor cur (carrying its corrected, current
// values) was a system-time correction of an earlier version. It returns the
// prior (superseded) row — same id, the pre-correction label and properties,
// system interval closed at fixtureCorrectionAt — and re-opens cur's system
// interval at the correction instant with a fresh decision LSN. The prior row
// keeps cur's original system_from and LSN (copied before cur is mutated). Label
// and properties are passed explicitly so a caller can correct either; today's
// seed corrects a single property and reuses the current label.
func supersedeSystem(cur *AnchorDTO, priorLabel string, priorProps map[string]any, newLSN int64) AnchorDTO {
	prior := *cur
	prior.Label = priorLabel
	prior.Properties = priorProps
	prior.SystemTo = tp(fixtureCorrectionAt)

	cur.SystemFrom = fixtureCorrectionAt
	cur.LSN = newLSN
	cur.TxnID = newLSN
	return prior
}

// seedAnchors builds ~142 deterministic bitemporal anchors across four node
// types. valid_from is spread across months so as_of projection is meaningful;
// a handful are closed (logically deleted) with a bounded valid window.
func seedAnchors() []AnchorDTO {
	const tenant = FixtureTenant
	var out []AnchorDTO
	lsn := int64(4000)

	nextLSN := func() int64 { lsn++; return lsn }

	// Spread valid_from across the first half of 2026 by index.
	validMonths := []string{
		"2026-01-04T00:00:00Z", "2026-01-19T00:00:00Z", "2026-02-02T00:00:00Z",
		"2026-02-17T00:00:00Z", "2026-03-03T00:00:00Z", "2026-03-20T00:00:00Z",
		"2026-04-06T00:00:00Z", "2026-04-21T00:00:00Z", "2026-05-05T00:00:00Z",
		"2026-05-19T00:00:00Z", "2026-06-01T00:00:00Z", "2026-06-15T00:00:00Z",
	}
	validFor := func(i int) time.Time { return mustUTC(validMonths[i%len(validMonths)]) }
	// system_from trails valid_from by a few hours (the decision time).
	systemFor := func(vf time.Time, i int) time.Time {
		return vf.Add(time.Duration(9)*time.Hour + time.Duration(i%50)*time.Minute)
	}

	firstNames := []string{"Ada", "Alan", "Grace", "Linus", "Margaret", "Edsger", "Donald", "Barbara", "Ken", "Dennis", "Katherine", "John", "Radia", "Leslie", "Tim"}
	lastNames := []string{"Lovelace", "Turing", "Hopper", "Torvalds", "Hamilton", "Dijkstra", "Knuth", "Liskov", "Thompson", "Ritchie", "Johnson", "McCarthy", "Perlman", "Lamport", "Berners-Lee"}
	titles := []string{"Principal Engineer", "Staff Engineer", "Senior Engineer", "Engineering Manager", "Architect", "Data Scientist"}
	depts := []string{"Platform", "Ledger", "Identity", "Analytics", "Frontend", "Reliability"}

	// Employees: 60.
	for i := 0; i < 60; i++ {
		vf := validFor(i)
		name := fmt.Sprintf("%s %s", firstNames[i%len(firstNames)], lastNames[(i/2)%len(lastNames)])
		a := AnchorDTO{
			ID:       fmt.Sprintf("anchor_employee_%04d", i),
			Type:     "Employee",
			Label:    name,
			TenantID: tenant,
			Properties: map[string]any{
				"email":      fmt.Sprintf("%s@demo", strings.ToLower(strings.ReplaceAll(name, " ", "."))),
				"title":      titles[i%len(titles)],
				"department": depts[i%len(depts)],
			},
			ValidFrom:  vf,
			SystemFrom: systemFor(vf, i),
			LSN:        nextLSN(),
		}
		a.TxnID = a.LSN
		// A few employees have left: closed with a bounded valid window.
		if i%17 == 0 && i > 0 {
			vt := vf.AddDate(0, 2, 0)
			a.ValidTo = tp(vt)
			a.Closed = true
		}
		// A few titles were later corrected (a system-time supersession): the
		// current row keeps the corrected title; a prior version carried the old.
		if i%9 == 0 && i > 0 {
			priorTitle := titles[(i+3)%len(titles)]
			out = append(out, supersedeSystem(&a, a.Label, propsWith(a.Properties, "title", priorTitle), nextLSN()))
		}
		out = append(out, a)
	}

	// Departments: 12.
	deptCenters := []string{"CC-1000", "CC-1010", "CC-1020", "CC-1030", "CC-1040", "CC-1050"}
	for i := 0; i < 12; i++ {
		vf := validFor(i)
		name := depts[i%len(depts)]
		if i >= len(depts) {
			name = name + " West"
		}
		a := AnchorDTO{
			ID:       fmt.Sprintf("anchor_department_%04d", i),
			Type:     "Department",
			Label:    name,
			TenantID: tenant,
			Properties: map[string]any{
				"name":        name,
				"head_count":  10 + i*3,
				"cost_center": deptCenters[i%len(deptCenters)],
			},
			ValidFrom:  vf,
			SystemFrom: systemFor(vf, i),
			LSN:        nextLSN(),
		}
		a.TxnID = a.LSN
		out = append(out, a)
	}

	// Projects: 40.
	projAdjs := []string{"Aurora", "Borealis", "Cascade", "Drift", "Ember", "Flux", "Glacier", "Horizon"}
	projStatus := []string{"active", "planning", "on_hold", "completed"}
	for i := 0; i < 40; i++ {
		vf := validFor(i)
		name := fmt.Sprintf("Project %s %d", projAdjs[i%len(projAdjs)], i)
		a := AnchorDTO{
			ID:       fmt.Sprintf("anchor_project_%04d", i),
			Type:     "Project",
			Label:    name,
			TenantID: tenant,
			Properties: map[string]any{
				"name":   name,
				"status": projStatus[i%len(projStatus)],
				"budget": 50000 + i*7500,
			},
			ValidFrom:  vf,
			SystemFrom: systemFor(vf, i),
			LSN:        nextLSN(),
		}
		a.TxnID = a.LSN
		if i%13 == 0 && i > 0 {
			vt := vf.AddDate(0, 3, 0)
			a.ValidTo = tp(vt)
			a.Closed = true
		}
		// A few project statuses were later corrected (system-time supersession).
		if i%11 == 0 && i > 0 {
			priorStatus := projStatus[(i+2)%len(projStatus)]
			out = append(out, supersedeSystem(&a, a.Label, propsWith(a.Properties, "status", priorStatus), nextLSN()))
		}
		out = append(out, a)
	}

	// Customers: 30.
	companies := []string{"Acme", "Globex", "Initech", "Umbrella", "Hooli", "Stark", "Wayne", "Wonka", "Cyberdyne", "Soylent"}
	tiers := []string{"enterprise", "business", "starter"}
	regions := []string{"EU", "NA", "APAC", "LATAM"}
	for i := 0; i < 30; i++ {
		vf := validFor(i)
		name := fmt.Sprintf("%s %s", companies[i%len(companies)], []string{"Corp", "Inc", "LLC"}[i%3])
		a := AnchorDTO{
			ID:       fmt.Sprintf("anchor_customer_%04d", i),
			Type:     "Customer",
			Label:    name,
			TenantID: tenant,
			Properties: map[string]any{
				"name":   name,
				"tier":   tiers[i%len(tiers)],
				"region": regions[i%len(regions)],
			},
			ValidFrom:  vf,
			SystemFrom: systemFor(vf, i),
			LSN:        nextLSN(),
		}
		a.TxnID = a.LSN
		// A few customer tiers were later corrected (system-time supersession).
		if i%9 == 0 && i > 0 {
			priorTier := tiers[(i+1)%len(tiers)]
			out = append(out, supersedeSystem(&a, a.Label, propsWith(a.Properties, "tier", priorTier), nextLSN()))
		}
		out = append(out, a)
	}

	return out
}
