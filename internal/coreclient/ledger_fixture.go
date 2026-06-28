// Fixture ledger source: a seeded, deterministic in-memory set of realistic
// bitemporal, LSN-ordered, append-chained ledger entries across three ledgers
// (GeneralLedger, AuditLog, DomainEvents). Honestly tagged source:"fixture", it
// gives the UI real paginated, filterable data in PR2 without a live Core.
// Supports the ledger catalog list (q over name+description) and per-ledger
// entry listing (descending lsn) with kind/q/as_of/lsn-range filters and stable
// cursor pagination.
package coreclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

// LedgerCount returns the number of seeded ledgers (test/diagnostic helper).
func (f *Fixture) LedgerCount() int { return len(f.ledgers) }

// EntryCount returns the number of seeded entries in a ledger (test helper).
func (f *Fixture) EntryCount(name string) int { return len(f.entries[name]) }

// ListLedgers applies tenant scoping and a `q` substring filter over
// name+description, then cursor-paginates a stable name sort.
func (f *Fixture) ListLedgers(_ context.Context, p ListLedgersParams) (*ListLedgersResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]LedgerSummary, 0, len(f.ledgers))
	if p.TenantID == FixtureTenant {
		for _, l := range f.ledgers {
			if q != "" && !ledgerMatchesQuery(l, q) {
				continue
			}
			filtered = append(filtered, l)
		}
	}

	// Stable, sortable order by opaque name.
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })

	total := len(filtered)
	items := []LedgerSummary{}
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

	return &ListLedgersResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// ListLedgerEntries resolves the named ledger, applies kind/q/as_of/lsn-range
// filters, then cursor-paginates a descending-lsn (newest first) order. An
// unknown ledger (or one in a foreign tenant) is a 404 not_found whose resource
// is "ledger:<name>".
func (f *Fixture) ListLedgerEntries(_ context.Context, p ListLedgerEntriesParams) (*ListLedgerEntriesResult, error) {
	if p.FromLSN != nil && p.ToLSN != nil && *p.FromLSN > *p.ToLSN {
		return nil, apierr.InvalidLSNRange(*p.FromLSN, *p.ToLSN)
	}
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	all, ok := f.entries[p.Name]
	if !ok || p.TenantID != FixtureTenant {
		return nil, apierr.NotFound("ledger:" + p.Name)
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]LedgerEntry, 0, len(all))
	for _, e := range all {
		if p.Kind != "" && e.Kind != p.Kind {
			continue
		}
		if p.FromLSN != nil && e.LSN < *p.FromLSN {
			continue
		}
		if p.ToLSN != nil && e.LSN > *p.ToLSN {
			continue
		}
		if p.AsOf != nil && !entryValidAt(e, *p.AsOf) {
			continue
		}
		if q != "" && !entryMatchesQuery(e, q) {
			continue
		}
		filtered = append(filtered, e)
	}

	// Newest first: strictly descending lsn (lsn is globally unique, so this is
	// a total order - stable, no dup/skip across pages).
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].LSN > filtered[j].LSN })

	total := len(filtered)
	items := []LedgerEntry{}
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

	return &ListLedgerEntriesResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// entryValidAt reports whether entry e is valid (business time) at instant t,
// via the shared half-open interval check over effective_from/effective_to.
func entryValidAt(e LedgerEntry, t time.Time) bool {
	return inInterval(e.EffectiveFrom, e.EffectiveTo, t)
}

// ledgerMatchesQuery reports whether q occurs in the ledger name or description
// (case-insensitive substring).
func ledgerMatchesQuery(l LedgerSummary, q string) bool {
	return strings.Contains(strings.ToLower(l.Name), q) ||
		strings.Contains(strings.ToLower(l.Description), q)
}

// entryMatchesQuery reports whether q occurs in the entry summary or any payload
// value (case-insensitive substring over summary+payload).
func entryMatchesQuery(e LedgerEntry, q string) bool {
	if strings.Contains(strings.ToLower(e.Summary), q) {
		return true
	}
	for _, v := range e.Payload {
		if strings.Contains(strings.ToLower(fmt.Sprint(v)), q) {
			return true
		}
	}
	return false
}

// --- Seed data -------------------------------------------------------------

// ledgerDef describes a seed ledger and the per-kind shape of its entries.
type ledgerDef struct {
	name        string
	kind        string // ledger kind (accounting/audit/event)
	description string
	entryKinds  []string
}

// seedLedgers builds three ledgers with a few hundred deterministic bitemporal,
// LSN-ordered, append-chained entries. The global lsn is strictly increasing in
// creation (recorded_at) order across all ledgers; seq is per-ledger monotonic;
// prev_lsn links each entry to the previous one in its own ledger (null first).
// effective_from is spread across the first half of 2026 so as_of projection is
// meaningful; a fraction of entries are superseded with a bounded effective_to.
func seedLedgers() ([]LedgerSummary, map[string][]LedgerEntry) {
	defs := []ledgerDef{
		{
			name:        "GeneralLedger",
			kind:        "accounting",
			description: "Double-entry accounting postings for the demo tenant",
			entryKinds:  []string{"posting", "adjustment", "reversal", "accrual"},
		},
		{
			name:        "AuditLog",
			kind:        "audit",
			description: "Security and administrative audit trail",
			entryKinds:  []string{"login", "permission_change", "config_change", "access_denied"},
		},
		{
			name:        "DomainEvents",
			kind:        "event",
			description: "Append-only domain event stream",
			entryKinds:  []string{"created", "updated", "deleted", "state_changed"},
		},
	}

	actors := []string{"admin@demo", "reader@demo", "system@demo", "ops@demo"}
	accounts := []string{"4000-Revenue", "5000-Expenses", "1000-Cash", "2000-Payable", "3000-Equity"}
	currencies := []string{"EUR", "USD", "GBP"}
	resources := []string{"tenant-settings", "billing-config", "rbac-policy", "feature-flags"}
	subjects := []string{"Invoice", "Order", "Account", "Subscription", "Shipment"}

	// effective_from spread across the first half of 2026 by index.
	validMonths := []string{
		"2026-01-04T00:00:00Z", "2026-01-19T00:00:00Z", "2026-02-02T00:00:00Z",
		"2026-02-17T00:00:00Z", "2026-03-03T00:00:00Z", "2026-03-20T00:00:00Z",
		"2026-04-06T00:00:00Z", "2026-04-21T00:00:00Z", "2026-05-05T00:00:00Z",
		"2026-05-19T00:00:00Z", "2026-06-01T00:00:00Z", "2026-06-15T00:00:00Z",
	}

	const total = 360
	start := mustUTC("2026-01-02T08:00:00Z")
	lsn := int64(7000)

	// Per-ledger running state for seq and the append-chain prev_lsn.
	seqByLedger := map[string]int64{}
	lastLSNByLedger := map[string]*int64{}

	entries := map[string][]LedgerEntry{}
	for _, d := range defs {
		entries[d.name] = []LedgerEntry{}
	}

	for i := 0; i < total; i++ {
		d := defs[i%len(defs)] // round-robin => balanced ledgers
		lsn++                  // global, strictly increasing in creation order
		seqByLedger[d.name]++
		seq := seqByLedger[d.name]

		recordedAt := start.Add(time.Duration(i) * 37 * time.Minute)
		effFrom := mustUTC(validMonths[i%len(validMonths)])
		entryKind := d.entryKinds[i%len(d.entryKinds)]

		e := LedgerEntry{
			ID:            fmt.Sprintf("entry_%07d", lsn),
			Ledger:        d.name,
			Seq:           seq,
			LSN:           lsn,
			TxnID:         lsn,
			Kind:          entryKind,
			Actor:         actors[i%len(actors)],
			RecordedAt:    recordedAt,
			EffectiveFrom: effFrom,
			PrevLSN:       lastLSNByLedger[d.name], // nil for the first entry
		}

		// Ledger-specific summary + payload (gives `q` real substrings to match).
		switch d.name {
		case "GeneralLedger":
			acct := accounts[i%len(accounts)]
			cur := currencies[i%len(currencies)]
			amount := 100 + (i%50)*25
			e.Summary = fmt.Sprintf("%s #%d to account %s", entryKind, seq, acct)
			e.Payload = map[string]any{
				"account":  acct,
				"amount":   amount,
				"currency": cur,
				"posted":   entryKind != "accrual",
			}
		case "AuditLog":
			res := resources[i%len(resources)]
			e.Summary = fmt.Sprintf("%s by %s on %s", entryKind, e.Actor, res)
			e.Payload = map[string]any{
				"resource":   res,
				"ip":         fmt.Sprintf("10.0.%d.%d", i%256, (i*7)%256),
				"successful": entryKind != "access_denied",
			}
		default: // DomainEvents
			subj := subjects[i%len(subjects)]
			e.Summary = fmt.Sprintf("%s #%d %s", subj, seq, entryKind)
			e.Payload = map[string]any{
				"subject":    subj,
				"version":    int(seq),
				"aggregate":  fmt.Sprintf("%s-%04d", strings.ToLower(subj), seq),
				"replayable": entryKind != "deleted",
			}
		}

		// A reference to a related node anchor on roughly every third entry.
		if i%3 == 0 {
			ref := fmt.Sprintf("node_employee_%04d", i%60)
			e.AnchorID = &ref
		}

		// Supersede roughly every seventh entry with a bounded effective window so
		// as_of valid-time projection excludes it outside that window.
		if i%7 == 3 {
			et := effFrom.AddDate(0, 1, 0)
			e.EffectiveTo = tp(et)
		}

		entries[d.name] = append(entries[d.name], e)
		cur := lsn
		lastLSNByLedger[d.name] = &cur
	}

	// Build summaries from the seeded entries (entries are in ascending lsn order,
	// so the last one is the most recent).
	ledgers := make([]LedgerSummary, 0, len(defs))
	for _, d := range defs {
		es := entries[d.name]
		s := LedgerSummary{
			Name:               d.name,
			Kind:               d.kind,
			Description:        d.description,
			EntryCountEstimate: len(es),
		}
		if n := len(es); n > 0 {
			s.LastLSN = es[n-1].LSN
			s.LastRecordedAt = es[n-1].RecordedAt
		}
		ledgers = append(ledgers, s)
	}

	return ledgers, entries
}
