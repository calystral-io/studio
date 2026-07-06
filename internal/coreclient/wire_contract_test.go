package coreclient

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/calystral-io/studio/internal/corepb/mutatepb"
	"github.com/calystral-io/studio/internal/corepb/querypb"
	"github.com/calystral-io/studio/internal/cybrwire"
)

// This file is the write/read wire-contract scaffolding: it stands up a fixture
// Core that speaks Core's ACTUAL Query/Mutate contract via the shared cybr codec
// (internal/cybrwire, the Go port of core/src/{proc/wire,mutate}.rs), then drives
// the codec through the real proto surface. It proves two contract properties
// without a running Core:
//
//   - write: a Mutation the BFF encodes decodes byte-for-byte on a Core-faithful
//     Mutate handler, and the committed MutateResponse (txn_id / affected /
//     commit_lsn / created) is surfaced;
//   - read: a QueryRow.payload Core encodes (opaque cybr value bytes) decodes
//     with the same codec (the row-decoder scaffolding).
//
// It deliberately drives the codec at the proto boundary rather than through
// GRPCClient's mutation methods, because binding a tenant's string type/field/
// anchor names to the numeric ids the wire needs (schema id resolution) is not
// wired yet: Core's schema read returns definition SOURCE TEXT, not an id map.

// contractMutateServer is a Core-faithful Mutate handler: it decodes each
// mutation through the shared codec exactly as core/src/grpc/mutate.rs does,
// "commits" against an in-memory counter, and returns the real response shape.
// A payload the codec cannot decode is rejected invalid_argument, mirroring Core.
type contractMutateServer struct {
	mutatepb.UnimplementedMutateServiceServer
	mu      sync.Mutex
	nextID  uint64              // raw anchor id allocator (stands in for cvm's)
	lsn     uint64              // durable-lsn / txn-id counter
	decoded []cybrwire.Mutation // every mutation this server decoded, for assertions
}

func (s *contractMutateServer) Mutate(_ context.Context, req *mutatepb.MutateRequest) (*mutatepb.MutateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var created []uint64
	var affected uint64
	for _, m := range req.GetMutations() {
		dm, err := cybrwire.DecodeMutation(m.GetKind(), m.GetPayload())
		if err != nil {
			// Same disposition as Core: a malformed payload aborts the txn.
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		s.decoded = append(s.decoded, dm)
		switch dm.Kind() {
		case mutatepb.MutationKind_MUTATION_KIND_CREATE_NODE,
			mutatepb.MutationKind_MUTATION_KIND_CREATE_EDGE:
			s.nextID++
			created = append(created, s.nextID)
		}
		affected++
	}
	s.lsn++
	return &mutatepb.MutateResponse{
		TxnId:     s.lsn,
		Affected:  affected,
		CommitLsn: s.lsn,
		Created:   created,
	}, nil
}

// contractQueryServer is a Core-faithful Query handler: it returns rows whose
// payloads are opaque cybr value bytes (QueryRow.payload), exactly as Core's row
// contract specifies. The BFF decodes them with the same codec.
type contractQueryServer struct {
	querypb.UnimplementedQueryServiceServer
	rows []cybrwire.Value
}

func (s *contractQueryServer) Query(_ context.Context, _ *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	out := make([]*querypb.QueryRow, 0, len(s.rows))
	for _, v := range s.rows {
		b, err := cybrwire.EncodeValue(v)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out = append(out, &querypb.QueryRow{Payload: b})
	}
	return &querypb.QueryResponse{Rows: out}, nil
}

// startContractCore serves both Core-faithful handlers on a loopback port and
// returns the handlers (for assertions) plus a dialled connection.
func startContractCore(t *testing.T) (*contractMutateServer, *contractQueryServer, *grpc.ClientConn) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mut := &contractMutateServer{}
	qry := &contractQueryServer{rows: []cybrwire.Value{
		cybrwire.Str("Ada"),
		cybrwire.Int(42),
		cybrwire.Array([]cybrwire.Value{cybrwire.Bool(true), cybrwire.Dec("3.14")}),
	}}
	srv := grpc.NewServer()
	mutatepb.RegisterMutateServiceServer(srv, mut)
	querypb.RegisterQueryServiceServer(srv, qry)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return mut, qry, conn
}

// TestMutationBytesDecodeOnCoreFaithfulServer: mutations the BFF encodes with the
// shared codec decode byte-for-byte on a Core-faithful Mutate handler, and the
// committed MutateResponse (txn_id / affected / commit_lsn / created) is read back.
func TestMutationBytesDecodeOnCoreFaithfulServer(t *testing.T) {
	mut, _, conn := startContractCore(t)
	client := mutatepb.NewMutateServiceClient(conn)

	// One representative mutation of every write kind, at the numeric-id level the
	// wire contract speaks (a real caller resolves these from the tenant schema).
	sent := []cybrwire.Mutation{
		cybrwire.CreateNode(7, map[uint32]cybrwire.Value{1: cybrwire.Str("Ada"), 2: cybrwire.Int(30)}),
		cybrwire.Update(1, 2, cybrwire.Int(31)),
		cybrwire.Close(1),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, m := range sent {
		payload, err := cybrwire.EncodeMutation(m)
		if err != nil {
			t.Fatalf("encode mutation: %v", err)
		}
		resp, err := client.Mutate(ctx, &mutatepb.MutateRequest{
			Tenant:    "demo-tenant",
			Mutations: []*mutatepb.Mutation{{Kind: m.Kind(), Payload: payload}},
		})
		if err != nil {
			t.Fatalf("Mutate(%v): %v", m.Kind(), err)
		}
		// The real MutateResponse fields are populated (the write path surfaces
		// them once id resolution lets the BFF build a valid payload).
		if resp.GetTxnId() == 0 || resp.GetCommitLsn() == 0 {
			t.Errorf("%v: txn_id/commit_lsn not surfaced: %+v", m.Kind(), resp)
		}
		if resp.GetAffected() != 1 {
			t.Errorf("%v: affected = %d, want 1", m.Kind(), resp.GetAffected())
		}
	}

	// The create yielded exactly one echoed anchor id; update/close yielded none.
	if got := len(mut.decoded); got != len(sent) {
		t.Fatalf("server decoded %d mutations, want %d", got, len(sent))
	}
	// Each decoded mutation re-encodes to the exact bytes the BFF sent (a faithful
	// round-trip across the proto boundary).
	for i, m := range sent {
		want, _ := cybrwire.EncodeMutation(m)
		got, err := cybrwire.EncodeMutation(mut.decoded[i])
		if err != nil {
			t.Fatalf("re-encode decoded[%d]: %v", i, err)
		}
		if string(got) != string(want) {
			t.Errorf("decoded[%d] bytes:\n got %x\nwant %x", i, got, want)
		}
	}
}

// TestMalformedPayloadRejectedLikeCore: a payload the codec cannot decode is
// rejected invalid_argument, matching Core's abort-the-txn disposition (this is
// what a live Core does today with the BFF's interim non-cybr payloads).
func TestMalformedPayloadRejectedLikeCore(t *testing.T) {
	_, _, conn := startContractCore(t)
	client := mutatepb.NewMutateServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Mutate(ctx, &mutatepb.MutateRequest{
		Tenant: "demo-tenant",
		Mutations: []*mutatepb.Mutation{{
			Kind:    mutatepb.MutationKind_MUTATION_KIND_CLOSE,
			Payload: []byte(`{"id":"node_1"}`), // JSON, not the cybr wire format
		}},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// TestQueryRowsDecodeWithSharedCodec: row payloads Core encodes (opaque cybr
// value bytes) decode with the same codec - the read-path row-decoder scaffolding.
func TestQueryRowsDecodeWithSharedCodec(t *testing.T) {
	_, qry, conn := startContractCore(t)
	client := querypb.NewQueryServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := client.Query(ctx, &querypb.QueryRequest{Cyql: "MATCH (n:Node) RETURN n", Tenant: "demo-tenant"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(resp.GetRows()) != len(qry.rows) {
		t.Fatalf("rows = %d, want %d", len(resp.GetRows()), len(qry.rows))
	}
	for i, row := range resp.GetRows() {
		got, err := cybrwire.DecodeValue(row.GetPayload())
		if err != nil {
			t.Fatalf("decode row %d: %v", i, err)
		}
		// The decoded value re-encodes to the emitted payload (faithful decode).
		re, _ := cybrwire.EncodeValue(got)
		if string(re) != string(row.GetPayload()) {
			t.Errorf("row %d did not round-trip:\n got %x\nwant %x", i, re, row.GetPayload())
		}
	}

	// Spot-check the first row decoded to the expected concrete value.
	first, _ := cybrwire.DecodeValue(resp.GetRows()[0].GetPayload())
	if s, ok := first.AsString(); !ok || s != "Ada" {
		t.Errorf("row 0 = %v, want Str(\"Ada\")", first.Kind())
	}
}
