// Package manifest implements a JSON dispatch manifest executor for OmniVM.
//
// A manifest is a structured IR where a PolyScript compiler emits ops that
// OmniVM dispatches to Python, JavaScript, Ruby, Java, and Go runtimes.
package manifest

import "encoding/json"

// Manifest is the top-level structure of a dispatch manifest.
type Manifest struct {
	Version        int           `json:"version"`
	DefaultRuntime string        `json:"defaultRuntime"`
	Ops            []*Op         `json:"ops"`
	Bridges        []*BridgeOp   `json:"bridges,omitempty"`
	TypeSummary    *TypeSummary  `json:"typeSummary,omitempty"`
	Diagnostics    []*Diagnostic `json:"diagnostics,omitempty"`
}

// Diagnostic is a non-fatal compiler diagnostic carried through the manifest.
type Diagnostic struct {
	Severity string          `json:"severity"`
	Code     string          `json:"code"`
	Message  string          `json:"message"`
	Span     *DiagnosticSpan `json:"span,omitempty"`
}

type DiagnosticSpan struct {
	Start  int `json:"start"`
	End    int `json:"end"`
	Line   int `json:"line,omitempty"`
	Column int `json:"column,omitempty"`
}

// BridgeOp describes a type-aware transformation at a cross-runtime boundary.
// Emitted by the PolyScript compiler's type system.
type BridgeOp struct {
	Binding string                 `json:"binding"`
	Op      string                 `json:"op"`
	From    string                 `json:"from"`
	To      string                 `json:"to"`
	Meta    map[string]interface{} `json:"meta,omitempty"`
}

// TypeSummary is a diagnostic summary of cross-runtime type crossings.
type TypeSummary struct {
	Crossings int `json:"crossings"`
	Safe      int `json:"safe"`
	Coerce    int `json:"coerce"`
	Check     int `json:"check"`
	Errors    int `json:"errors"`
}

// TableMetadata carries generic Arrow-compatible bulk data metadata through
// manifest table handles. It is intentionally protocol-shaped rather than
// tied to any producer library.
type TableMetadata struct {
	Dtype       *int32  `json:"dtype,omitempty"`
	ArrowFormat string  `json:"arrow_format,omitempty"`
	Buffer      string  `json:"buffer,omitempty"`
	Shape       []int64 `json:"shape,omitempty"`
	Strides     []int64 `json:"strides,omitempty"`
	Offset      int64   `json:"offset,omitempty"`
	NullCount   *int64  `json:"null_count,omitempty"`
	ReadOnly    bool    `json:"read_only"`
	MemorySpace string  `json:"memory_space,omitempty"`
}

// CallableShape carries compiler- or runtime-probed callable boundary evidence.
// It is advisory: runtimes may use it to choose a safer adapter path, but they
// must keep unsupported cases explicit when shape evidence is absent.
type CallableShape struct {
	AcceptsKwargs        bool                 `json:"acceptsKwargs,omitempty"`
	AcceptsOptionsObject bool                 `json:"acceptsOptionsObject,omitempty"`
	DestructuredKeys     []string             `json:"destructuredKeys,omitempty"`
	ParameterNames       []string             `json:"parameterNames,omitempty"`
	Arity                *int                 `json:"arity,omitempty"`
	JavaAdapter          *JavaCallableAdapter `json:"javaAdapter,omitempty"`
}

// JavaCallableAdapter describes a proven Java named-argument adapter shape.
type JavaCallableAdapter struct {
	Kind       string   `json:"kind,omitempty"`
	Method     string   `json:"method,omitempty"`
	TargetType string   `json:"targetType,omitempty"`
	Keys       []string `json:"keys,omitempty"`
}

// Op represents a single operation in the manifest.
type Op struct {
	OpType  string `json:"op"`
	Runtime string `json:"runtime"`

	// exec/eval/native
	Code      string            `json:"code,omitempty"`
	Bind      string            `json:"bind,omitempty"`
	Captures  map[string]string `json:"captures,omitempty"`
	Async     bool              `json:"async,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Disposer  string            `json:"disposer,omitempty"`
	Payload   *ValueExpr        `json:"payload,omitempty"`
	Format    string            `json:"format,omitempty"`
	Ownership string            `json:"ownership,omitempty"`
	Release   string            `json:"release,omitempty"`
	Metadata  *TableMetadata    `json:"metadata,omitempty"`

	// import
	Path          string `json:"path,omitempty"`
	DefaultImport string `json:"defaultImport,omitempty"`

	// func_def
	Name        string   `json:"name,omitempty"`
	Params      []*Param `json:"params,omitempty"`
	Body        []*Op    `json:"body,omitempty"`
	BodyRuntime string   `json:"bodyRuntime,omitempty"`
	Source      string   `json:"source,omitempty"`
	Exports     []string `json:"exports,omitempty"`
	Requires    []string `json:"requires,omitempty"`

	// return
	From  *Op        `json:"from,omitempty"`
	Value *ValueExpr `json:"value,omitempty"`

	// if
	Arms     []*IfArm `json:"arms,omitempty"`
	ElseBody []*Op    `json:"elseBody,omitempty"`

	// loop
	Mode     string     `json:"mode,omitempty"`
	Await    bool       `json:"await,omitempty"`
	Test     *CondExpr  `json:"test,omitempty"`
	Variable string     `json:"variable,omitempty"` // foreach loop variable
	Iterable *ValueExpr `json:"iterable,omitempty"` // foreach iterable

	// try
	Catches     []*CatchClause `json:"catches,omitempty"`     // try catches
	FinallyBody []*Op          `json:"finallyBody,omitempty"` // try finally

	// concat
	Segments []*ConcatSegment `json:"segments,omitempty"`

	// declare
	Mutable bool `json:"mutable,omitempty"`

	// assign
	Target   string `json:"target,omitempty"`
	Operator string `json:"operator,omitempty"`

	// parallel
	Branches []*Op `json:"branches,omitempty"`

	// chan
	Action  string      `json:"action,omitempty"`
	Channel string      `json:"channel,omitempty"`
	Size    interface{} `json:"size,omitempty"` // chan make buffer size

	// select
	Cases       []*SelectCase `json:"cases,omitempty"`
	DefaultBody []*Op         `json:"defaultBody,omitempty"`

	// func_def generator
	Generator bool `json:"generator,omitempty"`

	// yield
	Delegate bool `json:"delegate,omitempty"`

	// import specifiers
	Specifiers []*ImportSpec `json:"specifiers,omitempty"`

	// exec_compiled/eval_compiled
	Lang string `json:"lang,omitempty"`

	// eval with runtime:"go"
	Func string        `json:"func,omitempty"`
	Args []interface{} `json:"args,omitempty"`
}

// Param represents a function parameter in a func_def.
type Param struct {
	Name          string         `json:"name"`
	Spread        bool           `json:"spread,omitempty"`
	DefaultValue  interface{}    `json:"defaultValue,omitempty"`
	CallableShape *CallableShape `json:"callableShape,omitempty"`
}

// IfArm represents a single condition+body branch in an if op.
type IfArm struct {
	Test *CondExpr `json:"test"`
	Body []*Op     `json:"body"`
}

// CatchClause represents a catch block in a try op.
type CatchClause struct {
	Param   string `json:"param,omitempty"`
	Body    []*Op  `json:"body"`
	Runtime string `json:"runtime,omitempty"` // cross-runtime error bridging
}

// SelectCase represents a case in a select op.
type SelectCase struct {
	Action  string     `json:"action"`
	Channel string     `json:"channel"`
	Value   *ValueExpr `json:"value,omitempty"`
	Body    []*Op      `json:"body"`
}

// ImportSpec represents a named import specifier.
type ImportSpec struct {
	Imported string `json:"imported"`
	Local    string `json:"local"`
}

// CondExpr represents a conditional expression (used in if arms and loop tests).
type CondExpr struct {
	Kind     string            `json:"kind"`
	Runtime  string            `json:"runtime,omitempty"`
	Code     string            `json:"code,omitempty"`
	Name     string            `json:"name,omitempty"`
	Value    interface{}       `json:"value,omitempty"`
	Captures map[string]string `json:"captures,omitempty"`
}

// ConcatSegment represents a piece of a concat operation.
type ConcatSegment struct {
	Kind    string `json:"kind"`
	Value   string `json:"value,omitempty"`
	Name    string `json:"name,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Code    string `json:"code,omitempty"`
}

// ValueExpr represents a value expression (used in return ops).
type ValueExpr struct {
	Kind  string      `json:"kind"`
	Value interface{} `json:"value,omitempty"`
	Name  string      `json:"name,omitempty"`
}

// ParseManifest parses a JSON manifest from raw bytes.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if err := ValidateManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}
