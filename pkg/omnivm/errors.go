package omnivm

import (
	"encoding/json"
	"fmt"
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
	CauseChain          []RuntimeErrorCause // nested causes from languages that expose them
	BoundaryPath        string              // call/manifest boundary path, if available
	OriginalErrorHandle string              // source-runtime handle marker, if one was reported
	Details             map[string]interface{}
}

type RuntimeErrorCause struct {
	Type    string
	Message string
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

// ParseError parses a bridge error into a structured RuntimeError.
// It accepts the transport "ERR:" prefix, runtime prefixes such as "python: ",
// and manifest boundary prefixes. Plain values still return nil.
func ParseError(runtime, s string) *RuntimeError {
	body := strings.TrimSpace(s)
	hasTransportMarker := strings.HasPrefix(body, "ERR:")
	if hasTransportMarker {
		body = strings.TrimSpace(body[4:])
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
		CauseChain:          parseCauseChain(body),
		BoundaryPath:        boundaryPath(boundaryParts, sourceRuntime),
		OriginalErrorHandle: extractOriginalErrorHandle(body),
		Details:             parseDetails(body),
	}
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

func parseDetails(text string) map[string]interface{} {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Details: ") {
			continue
		}
		var details map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "Details: "))), &details); err == nil {
			return details
		}
	}
	return nil
}

func isMetadataLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(line, "caused by:") ||
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
