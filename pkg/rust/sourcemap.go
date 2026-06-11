package rust

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SourceMapEntry maps one verbatim item slice inside a generated Rust
// compilation unit back to the .poly source it was sliced from. Lines are
// 1-based. `UnitLine` is relative to the unit source string the func_def op
// carries (BEFORE the host injects runtime alias lines); `PolyLine` is the
// corresponding line in the original .poly file; `Lines` is the item's line
// count. Generated shim/glue lines have no entries.
type SourceMapEntry struct {
	UnitLine int `json:"unit_line"`
	PolyLine int `json:"poly_line"`
	Lines    int `json:"lines"`
}

// SourceMap carries the per-item line mapping for one compilation unit plus
// the originating file name ("" when the compiler did not know it).
type SourceMap struct {
	File    string
	Entries []*SourceMapEntry
}

// fileLabel is the coordinate prefix used in rewritten diagnostics.
func (m *SourceMap) fileLabel() string {
	if m.File != "" {
		return m.File
	}
	return "<poly>"
}

// MapUnitLine maps a 1-based line of the unit source (alias prefix already
// subtracted by the caller) to a .poly line. Returns ok=false for generated
// glue lines (shims, ABI markers, injected aliases) that map to nothing.
func (m *SourceMap) MapUnitLine(unitLine int) (int, bool) {
	if m == nil || len(m.Entries) == 0 || unitLine < 1 {
		return 0, false
	}
	// Entries are kept sorted by UnitLine; find the last entry at or before
	// unitLine and check the item's extent covers it.
	idx := sort.Search(len(m.Entries), func(i int) bool {
		return m.Entries[i].UnitLine > unitLine
	}) - 1
	if idx < 0 {
		return 0, false
	}
	e := m.Entries[idx]
	if unitLine >= e.UnitLine+e.Lines {
		return 0, false
	}
	return e.PolyLine + (unitLine - e.UnitLine), true
}

// rustc JSON diagnostic shapes (the subset we consume from
// `cargo build --message-format=json`).
type rustcSpan struct {
	FileName  string `json:"file_name"`
	LineStart int    `json:"line_start"`
	IsPrimary bool   `json:"is_primary"`
}

type rustcDiagnostic struct {
	Message  string      `json:"message"`
	Level    string      `json:"level"`
	Spans    []rustcSpan `json:"spans"`
	Rendered string      `json:"rendered"`
}

type cargoJSONLine struct {
	Reason  string           `json:"reason"`
	Message *rustcDiagnostic `json:"message"`
}

// libRsLocRE matches `src/lib.rs:LINE[:COL]` coordinates (with any path
// prefix, e.g. `units/u0123abcd/src/lib.rs`) in rendered rustc output.
var libRsLocRE = regexp.MustCompile(`((?:[\w./~-]+/)?src/lib\.rs):(\d+)(?::(\d+))?`)

// renderMappedCompileError turns `cargo build --message-format=json` output
// for a failed unit build into a diagnostic that points at the original
// .poly coordinates. It leads with one `<file>:<line>: <message>` header per
// error whose primary span maps, followed by the rustc-rendered snippets
// with every mappable `--> src/lib.rs:N` coordinate rewritten (unmappable
// ones are kept and tagged as generated glue — naming the generated
// boundary item when `unitSource` identifies one). Non-JSON stdout lines are
// skipped; when no compiler errors parse at all (cargo manifest/network
// failures), the raw human stderr is returned unchanged.
func renderMappedCompileError(stdout, stderr string, smap *SourceMap, aliasLines int, unitSource string) string {
	var headers []string
	var blocks []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var msg cargoJSONLine
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Reason != "compiler-message" || msg.Message == nil {
			continue
		}
		d := msg.Message
		if d.Level != "error" || strings.HasPrefix(d.Message, "aborting due to") {
			continue
		}
		headers = append(headers, mappedErrorHeader(d, smap, aliasLines, unitSource))
		if d.Rendered != "" {
			blocks = append(blocks, rewriteRenderedDiagnostic(d.Rendered, smap, aliasLines))
		}
	}
	if len(headers) == 0 {
		// Nothing parsed (cargo's own failure, not rustc's): current behavior.
		out := strings.TrimSpace(stderr)
		if out == "" {
			out = strings.TrimSpace(stdout)
		}
		return out
	}
	out := strings.Join(headers, "\n")
	if len(blocks) > 0 {
		out += "\n\n" + strings.TrimSpace(strings.Join(blocks, "\n"))
	}
	return out
}

// mappedErrorHeader builds the leading one-line location for one error:
// `<poly_file>:<poly_line>: <message>` when the primary span maps, a
// generated-glue note when it does not, and the bare message when the error
// has no span in the unit at all.
func mappedErrorHeader(d *rustcDiagnostic, smap *SourceMap, aliasLines int, unitSource string) string {
	for _, sp := range d.Spans {
		if !sp.IsPrimary || !strings.HasSuffix(sp.FileName, "src/lib.rs") {
			continue
		}
		if poly, ok := smap.MapUnitLine(sp.LineStart - aliasLines); ok {
			return fmt.Sprintf("%s:%d: %s", smap.fileLabel(), poly, d.Message)
		}
		if note := glueContext(unitSource, sp.LineStart, smap, aliasLines); note != "" {
			return fmt.Sprintf("%s (in generated glue at src/lib.rs:%d — %s)", d.Message, sp.LineStart, note)
		}
		return fmt.Sprintf("%s (in generated glue at src/lib.rs:%d)", d.Message, sp.LineStart)
	}
	return d.Message
}

// Generated boundary-glue item heads (docs/rust-boundary-generics.md): the
// Tier-3 Dyn instantiation wrapper, the Tier-2 dispatcher and its probe
// tokens, Tier-1 per-call-site stamps, and the owned-data export adapter.
var glueItemRE = regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?(?:fn\s+(__omnivm_\w+)|(?:struct|trait|impl(?:<\w+>)?)\s+.*\b__Omnivm(?:Probe|Hit|Miss)_(\w+))`)

// glueContext names the generated boundary item enclosing an unmappable
// glue line, so a Tier-1 stamp that fails the fn's bounds (a real type
// error at the call site) reads as what it is instead of an anonymous glue
// coordinate. `errLine` is 1-based into the FULL built source (alias lines
// included). Verbatim (.poly-mapped) lines reset the context.
func glueContext(unitSource string, errLine int, smap *SourceMap, aliasLines int) string {
	if unitSource == "" || errLine < 1 {
		return ""
	}
	lines := strings.Split(unitSource, "\n")
	if errLine > len(lines) {
		return ""
	}
	context := ""
	for i := 1; i <= errLine; i++ {
		if _, ok := smap.MapUnitLine(i - aliasLines); ok {
			context = ""
			continue
		}
		m := glueItemRE.FindStringSubmatch(lines[i-1])
		if m == nil {
			continue
		}
		switch name := m[1]; {
		case m[2] != "":
			context = fmt.Sprintf("the Tier-2 boundary dispatcher for fn '%s'", m[2])
		case strings.HasPrefix(name, "__omnivm_dispatch_"):
			context = fmt.Sprintf("the Tier-2 boundary dispatcher for fn '%s'", strings.TrimPrefix(name, "__omnivm_dispatch_"))
		case strings.HasPrefix(name, "__omnivm_dyn_"):
			context = fmt.Sprintf("the Dyn instantiation wrapper for fn '%s'", strings.TrimPrefix(name, "__omnivm_dyn_"))
		case strings.HasPrefix(name, "__omnivm_export_"):
			context = fmt.Sprintf("the owned-data export adapter for fn '%s'", strings.TrimPrefix(name, "__omnivm_export_"))
		default:
			if idx := strings.LastIndex(name, "__"); idx > len("__omnivm_") {
				fn := name[len("__omnivm_"):idx]
				context = fmt.Sprintf("the per-call-site stamp '%s' of fn '%s' — the instantiation at one call site fails these bounds; annotate or adjust that call", name, fn)
			} else {
				context = fmt.Sprintf("generated wrapper '%s'", name)
			}
		}
	}
	return context
}

// rewriteRenderedDiagnostic rewrites every `src/lib.rs:N[:C]` coordinate in a
// rustc-rendered snippet to the mapped .poly coordinate; coordinates landing
// in generated glue stay as-is with a note appended.
func rewriteRenderedDiagnostic(rendered string, smap *SourceMap, aliasLines int) string {
	return libRsLocRE.ReplaceAllStringFunc(rendered, func(loc string) string {
		parts := libRsLocRE.FindStringSubmatch(loc)
		line, err := strconv.Atoi(parts[2])
		if err != nil {
			return loc
		}
		poly, ok := smap.MapUnitLine(line - aliasLines)
		if !ok {
			return loc + " (generated glue)"
		}
		if parts[3] != "" {
			return fmt.Sprintf("%s:%d:%s", smap.fileLabel(), poly, parts[3])
		}
		return fmt.Sprintf("%s:%d", smap.fileLabel(), poly)
	})
}
