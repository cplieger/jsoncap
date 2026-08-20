package jsoncap_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cplieger/jsoncap/v2"
)

// This file pins the COST claim the rest of the suite leaves to prose: the
// walk's allocation is bounded by the caller's caps, not by the cardinality on
// the wire. Every other test here asserts semantics - parity, caps, budget,
// structure - and a bounded decoder that produced the right answers while
// allocating proportionally to hostile input would satisfy all of them and
// still be useless.
//
// Two things decide the SHAPE of the assertions, both measured on go1.27.0.
//
// Allocation COUNT is the right metric for the bound and the wrong one for the
// amplification. Measured on the hostile body below at 100000 elements:
// json.Unmarshal takes only 39 allocations against the bounded walk's 22,
// because it allocates ONE slice backing array - few allocations, 12974836
// bytes. So a count ratio says nothing, while the count's INDEPENDENCE from
// cardinality says everything. The byte gap is left to the benchmarks, where
// b.ReportAllocs prints B/op and no threshold has to be invented.
//
// An exact count cannot be asserted at all. Since Go 1.27 encoding/json is
// backed by encoding/json/v2, whose coder is POOLED, so under -race - which is
// how this suite runs in CI - a single testing.AllocsPerRun sample on any JSON
// path is nondeterministic: measured 21 to 25 across six trials at one input
// size, and a hard 21.0 with -race off. Hence minAllocsPerRun below and a
// ceiling rather than an equality. The floor is also what makes the numbers
// robust to a parallel sibling test allocating inside the measurement window:
// pollution can only ever raise a sample, never lower it.
//
// These tests do not call t.Parallel: AllocsPerRun pins GOMAXPROCS to 1 for its
// window, so running them concurrently would only add noise they then have to
// discount.

// hostileArray renders the amplification shape the package exists to bound: n
// compact serialized elements, cheap on the wire and expensive to decode. At
// n=100000 it is 300011 bytes of JSON that json.Unmarshal expands into
// 12974836 bytes of decoded structs and slice backing array - 43x its own wire
// size, which is why a byte cap on the body is not a bound on the decode.
func hostileArray(n int) []byte {
	var b strings.Builder
	b.Grow(3*n + 16)
	b.WriteString(`{"parts":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// minAllocsPerRun is testing.AllocsPerRun's steady-state cost: the minimum over
// several trials, which discards the pooled coder's cold-start samples and any
// allocation a concurrently running test contributed to the window.
func minAllocsPerRun(runs, trials int, f func()) float64 {
	best := testing.AllocsPerRun(runs, f)
	for range trials - 1 {
		if got := testing.AllocsPerRun(runs, f); got < best {
			best = got
		}
	}
	return best
}

// TestBoundedAllocationDoesNotScaleWithCardinality is the package's reason to
// exist, as an assertion: a caller whose per-array cap is 16 pays for 16
// elements whatever the body claims. The two bodies differ 1000-fold in
// cardinality and in wire size, and the walk must reject both at the same cost.
//
// Measured growth is 1.00x - 21.0 allocations for both bodies, identical with
// -race on and off - against a 1.5x ceiling, which leaves room for the pooled
// coder's -race wobble (a single sample ranges 21 to 25) without admitting a
// real regression. Red-checked against the materialize-then-check shape, where
// the walk decodes the whole array before applying the cap: that reports 34 and
// 69 for a growth of 2.03x and fails. Note the ceiling is a RATIO rather than a
// multiple of some absolute constant because the mutant inflates the baseline
// too, and note that growth there is logarithmic rather than linear - the
// regrow goes through append, so a million elements costs ~20 reallocations,
// not a million. That is why the ceiling is 1.5x and not 10x.
func TestBoundedAllocationDoesNotScaleWithCardinality(t *testing.T) {
	const cap = 16
	small, large := hostileArray(1000), hostileArray(1_000_000)

	// Both bodies must actually reach the cap, or the test measures nothing.
	if _, err := budgetWidget(small, 0, cap, 0); !errorIsArrayCap(err) {
		t.Fatalf("small body: err = %v, want ErrArrayCap (the measurement needs the cap to fire)", err)
	}
	if _, err := budgetWidget(large, 0, cap, 0); !errorIsArrayCap(err) {
		t.Fatalf("large body: err = %v, want ErrArrayCap (the measurement needs the cap to fire)", err)
	}

	lo := minAllocsPerRun(5, 4, func() { _, _ = budgetWidget(small, 0, cap, 0) })
	hi := minAllocsPerRun(5, 4, func() { _, _ = budgetWidget(large, 0, cap, 0) })

	if ceiling := 1.5 * lo; hi > ceiling {
		t.Errorf("allocations grew with cardinality: %.0f at 1000 elements (%d bytes) -> %.0f at 1000000 (%d bytes), want no growth (ceiling %.0f)",
			lo, len(small), hi, len(large), ceiling)
	}
}

// TestPreflightAllocationBoundedByDepthCeiling pins the cost half of the
// nesting bound. Since Go 1.27 the depth guard is encoding/json's, and MaxDepth
// documents the consequence as measured: a 1 MiB all-opens body is refused
// without recursing once per byte. TestPreflightBoundsAllOpensBody asserts only
// that it IS refused, which a walk that first recursed 1048576 frames would
// also satisfy. This asserts the cost: 64x more input for the same work,
// because the ceiling stops the walk at MaxDepth frames rather than at the end
// of the input. Measured 56 allocations at both sizes, growth 1.00x.
func TestPreflightAllocationBoundedByDepthCeiling(t *testing.T) {
	small := bytes.Repeat([]byte("["), 1<<16)
	large := bytes.Repeat([]byte("["), 1<<22)

	if err := jsoncap.Preflight(bytes.NewReader(large)); err == nil {
		t.Fatal("Preflight(4 MiB of open brackets) = nil, want it rejected")
	}

	lo := minAllocsPerRun(3, 4, func() { _ = jsoncap.Preflight(bytes.NewReader(small)) })
	hi := minAllocsPerRun(3, 4, func() { _ = jsoncap.Preflight(bytes.NewReader(large)) })

	if ceiling := 1.5 * lo; hi > ceiling {
		t.Errorf("allocations grew with input length: %.0f at %d bytes -> %.0f at %d bytes, want the cost bounded by MaxDepth (ceiling %.0f)",
			lo, len(small), hi, len(large), ceiling)
	}
}

// errorIsArrayCap keeps the cap check above readable without importing errors
// for one call.
func errorIsArrayCap(err error) bool {
	return err != nil && strings.Contains(err.Error(), jsoncap.ErrArrayCap.Error())
}

// BenchmarkHostileArray is the amplification claim as a trend rather than a
// threshold: run it and read B/op. Measured on go1.27.0 at 100000 elements from
// a 300011-byte body, json.Unmarshal allocates 12974836 B/op in 6703647 ns
// where the bounded walk allocates 1823 B/op in 6944 ns and refuses the body -
// 7117x fewer bytes and 965x faster, a gap no assertion has to name a number
// for.
//
// Neither arm calls b.SetBytes, deliberately: the bounded arm stops at element
// 16 and never reads the remaining 300 KB, so a MB/s figure would divide work
// that did not happen by time that did and report ~43 GB/s.
func BenchmarkHostileArray(b *testing.B) {
	body := hostileArray(100_000)

	b.Run("unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var w widget
			_ = json.Unmarshal(body, &w)
		}
	})

	b.Run("bounded-cap16", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = budgetWidget(body, 0, 16, 0)
		}
	})
}

// BenchmarkPreflight covers the two shapes whose cost is upstream-controlled:
// a wide object, where every key pays a foldKey canonicalization and a set
// insert, and the all-opens body the depth ceiling has to stop.
func BenchmarkPreflight(b *testing.B) {
	var wide strings.Builder
	wide.WriteByte('{')
	for i := range 1000 {
		if i > 0 {
			wide.WriteByte(',')
		}
		wide.WriteString(`"key`)
		wide.WriteString(strings.Repeat("x", 8))
		wide.WriteString(string(rune('a' + i%26)))
		wide.WriteString(`":1`)
	}
	wide.WriteByte('}')
	wideBody := []byte(wide.String())
	opens := bytes.Repeat([]byte("["), 1<<20)

	b.Run("wide-object", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(wideBody)))
		for b.Loop() {
			_ = jsoncap.Preflight(bytes.NewReader(wideBody))
		}
	})

	// No SetBytes on this arm: the depth ceiling stops the walk after MaxDepth
	// of the 1048576 brackets, so throughput over the whole body is fiction.
	b.Run("all-opens", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = jsoncap.Preflight(bytes.NewReader(opens))
		}
	})
}

// BenchmarkSkip is the unknown-field path. Skip builds no Go value, which is
// the claim its doc makes, but it is NOT allocation-free: json.Decoder.Token
// returns each token as an any, so the cost is proportional to the token count
// of the skipped value. Measured 5.02 allocations per skipped two-field object
// element (5023 for a 17027-byte array of 1000 of them), which is the number to
// compare against if this path is ever reworked.
func BenchmarkSkip(b *testing.B) {
	var body strings.Builder
	body.WriteString(`{"unknown":[`)
	for i := range 1000 {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"a":1,"b":"xx"}`)
	}
	body.WriteString(`],"name":"kept"}`)
	raw := []byte(body.String())

	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		d := jsoncap.NewDecoder(bytes.NewReader(raw), 0)
		var w widget
		_ = decodeWidget(d, &w, 0, 0)
		_ = d.End()
	}
}
