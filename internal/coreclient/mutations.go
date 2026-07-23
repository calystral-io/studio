package coreclient

import (
	"context"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

// nextLSN returns the next monotonic decision LSN. Caller must hold f.mu.Lock.
func (f *Fixture) nextLSN() int64 {
	f.nextLSNVal++
	return f.nextLSNVal
}

// mutationInstant returns a system-time instant strictly after the previous
// mutation's, so successive versions never share a system boundary (which would
// create a zero-width interval and break the non-overlap invariant). Caller must
// hold f.mu.Lock.
func (f *Fixture) mutationInstant() time.Time {
	now := time.Now().UTC()
	if !now.After(f.lastMutationAt) {
		now = f.lastMutationAt.Add(time.Nanosecond)
	}
	f.lastMutationAt = now
	return now
}

// currentIndex returns the index of the current (open, system_to == nil) version
// of id within tenant, or -1 when the id has no version at all. Caller must hold
// at least f.mu.RLock.
func (f *Fixture) currentIndex(tenant, id string) int {
	for i := range f.anchors {
		a := &f.anchors[i]
		if a.TenantID == tenant && a.ID == id && a.SystemTo == nil {
			return i
		}
	}
	return -1
}

// cloneProps returns a shallow copy so a new version never aliases the
// superseded version's property map.
func cloneProps(p map[string]any) map[string]any {
	m := make(map[string]any, len(p))
	for k, v := range p {
		m[k] = v
	}
	return m
}

// cloneAnchor returns a deep-enough copy (fresh Properties map and time
// pointers) so a mutation result handed back to the caller never aliases the
// stored version across the lock boundary.
func cloneAnchor(a AnchorDTO) AnchorDTO {
	a.Properties = cloneProps(a.Properties)
	if a.ValidTo != nil {
		vt := *a.ValidTo
		a.ValidTo = &vt
	}
	if a.SystemTo != nil {
		st := *a.SystemTo
		a.SystemTo = &st
	}
	return a
}

// CreateAnchor appends a brand-new open anchor version. The id must not already
// exist in the tenant (else 409 already_exists).
func (f *Fixture) CreateAnchor(_ context.Context, p CreateAnchorParams) (*AnchorMutationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// currentIndex finds an OPEN version; create rejects any id that still has
	// one. The invariant that every existing id has exactly one open version
	// (every supersede pairs with a new open row) makes this a full existence
	// check.
	if f.currentIndex(p.TenantID, p.ID) != -1 {
		return nil, apierr.AlreadyExists("node:" + p.ID)
	}

	at := f.mutationInstant()
	lsn := f.nextLSN()
	validFrom := at
	if p.ValidFrom != nil {
		validFrom = p.ValidFrom.UTC()
	}
	a := AnchorDTO{
		ID:         p.ID,
		Type:       p.Type,
		Label:      p.Label,
		TenantID:   p.TenantID,
		Properties: cloneProps(p.Properties),
		ValidFrom:  validFrom,
		SystemFrom: at,
		Revision:   lsn,
		TxnID:      lsn,
	}
	f.anchors = append(f.anchors, a)
	return &AnchorMutationResult{Anchor: cloneAnchor(a), Source: SourceFixture}, nil
}

// CorrectAnchor records a system-time correction: it supersedes the current
// version (closing its system interval at the mutation instant) and appends a
// new current version carrying the corrected label/properties, with the valid
// window unchanged. 404 when the id has no version; 409 on a stale expected_revision.
func (f *Fixture) CorrectAnchor(_ context.Context, p CorrectAnchorParams) (*AnchorMutationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := f.currentIndex(p.TenantID, p.ID)
	if idx < 0 {
		return nil, apierr.NotFound("node:" + p.ID)
	}
	cur := f.anchors[idx]
	if p.ExpectedRevision != nil && *p.ExpectedRevision != cur.Revision {
		return nil, apierr.PreconditionFailed(*p.ExpectedRevision, cur.Revision)
	}

	at := f.mutationInstant()
	lsn := f.nextLSN()

	f.anchors[idx].SystemTo = &at

	next := cur
	next.Properties = cloneProps(cur.Properties)
	if p.Label != nil {
		next.Label = *p.Label
	}
	if p.Properties != nil {
		next.Properties = cloneProps(p.Properties)
	}
	next.SystemFrom = at
	next.SystemTo = nil
	next.Revision = lsn
	next.TxnID = lsn

	f.anchors = append(f.anchors, next)
	superseded := cloneAnchor(f.anchors[idx])
	return &AnchorMutationResult{Anchor: cloneAnchor(next), Superseded: &superseded, Source: SourceFixture}, nil
}

// CloseAnchor logically closes an anchor in valid-time: it supersedes the
// current version and appends a new current version with valid_to set and
// closed = true. 404 when the id has no version; 400 invalid_request when it is
// already closed; 409 on a stale expected_revision.
func (f *Fixture) CloseAnchor(_ context.Context, p CloseAnchorParams) (*AnchorMutationResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	idx := f.currentIndex(p.TenantID, p.ID)
	if idx < 0 {
		return nil, apierr.NotFound("node:" + p.ID)
	}
	cur := f.anchors[idx]
	if cur.Closed {
		return nil, apierr.InvalidRequest("closed", "node is already closed; use a correction to change it")
	}
	if p.ExpectedRevision != nil && *p.ExpectedRevision != cur.Revision {
		return nil, apierr.PreconditionFailed(*p.ExpectedRevision, cur.Revision)
	}

	at := f.mutationInstant()
	validTo := at
	if p.ValidTo != nil {
		validTo = p.ValidTo.UTC()
	}
	// A valid_to before the anchor's valid_from would make a negative-width valid
	// interval that no projection could ever resolve - reject it.
	if validTo.Before(cur.ValidFrom) {
		return nil, apierr.InvalidRequest("valid_to", "must not be before the node's valid_from")
	}
	lsn := f.nextLSN()

	f.anchors[idx].SystemTo = &at

	next := cur
	next.Properties = cloneProps(cur.Properties)
	next.ValidTo = &validTo
	next.Closed = true
	next.SystemFrom = at
	next.SystemTo = nil
	next.Revision = lsn
	next.TxnID = lsn

	f.anchors = append(f.anchors, next)
	superseded := cloneAnchor(f.anchors[idx])
	return &AnchorMutationResult{Anchor: cloneAnchor(next), Superseded: &superseded, Source: SourceFixture}, nil
}
