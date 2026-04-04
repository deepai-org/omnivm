// Package cli handles OmniVM command-line parsing, language detection, and shebang handling.
package cli

import (
	"fmt"
	"path/filepath"
	"strings"
)

type Mode int

const (
	ModeREPL Mode = iota
	ModeRun       // Run a file
	ModeExec      // Execute inline code (legacy -python/-js/-java/-ruby flags)
)

type Command struct {
	Mode     Mode
	File     string   // File path (ModeRun)
	Code     string   // Inline code (ModeExec)
	Language string   // Detected or specified language
	Args     []string // Arguments to pass to the script
}

var extToLang = map[string]string{
	".py":    "python",
	".js":    "javascript",
	".java":  "java",
	".class": "java",
	".jar":   "java",
	".rb":    "ruby",
	".go":    "go",
}

var legacyFlags = map[string]string{
	"-python": "python",
	"-js":     "javascript",
	"-java":   "java",
	"-ruby":   "ruby",
	"-go":     "go",
}

func DetectLanguage(filename string) (string, error) {
	ext := filepath.Ext(filename)
	if lang, ok := extToLang[ext]; ok {
		return lang, nil
	}
	return "", fmt.Errorf("unknown file extension: %q", ext)
}

func StripShebang(code string) string {
	if strings.HasPrefix(code, "#!") {
		if idx := strings.Index(code, "\n"); idx >= 0 {
			return code[idx+1:]
		}
		return ""
	}
	return code
}

func Parse(args []string) (Command, error) {
	if len(args) == 0 {
		return Command{Mode: ModeREPL}, nil
	}

	// Handle "run" subcommand
	if args[0] == "run" {
		if len(args) < 2 {
			return Command{}, fmt.Errorf("usage: omnivm run <file> [args...]")
		}
		lang, err := DetectLanguage(args[1])
		if err != nil {
			return Command{}, err
		}
		return Command{
			Mode:     ModeRun,
			File:     args[1],
			Language: lang,
			Args:     args[2:],
		}, nil
	}

	// Handle legacy -file flag
	if args[0] == "-file" {
		if len(args) < 2 {
			return Command{}, fmt.Errorf("usage: omnivm -file <path>")
		}
		lang, err := DetectLanguage(args[1])
		if err != nil {
			return Command{}, err
		}
		return Command{
			Mode:     ModeRun,
			File:     args[1],
			Language: lang,
			Args:     args[2:],
		}, nil
	}

	// Handle legacy -python/-js/-java/-ruby flags
	if lang, ok := legacyFlags[args[0]]; ok {
		if len(args) < 2 {
			return Command{}, fmt.Errorf("usage: omnivm %s <code>", args[0])
		}
		return Command{
			Mode:     ModeExec,
			Language: lang,
			Code:     args[1],
		}, nil
	}

	return Command{}, fmt.Errorf("unknown command: %s\nUsage: omnivm run <file> [args...] | omnivm [repl]", args[0])
}

// RequiredRuntimes returns which runtimes need to be initialized for the given command.
// REPL needs all runtimes. File/exec mode only needs the target language.
func RequiredRuntimes(cmd Command) []string {
	if cmd.Mode == ModeREPL {
		return []string{"python", "javascript", "java", "ruby", "go"}
	}
	return []string{cmd.Language}
}
