package golang

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecuteFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "hello.go")
	os.WriteFile(f, []byte(`package main

import "fmt"

func main() {
	fmt.Println("hello from go")
}
`), 0644)

	rt := New()
	result := rt.ExecuteFile(f, nil, nil)
	if result.Err != nil {
		t.Fatalf("ExecuteFile: %v", result.Err)
	}
	if strings.TrimSpace(result.Output) != "hello from go" {
		t.Errorf("output = %q, want 'hello from go'", result.Output)
	}
}

func TestExecuteFileWithArgs(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "args.go")
	os.WriteFile(f, []byte(`package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println(os.Args[1:])
}
`), 0644)

	rt := New()
	result := rt.ExecuteFile(f, []string{"foo", "bar"}, nil)
	if result.Err != nil {
		t.Fatalf("ExecuteFile: %v", result.Err)
	}
	if !strings.Contains(result.Output, "foo") || !strings.Contains(result.Output, "bar") {
		t.Errorf("output = %q, want args foo bar", result.Output)
	}
}

func TestExecuteFileWithStdin(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "stdin.go")
	os.WriteFile(f, []byte(`package main

import (
	"bufio"
	"fmt"
	"os"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Println("got:", scanner.Text())
	}
}
`), 0644)

	rt := New()
	result := rt.ExecuteFile(f, nil, strings.NewReader("line1\nline2\n"))
	if result.Err != nil {
		t.Fatalf("ExecuteFile: %v", result.Err)
	}
	if !strings.Contains(result.Output, "got: line1") || !strings.Contains(result.Output, "got: line2") {
		t.Errorf("output = %q", result.Output)
	}
}

func TestExecuteFileCompileError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad.go")
	os.WriteFile(f, []byte(`package main

func main() {
	fmt.Prntln("oops")
}
`), 0644)

	rt := New()
	result := rt.ExecuteFile(f, nil, nil)
	if result.Err == nil {
		t.Fatal("expected compile error")
	}
	if !strings.Contains(result.Err.Error(), "undefined") {
		t.Errorf("error = %q, expected 'undefined'", result.Err.Error())
	}
}

func TestExecuteFileExitCode(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "exit.go")
	os.WriteFile(f, []byte(`package main

import "os"

func main() {
	os.Exit(42)
}
`), 0644)

	rt := New()
	result := rt.ExecuteFile(f, nil, nil)
	if result.Err == nil {
		t.Fatal("expected exit error")
	}
	if result.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", result.ExitCode)
	}
}

func TestExecuteFileEnvironment(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "env.go")
	os.WriteFile(f, []byte(`package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println(os.Getenv("HOME"))
	wd, _ := os.Getwd()
	fmt.Println(wd)
}
`), 0644)

	rt := New()
	result := rt.ExecuteFile(f, nil, nil)
	if result.Err != nil {
		t.Fatalf("ExecuteFile: %v", result.Err)
	}
	// Should have HOME and CWD in output
	if !strings.Contains(result.Output, "/") {
		t.Errorf("expected path output, got: %q", result.Output)
	}
}
