package cli

import (
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		file string
		want string
		err  bool
	}{
		{"hello.py", "python", false},
		{"app.js", "javascript", false},
		{"Main.java", "java", false},
		{"script.rb", "ruby", false},
		{"main.go", "go", false},
		{"unknown.xyz", "", true},
		{"noext", "", true},
	}
	for _, tt := range tests {
		got, err := DetectLanguage(tt.file)
		if tt.err && err == nil {
			t.Errorf("DetectLanguage(%q) expected error", tt.file)
		}
		if !tt.err && err != nil {
			t.Errorf("DetectLanguage(%q) unexpected error: %v", tt.file, err)
		}
		if got != tt.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", tt.file, got, tt.want)
		}
	}
}

func TestStripShebang(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"#!/usr/bin/env omnivm run\nprint('hi')\n", "print('hi')\n"},
		{"#!/usr/bin/env python3\nprint('hi')\n", "print('hi')\n"},
		{"#!omnivm run\nprint('hi')\n", "print('hi')\n"},
		{"print('hi')\n", "print('hi')\n"},
		{"", ""},
		{"#!/usr/bin/env omnivm run\n", ""},
	}
	for _, tt := range tests {
		got := StripShebang(tt.input)
		if got != tt.want {
			t.Errorf("StripShebang(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	// omnivm run script.py arg1 arg2
	args := []string{"run", "script.py", "arg1", "arg2"}
	cmd, err := Parse(args)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Mode != ModeRun {
		t.Errorf("Mode = %v, want ModeRun", cmd.Mode)
	}
	if cmd.File != "script.py" {
		t.Errorf("File = %q, want 'script.py'", cmd.File)
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "arg1" || cmd.Args[1] != "arg2" {
		t.Errorf("Args = %v, want [arg1 arg2]", cmd.Args)
	}
	if cmd.Language != "python" {
		t.Errorf("Language = %q, want 'python'", cmd.Language)
	}
}

func TestParseRunNoFile(t *testing.T) {
	_, err := Parse([]string{"run"})
	if err == nil {
		t.Error("expected error for 'run' with no file")
	}
}

func TestParseREPL(t *testing.T) {
	cmd, err := Parse([]string{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Mode != ModeREPL {
		t.Errorf("Mode = %v, want ModeREPL", cmd.Mode)
	}
}

func TestParseLegacyFlags(t *testing.T) {
	// Legacy: omnivm -python "code"
	cmd, err := Parse([]string{"-python", "print('hi')"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Mode != ModeExec {
		t.Errorf("Mode = %v, want ModeExec", cmd.Mode)
	}
	if cmd.Language != "python" {
		t.Errorf("Language = %q, want 'python'", cmd.Language)
	}
	if cmd.Code != "print('hi')" {
		t.Errorf("Code = %q, want \"print('hi')\"", cmd.Code)
	}
}

func TestParseLegacyFile(t *testing.T) {
	cmd, err := Parse([]string{"-file", "script.py"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Mode != ModeRun {
		t.Errorf("Mode = %v, want ModeRun", cmd.Mode)
	}
	if cmd.File != "script.py" {
		t.Errorf("File = %q, want 'script.py'", cmd.File)
	}
}

func TestParseRunWithJSExtension(t *testing.T) {
	cmd, err := Parse([]string{"run", "app.js", "--port", "3000"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Language != "javascript" {
		t.Errorf("Language = %q, want 'javascript'", cmd.Language)
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "--port" || cmd.Args[1] != "3000" {
		t.Errorf("Args = %v, want [--port 3000]", cmd.Args)
	}
}

func TestParseRunWithGoExtension(t *testing.T) {
	cmd, err := Parse([]string{"run", "main.go"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cmd.Language != "go" {
		t.Errorf("Language = %q, want 'go'", cmd.Language)
	}
}
