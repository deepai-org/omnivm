package manifest

import (
	"fmt"
	"strings"
)

var supportedRuntimes = map[string]bool{
	"python":     true,
	"javascript": true,
	"ruby":       true,
	"java":       true,
	"go":         true,
}

var supportedOps = map[string]bool{
	"exec":          true,
	"eval":          true,
	"native":        true,
	"import":        true,
	"func_def":      true,
	"return":        true,
	"if":            true,
	"loop":          true,
	"concat":        true,
	"declare":       true,
	"assign":        true,
	"try":           true,
	"throw":         true,
	"parallel":      true,
	"chan":          true,
	"select":        true,
	"spawn":         true,
	"yield":         true,
	"await":         true,
	"resource":      true,
	"table":         true,
	"job":           true,
	"exec_compiled": true,
	"eval_compiled": true,
	"call_typed":    true,
}

// ValidateManifest checks the structural contract that the manifest executor
// relies on. It intentionally validates shape, not semantic binding liveness;
// liveness errors still belong to execution so dynamic manifests can work.
func ValidateManifest(m *Manifest) error {
	if m == nil {
		return fmt.Errorf("manifest is nil")
	}
	if m.Version != 1 {
		return fmt.Errorf("manifest.version: unsupported version %d", m.Version)
	}
	if m.DefaultRuntime == "" {
		return fmt.Errorf("manifest.defaultRuntime: required")
	}
	if err := validateRuntime("manifest.defaultRuntime", m.DefaultRuntime); err != nil {
		return err
	}
	for i, op := range m.Ops {
		if err := validateOp(fmt.Sprintf("ops[%d]", i), op); err != nil {
			return err
		}
	}
	return nil
}

func validateOp(path string, op *Op) error {
	if op == nil {
		return fmt.Errorf("%s: op is null", path)
	}
	if op.OpType == "" {
		return fmt.Errorf("%s.op: required", path)
	}
	if !supportedOps[op.OpType] {
		return fmt.Errorf("%s.op: unsupported op %q", path, op.OpType)
	}
	if op.Runtime != "" {
		if err := validateRuntime(path+".runtime", op.Runtime); err != nil {
			return err
		}
	}

	switch op.OpType {
	case "exec", "native":
		if op.Code == "" && op.Func == "" {
			return fmt.Errorf("%s: %s requires code or func", path, op.OpType)
		}
	case "eval":
		if op.Code == "" && op.Func == "" {
			return fmt.Errorf("%s: eval requires code or func", path)
		}
	case "import":
		if op.Path == "" {
			return fmt.Errorf("%s.path: import requires path", path)
		}
	case "func_def":
		if op.Name == "" {
			return fmt.Errorf("%s.name: func_def requires name", path)
		}
		if op.BodyRuntime != "" {
			if err := validateRuntime(path+".bodyRuntime", op.BodyRuntime); err != nil {
				return err
			}
		}
		for i, param := range op.Params {
			if param == nil || param.Name == "" {
				return fmt.Errorf("%s.params[%d].name: required", path, i)
			}
		}
		for i, child := range op.Body {
			if err := validateOp(fmt.Sprintf("%s.body[%d]", path, i), child); err != nil {
				return err
			}
		}
	case "return":
		if op.From != nil {
			if err := validateOp(path+".from", op.From); err != nil {
				return err
			}
		}
		if op.Value != nil {
			if err := validateValueExpr(path+".value", op.Value); err != nil {
				return err
			}
		}
	case "if":
		if len(op.Arms) == 0 {
			return fmt.Errorf("%s.arms: if requires at least one arm", path)
		}
		for i, arm := range op.Arms {
			if arm == nil {
				return fmt.Errorf("%s.arms[%d]: arm is null", path, i)
			}
			if arm.Test == nil {
				return fmt.Errorf("%s.arms[%d].test: required", path, i)
			}
			for j, child := range arm.Body {
				if err := validateOp(fmt.Sprintf("%s.arms[%d].body[%d]", path, i, j), child); err != nil {
					return err
				}
			}
		}
		for i, child := range op.ElseBody {
			if err := validateOp(fmt.Sprintf("%s.elseBody[%d]", path, i), child); err != nil {
				return err
			}
		}
	case "loop":
		if op.Mode == "" {
			return fmt.Errorf("%s.mode: loop requires mode", path)
		}
		for i, child := range op.Body {
			if err := validateOp(fmt.Sprintf("%s.body[%d]", path, i), child); err != nil {
				return err
			}
		}
	case "declare":
		if op.Bind == "" {
			return fmt.Errorf("%s.bind: declare requires bind", path)
		}
		if op.Value != nil {
			if err := validateValueExpr(path+".value", op.Value); err != nil {
				return err
			}
		}
		if op.From != nil {
			if err := validateOp(path+".from", op.From); err != nil {
				return err
			}
		}
	case "assign":
		if op.Target == "" {
			return fmt.Errorf("%s.target: assign requires target", path)
		}
		if op.Operator == "" {
			return fmt.Errorf("%s.operator: assign requires operator", path)
		}
		if op.Value != nil {
			if err := validateValueExpr(path+".value", op.Value); err != nil {
				return err
			}
		}
		if op.From != nil {
			if err := validateOp(path+".from", op.From); err != nil {
				return err
			}
		}
	case "try":
		for i, child := range op.Body {
			if err := validateOp(fmt.Sprintf("%s.body[%d]", path, i), child); err != nil {
				return err
			}
		}
		for i, c := range op.Catches {
			if c == nil {
				return fmt.Errorf("%s.catches[%d]: catch is null", path, i)
			}
			if c.Runtime != "" {
				if err := validateRuntime(fmt.Sprintf("%s.catches[%d].runtime", path, i), c.Runtime); err != nil {
					return err
				}
			}
			for j, child := range c.Body {
				if err := validateOp(fmt.Sprintf("%s.catches[%d].body[%d]", path, i, j), child); err != nil {
					return err
				}
			}
		}
		for i, child := range op.FinallyBody {
			if err := validateOp(fmt.Sprintf("%s.finallyBody[%d]", path, i), child); err != nil {
				return err
			}
		}
	case "throw":
		if op.Value == nil {
			return fmt.Errorf("%s.value: throw requires value", path)
		}
		return validateValueExpr(path+".value", op.Value)
	case "parallel":
		if len(op.Branches) == 0 {
			return fmt.Errorf("%s.branches: parallel requires at least one branch", path)
		}
		for i, child := range op.Branches {
			if err := validateParallelBranch(fmt.Sprintf("%s.branches[%d]", path, i), child); err != nil {
				return err
			}
		}
	case "chan":
		return validateChanOp(path, op)
	case "select":
		if len(op.Cases) == 0 {
			return fmt.Errorf("%s.cases: select requires at least one case", path)
		}
		for i, c := range op.Cases {
			if c == nil {
				return fmt.Errorf("%s.cases[%d]: case is null", path, i)
			}
			if c.Action != "send" && c.Action != "recv" {
				return fmt.Errorf("%s.cases[%d].action: expected send or recv, got %q", path, i, c.Action)
			}
			if c.Channel == "" {
				return fmt.Errorf("%s.cases[%d].channel: required", path, i)
			}
			if c.Action == "send" && c.Value == nil {
				return fmt.Errorf("%s.cases[%d].value: send case requires value", path, i)
			}
			if c.Value != nil {
				if err := validateValueExpr(fmt.Sprintf("%s.cases[%d].value", path, i), c.Value); err != nil {
					return err
				}
			}
			for j, child := range c.Body {
				if err := validateOp(fmt.Sprintf("%s.cases[%d].body[%d]", path, i, j), child); err != nil {
					return err
				}
			}
		}
		for i, child := range op.DefaultBody {
			if err := validateOp(fmt.Sprintf("%s.defaultBody[%d]", path, i), child); err != nil {
				return err
			}
		}
	case "spawn":
		if op.Runtime == "" {
			return fmt.Errorf("%s.runtime: spawn requires runtime", path)
		}
		if op.Code == "" {
			return fmt.Errorf("%s.code: spawn requires code", path)
		}
	case "await":
		if op.From == nil {
			return fmt.Errorf("%s.from: await requires source eval op", path)
		}
		if err := validateOp(path+".from", op.From); err != nil {
			return err
		}
	case "resource":
		switch op.Action {
		case "open":
			if op.Bind == "" {
				return fmt.Errorf("%s.bind: resource open requires bind", path)
			}
		case "close":
			if op.Target == "" && op.Bind == "" {
				return fmt.Errorf("%s.target: resource close requires target", path)
			}
		default:
			return fmt.Errorf("%s.action: unknown resource action %q", path, op.Action)
		}
	case "table":
		switch op.Action {
		case "export":
			if op.Bind == "" {
				return fmt.Errorf("%s.bind: table export requires bind", path)
			}
			if op.Value != nil {
				if err := validateValueExpr(path+".value", op.Value); err != nil {
					return err
				}
			}
		case "release":
			if op.Target == "" {
				return fmt.Errorf("%s.target: table release requires target", path)
			}
		default:
			return fmt.Errorf("%s.action: unknown table action %q", path, op.Action)
		}
	case "job":
		switch op.Action {
		case "enqueue":
			if op.Bind == "" {
				return fmt.Errorf("%s.bind: job enqueue requires bind", path)
			}
			if op.Payload != nil {
				if err := validateValueExpr(path+".payload", op.Payload); err != nil {
					return err
				}
			}
		case "complete":
			if op.Target == "" {
				return fmt.Errorf("%s.target: job complete requires target", path)
			}
			if op.Value != nil {
				if err := validateValueExpr(path+".value", op.Value); err != nil {
					return err
				}
			}
		case "wait":
			if op.Target == "" {
				return fmt.Errorf("%s.target: job wait requires target", path)
			}
		default:
			return fmt.Errorf("%s.action: unknown job action %q", path, op.Action)
		}
	case "concat":
		if op.Bind == "" {
			return fmt.Errorf("%s.bind: concat requires bind", path)
		}
		if len(op.Segments) == 0 {
			return fmt.Errorf("%s.segments: concat requires at least one segment", path)
		}
		for i, seg := range op.Segments {
			if seg == nil || seg.Kind == "" {
				return fmt.Errorf("%s.segments[%d].kind: required", path, i)
			}
		}
	case "exec_compiled", "eval_compiled":
		if op.Lang == "" {
			return fmt.Errorf("%s.lang: %s requires lang", path, op.OpType)
		}
		if op.Code == "" {
			return fmt.Errorf("%s.code: %s requires code", path, op.OpType)
		}
		if op.OpType == "eval_compiled" && op.Bind == "" {
			return fmt.Errorf("%s.bind: eval_compiled requires bind", path)
		}
	}
	return nil
}

func validateParallelBranch(path string, op *Op) error {
	if op == nil {
		return fmt.Errorf("%s: branch is null", path)
	}
	if op.OpType == "" {
		if op.Code == "" {
			return fmt.Errorf("%s.code: implicit eval branch requires code", path)
		}
		if op.Bind == "" {
			return fmt.Errorf("%s.bind: implicit eval branch requires bind", path)
		}
		if op.Runtime != "" {
			if err := validateRuntime(path+".runtime", op.Runtime); err != nil {
				return err
			}
		}
		return nil
	}
	return validateOp(path, op)
}

func validateRuntime(path, runtime string) error {
	if supportedRuntimes[runtime] {
		return nil
	}
	names := make([]string, 0, len(supportedRuntimes))
	for name := range supportedRuntimes {
		names = append(names, name)
	}
	return fmt.Errorf("%s: unsupported runtime %q; supported runtimes: %s", path, runtime, strings.Join(names, ", "))
}

func validateChanOp(path string, op *Op) error {
	switch op.Action {
	case "make":
		return nil
	case "send":
		if op.Channel == "" {
			return fmt.Errorf("%s.channel: chan send requires channel", path)
		}
		if op.Value == nil {
			return fmt.Errorf("%s.value: chan send requires value", path)
		}
		if err := validateValueExpr(path+".value", op.Value); err != nil {
			return err
		}
	case "recv":
		if op.Channel == "" {
			return fmt.Errorf("%s.channel: chan recv requires channel", path)
		}
	case "close":
		if op.Channel == "" {
			return fmt.Errorf("%s.channel: chan close requires channel", path)
		}
	default:
		return fmt.Errorf("%s.action: unknown chan action %q", path, op.Action)
	}
	return nil
}

func validateValueExpr(path string, value *ValueExpr) error {
	if value == nil {
		return fmt.Errorf("%s: value is null", path)
	}
	switch value.Kind {
	case "literal":
		return nil
	case "ref":
		if value.Name == "" {
			return fmt.Errorf("%s.name: ref value requires name", path)
		}
		return nil
	default:
		return fmt.Errorf("%s.kind: unknown value kind %q", path, value.Kind)
	}
}
