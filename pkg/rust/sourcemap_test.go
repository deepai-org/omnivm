package rust

import (
	"strings"
	"testing"
)

func TestSourceMapMapUnitLine(t *testing.T) {
	smap := &SourceMap{File: "review.poly", Entries: []*SourceMapEntry{
		{UnitLine: 1, PolyLine: 6, Lines: 1},
		{UnitLine: 3, PolyLine: 8, Lines: 4},
		{UnitLine: 8, PolyLine: 13, Lines: 4},
	}}
	cases := []struct {
		unitLine int
		want     int
		ok       bool
	}{
		{1, 6, true},   // first line of first item
		{2, 0, false},  // blank separator: glue
		{3, 8, true},   // item start
		{6, 11, true},  // last line of 4-line item
		{7, 0, false},  // separator
		{8, 13, true},
		{11, 16, true},
		{12, 0, false}, // past the last item (shim glue)
		{0, 0, false},
		{-3, 0, false},
	}
	for _, c := range cases {
		got, ok := smap.MapUnitLine(c.unitLine)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("MapUnitLine(%d) = (%d, %v), want (%d, %v)", c.unitLine, got, ok, c.want, c.ok)
		}
	}
	var nilMap *SourceMap
	if _, ok := nilMap.MapUnitLine(1); ok {
		t.Error("nil map must not map")
	}
}

func TestRenderMappedCompileError(t *testing.T) {
	smap := &SourceMap{File: "review.poly", Entries: []*SourceMapEntry{
		{UnitLine: 1, PolyLine: 6, Lines: 1},
		{UnitLine: 3, PolyLine: 13, Lines: 5},
	}}
	// Two alias lines injected: lib.rs line 7 == unit line 5 == poly line 15.
	stdout := strings.Join([]string{
		`   Compiling omnivm-unit-u1234 v0.1.0`, // non-JSON noise: skipped
		`{"reason":"compiler-artifact","target":{"name":"other"}}`,
		`{"reason":"compiler-message","message":{"message":"mismatched types","level":"error",` +
			`"spans":[{"file_name":"units/u1234/src/lib.rs","line_start":7,"is_primary":true}],` +
			`"rendered":"error[E0308]: mismatched types\n --> units/u1234/src/lib.rs:7:18\n  |\n"}}`,
		`{"reason":"compiler-message","message":{"message":"aborting due to 1 previous error","level":"error","spans":[],"rendered":""}}`,
		`{"reason":"build-finished","success":false}`,
	}, "\n")
	out := renderMappedCompileError(stdout, "error: could not compile `omnivm-unit-u1234`", smap, 2, "")

	if !strings.HasPrefix(out, "review.poly:15: mismatched types") {
		t.Fatalf("missing mapped header, got:\n%s", out)
	}
	if !strings.Contains(out, "--> review.poly:15:18") {
		t.Fatalf("rendered snippet coordinate not rewritten:\n%s", out)
	}
	if strings.Contains(out, "lib.rs:7") {
		t.Fatalf("raw lib.rs coordinate leaked:\n%s", out)
	}
	if strings.Contains(out, "aborting due to") {
		t.Fatalf("abort summary should be dropped:\n%s", out)
	}
}

func TestRenderMappedCompileErrorGlueAndFallback(t *testing.T) {
	smap := &SourceMap{File: "app.poly", Entries: []*SourceMapEntry{
		{UnitLine: 1, PolyLine: 3, Lines: 2},
	}}
	// Error in generated glue (lib.rs line 9, unit line 9, past the map).
	stdout := `{"reason":"compiler-message","message":{"message":"cannot find function ` + "`missing`" + `","level":"error",` +
		`"spans":[{"file_name":"units/uffff/src/lib.rs","line_start":9,"is_primary":true}],` +
		`"rendered":"error[E0425]: cannot find function\n --> units/uffff/src/lib.rs:9:1\n"}}`
	out := renderMappedCompileError(stdout, "", smap, 0, "")
	if !strings.Contains(out, "(in generated glue at src/lib.rs:9)") {
		t.Fatalf("glue header note missing:\n%s", out)
	}
	if !strings.Contains(out, "units/uffff/src/lib.rs:9:1 (generated glue)") {
		t.Fatalf("glue rendered note missing:\n%s", out)
	}

	// No compiler messages at all (cargo-level failure): raw stderr wins.
	fallback := renderMappedCompileError("not json at all\n", "error: failed to get `leftpad`", smap, 0, "")
	if fallback != "error: failed to get `leftpad`" {
		t.Fatalf("fallback = %q", fallback)
	}
}

func TestGlueContextNamesBoundaryWrappers(t *testing.T) {
	smap := &SourceMap{File: "app.poly", Entries: []*SourceMapEntry{
		{UnitLine: 1, PolyLine: 3, Lines: 2},
	}}
	unitSource := strings.Join([]string{
		"fn largest<T: std::hash::Hash>(a: T) -> T {", // unit line 1 (mapped)
		"    a",                                       // 2 (mapped)
		"",                                            // 3
		"fn __omnivm_largest__f64(a: f64) -> f64 {", // 4: Tier-1 stamp
		"    largest(a)",                            // 5: bound failure here
		"}",                                         // 6
		"fn __omnivm_dyn_largest(a: omnivm::Dyn) -> omnivm::Dyn {", // 7
		"    largest(a)", // 8
		"}",              // 9
		"struct __OmnivmProbe_pick<T>(::std::marker::PhantomData<T>);", // 10
		"fn noise() {}", // 11
	}, "\n")

	if got := glueContext(unitSource, 5, smap, 0); !strings.Contains(got, "per-call-site stamp '__omnivm_largest__f64' of fn 'largest'") {
		t.Fatalf("tier-1 context = %q", got)
	}
	if got := glueContext(unitSource, 8, smap, 0); got != "the Dyn instantiation wrapper for fn 'largest'" {
		t.Fatalf("tier-3 context = %q", got)
	}
	if got := glueContext(unitSource, 11, smap, 0); !strings.Contains(got, "Tier-2 boundary dispatcher for fn 'pick'") {
		t.Fatalf("tier-2 context = %q", got)
	}
	// Verbatim (.poly-mapped) lines never report glue context.
	if got := glueContext(unitSource, 2, smap, 0); got != "" {
		t.Fatalf("mapped line context = %q", got)
	}

	// The header carries the note when the source identifies the wrapper.
	stdout := `{"reason":"compiler-message","message":{"message":"the trait bound ` +
		"`f64: Hash`" + ` is not satisfied","level":"error",` +
		`"spans":[{"file_name":"units/uffff/src/lib.rs","line_start":5,"is_primary":true}],"rendered":""}}`
	out := renderMappedCompileError(stdout, "", smap, 0, unitSource)
	if !strings.Contains(out, "in generated glue at src/lib.rs:5 — the per-call-site stamp") {
		t.Fatalf("header note missing:\n%s", out)
	}
}
