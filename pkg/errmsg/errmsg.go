// Package errmsg enhances runtime error messages with actionable suggestions.
package errmsg

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	pyModuleNotFound = regexp.MustCompile(`ModuleNotFoundError: No module named '([^']+)'`)
	jsModuleNotFound = regexp.MustCompile(`Cannot find module '([^']+)'`)
	rbLoadError      = regexp.MustCompile(`LoadError: cannot load such file -- (.+)`)
	javaClassNotFound = regexp.MustCompile(`ClassNotFoundException: (.+)`)
	goUndefined      = regexp.MustCompile(`undefined: (\S+)`)
	pyTracebackLine  = regexp.MustCompile(`File "([^"]+)", line (\d+)`)
)

func Enhance(lang, errMsg string) string {
	switch lang {
	case "python":
		if m := pyModuleNotFound.FindStringSubmatch(errMsg); m != nil {
			return fmt.Sprintf("%s\n\n  Hint: pip install %s", errMsg, m[1])
		}
	case "javascript":
		if m := jsModuleNotFound.FindStringSubmatch(errMsg); m != nil {
			return fmt.Sprintf("%s\n\n  Hint: npm install %s", errMsg, m[1])
		}
	case "ruby":
		if m := rbLoadError.FindStringSubmatch(errMsg); m != nil {
			return fmt.Sprintf("%s\n\n  Hint: gem install %s", errMsg, strings.TrimSpace(m[1]))
		}
	case "java":
		if m := javaClassNotFound.FindStringSubmatch(errMsg); m != nil {
			cls := strings.TrimSpace(m[1])
			return fmt.Sprintf("%s\n\n  Hint: Ensure %s is on the classpath. Mount JARs to /omnivm/libs/", errMsg, cls)
		}
	case "go":
		if m := goUndefined.FindStringSubmatch(errMsg); m != nil {
			sym := m[1]
			suggestion := suggestGoSymbol(sym)
			if suggestion != "" {
				return fmt.Sprintf("%s\n\n  Did you mean: %s?", errMsg, suggestion)
			}
		}
	}
	return errMsg
}

func FormatTraceback(lang, raw string) string {
	switch lang {
	case "python":
		return formatPythonTraceback(raw)
	case "javascript", "ruby", "java", "go":
		return raw
	default:
		return raw
	}
}

func formatPythonTraceback(raw string) string {
	lines := strings.Split(raw, "\n")
	var out []string
	for _, line := range lines {
		if m := pyTracebackLine.FindStringSubmatch(line); m != nil {
			// Reformat "File "x.py", line 10" → "  x.py:10"
			out = append(out, fmt.Sprintf("  %s:%s", m[1], m[2]))
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Common Go stdlib typos
var goTypoMap = map[string]string{
	"fmt.Prntln":  "fmt.Println",
	"fmt.Printl":  "fmt.Println",
	"fmt.Prinltn": "fmt.Println",
	"fmt.Printn":  "fmt.Println",
	"fmt.Printfn": "fmt.Printf",
	"fmt.Sprintl": "fmt.Sprintf",
}

func suggestGoSymbol(sym string) string {
	if s, ok := goTypoMap[sym]; ok {
		return s
	}
	// Check for close matches by prefix
	for typo, correct := range goTypoMap {
		if strings.HasPrefix(sym, typo[:len(typo)-2]) {
			return correct
		}
	}
	return ""
}
