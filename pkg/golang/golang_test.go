package golang

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func skipIfNoPlugins(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Go plugins only reliably work on Linux")
	}
}

func newInitialized(t *testing.T) *Runtime {
	t.Helper()
	skipIfNoPlugins(t)
	rt := New()
	if err := rt.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	t.Cleanup(func() { rt.Shutdown() })
	return rt
}

func TestExecuteFile(t *testing.T) {
	rt := newInitialized(t)
	f := filepath.Join(t.TempDir(), "hello.go")
	os.WriteFile(f, []byte(`package main

import "fmt"

func main() {
	fmt.Println("hello from go")
}
`), 0644)

	result := rt.ExecuteFile(f, nil, nil)
	if result.Err != nil {
		t.Fatalf("ExecuteFile: %v", result.Err)
	}
	if strings.TrimSpace(result.Output) != "hello from go" {
		t.Errorf("output = %q, want 'hello from go'", result.Output)
	}
}

func TestExecuteFileWithArgs(t *testing.T) {
	rt := newInitialized(t)
	f := filepath.Join(t.TempDir(), "args.go")
	os.WriteFile(f, []byte(`package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Println(strings.Join(os.Args[1:], " "))
}
`), 0644)

	result := rt.ExecuteFile(f, []string{"foo", "bar"}, nil)
	if result.Err != nil {
		t.Fatalf("ExecuteFile: %v", result.Err)
	}
	if strings.TrimSpace(result.Output) != "foo bar" {
		t.Errorf("output = %q, want 'foo bar'", result.Output)
	}
}

func TestExecuteFileCompileError(t *testing.T) {
	rt := newInitialized(t)
	f := filepath.Join(t.TempDir(), "bad.go")
	os.WriteFile(f, []byte(`package main

func main() {
	fmt.Prntln("oops")
}
`), 0644)

	result := rt.ExecuteFile(f, nil, nil)
	if result.Err == nil {
		t.Fatal("expected compile error")
	}
	if !strings.Contains(result.Err.Error(), "undefined") {
		t.Errorf("error = %q, expected 'undefined'", result.Err.Error())
	}
}

func TestExecute(t *testing.T) {
	rt := newInitialized(t)
	result := rt.Execute(`fmt.Println("snippet works")`)
	if result.Err != nil {
		t.Fatalf("Execute: %v", result.Err)
	}
	if strings.TrimSpace(result.Output) != "snippet works" {
		t.Errorf("output = %q", result.Output)
	}
}

func TestEval(t *testing.T) {
	rt := newInitialized(t)
	result := rt.Eval("1 + 2")
	if result.Err != nil {
		t.Fatalf("Eval: %v", result.Err)
	}
	if result.Value != "3" {
		t.Errorf("value = %q, want '3'", result.Value)
	}
}

func TestTransformMain(t *testing.T) {
	src := `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`
	out, err := transformMain(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "func Main()") {
		t.Errorf("expected 'func Main()' in output:\n%s", out)
	}
	if strings.Contains(out, "func main()") {
		t.Errorf("'func main()' should be renamed:\n%s", out)
	}
}

func TestTransformMainNotFound(t *testing.T) {
	src := `package main

func helper() {}
`
	_, err := transformMain(src)
	if err == nil {
		t.Fatal("expected error for missing main()")
	}
}
