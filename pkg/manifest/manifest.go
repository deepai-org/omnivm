// Package manifest implements a JSON dispatch manifest executor for OmniVM.
//
// A manifest is a structured IR where a PolyScript compiler emits ops that
// OmniVM dispatches to Python, JavaScript, Ruby, Java, and Go runtimes.
package manifest

import "encoding/json"

// Manifest is the top-level structure of a dispatch manifest.
type Manifest struct {
	Version        int    `json:"version"`
	DefaultRuntime string `json:"defaultRuntime"`
	Ops            []*Op  `json:"ops"`
}

// Op represents a single operation in the manifest.
type Op struct {
	OpType  string `json:"op"`
	Runtime string `json:"runtime"`

	// exec/eval/native
	Code     string            `json:"code,omitempty"`
	Bind     string            `json:"bind,omitempty"`
	Captures map[string]string `json:"captures,omitempty"`
	Async    bool              `json:"async,omitempty"`

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
	Mode string    `json:"mode,omitempty"`
	Test *CondExpr `json:"test,omitempty"`

	// concat
	Segments []*ConcatSegment `json:"segments,omitempty"`

	// declare
	Mutable bool `json:"mutable,omitempty"`

	// assign
	Target   string `json:"target,omitempty"`
	Operator string `json:"operator,omitempty"`

	// parallel
	Branches []*Op `json:"branches,omitempty"`

	// exec_compiled/eval_compiled
	Lang string `json:"lang,omitempty"`

	// eval with runtime:"go"
	Func string `json:"func,omitempty"`
	Args []interface{} `json:"args,omitempty"`
}

// Param represents a function parameter in a func_def.
type Param struct {
	Name         string      `json:"name"`
	Spread       bool        `json:"spread,omitempty"`
	DefaultValue interface{} `json:"defaultValue,omitempty"`
}

// IfArm represents a single condition+body branch in an if op.
type IfArm struct {
	Test *CondExpr `json:"test"`
	Body []*Op     `json:"body"`
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
	return &m, nil
}
