// Fixture runtime source: a seeded, deterministic in-memory snapshot of the cvm
// execution engine's runtime state - the in-process VM metric registry (the 18
// named cvm_* series across storage/transactions/raft/calvin), the
// content-addressed plan cache (rollup + resident entries), and the cybr opcode
// instruction set with execution profiling. Honestly tagged source:"fixture", it
// gives the operator UI real paginated, filterable data in PR4 without a live
// Core. Like the cluster view this is live state, not bitemporal: each DTO
// carries an `observed_at` snapshot instant.
//
// Two fields are FORWARD-LOOKING telemetry that the cvm interpreter does not
// tally today (it dispatches a flat match over Opcode without per-opcode or
// instruction counters): OpcodeDTO.ExecCount and RuntimeSummary.InstructionsExecuted.
// The fixture seeds representative values so the contract and UI can carry them.
// Opcode discriminants are assigned sequentially within each documented cybr
// v0.2.8 category range; mnemonics and categories are the real instruction set.
package coreclient

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Opcode categories (stable display order mirrors ascending discriminant).
const (
	catStorage     = "storage"
	catIteration   = "iteration"
	catComparison  = "comparison"
	catControlFlow = "control_flow"
	catArithmetic  = "arithmetic"
	catProjection  = "projection"
	catRecursion   = "recursion"
	catMutation    = "mutation"
	catRecord      = "record"
	catTransaction = "transaction"
	catConversion  = "conversion"
	catLoad        = "load"
	catMisc        = "misc"
	catStream      = "stream"
	catChannel     = "channel"
	catArray       = "array"
	catJSON        = "json"
	catAggregate   = "aggregate"
	catSortGroup   = "sort_group"
	catScan        = "scan"
	catPartition   = "partition"
	catLedger      = "ledger"
)

// shortFormMax is the exclusive upper discriminant for single-byte ("short
// form") opcodes; the 0x0100+ range is two-byte encoded.
const shortFormMax = 0x100

// planCacheCapacityBytes is the cvm plan-cache default byte budget (64 MiB).
const planCacheCapacityBytes = uint64(64) << 20

// RuntimeSummary implements CoreClient. Returns the precomputed runtime rollup.
// The runtime is shared operator infrastructure, so it is not tenant-scoped.
func (f *Fixture) RuntimeSummary(_ context.Context, _ RuntimeSummaryParams) (*RuntimeSummaryResult, error) {
	return &RuntimeSummaryResult{Summary: f.runtime, Source: SourceFixture}, nil
}

// ListOpcodes applies category/q filters, then cursor-paginates a stable code
// sort. Q matches the mnemonic (case-insensitive substring).
func (f *Fixture) ListOpcodes(_ context.Context, p ListOpcodesParams) (*ListOpcodesResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]OpcodeDTO, 0, len(f.opcodes))
	for _, op := range f.opcodes {
		if p.Category != "" && op.Category != p.Category {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(op.Mnemonic), q) {
			continue
		}
		filtered = append(filtered, op)
	}

	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Code < filtered[j].Code })

	total := len(filtered)
	items := []OpcodeDTO{}
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

	return &ListOpcodesResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// ListPlanCacheEntries applies pinned/q filters, then cursor-paginates a stable
// key sort. Pinned is "true"/"false" (an exact filter; any other non-empty value
// matches nothing). Q matches the entry key (case-insensitive substring).
func (f *Fixture) ListPlanCacheEntries(_ context.Context, p ListPlanCacheEntriesParams) (*ListPlanCacheEntriesResult, error) {
	offset, err := decodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}

	q := strings.ToLower(strings.TrimSpace(p.Q))
	filtered := make([]PlanCacheEntryDTO, 0, len(f.planCache))
	for _, e := range f.planCache {
		if !pinnedMatches(p.Pinned, e.Pinned) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Key), q) {
			continue
		}
		filtered = append(filtered, e)
	}

	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Key < filtered[j].Key })

	total := len(filtered)
	items := []PlanCacheEntryDTO{}
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

	return &ListPlanCacheEntriesResult{Items: items, Page: page, Source: SourceFixture}, nil
}

// pinnedMatches reports whether an entry's pinned flag satisfies the filter.
// "" matches all; "true"/"false" are exact; any other value matches nothing.
func pinnedMatches(filter string, pinned bool) bool {
	switch filter {
	case "":
		return true
	case "true":
		return pinned
	case "false":
		return !pinned
	default:
		return false
	}
}

// --- Seed data -------------------------------------------------------------

// opcodeGroup is one category's contiguous block of mnemonics; codes are
// assigned base, base+1, ... in listed order.
type opcodeGroup struct {
	category  string
	base      int
	mnemonics []string
}

// opcodeGroups is the cybr v0.2.8 instruction set grouped by category, in
// ascending discriminant order. Mnemonics/categories are the real instruction
// set; within-category discriminants are assigned sequentially from the
// documented category base.
var opcodeGroups = []opcodeGroup{
	{catStorage, 0x00, []string{"Pin", "Unpin", "ReadHdr", "ReadField", "ReadEntry", "ReadVariant", "LoadAdj", "LoadIncoming", "LoadVersionChain", "LoadIndex", "IndexSeek", "Follow"}},
	{catIteration, 0x20, []string{"IterNext", "IterRewind", "IterSeek", "IterClose"}},
	{catComparison, 0x30, []string{"CmpEq", "CmpNeq", "CmpLt", "CmpLe", "CmpGt", "CmpGe", "CmpEdge", "CmpType", "CmpInf", "CmpVariant"}},
	{catControlFlow, 0x50, []string{"Jmp", "JmpEq", "JmpNeq", "JmpTrue", "JmpFalse", "JmpEnd", "JmpEmpty", "CallFn", "CallExtern", "CallStdlib", "TailCall", "Ret"}},
	{catArithmetic, 0x70, []string{"AddI64", "SubI64", "MulI64", "DivI64", "NegI64", "AddDec", "SubDec", "MulDec", "DivDec", "NegDec", "And", "Or", "Not"}},
	{catProjection, 0xA0, []string{"PushCol", "RowDone", "Emit", "Collect"}},
	{catRecursion, 0xB0, []string{"NewFrontier", "PushFrontier", "PopFrontier", "NewPath", "PathExtend", "PathDepth", "PathCycle", "PathNodes", "PathEdges", "PathStart", "PathEnd"}},
	{catMutation, 0xC0, []string{"CreateNode", "CreateEdge", "UpdateField", "CloseValid", "DeleteNode", "DeleteEdge", "Mov"}},
	{catRecord, 0xD0, []string{"NewRecord", "WriteField"}},
	{catTransaction, 0xD8, []string{"BeginSync", "Commit", "Abort", "EmitEvent", "Now", "ReadSetAdd", "WriteSetAdd"}},
	{catConversion, 0xE0, []string{"CastInt", "CastDec", "CastStr", "OptionWrap", "OptionUnwrap", "OptionCheck", "DtToDate", "MakeVariant"}},
	{catLoad, 0xF0, []string{"LoadConst", "LoadParam", "LoadContext", "LoadResultsAll"}},
	{catMisc, 0xF8, []string{"Panic"}},
	{catStream, 0x0100, []string{"Subscribe", "StreamTumble", "StreamSlide", "StreamSession", "StreamMap", "StreamFilter", "StreamFold", "StreamLimit", "StreamSkip", "StreamDistinct", "StreamGroup", "StreamZip", "StreamFirst", "StreamCount"}},
	{catChannel, 0x0120, []string{"Send", "RecvOne", "RecvWait", "RecvReceipt", "Ack", "Nack", "OpenChannel", "OpenEphemeralChannel"}},
	{catArray, 0x0130, []string{"ArrayMap", "ArrayFilter", "ArrayFold", "ArrayAvg", "ArraySum", "ArrayMin", "ArrayMax", "ArrayLen", "ArrayGet", "ArraySort", "ArrayDistinct", "ArrayZip", "ArrayToStream", "ArrayDedupKey", "Intersect", "Except"}},
	{catJSON, 0x0140, []string{"JsonGet", "JsonAs", "JsonAsOpt", "JsonKind"}},
	{catAggregate, 0x0150, []string{"Sum", "Min", "Max", "Avg", "Count"}},
	{catSortGroup, 0x0160, []string{"Sort", "GroupByKey"}},
	{catScan, 0x0170, []string{"ScanType"}},
	{catPartition, 0x0180, []string{"EmitToPartition", "JumpHash", "LoadPartitionCounter", "StorePartitionCounter", "HashField"}},
	{catLedger, 0x01A0, []string{"LedgerAppend", "LedgerReadEntry", "LedgerScanRange"}},
}

// categoryWeight scales how hot a category's opcodes are in the seeded execution
// profile (storage/comparison/control-flow/load dominate a read workload; stream
// and ledger ops are comparatively rare). Zero means never executed.
var categoryWeight = map[string]uint64{
	catStorage: 900, catIteration: 420, catComparison: 760, catControlFlow: 680,
	catArithmetic: 240, catProjection: 300, catRecursion: 90, catMutation: 70,
	catRecord: 110, catTransaction: 130, catConversion: 160, catLoad: 880,
	catMisc: 0, catStream: 40, catChannel: 30, catArray: 120, catJSON: 50,
	catAggregate: 95, catSortGroup: 60, catScan: 200, catPartition: 25, catLedger: 35,
}

// seedRuntime builds the opcode instruction set (with a deterministic execution
// profile), the plan-cache entries, and the runtime summary - whose plan-cache
// rollup and headline counters are DERIVED from the seeded rows so they always
// agree with them.
func seedRuntime() (opcodes []OpcodeDTO, planCache []PlanCacheEntryDTO, summary RuntimeSummary) {
	observedAt := mustUTC("2026-06-27T09:00:00Z")

	// Opcodes with a deterministic execution profile.
	var instructionsExecuted uint64
	i := 0
	for _, g := range opcodeGroups {
		for k, mnemonic := range g.mnemonics {
			code := g.base + k
			// Deterministic, category-weighted count with per-opcode spread; the
			// first opcode in each hot category is the hottest.
			w := categoryWeight[g.category]
			count := w * uint64(1000+(i*37)%900) / uint64(1+k)
			instructionsExecuted += count
			opcodes = append(opcodes, OpcodeDTO{
				Mnemonic:   mnemonic,
				Code:       code,
				CodeHex:    fmt.Sprintf("0x%04X", code),
				Category:   g.category,
				ShortForm:  code < shortFormMax,
				ExecCount:  count,
				ObservedAt: observedAt,
			})
			i++
		}
	}
	// Second pass now that the total is known: per-mille share of all executed
	// instructions.
	for idx := range opcodes {
		if instructionsExecuted > 0 {
			opcodes[idx].ExecShareMilli = int(opcodes[idx].ExecCount * 1000 / instructionsExecuted)
		}
	}

	// Plan-cache entries (content-addressed). Keys are deterministic 32-byte
	// hex; a couple are pinned (excluded from the resident-bytes budget).
	const entryCount = 40
	var residentBytes uint64
	var freqSum uint64
	for n := 0; n < entryCount; n++ {
		size := uint64(1024 + (n*2731)%262144) // ~1 KiB .. ~256 KiB bitcode
		freq := uint64((n*7)%64) + 1
		pinned := n%19 == 0 // entries 0, 19, 38 pinned
		refCount := 1 + (n % 4)
		planCache = append(planCache, PlanCacheEntryDTO{
			Key:        seedCacheKey(n),
			SizeBytes:  size,
			Cost:       uint64(50_000 + (n*4099)%900_000),
			Freq:       freq,
			RefCount:   refCount,
			Pinned:     pinned,
			ObservedAt: observedAt,
		})
		freqSum += freq
		if !pinned {
			residentBytes += size
		}
	}

	// Derive the cache rollup from the rows. Each resident OR evicted entry cost
	// one initial miss; subsequent accesses (freq-1) are hits.
	const evictions = uint64(1_284)
	inserts := uint64(entryCount) + evictions
	misses := inserts
	var hits uint64
	for _, e := range planCache {
		hits += e.Freq - 1
	}
	planCacheStats := PlanCacheStats{
		Hits:          hits,
		Misses:        misses,
		Inserts:       inserts,
		Evictions:     evictions,
		Entries:       len(planCache),
		ResidentBytes: residentBytes,
		CapacityBytes: planCacheCapacityBytes,
		HitRateMilli:  rateMilli(hits, hits+misses),
	}

	groups := seedMetricGroups()
	summary = RuntimeSummary{
		UptimeSeconds:        372_240, // ~4.3 days
		InstructionsExecuted: instructionsExecuted,
		ActiveTransactions:   int(gaugeValue(groups, "cvm_txn_active")),
		OpcodeCount:          len(opcodes),
		MetricSeriesCount:    countSeries(groups),
		PlanCache:            planCacheStats,
		MetricGroups:         groups,
		ObservedAt:           observedAt,
	}
	return opcodes, planCache, summary
}

// seedCacheKey mints a deterministic 64-hex-char (32-byte) content address.
func seedCacheKey(n int) string {
	var b strings.Builder
	x := uint64(n)*0x9E3779B97F4A7C15 + 0x1234567 // mix so keys are non-sequential
	for j := 0; j < 4; j++ {
		x = x*6364136223846793005 + 1442695040888963407
		b.WriteString(fmt.Sprintf("%016x", x))
	}
	return b.String()
}

// rateMilli is num/den in per-mille (0..1000), 0 when den is 0.
func rateMilli(num, den uint64) int {
	if den == 0 {
		return 0
	}
	return int(num * 1000 / den)
}

// i64 returns a pointer to a scalar metric value.
func i64(v int64) *int64 { return &v }

// counter/gauge/histogram build a MetricSeries of the given kind.
func counter(name, help string, v int64) MetricSeries {
	return MetricSeries{Name: name, Kind: "counter", Help: help, Value: i64(v)}
}
func gauge(name, help string, v int64) MetricSeries {
	return MetricSeries{Name: name, Kind: "gauge", Help: help, Value: i64(v)}
}
func histogram(name, help string, h HistogramValue) MetricSeries {
	return MetricSeries{Name: name, Kind: "histogram", Help: help, Histogram: &h}
}

// nanoHistogram builds a cumulative latency histogram from (upper-bound, count)
// pairs plus a +Inf overflow count; sum is supplied directly. Counts are the
// cumulative bucket counts in ascending bound order.
func nanoHistogram(sum uint64, bounds []uint64, counts []uint64, inf uint64) HistogramValue {
	buckets := make([]HistogramBucket, 0, len(bounds)+1)
	for i, b := range bounds {
		ub := b
		buckets = append(buckets, HistogramBucket{UpperBound: &ub, Count: counts[i]})
	}
	buckets = append(buckets, HistogramBucket{UpperBound: nil, Count: inf})
	return HistogramValue{Buckets: buckets, Sum: sum, Count: inf}
}

// seedMetricGroups builds the 18 named cvm_* metric series grouped by subsystem,
// matching the in-process registry (storage, transactions, raft, calvin).
func seedMetricGroups() []MetricGroup {
	return []MetricGroup{
		{Subsystem: catStorage, Series: []MetricSeries{
			counter("cvm_storage_page_reads_total", "Total storage page reads.", 184_203_551),
			counter("cvm_storage_page_writes_total", "Total storage page writes.", 38_119_004),
			counter("cvm_storage_anchor_reads_total", "Total anchor reads.", 91_882_117),
			counter("cvm_storage_scans_opened_total", "Total storage scans opened.", 2_004_551),
			histogram("cvm_storage_wal_fsync_nanos", "WAL fsync latency (ns).",
				nanoHistogram(41_882_551_000,
					[]uint64{50_000, 100_000, 250_000, 500_000, 1_000_000},
					[]uint64{120_004, 410_551, 870_118, 1_044_900, 1_090_204}, 1_092_551)),
			histogram("cvm_storage_commit_nanos", "Commit latency (ns).",
				nanoHistogram(98_551_204_000,
					[]uint64{100_000, 250_000, 500_000, 1_000_000, 5_000_000},
					[]uint64{210_551, 640_118, 980_204, 1_180_900, 1_240_551}, 1_244_004)),
			gauge("cvm_storage_lsm_compaction_queue_depth", "LSM compaction queue depth.", 3),
			gauge("cvm_storage_bplus_depth", "B+ tree height.", 4),
		}},
		{Subsystem: catTransaction, Series: []MetricSeries{
			gauge("cvm_txn_active", "Currently active transactions.", 7),
			counter("cvm_txn_retries_total", "Total transaction retries.", 442_018),
			counter("cvm_txn_conflicts_total", "Total OCC validation conflicts.", 88_211),
			counter("cvm_txn_deadline_cancels_total", "Total deadline cancellations.", 1_204),
			counter("cvm_txn_commits_total", "Total committed transactions.", 50_882_004),
		}},
		{Subsystem: "raft", Series: []MetricSeries{
			counter("cvm_raft_entries_appended_total", "Total Raft log entries appended.", 73_551_900),
			counter("cvm_raft_snapshot_bytes_total", "Total Raft snapshot bytes written.", 9_223_372_036),
			counter("cvm_raft_leader_changes_total", "Total Raft leader changes.", 37),
		}},
		{Subsystem: "calvin", Series: []MetricSeries{
			counter("cvm_calvin_sequencer_batches_total", "Total Calvin sequencer batches.", 5_118_330),
			histogram("cvm_calvin_cross_region_rtt_nanos", "Cross-region RTT (ns).",
				nanoHistogram(8_551_204_551_000,
					[]uint64{1_000_000, 5_000_000, 20_000_000, 50_000_000, 100_000_000},
					[]uint64{40_118, 210_551, 690_204, 980_900, 1_044_551}, 1_050_204)),
		}},
	}
}

// countSeries totals the metric series across all groups.
func countSeries(groups []MetricGroup) int {
	n := 0
	for _, g := range groups {
		n += len(g.Series)
	}
	return n
}

// gaugeValue returns the scalar of the named gauge/counter series, or 0.
func gaugeValue(groups []MetricGroup, name string) int64 {
	for _, g := range groups {
		for _, s := range g.Series {
			if s.Name == name && s.Value != nil {
				return *s.Value
			}
		}
	}
	return 0
}
