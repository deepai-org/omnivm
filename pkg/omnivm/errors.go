package omnivm

import (
	"fmt"
	"strings"
)

// RuntimeError is a structured error from a guest runtime, preserving
// the exception type, message, and traceback across the bridge.
type RuntimeError struct {
	Runtime   string // which runtime threw (e.g. "python", "javascript")
	Type      string // exception type name (e.g. "SyntaxError", "DoesNotExist")
	Message   string // human-readable error message
	Traceback string // full stack trace from the source runtime
}

func (e *RuntimeError) Error() string {
	var b strings.Builder
	b.WriteString(e.Runtime)
	if e.Type != "" {
		b.WriteString(": ")
		b.WriteString(e.Type)
	}
	b.WriteString(": ")
	b.WriteString(e.Message)
	if e.Traceback != "" {
		b.WriteString("\n")
		b.WriteString(e.Traceback)
	}
	return b.String()
}

// ParseError parses an "ERR:..." string into a structured RuntimeError.
// Returns nil if s does not start with "ERR:".
func ParseError(runtime, s string) *RuntimeError {
	if !strings.HasPrefix(s, "ERR:") {
		return nil
	}
	body := s[4:] // strip "ERR:"

	// Split first line from traceback (if any)
	message := body
	traceback := ""
	if idx := strings.Index(body, "\n"); idx >= 0 {
		message = body[:idx]
		traceback = body[idx+1:]
	}

	// Try to extract "TypeName: message" from the first line
	errType := ""
	if idx := strings.Index(message, ": "); idx >= 0 {
		candidate := message[:idx]
		// Heuristic: error type names are typically PascalCase or contain no spaces
		if !strings.Contains(candidate, " ") {
			errType = candidate
			message = message[idx+2:]
		}
	}

	return &RuntimeError{
		Runtime:   runtime,
		Type:      errType,
		Message:   message,
		Traceback: traceback,
	}
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
