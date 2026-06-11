package manifest

import (
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// countingInjectionRuntime returns a scripted runtime that records every
// capture-injection execute mentioning the given binding name.
func countingInjectionRuntime(name, binding string, injected *[]string) *scriptedRuntime {
	return &scriptedRuntime{name: name, eval: func(code string) pkg.Result {
		if strings.Contains(code, "__omnivm_materialize_capture") && strings.Contains(code, binding) {
			*injected = append(*injected, code)
		}
		return pkg.Result{}
	}}
}

// TestAutoInjectDedupUnchangedBinding: two consecutive native ops referencing
// the same unchanged plain-value binding inject it once — the second op skips
// re-injection because the binding's version is already live in the runtime.
// Rebinding through the manifest bumps the version and re-injects.
func TestAutoInjectDedupUnchangedBinding(t *testing.T) {
	var injected []string
	py := countingInjectionRuntime("python", "cfg", &injected)
	e := NewExecutor(map[string]pkg.Runtime{"python": py})
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "declare", Bind: "cfg", Value: &ValueExpr{Kind: "literal", Value: "v1"}},
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
	}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(injected) != 1 {
		t.Fatalf("unchanged binding injected %d times, want 1:\n%s",
			len(injected), strings.Join(injected, "\n---\n"))
	}

	// Mutating the binding through the manifest re-injects exactly once more.
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "assign", Target: "cfg", Value: &ValueExpr{Kind: "literal", Value: "v2"}},
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
	}}); err != nil {
		t.Fatalf("Execute after assign: %v", err)
	}
	if len(injected) != 2 {
		t.Fatalf("after manifest mutation, total injections = %d, want 2:\n%s",
			len(injected), strings.Join(injected, "\n---\n"))
	}
	if !strings.Contains(injected[1], "v2") {
		t.Fatalf("re-injection does not carry the mutated value: %.300q", injected[1])
	}
}

// TestAutoInjectDedupScopeShadowing: a scope-local shadow injects its value;
// after the scope pops, the outer value must re-inject (the pop bumps the
// name's version, invalidating the inner copy's record).
func TestAutoInjectDedupScopeShadowing(t *testing.T) {
	var injected []string
	py := countingInjectionRuntime("python", "cfg", &injected)
	e := NewExecutor(map[string]pkg.Runtime{"python": py})
	e.setBinding("cfg", "outer")
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
	}}); err != nil {
		t.Fatalf("Execute outer: %v", err)
	}
	e.pushScope()
	e.setBinding("cfg", "inner")
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
	}}); err != nil {
		t.Fatalf("Execute inner: %v", err)
	}
	e.popScope()
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
	}}); err != nil {
		t.Fatalf("Execute after pop: %v", err)
	}
	if len(injected) != 3 {
		t.Fatalf("shadow sequence injections = %d, want 3 (outer, inner, outer again):\n%s",
			len(injected), strings.Join(injected, "\n---\n"))
	}
	if !strings.Contains(injected[1], "inner") || !strings.Contains(injected[2], "outer") {
		t.Fatalf("wrong values crossed: inner=%.120q after-pop=%.120q", injected[1], injected[2])
	}
}

// TestAutoInjectDedupPerRuntimeIsolation: an injection into one runtime must
// not suppress the first injection into another.
func TestAutoInjectDedupPerRuntimeIsolation(t *testing.T) {
	var pyInjected, jsInjected []string
	py := countingInjectionRuntime("python", "cfg", &pyInjected)
	js := countingInjectionRuntime("javascript", "cfg", &jsInjected)
	e := NewExecutor(map[string]pkg.Runtime{"python": py, "javascript": js})
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "declare", Bind: "cfg", Value: &ValueExpr{Kind: "literal", Value: "shared"}},
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
		{OpType: "exec", Runtime: "javascript", Code: "console.log(cfg)"},
		{OpType: "exec", Runtime: "python", Code: "print(cfg)"},
		{OpType: "exec", Runtime: "javascript", Code: "console.log(cfg)"},
	}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(pyInjected) != 1 || len(jsInjected) != 1 {
		t.Fatalf("per-runtime injections = python:%d javascript:%d, want 1/1",
			len(pyInjected), len(jsInjected))
	}
}
