package coreclient

import (
	"sync"
	"testing"
	"time"

	"github.com/calystral-io/studio/internal/apierr"
)

// currentVersion returns the current (open) version of id, failing if absent.
func currentVersion(t *testing.T, f *Fixture, id string) AnchorDTO {
	t.Helper()
	res, err := f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: id})
	if err != nil {
		t.Fatalf("history %s: %v", id, err)
	}
	for _, v := range res.Versions {
		if v.SystemTo == nil {
			return v
		}
	}
	t.Fatalf("no current version for %s", id)
	return AnchorDTO{}
}

func TestFixtureCreateAnchor(t *testing.T) {
	f := NewFixture()
	before := f.Count()

	res, err := f.CreateAnchor(ctx(), CreateAnchorParams{
		TenantID: FixtureTenant, ID: "node_new_0001", Type: "Service", Label: "Billing",
		Properties: map[string]any{"tier": "gold"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Anchor.SystemTo != nil || res.Anchor.ValidTo != nil || res.Anchor.Closed {
		t.Errorf("created anchor should be open and not closed: %+v", res.Anchor)
	}
	if res.Anchor.Revision <= 4000 {
		t.Errorf("runtime revision %d should exceed seeded revisions", res.Anchor.Revision)
	}
	if f.Count() != before+1 {
		t.Errorf("count = %d, want %d", f.Count(), before+1)
	}

	// It appears in the default current view.
	list, _ := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 500})
	found := false
	for _, a := range list.Items {
		if a.ID == "node_new_0001" {
			found = true
		}
	}
	if !found {
		t.Error("created anchor missing from list")
	}

	// Duplicate id -> already_exists.
	_, err = f.CreateAnchor(ctx(), CreateAnchorParams{TenantID: FixtureTenant, ID: "node_new_0001", Type: "Service", Label: "Dup"})
	assertCode(t, err, apierr.CodeAlreadyExists)

	// Same id in a different tenant is allowed (tenant-scoped uniqueness).
	if _, err := f.CreateAnchor(ctx(), CreateAnchorParams{TenantID: "other-tenant", ID: "node_new_0001", Type: "Service", Label: "Other"}); err != nil {
		t.Errorf("cross-tenant create should succeed: %v", err)
	}

	// Property map is not aliased: mutating the input afterward doesn't change the store.
	props := map[string]any{"k": "v"}
	_, _ = f.CreateAnchor(ctx(), CreateAnchorParams{TenantID: FixtureTenant, ID: "node_iso", Type: "X", Label: "Y", Properties: props})
	props["k"] = "mutated"
	stored := currentVersion(t, f, "node_iso")
	if stored.Properties["k"] != "v" {
		t.Errorf("stored property aliased input map: %v", stored.Properties["k"])
	}

	// An explicit valid_from is honored.
	vf := mustUTC("2026-01-01T00:00:00Z")
	cr, err := f.CreateAnchor(ctx(), CreateAnchorParams{TenantID: FixtureTenant, ID: "node_vf", Type: "X", Label: "Y", ValidFrom: &vf})
	if err != nil {
		t.Fatal(err)
	}
	if !cr.Anchor.ValidFrom.Equal(vf) {
		t.Errorf("valid_from = %v, want %v", cr.Anchor.ValidFrom, vf)
	}
}

func TestFixtureCorrectAnchor(t *testing.T) {
	f := NewFixture()
	const id = "node_employee_0001" // a single-version (uncorrected) anchor
	orig := currentVersion(t, f, id)

	newLabel := "Ada L. (corrected)"
	res, err := f.CorrectAnchor(ctx(), CorrectAnchorParams{
		TenantID: FixtureTenant, ID: id, Label: &newLabel,
		Properties: map[string]any{"title": "Distinguished Engineer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Anchor.Label != newLabel || res.Anchor.SystemTo != nil {
		t.Errorf("corrected current = %+v", res.Anchor)
	}
	if res.Superseded == nil || res.Superseded.SystemTo == nil {
		t.Fatal("expected a superseded prior with system_to set")
	}
	// The superseded prior keeps the OLD label; valid window unchanged.
	if res.Superseded.Label != orig.Label {
		t.Errorf("superseded label = %q, want original %q", res.Superseded.Label, orig.Label)
	}
	if !res.Anchor.ValidFrom.Equal(orig.ValidFrom) || res.Anchor.ValidTo != orig.ValidTo {
		t.Error("correction must not change the valid window")
	}

	// The returned result must not alias the stored maps (mutating it is inert).
	res.Anchor.Properties["title"] = "tampered"
	if cur := currentVersion(t, f, id); cur.Properties["title"] == "tampered" {
		t.Error("mutation result aliases the stored property map")
	}

	// History now has two versions; one current, one superseded.
	hist := currentVersion(t, f, id)
	if hist.Label != newLabel {
		t.Errorf("current label = %q, want %q", hist.Label, newLabel)
	}

	// Non-overlap: exactly one version resolves at (now, current) and the prior
	// resolves just before the correction instant.
	full, _ := f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: id})
	if len(full.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(full.Versions))
	}

	// Stale precondition -> 409.
	wrong := int64(1)
	_, err = f.CorrectAnchor(ctx(), CorrectAnchorParams{TenantID: FixtureTenant, ID: id, Label: &newLabel, ExpectedRevision: &wrong})
	assertCode(t, err, apierr.CodePreconditionFailed)

	// Matching precondition -> success.
	cur := currentVersion(t, f, id)
	if _, err := f.CorrectAnchor(ctx(), CorrectAnchorParams{TenantID: FixtureTenant, ID: id, Label: &newLabel, ExpectedRevision: &cur.Revision}); err != nil {
		t.Errorf("matching precondition should succeed: %v", err)
	}

	// Unknown id -> 404.
	_, err = f.CorrectAnchor(ctx(), CorrectAnchorParams{TenantID: FixtureTenant, ID: "node_nope", Label: &newLabel})
	assertCode(t, err, apierr.CodeNotFound)
}

func TestFixtureCloseAnchor(t *testing.T) {
	f := NewFixture()
	const id = "node_employee_0002"

	res, err := f.CloseAnchor(ctx(), CloseAnchorParams{TenantID: FixtureTenant, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Anchor.Closed || res.Anchor.ValidTo == nil || res.Anchor.SystemTo != nil {
		t.Errorf("closed current = %+v", res.Anchor)
	}
	if res.Superseded == nil || res.Superseded.Closed {
		t.Error("superseded prior should be the still-open (not closed) version")
	}

	// Closing an already-closed anchor -> 400 invalid_request.
	_, err = f.CloseAnchor(ctx(), CloseAnchorParams{TenantID: FixtureTenant, ID: id})
	assertCode(t, err, apierr.CodeInvalidRequest)

	// After valid_to, a valid-time projection excludes it; history still has both.
	after := res.Anchor.ValidTo.Add(time.Hour)
	list, _ := f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 500, AsOf: &after})
	for _, a := range list.Items {
		if a.ID == id {
			t.Error("closed anchor should be excluded from the post-valid_to projection")
		}
	}
	hist, _ := f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: id})
	if len(hist.Versions) != 2 {
		t.Errorf("history versions = %d, want 2", len(hist.Versions))
	}
}

// TestFixtureMutationConcurrency exercises the lock under -race: concurrent
// create/correct/close mutations interleaved with reads, then asserts each id
// ends with exactly one current version.
func TestFixtureMutationConcurrency(t *testing.T) {
	f := NewFixture()
	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "node_conc_" + string(rune('a'+n))
			_, _ = f.CreateAnchor(ctx(), CreateAnchorParams{TenantID: FixtureTenant, ID: id, Type: "X", Label: "L"})
			label := "L2"
			_, _ = f.CorrectAnchor(ctx(), CorrectAnchorParams{TenantID: FixtureTenant, ID: id, Label: &label})
			_, _ = f.CloseAnchor(ctx(), CloseAnchorParams{TenantID: FixtureTenant, ID: id})
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.ListAnchors(ctx(), ListAnchorsParams{TenantID: FixtureTenant, PageSize: 500})
			_, _ = f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: "node_employee_0001"})
		}()
	}
	wg.Wait()

	// Each created id ends with exactly one current version (closed).
	for i := 0; i < 16; i++ {
		id := "node_conc_" + string(rune('a'+i))
		hist, err := f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: id})
		if err != nil {
			t.Fatalf("history %s: %v", id, err)
		}
		current := 0
		for _, v := range hist.Versions {
			if v.SystemTo == nil {
				current++
			}
		}
		if current != 1 {
			t.Errorf("%s has %d current versions, want 1", id, current)
		}
	}
}

// TestFixtureConcurrentSameID serializes many corrections of ONE id under
// -race, asserting the lock keeps exactly one current version.
func TestFixtureConcurrentSameID(t *testing.T) {
	f := NewFixture()
	const id = "node_employee_0003"
	var wg sync.WaitGroup
	const n = 20
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			label := "L" + string(rune('a'+k))
			_, _ = f.CorrectAnchor(ctx(), CorrectAnchorParams{TenantID: FixtureTenant, ID: id, Label: &label})
		}(i)
	}
	wg.Wait()

	hist, err := f.GetAnchorHistory(ctx(), GetAnchorHistoryParams{TenantID: FixtureTenant, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	current := 0
	for _, v := range hist.Versions {
		if v.SystemTo == nil {
			current++
		}
	}
	if current != 1 {
		t.Errorf("current versions = %d, want 1", current)
	}
	if len(hist.Versions) != n+1 {
		t.Errorf("versions = %d, want %d (1 original + %d corrections)", len(hist.Versions), n+1, n)
	}
}

func assertCode(t *testing.T, err error, want apierr.Code) {
	t.Helper()
	ae, ok := err.(*apierr.APIError)
	if !ok || ae.Code != want {
		t.Fatalf("err = %v, want code %s", err, want)
	}
}
