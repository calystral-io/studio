package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/calystral-io/studio/internal/corepb/querypb"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// capturingQuery records the last QueryRequest Core received, so a test can
// assert what the BFF actually put on the wire. It returns an empty result
// (the endpoint then renders 200 with no rows).
type capturingQuery struct {
	querypb.UnimplementedQueryServiceServer
	mu   sync.Mutex
	last *querypb.QueryRequest
}

func (c *capturingQuery) Query(_ context.Context, req *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	c.mu.Lock()
	c.last = req
	c.mu.Unlock()
	return &querypb.QueryResponse{}, nil
}

func (c *capturingQuery) request() *querypb.QueryRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// A WHERE filter value on GET /api/v1/cluster/nodes must reach Core as a cybr
// query PARAM (querypb.Params), in first-appearance order - not be dropped. This
// is the BFF half of the param-anchored filter path; without it Core's compiled
// LOAD_PARAM slots are never bound.
func TestClusterNodesFilterReachesOutboundParams(t *testing.T) {
	qs := &capturingQuery{}
	s := newGRPCServerWithQuery(t, qs)

	rec := do(t, s, http.MethodGet, "/api/v1/cluster/nodes?region=ap-south-1&status=draining", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	req := qs.request()
	if req == nil {
		t.Fatal("Core never received a query")
	}
	if len(req.Params) != 2 {
		t.Fatalf("outbound params = %d, want 2 (region, status); cyql=%q", len(req.Params), req.Cyql)
	}
	for i, want := range []string{"ap-south-1", "draining"} {
		v, err := cybrwire.DecodeValue(req.Params[i])
		if err != nil {
			t.Fatalf("param %d decode: %v", i, err)
		}
		got, ok := v.AsString()
		if !ok || got != want {
			t.Errorf("param %d = %q (ok=%v), want %q", i, got, ok, want)
		}
	}
	// The value is also present in the CyQL text - the literal the compiler lowers
	// into the LOAD_PARAM slot the param binds into.
	if !strings.Contains(req.Cyql, "ap-south-1") || !strings.Contains(req.Cyql, "draining") {
		t.Errorf("cyql missing filter literals: %q", req.Cyql)
	}
}

// An UNFILTERED list sends no params (so Core's compiled query, which has no
// LOAD_PARAM slots, is not handed spurious values).
func TestClusterNodesNoFilterSendsNoParams(t *testing.T) {
	qs := &capturingQuery{}
	s := newGRPCServerWithQuery(t, qs)

	rec := do(t, s, http.MethodGet, "/api/v1/cluster/nodes", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if req := qs.request(); req == nil || len(req.Params) != 0 {
		t.Fatalf("unfiltered params = %v, want none", req.GetParams())
	}
}
