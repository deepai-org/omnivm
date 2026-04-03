package errmsg

import (
	"strings"
	"testing"
)

func TestEnhancePythonImportError(t *testing.T) {
	err := "python: ModuleNotFoundError: No module named 'requests'"
	got := Enhance("python", err)
	if !strings.Contains(got, "pip install requests") {
		t.Errorf("expected pip install suggestion, got: %s", got)
	}
}

func TestEnhancePythonSyntaxError(t *testing.T) {
	err := `python: SyntaxError: invalid syntax (line 3)`
	got := Enhance("python", err)
	if !strings.Contains(got, "SyntaxError") {
		t.Errorf("expected SyntaxError preserved, got: %s", got)
	}
}

func TestEnhanceJSModuleNotFound(t *testing.T) {
	err := `javascript: Error: Cannot find module 'express'`
	got := Enhance("javascript", err)
	if !strings.Contains(got, "npm install express") {
		t.Errorf("expected npm install suggestion, got: %s", got)
	}
}

func TestEnhanceRubyLoadError(t *testing.T) {
	err := `ruby: LoadError: cannot load such file -- sinatra`
	got := Enhance("ruby", err)
	if !strings.Contains(got, "gem install sinatra") {
		t.Errorf("expected gem install suggestion, got: %s", got)
	}
}

func TestEnhanceJavaClassNotFound(t *testing.T) {
	err := `java: ClassNotFoundException: com.google.gson.Gson`
	got := Enhance("java", err)
	if !strings.Contains(got, "classpath") || !strings.Contains(got, "com.google.gson.Gson") {
		t.Errorf("expected classpath suggestion, got: %s", got)
	}
}

func TestEnhancePassthrough(t *testing.T) {
	err := "python: some random error"
	got := Enhance("python", err)
	if got != err {
		t.Errorf("expected passthrough, got: %s", got)
	}
}

func TestEnhanceGoCompileError(t *testing.T) {
	err := `go: ./main.go:5:2: undefined: fmt.Prntln`
	got := Enhance("go", err)
	if !strings.Contains(got, "fmt.Prntln") {
		t.Errorf("expected error preserved, got: %s", got)
	}
	if !strings.Contains(got, "Did you mean") {
		t.Errorf("expected suggestion, got: %s", got)
	}
}

func TestExtractPythonTraceback(t *testing.T) {
	raw := `Traceback (most recent call last):
  File "script.py", line 10, in <module>
    result = process(data)
  File "script.py", line 5, in process
    return data / 0
ZeroDivisionError: division by zero`
	got := FormatTraceback("python", raw)
	if !strings.Contains(got, "script.py:10") {
		t.Errorf("expected file:line format, got: %s", got)
	}
	if !strings.Contains(got, "ZeroDivisionError") {
		t.Errorf("expected error type, got: %s", got)
	}
}

func TestFormatTracebackJS(t *testing.T) {
	raw := `TypeError: Cannot read properties of undefined (reading 'foo')
    at bar (/app/script.js:10:5)
    at Object.<anonymous> (/app/script.js:15:1)`
	got := FormatTraceback("javascript", raw)
	if !strings.Contains(got, "TypeError") {
		t.Errorf("expected error type preserved, got: %s", got)
	}
}

func TestFormatTracebackUnknown(t *testing.T) {
	raw := "some error"
	got := FormatTraceback("unknown", raw)
	if got != raw {
		t.Errorf("expected passthrough for unknown, got: %s", got)
	}
}
