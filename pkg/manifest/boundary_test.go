package manifest

import "testing"

func TestClassifySerializedBoundary(t *testing.T) {
	cases := []struct {
		name     string
		jsonVal  string
		ops      []*BridgeOp
		form     BoundaryForm
		explicit bool
		fallback bool
	}{
		{name: "primitive", jsonVal: "42", form: BoundaryCopy},
		{name: "string", jsonVal: `"hello"`, form: BoundaryCopy},
		{name: "null", jsonVal: "null", form: BoundaryCopy},
		{name: "object fallback", jsonVal: `{"items":[1,2,3]}`, form: BoundaryJSONFallback, fallback: true},
		{name: "array fallback", jsonVal: `[1,2,3]`, form: BoundaryJSONFallback, fallback: true},
		{name: "empty fallback", jsonVal: "", form: BoundaryJSONFallback, fallback: true},
		{name: "arrow bridge", jsonVal: `[1,2,3]`, ops: []*BridgeOp{{Op: "share_memory"}}, form: BoundaryArrow, explicit: true},
		{name: "stream bridge", jsonVal: `[1,2,3]`, ops: []*BridgeOp{{Op: "stream_proxy"}}, form: BoundaryStream, explicit: true},
		{name: "ref bridge", jsonVal: `{"callable":true}`, ops: []*BridgeOp{{Op: "proxy_with_finalizer"}}, form: BoundaryRef, explicit: true},
		{name: "typed bridge", jsonVal: `7`, ops: []*BridgeOp{{Op: "narrow"}}, form: BoundaryCopy, explicit: true},
		{
			name:    "compose keeps strongest bridge form",
			jsonVal: `[1,2,3]`,
			ops: []*BridgeOp{{
				Op:   "compose",
				Meta: map[string]interface{}{"steps": []interface{}{"unwrap_result", "share_memory"}},
			}},
			form:     BoundaryArrow,
			explicit: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySerializedBoundary(tc.jsonVal, tc.ops)
			if got.Form != tc.form || got.Explicit != tc.explicit || got.Fallback != tc.fallback {
				t.Fatalf("decision = %+v, want form=%s explicit=%v fallback=%v", got, tc.form, tc.explicit, tc.fallback)
			}
		})
	}
}

func TestClassifyLocalCaptureBoundary(t *testing.T) {
	cases := []struct {
		name string
		val  interface{}
		form BoundaryForm
	}{
		{name: "primitive", val: 42, form: BoundaryCopy},
		{name: "resource", val: &ResourceRef{Runtime: "python", Kind: "file"}, form: BoundaryRef},
		{name: "job", val: &JobHandle{Runtime: "javascript", Kind: "task"}, form: BoundaryRef},
		{name: "table", val: &TableRef{Runtime: "python", Format: "table"}, form: BoundaryRef},
		{name: "arrow table", val: &TableRef{Runtime: "python", Format: "arrow.c.data"}, form: BoundaryArrow},
		{name: "channel", val: &ChanRef{}, form: BoundaryStream},
		{name: "byte buffer", val: []byte("payload"), form: BoundaryArrow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLocalCaptureBoundary(tc.val)
			if got.Form != tc.form {
				t.Fatalf("form = %s, want %s", got.Form, tc.form)
			}
		})
	}
}
