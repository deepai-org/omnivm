package manifest

import "strings"

// BoundaryForm is the runtime-neutral shape used when a value crosses from one
// runtime to another. It is deliberately protocol-shaped, not library-shaped.
type BoundaryForm string

const (
	BoundaryCopy         BoundaryForm = "copy"
	BoundaryRef          BoundaryForm = "ref"
	BoundaryStream       BoundaryForm = "stream"
	BoundaryArrow        BoundaryForm = "arrow"
	BoundaryJSONFallback BoundaryForm = "json_fallback"
)

type boundaryDecision struct {
	Form     BoundaryForm
	Explicit bool
	Fallback bool
	Reason   string
}

func classifyLocalCaptureBoundary(val interface{}) boundaryDecision {
	switch v := val.(type) {
	case *ResourceRef:
		return boundaryDecision{Form: BoundaryRef, Explicit: true, Reason: "runtime-owned resource handle"}
	case *TableRef:
		if strings.HasPrefix(v.Format, "arrow") {
			return boundaryDecision{Form: BoundaryArrow, Explicit: true, Reason: "arrow-compatible table handle"}
		}
		return boundaryDecision{Form: BoundaryRef, Explicit: true, Reason: "table handle"}
	case *JobHandle:
		return boundaryDecision{Form: BoundaryRef, Explicit: true, Reason: "runtime-owned job handle"}
	case *ChanRef:
		return boundaryDecision{Form: BoundaryStream, Explicit: true, Reason: "channel stream handle"}
	case []byte:
		return boundaryDecision{Form: BoundaryArrow, Reason: "contiguous byte buffer"}
	default:
		return boundaryDecision{Form: BoundaryCopy, Reason: "manifest value copy"}
	}
}

func classifySerializedBoundary(jsonVal string, ops []*BridgeOp) boundaryDecision {
	if form := boundaryFormFromBridgeOps(ops); form != "" {
		return boundaryDecision{Form: form, Explicit: true, Reason: "manifest bridge op"}
	}

	trimmed := strings.TrimSpace(jsonVal)
	if trimmed == "" || trimmed == "undefined" {
		return boundaryDecision{
			Form:     BoundaryJSONFallback,
			Fallback: true,
			Reason:   "empty serialized value",
		}
	}
	if trimmed == "null" {
		return boundaryDecision{Form: BoundaryCopy, Reason: "null copy"}
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return boundaryDecision{
			Form:     BoundaryJSONFallback,
			Fallback: true,
			Reason:   "no bridge op for complex or opaque value; using JSON copy fallback",
		}
	}
	return boundaryDecision{Form: BoundaryCopy, Reason: "primitive copy"}
}

func boundaryFormFromBridgeOps(ops []*BridgeOp) BoundaryForm {
	form := BoundaryForm("")
	for _, op := range ops {
		next := boundaryFormFromBridgeOp(op)
		if boundaryFormRank(next) > boundaryFormRank(form) {
			form = next
		}
	}
	return form
}

func boundaryFormFromBridgeOp(op *BridgeOp) BoundaryForm {
	if op == nil {
		return ""
	}
	switch op.Op {
	case "share_memory":
		return BoundaryArrow
	case "stream_proxy", "channel_bridge":
		return BoundaryStream
	case "proxy_callable", "proxy_with_finalizer", "attach_disposer":
		return BoundaryRef
	case "identity", "widen", "narrow", "to_string", "parse_int", "parse_float",
		"serialize", "deserialize", "copy_array", "copy_buffer", "wrap_option",
		"unwrap_option", "wrap_result", "unwrap_result", "throw_typed",
		"catch_to_result", "tag_dispatch", "struct_to_dict", "dict_to_struct",
		"struct_reshape", "await_resolve":
		return BoundaryCopy
	case "compose":
		return boundaryFormFromComposeOp(op)
	default:
		return ""
	}
}

func boundaryFormFromComposeOp(op *BridgeOp) BoundaryForm {
	if op == nil || op.Meta == nil {
		return BoundaryCopy
	}
	steps, ok := op.Meta["steps"].([]interface{})
	if !ok {
		return BoundaryCopy
	}
	form := BoundaryForm("")
	for _, step := range steps {
		stepName, ok := step.(string)
		if !ok {
			continue
		}
		next := boundaryFormFromBridgeOp(&BridgeOp{Op: stepName})
		if boundaryFormRank(next) > boundaryFormRank(form) {
			form = next
		}
	}
	if form == "" {
		return BoundaryCopy
	}
	return form
}

func boundaryFormRank(form BoundaryForm) int {
	switch form {
	case BoundaryArrow:
		return 5
	case BoundaryStream:
		return 4
	case BoundaryRef:
		return 3
	case BoundaryJSONFallback:
		return 2
	case BoundaryCopy:
		return 1
	default:
		return 0
	}
}
