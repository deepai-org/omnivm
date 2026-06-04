package omnivm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"
)

// RuntimeError is a structured error from a guest runtime, preserving
// the exception type, message, and traceback across the bridge.
type RuntimeError struct {
	Runtime             string              // which runtime threw (e.g. "python", "javascript")
	OriginRuntime       string              // alias for Runtime in structured error envelopes
	Type                string              // exception type name (e.g. "SyntaxError", "DoesNotExist")
	Message             string              // human-readable error message
	Traceback           string              // full stack trace from the source runtime
	StackFrames         []string            // normalized non-metadata traceback lines
	CauseChain          []RuntimeErrorCause // nested causes from languages that expose them
	BoundaryPath        string              // call/manifest boundary path, if available
	OriginalErrorHandle string              // source-runtime handle marker, if one was reported
	Details             interface{}
}

type RuntimeErrorCause struct {
	Runtime             string
	OriginRuntime       string
	Type                string
	Message             string
	BoundaryPath        string
	OriginalErrorHandle string
}

func (e *RuntimeError) Error() string {
	var b strings.Builder
	if e.Runtime != "" {
		b.WriteString(e.Runtime)
	}
	if e.Type != "" {
		if b.Len() > 0 {
			b.WriteString(": ")
		}
		b.WriteString(e.Type)
	}
	if b.Len() > 0 {
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	if e.Traceback != "" {
		b.WriteString("\n")
		b.WriteString(e.Traceback)
	}
	return b.String()
}

// ToMap returns the normalized, JSON-serializable runtime error envelope.
func (e *RuntimeError) ToMap() map[string]interface{} {
	if e == nil {
		return nil
	}
	causes := make([]map[string]string, 0, len(e.CauseChain))
	for _, cause := range e.CauseChain {
		item := map[string]string{
			"type":    cause.Type,
			"message": cause.Message,
		}
		if cause.Runtime != "" {
			item["runtime"] = cause.Runtime
		}
		if cause.OriginRuntime != "" {
			item["origin_runtime"] = cause.OriginRuntime
		}
		if cause.BoundaryPath != "" {
			item["boundary_path"] = cause.BoundaryPath
		}
		if cause.OriginalErrorHandle != "" {
			item["original_error_handle"] = cause.OriginalErrorHandle
		}
		causes = append(causes, item)
	}
	origin := e.OriginRuntime
	if origin == "" {
		origin = e.Runtime
	}
	return map[string]interface{}{
		"runtime":               e.Runtime,
		"origin_runtime":        origin,
		"type":                  e.Type,
		"message":               e.Message,
		"traceback":             e.Traceback,
		"stack_frames":          append([]string(nil), e.StackFrames...),
		"cause_chain":           causes,
		"boundary_path":         e.BoundaryPath,
		"original_error_handle": e.OriginalErrorHandle,
		"details":               copyJSONValue(e.Details),
	}
}

func copyJSONValue(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		copied := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			copied[key] = copyJSONValue(item)
		}
		return copied
	case map[string]string:
		copied := make(map[string]string, len(typed))
		for key, item := range typed {
			copied[key] = item
		}
		return copied
	case []interface{}:
		copied := make([]interface{}, len(typed))
		for i, item := range typed {
			copied[i] = copyJSONValue(item)
		}
		return copied
	case []string:
		return append([]string(nil), typed...)
	case []map[string]string:
		copied := make([]map[string]string, len(typed))
		for i, item := range typed {
			if item == nil {
				continue
			}
			entry := make(map[string]string, len(item))
			for key, value := range item {
				entry[key] = value
			}
			copied[i] = entry
		}
		return copied
	default:
		return copyJSONValueReflect(typed)
	}
}

func copyJSONValueReflect(value interface{}) interface{} {
	copied, ok := copyJSONReflectValue(reflect.ValueOf(value))
	if !ok {
		return value
	}
	return copied.Interface()
}

func copyJSONReflectValue(value reflect.Value) (reflect.Value, bool) {
	if !value.IsValid() {
		return value, false
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type()), true
		}
		copied, ok := copyJSONReflectValue(value.Elem())
		if !ok {
			return value, false
		}
		return copied, true
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type()), true
		}
		copied := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			copied.SetMapIndex(iter.Key(), copyJSONAssignableValue(iter.Value(), value.Type().Elem()))
		}
		return copied, true
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type()), true
		}
		copied := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			copied.Index(i).Set(copyJSONAssignableValue(value.Index(i), value.Type().Elem()))
		}
		return copied, true
	case reflect.Array:
		copied := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			copied.Index(i).Set(copyJSONAssignableValue(value.Index(i), value.Type().Elem()))
		}
		return copied, true
	default:
		return value, false
	}
}

func copyJSONAssignableValue(value reflect.Value, target reflect.Type) reflect.Value {
	copied, ok := copyJSONReflectValue(value)
	if !ok {
		copied = value
	}
	if copied.Type().AssignableTo(target) {
		return copied
	}
	if copied.Type().ConvertibleTo(target) {
		return copied.Convert(target)
	}
	return value
}

// ParseError parses a bridge error into a structured RuntimeError.
// It accepts the transport "ERR:" prefix, runtime prefixes such as "python: ",
// and manifest boundary prefixes. Plain values still return nil.
func ParseError(runtime, s string) *RuntimeError {
	body := strings.TrimSpace(s)
	hasTransportMarker := strings.HasPrefix(body, "ERR:")
	if hasTransportMarker {
		body = strings.TrimSpace(body[4:])
	}

	if re := parseStructuredErrorEnvelope(body, runtime); re != nil {
		return re
	}

	sourceRuntime := normalizeRuntime(runtime)
	boundaryParts := []string{}
	recognized := hasTransportMarker
	var matched bool
	body, matched = stripBoundaryPrefix(body, "execute manifest", &boundaryParts)
	recognized = recognized || matched
	body, matched = stripBoundaryPrefix(body, "load manifest module", &boundaryParts)
	recognized = recognized || matched
	body, matched = stripBoundaryPrefix(body, "manifest module call", &boundaryParts)
	recognized = recognized || matched
	body, matched = stripCallBoundary(body, &sourceRuntime, &boundaryParts)
	recognized = recognized || matched
	body, matched = stripRuntimeRefAssignPrefix(body, &sourceRuntime)
	recognized = recognized || matched
	body, matched = stripRuntimePrefixes(body, &sourceRuntime)
	recognized = recognized || matched

	if re := parseStructuredErrorEnvelope(body, sourceRuntime); re != nil {
		if len(boundaryParts) > 0 {
			defaultBoundary := boundaryPath(nil, re.Runtime)
			if re.BoundaryPath == "" || re.BoundaryPath == defaultBoundary {
				re.BoundaryPath = boundaryPath(boundaryParts, re.Runtime)
			}
		}
		return re
	}

	if !recognized {
		return nil
	}

	// Split first line from traceback (if any)
	message := body
	traceback := ""
	if idx := strings.Index(body, "\n"); idx >= 0 {
		message = body[:idx]
		traceback = body[idx+1:]
	}

	// Try to extract "TypeName: message" from the first line
	errType := ""
	parseLine := message
	if strings.HasPrefix(message, "Traceback ") {
		traceback = body
		for _, line := range reverseNonEmptyLines(body) {
			if isMetadataLine(line) {
				continue
			}
			if idx := strings.Index(line, ": "); idx >= 0 && isErrorTypeCandidate(line[:idx]) {
				parseLine = line
				break
			}
		}
	}
	if idx := strings.Index(parseLine, ": "); idx >= 0 {
		candidate := parseLine[:idx]
		if isErrorTypeCandidate(candidate) {
			if sourceRuntime == "python" {
				if dot := strings.LastIndex(candidate, "."); dot >= 0 {
					candidate = candidate[dot+1:]
				}
			}
			errType = candidate
			message = parseLine[idx+2:]
		}
	}

	return &RuntimeError{
		Runtime:             sourceRuntime,
		OriginRuntime:       sourceRuntime,
		Type:                errType,
		Message:             message,
		Traceback:           traceback,
		StackFrames:         stackFrames(traceback),
		CauseChain:          parseCauseChain(body),
		BoundaryPath:        boundaryPath(boundaryParts, sourceRuntime),
		OriginalErrorHandle: extractOriginalErrorHandle(body),
		Details:             parseDetails(body),
	}
}

func parseStructuredErrorEnvelope(body, fallbackRuntime string) *RuntimeError {
	if body == "" || body[0] != '{' {
		return nil
	}
	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return nil
	}
	if len(envelope) == 0 {
		return nil
	}
	runtimeName, _ := envelope["runtime"].(string)
	originRuntime, _ := envelope["origin_runtime"].(string)
	errType, _ := envelope["type"].(string)
	message, _ := envelope["message"].(string)
	traceback, _ := envelope["traceback"].(string)
	boundary, _ := envelope["boundary_path"].(string)
	handle, _ := envelope["original_error_handle"].(string)
	if runtimeName == "" {
		runtimeName = normalizeRuntime(fallbackRuntime)
	} else {
		runtimeName = normalizeRuntime(runtimeName)
	}
	if originRuntime == "" {
		originRuntime = runtimeName
	} else {
		originRuntime = normalizeRuntime(originRuntime)
	}
	if boundary == "" {
		boundary = boundaryPath(nil, runtimeName)
	}
	if runtimeName == "" && errType == "" && message == "" && traceback == "" {
		return nil
	}
	return &RuntimeError{
		Runtime:             runtimeName,
		OriginRuntime:       originRuntime,
		Type:                errType,
		Message:             message,
		Traceback:           traceback,
		StackFrames:         stringSliceEnvelopeValue(envelope["stack_frames"], stackFrames(traceback)),
		CauseChain:          causeChainEnvelopeValue(envelope["cause_chain"]),
		BoundaryPath:        boundary,
		OriginalErrorHandle: handle,
		Details:             copyJSONValue(envelope["details"]),
	}
}

func stringSliceEnvelopeValue(value interface{}, fallback []string) []string {
	items, ok := value.([]interface{})
	if !ok {
		return fallback
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return fallback
		}
		out = append(out, text)
	}
	return out
}

func causeChainEnvelopeValue(value interface{}) []RuntimeErrorCause {
	items, ok := value.([]interface{})
	if !ok {
		return nil
	}
	out := make([]RuntimeErrorCause, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		cause := RuntimeErrorCause{}
		if runtimeName, ok := entry["runtime"].(string); ok {
			cause.Runtime = normalizeRuntime(runtimeName)
		}
		if originRuntime, ok := entry["origin_runtime"].(string); ok {
			cause.OriginRuntime = normalizeRuntime(originRuntime)
		}
		if causeType, ok := entry["type"].(string); ok {
			cause.Type = causeType
		}
		if message, ok := entry["message"].(string); ok {
			cause.Message = message
		}
		if boundaryPath, ok := entry["boundary_path"].(string); ok {
			cause.BoundaryPath = boundaryPath
		}
		if handle, ok := entry["original_error_handle"].(string); ok {
			cause.OriginalErrorHandle = handle
		}
		out = append(out, cause)
	}
	return out
}

func stackFrames(traceback string) []string {
	frames := []string{}
	for _, line := range strings.Split(traceback, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || isMetadataLine(line) {
			continue
		}
		frames = append(frames, line)
	}
	return frames
}

func stripBoundaryPrefix(text, prefix string, parts *[]string) (string, bool) {
	marker := prefix + ":"
	if strings.HasPrefix(text, marker) {
		*parts = append(*parts, prefix)
		return strings.TrimSpace(text[len(marker):]), true
	}
	return text, false
}

func stripCallBoundary(text string, runtime *string, parts *[]string) (string, bool) {
	colon := strings.Index(text, ": ")
	if colon <= 0 {
		return text, false
	}
	head := text[:colon]
	open := strings.IndexByte(head, '[')
	close := strings.IndexByte(head, ']')
	if open <= 0 || close <= open || close != len(head)-1 {
		return text, false
	}
	op := strings.TrimSpace(head[:open])
	rt := head[open+1 : close]
	if !isIdentifierLike(op) || !isRuntimeLike(rt) {
		return text, false
	}
	*runtime = normalizeRuntime(rt)
	*parts = append(*parts, op+"["+rt+"]")
	return strings.TrimSpace(text[colon+2:]), true
}

func stripRuntimePrefixes(text string, runtime *string) (string, bool) {
	matched := false
	for {
		colon := strings.Index(text, ": ")
		if colon <= 0 {
			return text, matched
		}
		prefix := strings.TrimSpace(text[:colon])
		if !isRuntimeLike(prefix) {
			return text, matched
		}
		*runtime = normalizeRuntime(prefix)
		text = strings.TrimSpace(text[colon+2:])
		matched = true
	}
}

func stripRuntimeRefAssignPrefix(text string, runtime *string) (string, bool) {
	const prefix = "runtime ref assign ["
	if !strings.HasPrefix(text, prefix) {
		return text, false
	}
	rest := text[len(prefix):]
	close := strings.Index(rest, "]: ")
	if close < 0 {
		return text, false
	}
	rt := rest[:close]
	if !isRuntimeLike(rt) {
		return text, false
	}
	*runtime = normalizeRuntime(rt)
	return strings.TrimSpace(rest[close+3:]), true
}

func parseCauseChain(text string) []RuntimeErrorCause {
	var causes []RuntimeErrorCause
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Caused by: ") {
			continue
		}
		detail := strings.TrimSpace(strings.TrimPrefix(line, "Caused by: "))
		cause := RuntimeErrorCause{Message: detail}
		if idx := strings.Index(detail, ": "); idx >= 0 && isErrorTypeCandidate(detail[:idx]) {
			cause.Type = detail[:idx]
			cause.Message = detail[idx+2:]
		}
		causes = append(causes, cause)
	}
	return causes
}

func parseDetails(text string) interface{} {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Details: ") {
			continue
		}
		var details interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "Details: "))), &details); err == nil {
			return details
		}
	}
	return nil
}

func isMetadataLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(lower, "caused by:") ||
		strings.HasPrefix(lower, "details:") ||
		strings.HasPrefix(lower, "original_error_handle:") ||
		strings.HasPrefix(lower, "original error handle:") ||
		strings.HasPrefix(lower, "original-error-handle:")
}

func extractOriginalErrorHandle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "original_error_handle:") ||
			strings.HasPrefix(lower, "original error handle:") ||
			strings.HasPrefix(lower, "original-error-handle:") {
			if idx := strings.Index(line, ":"); idx >= 0 {
				return strings.TrimSpace(line[idx+1:])
			}
		}
	}
	return ""
}

func reverseNonEmptyLines(text string) []string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func boundaryPath(parts []string, runtime string) string {
	if len(parts) > 0 {
		return strings.Join(parts, " > ")
	}
	if runtime != "" {
		return "call[" + runtime + "]"
	}
	return ""
}

func isRuntimeLike(value string) bool {
	switch normalizeRuntime(value) {
	case "python", "javascript", "ruby", "java", "go", "__manifest":
		return true
	default:
		return false
	}
}

func normalizeRuntime(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "js", "node":
		return "javascript"
	case "jvm":
		return "java"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func isIdentifierLike(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

func isErrorTypeCandidate(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if !unicode.IsLetter(r) && r != '_' {
				return false
			}
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' && r != '$' && r != ':' {
			return false
		}
	}
	return true
}

// ErrNotStarted is returned when Call/Execute is invoked before Start().
var ErrNotStarted = fmt.Errorf("omnivm: VM not started")

// ErrAlreadyStarted is returned when Start() is called twice.
var ErrAlreadyStarted = fmt.Errorf("omnivm: VM already started")

// ErrShutdown is returned when Call/Execute is invoked after Shutdown().
var ErrShutdown = fmt.Errorf("omnivm: VM shut down")

// ErrDrainTimeout is returned when Shutdown times out waiting for the dispatcher.
var ErrDrainTimeout = fmt.Errorf("omnivm: dispatcher drain timed out")

// ErrUnknownRuntime is returned when Call/Execute references an unregistered runtime.
type ErrUnknownRuntime struct {
	Name string
}

func (e *ErrUnknownRuntime) Error() string {
	return fmt.Sprintf("omnivm: unknown runtime %q", e.Name)
}
