package manifest

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// BoundaryDecisionReport is the static preflight view used by doctor mode.
// It reports boundary choices that can be known before guest runtimes execute.
type BoundaryDecisionReport struct {
	Valid          bool               `json:"valid"`
	Diagnostics    []*Diagnostic      `json:"diagnostics,omitempty"`
	Decisions      []BoundaryDecision `json:"decisions"`
	RiskyFallbacks []BoundaryDecision `json:"riskyFallbacks,omitempty"`
}

// BoundaryDecision describes one manifest boundary or adapter decision.
type BoundaryDecision struct {
	Path     string       `json:"path"`
	Kind     string       `json:"kind"`
	Binding  string       `json:"binding,omitempty"`
	From     string       `json:"from,omitempty"`
	To       string       `json:"to,omitempty"`
	Form     BoundaryForm `json:"form,omitempty"`
	Explicit bool         `json:"explicit,omitempty"`
	Fallback bool         `json:"fallback,omitempty"`
	Adapter  string       `json:"adapter,omitempty"`
	Reason   string       `json:"reason"`
}

type doctorBinding struct {
	Runtime string
	Kind    string
}

// VerifyManifestBoundaries validates the manifest and reports static boundary
// decisions without initializing or calling any guest runtime.
func VerifyManifestBoundaries(m *Manifest) (*BoundaryDecisionReport, error) {
	if err := ValidateManifest(m); err != nil {
		return nil, err
	}

	v := &manifestBoundaryVerifier{
		report:   &BoundaryDecisionReport{Valid: true, Diagnostics: m.Diagnostics},
		bindings: make(map[string]doctorBinding),
		bridges:  buildBridgeIndex(m.Bridges),
	}
	for i, op := range m.Ops {
		v.visitOp(fmt.Sprintf("ops[%d]", i), op, m.DefaultRuntime)
	}
	return v.report, nil
}

// FormatBoundaryDecisionReport renders a stable human-readable doctor report.
func FormatBoundaryDecisionReport(report *BoundaryDecisionReport) string {
	if report == nil {
		return "omnivm doctor: no report\n"
	}
	var out bytes.Buffer
	status := "ok"
	if !report.Valid {
		status = "invalid"
	}
	fmt.Fprintf(&out, "omnivm doctor: %s\n", status)
	fmt.Fprintf(&out, "decisions: %d\n", len(report.Decisions))
	if len(report.RiskyFallbacks) > 0 {
		fmt.Fprintf(&out, "risky fallbacks: %d\n", len(report.RiskyFallbacks))
	}
	if len(report.Diagnostics) > 0 {
		fmt.Fprintf(&out, "compiler diagnostics: %d\n", len(report.Diagnostics))
		for _, d := range report.Diagnostics {
			if d == nil {
				continue
			}
			fmt.Fprintf(&out, "- diagnostic %s %s: %s\n", d.Severity, d.Code, d.Message)
		}
	}
	for _, d := range report.Decisions {
		fmt.Fprintf(&out, "- %s kind=%s", d.Path, d.Kind)
		if d.Binding != "" {
			fmt.Fprintf(&out, " binding=%s", d.Binding)
		}
		if d.From != "" || d.To != "" {
			fmt.Fprintf(&out, " %s->%s", emptyAsUnknown(d.From), emptyAsUnknown(d.To))
		}
		if d.Form != "" {
			fmt.Fprintf(&out, " form=%s", d.Form)
		}
		if d.Adapter != "" {
			fmt.Fprintf(&out, " adapter=%s", d.Adapter)
		}
		if d.Explicit {
			fmt.Fprint(&out, " explicit")
		}
		if d.Fallback {
			fmt.Fprint(&out, " fallback")
		}
		if d.Reason != "" {
			fmt.Fprintf(&out, " reason=%s", d.Reason)
		}
		fmt.Fprintln(&out)
	}
	return out.String()
}

type manifestBoundaryVerifier struct {
	report   *BoundaryDecisionReport
	bindings map[string]doctorBinding
	bridges  map[string][]*BridgeOp
}

func (v *manifestBoundaryVerifier) visitOp(path string, op *Op, defaultRuntime string) {
	if op == nil {
		return
	}
	runtimeName := nonEmpty(op.Runtime, defaultRuntime)
	if op.BodyRuntime != "" {
		runtimeName = op.BodyRuntime
	}

	v.recordOpBoundary(path, op, runtimeName)
	v.recordCaptures(path, op, runtimeName)
	v.recordParams(path, op, runtimeName)
	v.recordBind(op, runtimeName)

	for i, child := range op.Body {
		v.visitOp(fmt.Sprintf("%s.body[%d]", path, i), child, nonEmpty(op.BodyRuntime, runtimeName))
	}
	for i, arm := range op.Arms {
		if arm == nil {
			continue
		}
		for j, child := range arm.Body {
			v.visitOp(fmt.Sprintf("%s.arms[%d].body[%d]", path, i, j), child, runtimeName)
		}
	}
	for i, child := range op.ElseBody {
		v.visitOp(fmt.Sprintf("%s.elseBody[%d]", path, i), child, runtimeName)
	}
	for i, c := range op.Catches {
		if c == nil {
			continue
		}
		catchRuntime := nonEmpty(c.Runtime, runtimeName)
		for j, child := range c.Body {
			v.visitOp(fmt.Sprintf("%s.catches[%d].body[%d]", path, i, j), child, catchRuntime)
		}
	}
	for i, child := range op.FinallyBody {
		v.visitOp(fmt.Sprintf("%s.finallyBody[%d]", path, i), child, runtimeName)
	}
	for i, child := range op.Branches {
		v.visitOp(fmt.Sprintf("%s.branches[%d]", path, i), child, runtimeName)
	}
	if op.From != nil {
		v.visitOp(path+".from", op.From, runtimeName)
	}
}

func (v *manifestBoundaryVerifier) recordOpBoundary(path string, op *Op, runtimeName string) {
	switch op.OpType {
	case "resource":
		if op.Action == "open" && op.Bind != "" {
			v.add(BoundaryDecision{Path: path, Kind: "ref", Binding: op.Bind, From: runtimeName, Form: BoundaryRef, Explicit: true, Reason: "resource handle stays runtime-owned"})
		}
	case "job":
		if op.Action == "enqueue" && op.Bind != "" {
			v.add(BoundaryDecision{Path: path, Kind: "ref", Binding: op.Bind, From: runtimeName, Form: BoundaryRef, Explicit: true, Reason: "job handle crosses by reference"})
		}
	case "table":
		if op.Action == "export" && op.Bind != "" {
			form := BoundaryRef
			reason := "table handle crosses by reference"
			if strings.HasPrefix(op.Format, "arrow") {
				form = BoundaryArrow
				reason = "Arrow-compatible table handle"
			}
			v.add(BoundaryDecision{Path: path, Kind: "arrow", Binding: op.Bind, From: runtimeName, Form: form, Explicit: true, Reason: reason})
		}
	case "chan":
		if op.Action == "make" {
			binding := op.Bind
			if binding == "" {
				binding = op.Channel
			}
			if binding != "" {
				v.add(BoundaryDecision{Path: path, Kind: "stream", Binding: binding, From: runtimeName, Form: BoundaryStream, Explicit: true, Reason: "manifest channel is a lazy stream handle"})
			}
		}
	}
}

func (v *manifestBoundaryVerifier) recordCaptures(path string, op *Op, to string) {
	if len(op.Captures) == 0 {
		return
	}
	names := make([]string, 0, len(op.Captures))
	for name := range op.Captures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, varName := range names {
		bindingName := op.Captures[varName]
		source, ok := v.bindings[bindingName]
		decision := BoundaryDecision{Path: path + ".captures." + varName, Kind: "capture", Binding: bindingName, To: to}
		if ok {
			decision.From = source.Runtime
			switch source.Kind {
			case "resource", "job":
				decision.Form = BoundaryRef
				decision.Explicit = true
				decision.Reason = source.Kind + " binding crosses as runtime ref"
			case "table":
				decision.Form = BoundaryArrow
				decision.Explicit = true
				decision.Reason = "table binding crosses through Arrow/table descriptor"
			case "channel":
				decision.Form = BoundaryStream
				decision.Explicit = true
				decision.Reason = "channel binding crosses as lazy stream"
			case "import":
				decision.Form = BoundaryRef
				decision.Explicit = true
				decision.Reason = "runtime import stays runtime-local"
			default:
				decision = v.captureDecisionFromBridge(decision, source.Runtime, to)
			}
		} else {
			decision = v.captureDecisionFromBridge(decision, "", to)
		}
		v.add(decision)
	}
}

func (v *manifestBoundaryVerifier) captureDecisionFromBridge(decision BoundaryDecision, from, to string) BoundaryDecision {
	decision.From = from
	if from != "" && from == to {
		decision.Form = BoundaryRef
		decision.Explicit = true
		decision.Reason = "same-runtime binding stays as local reference"
		return decision
	}

	ops := v.bridges[bridgeKey(decision.Binding, from, to)]
	if len(ops) == 0 && from == "" {
		for key, candidates := range v.bridges {
			parts := strings.Split(key, "|")
			if len(parts) == 3 && parts[0] == decision.Binding && parts[2] == to {
				ops = candidates
				decision.From = parts[1]
				break
			}
		}
	}
	if form := boundaryFormFromBridgeOps(ops); form != "" {
		decision.Form = form
		decision.Explicit = true
		decision.Reason = "manifest bridge op"
		return decision
	}

	decision.Form = BoundaryJSONFallback
	decision.Fallback = true
	decision.Reason = "no static bridge or source handle evidence; runtime may use JSON copy fallback"
	return decision
}

func (v *manifestBoundaryVerifier) recordParams(path string, op *Op, runtimeName string) {
	if op.OpType != "func_def" {
		return
	}
	for i, param := range op.Params {
		if param == nil || param.CallableShape == nil {
			continue
		}
		adapter := doctorCallableShapeSummary(param.CallableShape)
		if adapter == "" {
			continue
		}
		v.add(BoundaryDecision{
			Path:     fmt.Sprintf("%s.params[%d]", path, i),
			Kind:     "kwargs_adapter",
			Binding:  param.Name,
			To:       runtimeName,
			Form:     BoundaryRef,
			Explicit: true,
			Adapter:  adapter,
			Reason:   "callable shape supports keyword argument adapter",
		})
	}
}

func (v *manifestBoundaryVerifier) recordBind(op *Op, runtimeName string) {
	kind := "value"
	switch op.OpType {
	case "resource":
		if op.Action != "open" {
			return
		}
		kind = "resource"
	case "table":
		if op.Action != "export" {
			return
		}
		kind = "table"
	case "job":
		if op.Action != "enqueue" {
			return
		}
		kind = "job"
	case "chan":
		if op.Action != "make" {
			return
		}
		kind = "channel"
	case "import":
		kind = "import"
	}
	if op.Bind != "" {
		v.bindings[op.Bind] = doctorBinding{Runtime: runtimeName, Kind: kind}
	}
	if op.OpType == "import" && op.DefaultImport != "" {
		v.bindings[op.DefaultImport] = doctorBinding{Runtime: runtimeName, Kind: kind}
	}
	for _, spec := range op.Specifiers {
		if spec != nil && spec.Local != "" {
			v.bindings[spec.Local] = doctorBinding{Runtime: runtimeName, Kind: kind}
		}
	}
}

func (v *manifestBoundaryVerifier) add(decision BoundaryDecision) {
	v.report.Decisions = append(v.report.Decisions, decision)
	if decision.Fallback {
		v.report.RiskyFallbacks = append(v.report.RiskyFallbacks, decision)
	}
}

func doctorCallableShapeSummary(shape *CallableShape) string {
	parts := make([]string, 0, 4)
	if shape.AcceptsKwargs {
		parts = append(parts, "kwargs")
	}
	if shape.AcceptsOptionsObject {
		parts = append(parts, "options_object")
	}
	if shape.JavaAdapter != nil {
		parts = append(parts, "java_"+shape.JavaAdapter.Kind)
	}
	if len(shape.DestructuredKeys) > 0 {
		keys := append([]string(nil), shape.DestructuredKeys...)
		sort.Strings(keys)
		parts = append(parts, "keys="+strings.Join(keys, ","))
	}
	return strings.Join(parts, ";")
}

func emptyAsUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
