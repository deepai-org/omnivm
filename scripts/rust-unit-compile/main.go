// rust-unit-compile: toolchain driver for the level-2 registry compile sweep
// (scripts/rust-compile-sweep.sh).
//
// Reads "<rel>\t<manifest.json>" lines from stdin, extracts every Rust
// func_def unit (source + exports) from each manifest, completes the source
// exactly like the manifest executor does (pkg/manifest rust_plugin.go
// rustUnitSource: export shims when absent, the unit ABI marker), and
// compiles it through the REAL production toolchain:
// rust.GetToolchain().BuildUnit — the same cargo workspace, dependency
// inference, runtime-alias injection, and artifact cache the executor uses.
//
// It is deliberately ONE process over the whole sweep: workspace builds
// serialize process-wide (flock + buildMu), and the in-memory dedup map plus
// the on-disk artifact cache make repeated identical units free.
//
// Output: one tab-separated line per input line, flushed per result:
//
//	ok      <rel>  built <ms>ms | cached
//	fail    <rel>  <first rustc/cargo error line>
//	trivial <rel>  unit contains only the injected probe fn (item-free file)
//	no-unit <rel>  manifest has no rust func_def op
//	error   <rel>  <driver problem: unreadable/unparseable manifest>
//
// Build ad hoc (never shipped): go build -o <tmp> ./scripts/rust-unit-compile
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/omnivm/omnivm/pkg/rust"
)

type op struct {
	Op          string            `json:"op"`
	Runtime     string            `json:"runtime"`
	BodyRuntime string            `json:"bodyRuntime"`
	Name        string            `json:"name"`
	Async       bool              `json:"async"`
	Source      string            `json:"source"`
	Exports     []string          `json:"exports"`
	Params      []json.RawMessage `json:"params"`
	Body        []op              `json:"body"`
}

type manifestDoc struct {
	Ops []op `json:"ops"`
}

type unit struct {
	source  string
	exports []string
}

// collectRustUnits walks the op tree and returns the distinct Rust
// compilation units carried by func_def ops (every Rust func_def in a
// program carries the same unit, so this is normally exactly one).
func collectRustUnits(ops []op, tc *rust.Toolchain) map[string]unit {
	units := map[string]unit{}
	var walk func(ops []op)
	walk = func(ops []op) {
		for i := range ops {
			o := &ops[i]
			if o.Op == "func_def" && o.BodyRuntime == "rust" && o.Source != "" {
				// Mirror pkg/manifest compileRustPlugin's export defaulting.
				exports := append([]string(nil), o.Exports...)
				if len(exports) == 0 && o.Name != "" {
					exports = []string{o.Name}
				}
				source := completeUnitSource(o, exports)
				units[tc.UnitCacheKey(source, exports)] = unit{source: source, exports: exports}
			}
			walk(o.Body)
		}
	}
	walk(ops)
	return units
}

// completeUnitSource mirrors pkg/manifest rust_plugin.go rustUnitSource (the
// function is unexported there): codegen-emitted sources already carry export
// shims, hand-written ones get them generated, and every unit gets the ABI
// marker. Keeping this byte-identical to the executor's completion is what
// makes the sweep compile the EXACT unit the runtime would.
func completeUnitSource(o *op, exports []string) string {
	source := o.Source
	var extra strings.Builder
	if !strings.Contains(source, "export_fn!") && !strings.Contains(source, "export_async_fn!") {
		arity := len(o.Params)
		macro := "export_fn!"
		if o.Async {
			macro = "export_async_fn!"
		}
		for _, exportName := range exports {
			fmt.Fprintf(&extra, "omnivm::%s(OmniVMCall_%s, %s, %d);\n", macro, exportName, exportName, arity)
		}
	}
	if !strings.Contains(source, "unit_abi_marker!") {
		extra.WriteString("omnivm::unit_abi_marker!();\n")
	}
	if extra.Len() == 0 {
		return source
	}
	return source + "\n" + extra.String()
}

// isTrivialProbeUnit reports whether a unit contains nothing but the sweep's
// injected probe fn and its shim/marker — i.e. the registry file contributed
// no items at all (comment-only files). Those are skips, not passes.
func isTrivialProbeUnit(source string) bool {
	for _, line := range strings.Split(source, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.Contains(t, "__omnivm_probe") ||
			strings.Contains(t, "unit_abi_marker!") {
			continue
		}
		return false
	}
	return true
}

var rustcErrRE = regexp.MustCompile(`(?m)^error(\[[A-Z0-9]+\])?[:!].*$`)

// firstErrorLine pulls the first rustc/cargo `error...` line out of a
// BuildUnit error ("rust compilation failed:\n<cargo output>...").
func firstErrorLine(msg string) string {
	if m := rustcErrRE.FindString(msg); m != "" {
		return squash(m)
	}
	for _, line := range strings.Split(msg, "\n") {
		if t := strings.TrimSpace(line); t != "" && t != "rust compilation failed:" {
			return squash(t)
		}
	}
	return "unknown error"
}

func squash(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func main() {
	tc, err := rust.GetToolchain()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rust-unit-compile: toolchain init: %v\n", err)
		os.Exit(2)
	}

	type result struct {
		ok  bool
		msg string
	}
	built := map[string]result{} // unit cache key -> outcome (failures recompile in BuildUnit; not here)

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		line := strings.TrimRight(in.Text(), "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		rel, path, found := strings.Cut(line, "\t")
		if !found {
			path = rel
		}
		emit := func(status, detail string) {
			fmt.Fprintf(out, "%s\t%s\t%s\n", status, rel, detail)
			out.Flush()
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			emit("error", squash(readErr.Error()))
			continue
		}
		var doc manifestDoc
		if jsonErr := json.Unmarshal(data, &doc); jsonErr != nil {
			emit("error", "manifest unparseable: "+squash(jsonErr.Error()))
			continue
		}

		units := collectRustUnits(doc.Ops, tc)
		if len(units) == 0 {
			emit("no-unit", "manifest has no rust func_def op")
			continue
		}
		allTrivial := true
		for _, u := range units {
			if !isTrivialProbeUnit(u.source) {
				allTrivial = false
				break
			}
		}
		if allTrivial {
			emit("trivial", "unit contains only the injected probe fn")
			continue
		}

		failMsg := ""
		detail := "cached"
		for key, u := range units {
			r, seen := built[key]
			if !seen {
				start := time.Now()
				_, buildErr := tc.BuildUnit(u.source, u.exports)
				ms := time.Since(start).Milliseconds()
				if buildErr != nil {
					r = result{ok: false, msg: firstErrorLine(buildErr.Error())}
				} else {
					r = result{ok: true, msg: fmt.Sprintf("built %dms", ms)}
				}
				built[key] = r
				detail = r.msg
			}
			if !r.ok && failMsg == "" {
				failMsg = r.msg
			}
		}
		if failMsg != "" {
			emit("fail", failMsg)
		} else {
			emit("ok", detail)
		}
	}
	if scanErr := in.Err(); scanErr != nil {
		fmt.Fprintf(os.Stderr, "rust-unit-compile: stdin: %v\n", scanErr)
		os.Exit(2)
	}
}
