package manifest

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// BridgeError is returned when a bridge op fails at runtime (e.g., narrowing overflow).
type BridgeError struct {
	Op      string
	Binding string
	Detail  string
}

func (e *BridgeError) Error() string {
	return fmt.Sprintf("bridge %s on %q: %s", e.Op, e.Binding, e.Detail)
}

// bridgeKey returns the lookup key for a bridge op: "binding|from|to".
// A single binding can cross multiple boundaries, so from/to are part of the key.
func bridgeKey(binding, from, to string) string {
	return binding + "|" + from + "|" + to
}

// buildBridgeIndex creates the bridge lookup map from manifest bridges.
func buildBridgeIndex(bridges []*BridgeOp) map[string][]*BridgeOp {
	idx := make(map[string][]*BridgeOp)
	for _, b := range bridges {
		key := bridgeKey(b.Binding, b.From, b.To)
		idx[key] = append(idx[key], b)
	}
	return idx
}

// applyBridge applies a single bridge op to a value, returning the transformed value.
func applyBridge(b *BridgeOp, val interface{}) (interface{}, error) {
	// Check guard before applying the op
	if err := checkGuard(b, val); err != nil {
		return nil, err
	}

	switch b.Op {
	case "identity":
		return val, nil

	case "widen":
		return applyWiden(b, val)

	case "narrow":
		return applyNarrow(b, val)

	case "to_string":
		return applyToString(val), nil

	case "parse_int":
		return applyParseInt(b, val)

	case "parse_float":
		return applyParseFloat(b, val)

	case "serialize":
		return applySerialize(b, val)

	case "deserialize":
		return applyDeserialize(b, val)

	case "copy_array":
		return applyCopyArray(val)

	case "copy_buffer":
		return applyCopyBuffer(val)

	case "wrap_option":
		return map[string]interface{}{"some": val}, nil

	case "unwrap_option":
		return applyUnwrapOption(b, val)

	case "wrap_result":
		return map[string]interface{}{"ok": val}, nil

	case "unwrap_result":
		return applyUnwrapResult(b, val)

	case "throw_typed":
		return applyThrowTyped(b, val)

	case "catch_to_result":
		// catch_to_result is handled at the executor level (wraps executeOps in recovery).
		// At the value level, this is a passthrough.
		return val, nil

	case "compose":
		return applyCompose(b, val)

	case "share_memory":
		// Zero-copy: passthrough for now. Future: pointer+length transfer.
		return val, nil

	case "proxy_callable":
		// Handled at executor level via stub registration. Passthrough here.
		return val, nil

	case "proxy_with_finalizer":
		// Future: register release hook. Passthrough for now.
		return val, nil

	case "attach_disposer":
		// Future: wrap with disposer. Passthrough for now.
		return val, nil

	case "stream_proxy", "channel_bridge":
		// Future: async stream/channel bridging. Passthrough for now.
		return val, nil

	case "await_resolve":
		// Handled at executor level (pump event loops). Passthrough here.
		return val, nil

	case "tag_dispatch":
		return applyTagDispatch(b, val)

	case "struct_to_dict":
		// Most runtime values are already dict-like after JSON round-trip.
		return val, nil

	case "dict_to_struct":
		// Future: validate fields against target struct definition.
		return val, nil

	case "struct_reshape":
		return applyStructReshape(b, val)

	default:
		return nil, &BridgeError{Op: b.Op, Binding: b.Binding, Detail: "unknown bridge op"}
	}
}

// applyWiden performs numeric widening (always succeeds).
func applyWiden(b *BridgeOp, val interface{}) (interface{}, error) {
	f := toFloat64(val)
	// Widening to a float type stays float; widening to int type stays int if possible.
	toType, _ := b.Meta["to"].(string)
	if strings.HasPrefix(toType, "f") {
		return f, nil
	}
	if f == math.Trunc(f) {
		return int64(f), nil
	}
	return f, nil
}

// applyNarrow performs numeric narrowing with range checking.
func applyNarrow(b *BridgeOp, val interface{}) (interface{}, error) {
	f := toFloat64(val)
	toType, _ := b.Meta["to"].(string)

	switch toType {
	case "i8":
		if f < -128 || f > 127 || f != math.Trunc(f) {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g out of i8 range [-128, 127]", f)}
		}
		return int64(f), nil
	case "i16":
		if f < -32768 || f > 32767 || f != math.Trunc(f) {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g out of i16 range", f)}
		}
		return int64(f), nil
	case "i32":
		if f < -2147483648 || f > 2147483647 || f != math.Trunc(f) {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g out of i32 range", f)}
		}
		return int64(f), nil
	case "u8":
		if f < 0 || f > 255 || f != math.Trunc(f) {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g out of u8 range [0, 255]", f)}
		}
		return int64(f), nil
	case "u16":
		if f < 0 || f > 65535 || f != math.Trunc(f) {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g out of u16 range", f)}
		}
		return int64(f), nil
	case "u32":
		if f < 0 || f > 4294967295 || f != math.Trunc(f) {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g out of u32 range", f)}
		}
		return int64(f), nil
	case "f32":
		if f > math.MaxFloat32 || f < -math.MaxFloat32 {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g out of f32 range", f)}
		}
		return float64(float32(f)), nil
	default:
		// Unknown target type — try integer narrowing with i32 as default
		if f < -2147483648 || f > 2147483647 || f != math.Trunc(f) {
			return nil, &BridgeError{Op: "narrow", Binding: b.Binding, Detail: fmt.Sprintf("%.6g cannot narrow to %s", f, toType)}
		}
		return int64(f), nil
	}
}

func applyToString(val interface{}) string {
	if val == nil {
		return "null"
	}
	switch v := val.(type) {
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", val)
	}
}

func applyParseInt(b *BridgeOp, val interface{}) (interface{}, error) {
	s := fmt.Sprintf("%v", val)
	// Strip surrounding whitespace
	s = strings.TrimSpace(s)
	// Try parsing as float first (handles "3.0")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, &BridgeError{Op: "parse_int", Binding: b.Binding, Detail: fmt.Sprintf("cannot parse %q as int", s)}
	}
	if f != math.Trunc(f) {
		return nil, &BridgeError{Op: "parse_int", Binding: b.Binding, Detail: fmt.Sprintf("%q is not an integer", s)}
	}
	return int64(f), nil
}

func applyParseFloat(b *BridgeOp, val interface{}) (interface{}, error) {
	s := fmt.Sprintf("%v", val)
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, &BridgeError{Op: "parse_float", Binding: b.Binding, Detail: fmt.Sprintf("cannot parse %q as float", s)}
	}
	return f, nil
}

func applySerialize(b *BridgeOp, val interface{}) (interface{}, error) {
	format, _ := b.Meta["format"].(string)
	switch format {
	case "json", "":
		data, err := json.Marshal(val)
		if err != nil {
			return nil, &BridgeError{Op: "serialize", Binding: b.Binding, Detail: err.Error()}
		}
		return string(data), nil
	case "msgpack":
		// Future: msgpack support
		return nil, &BridgeError{Op: "serialize", Binding: b.Binding, Detail: "msgpack not yet supported"}
	default:
		return nil, &BridgeError{Op: "serialize", Binding: b.Binding, Detail: fmt.Sprintf("unknown format %q", format)}
	}
}

func applyDeserialize(b *BridgeOp, val interface{}) (interface{}, error) {
	format, _ := b.Meta["format"].(string)
	switch format {
	case "json", "":
		s, ok := val.(string)
		if !ok {
			return nil, &BridgeError{Op: "deserialize", Binding: b.Binding, Detail: "expected string input"}
		}
		var out interface{}
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, &BridgeError{Op: "deserialize", Binding: b.Binding, Detail: err.Error()}
		}
		return out, nil
	case "msgpack":
		return nil, &BridgeError{Op: "deserialize", Binding: b.Binding, Detail: "msgpack not yet supported"}
	default:
		return nil, &BridgeError{Op: "deserialize", Binding: b.Binding, Detail: fmt.Sprintf("unknown format %q", format)}
	}
}

func applyCopyArray(val interface{}) (interface{}, error) {
	// Deep copy via JSON round-trip
	data, err := json.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("copy_array: marshal: %w", err)
	}
	var out interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("copy_array: unmarshal: %w", err)
	}
	return out, nil
}

func applyCopyBuffer(val interface{}) (interface{}, error) {
	// For now, same as copy_array (no zero-copy path yet)
	return applyCopyArray(val)
}

func applyUnwrapOption(b *BridgeOp, val interface{}) (interface{}, error) {
	if val == nil {
		return nil, &BridgeError{Op: "unwrap_option", Binding: b.Binding, Detail: "value is null/None"}
	}
	// Check for wrapped option format {"some": value}
	if m, ok := val.(map[string]interface{}); ok {
		if v, has := m["some"]; has {
			return v, nil
		}
		if _, has := m["none"]; has {
			return nil, &BridgeError{Op: "unwrap_option", Binding: b.Binding, Detail: "value is None"}
		}
	}
	// Check for string representations of null
	if s, ok := val.(string); ok {
		switch strings.ToLower(s) {
		case "null", "none", "nil", "undefined":
			return nil, &BridgeError{Op: "unwrap_option", Binding: b.Binding, Detail: "value is " + s}
		}
	}
	// Non-null value — pass through
	return val, nil
}

func applyUnwrapResult(b *BridgeOp, val interface{}) (interface{}, error) {
	m, ok := val.(map[string]interface{})
	if !ok {
		// Not wrapped in Result — treat as Ok
		return val, nil
	}
	if v, has := m["ok"]; has {
		return v, nil
	}
	if errVal, has := m["err"]; has {
		return nil, &BridgeError{Op: "unwrap_result", Binding: b.Binding, Detail: fmt.Sprintf("%v", errVal)}
	}
	// Neither ok nor err — pass through as-is
	return val, nil
}

func applyThrowTyped(b *BridgeOp, val interface{}) (interface{}, error) {
	m, ok := val.(map[string]interface{})
	if !ok {
		return val, nil
	}
	if errVal, has := m["err"]; has {
		errorKind, _ := b.Meta["errorKind"].(string)
		detail := fmt.Sprintf("%v", errVal)
		if errorKind != "" {
			detail = errorKind + ": " + detail
		}
		return nil, &BridgeError{Op: "throw_typed", Binding: b.Binding, Detail: detail}
	}
	if v, has := m["ok"]; has {
		return v, nil
	}
	return val, nil
}

func applyCompose(b *BridgeOp, val interface{}) (interface{}, error) {
	stepsRaw, ok := b.Meta["steps"]
	if !ok {
		return val, nil
	}
	steps, ok := stepsRaw.([]interface{})
	if !ok {
		return nil, &BridgeError{Op: "compose", Binding: b.Binding, Detail: "steps must be an array"}
	}

	current := val
	for _, stepRaw := range steps {
		stepName, ok := stepRaw.(string)
		if !ok {
			// Step could be an object with its own meta and guard
			stepMap, ok := stepRaw.(map[string]interface{})
			if !ok {
				return nil, &BridgeError{Op: "compose", Binding: b.Binding, Detail: fmt.Sprintf("invalid step: %v", stepRaw)}
			}
			stepName, _ = stepMap["op"].(string)
			meta, _ := stepMap["meta"].(map[string]interface{})
			stepBridge := &BridgeOp{
				Binding: b.Binding,
				Op:      stepName,
				From:    b.From,
				To:      b.To,
				Meta:    meta,
			}
			var err error
			current, err = applyBridge(stepBridge, current)
			if err != nil {
				return nil, err
			}
			continue
		}
		// Simple string step name — create a minimal bridge op
		stepBridge := &BridgeOp{
			Binding: b.Binding,
			Op:      stepName,
			From:    b.From,
			To:      b.To,
			Meta:    b.Meta, // inherit parent meta for context
		}
		var err error
		current, err = applyBridge(stepBridge, current)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func applyTagDispatch(b *BridgeOp, val interface{}) (interface{}, error) {
	// Enum→union: add tag field. Union→enum: validate tag.
	m, ok := val.(map[string]interface{})
	if !ok {
		return val, nil
	}

	// Check if it already has a tag
	if tag, has := m["tag"]; has {
		// Validate against allowed variants
		variantsRaw, _ := b.Meta["variants"]
		if variants, ok := variantsRaw.([]interface{}); ok {
			tagStr := fmt.Sprintf("%v", tag)
			valid := false
			for _, v := range variants {
				if fmt.Sprintf("%v", v) == tagStr {
					valid = true
					break
				}
			}
			if !valid {
				return nil, &BridgeError{Op: "tag_dispatch", Binding: b.Binding, Detail: fmt.Sprintf("unknown variant %q", tagStr)}
			}
		}
		return val, nil
	}

	// No tag — if there's exactly one key, use it as the tag (Rust enum style)
	if len(m) == 1 {
		for k, v := range m {
			return map[string]interface{}{"tag": k, "value": v}, nil
		}
	}

	return val, nil
}

func applyStructReshape(b *BridgeOp, val interface{}) (interface{}, error) {
	m, ok := val.(map[string]interface{})
	if !ok {
		return val, nil
	}
	fieldMapRaw, _ := b.Meta["fieldMap"]
	fieldMap, ok := fieldMapRaw.(map[string]interface{})
	if !ok {
		return val, nil
	}

	result := make(map[string]interface{})
	for oldName, newNameRaw := range fieldMap {
		newName, _ := newNameRaw.(string)
		if v, has := m[oldName]; has {
			result[newName] = v
		}
	}
	// Copy fields not in the map unchanged
	for k, v := range m {
		if _, remapped := fieldMap[k]; !remapped {
			result[k] = v
		}
	}
	return result, nil
}

// checkGuard evaluates the guard hint for a bridge op if present.
// Guards are Go-side checks only (we don't execute runtime-specific guard code here).
func checkGuard(b *BridgeOp, val interface{}) error {
	if b.Meta == nil {
		return nil
	}
	guardRaw, ok := b.Meta["guard"]
	if !ok {
		return nil
	}
	guardMap, ok := guardRaw.(map[string]interface{})
	if !ok {
		return nil
	}

	// Use Go guard if available
	goGuard, ok := guardMap["go"].(string)
	if !ok || goGuard == "" {
		return nil
	}

	// We can't eval arbitrary Go code, but we can handle common patterns
	// For now, guards are informational — the actual range checks happen
	// in the op implementations (narrow, parse_int, etc.)
	_ = goGuard
	return nil
}

// toFloat64 converts a value to float64 for numeric bridge ops.
func toFloat64(val interface{}) float64 {
	switch v := val.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case json.Number:
		f, _ := v.Float64()
		return f
	case RuntimeRef:
		return toFloat64(v.Value)
	default:
		return 0
	}
}
