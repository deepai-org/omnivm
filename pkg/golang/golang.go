// Package golang provides a Go runtime using Go plugins for in-process execution.
// User code is compiled as a plugin (-buildmode=plugin), loaded, and executed
// in the same process — making Go an equal peer to Python, JS, Java, and Ruby.
package golang

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"plugin"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/omnivm/omnivm/pkg"
)

// goModVersion returns the Go version string for generated go.mod files,
// derived from the running binary's Go toolchain to avoid plugin mismatches.
func goModVersion() string {
	// runtime.Version() returns e.g. "go1.22.5"
	v := strings.TrimPrefix(runtime.Version(), "go")
	// Use major.minor only (e.g. "1.22")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// globalCounter ensures unique plugin module names across all Runtime instances,
// since Go's plugin system rejects loading plugins with duplicate module paths.
var globalCounter uint64

// Runtime implements pkg.Runtime and pkg.FileExecutor for Go via plugins.
type Runtime struct {
	// BridgeFn is set by the host to enable cross-runtime calls from Go plugins.
	BridgeFn func(runtime, code string) string

	tempDir string
	counter int

	// loadedPlugins maps plugin names to loaded plugin handles for pre-compiled plugins.
	loadedPlugins map[string]*plugin.Plugin
}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Name() string { return "go" }

func (r *Runtime) Initialize() error {
	dir, err := os.MkdirTemp("", "omnivm-go-")
	if err != nil {
		return fmt.Errorf("go: %w", err)
	}
	r.tempDir = dir
	return nil
}

// SetBridgeCallback is a no-op for Go. The host sets BridgeFn directly
// since Go plugins use Go closures, not C function pointers.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {}

func (r *Runtime) Pump() {}

func (r *Runtime) Shutdown() error {
	if r.tempDir != "" {
		os.RemoveAll(r.tempDir)
	}
	return nil
}

// Execute compiles a Go code snippet as a plugin and runs it.
// Each call is independent — no shared state between calls.
func (r *Runtime) Execute(code string) pkg.Result {
	src := wrapSnippet(code)
	return r.compileAndRun(src, "Run", "", nil)
}

// Eval evaluates a Go expression and returns its value as a string.
// If the expression looks like "plugin.Func(args)", it calls the loaded plugin directly.
// Otherwise it compiles and runs the expression as a snippet.
func (r *Runtime) Eval(code string) pkg.Result {
	// Check for plugin call pattern: "pluginname.FuncName(args)"
	if r.loadedPlugins != nil {
		if pluginName, funcName, args, ok := parsePluginCall(code); ok {
			if _, loaded := r.loadedPlugins[pluginName]; loaded {
				result, err := r.CallPlugin(pluginName, funcName, args)
				if err != nil {
					return pkg.Result{Err: err}
				}
				return pkg.Result{Value: result, Output: result}
			}
		}
	}

	src := wrapEval(code)
	result := r.compileAndRun(src, "Eval", "", nil)
	if result.Err == nil {
		result.Value = result.Output
		result.Output = ""
	}
	return result
}

// ExecuteFile compiles a .go file as a plugin and calls its Main() function.
// The source is transformed: func main() → func Main() (exported for plugin lookup).
func (r *Runtime) ExecuteFile(path string, args []string, stdin io.Reader) pkg.Result {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("go: %w", err), ExitCode: 1}
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("go: %w", err), ExitCode: 1}
	}

	transformed, err := transformMain(string(data))
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("go: %w", err), ExitCode: 1}
	}

	return r.compileAndRun(transformed, "Main", absPath, args)
}

// compileAndRun builds a Go plugin from source, loads it, and calls the named entrypoint.
func (r *Runtime) compileAndRun(src, entrypoint, filePath string, args []string) pkg.Result {
	r.counter++
	buildDir := filepath.Join(r.tempDir, fmt.Sprintf("build_%d", r.counter))
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return pkg.Result{Err: fmt.Errorf("go: %w", err), ExitCode: 1}
	}

	// Write source and bridge shim
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(src), 0644); err != nil {
		return pkg.Result{Err: fmt.Errorf("go: %w", err), ExitCode: 1}
	}
	if err := os.WriteFile(filepath.Join(buildDir, "_bridge.go"), []byte(bridgeShimSource), 0644); err != nil {
		return pkg.Result{Err: fmt.Errorf("go: %w", err), ExitCode: 1}
	}
	if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), []byte(fmt.Sprintf("module omnivm-plugin-%d\n\ngo %s\n", atomic.AddUint64(&globalCounter, 1), goModVersion())), 0644); err != nil {
		return pkg.Result{Err: fmt.Errorf("go: %w", err), ExitCode: 1}
	}

	// Compile as plugin
	soPath := filepath.Join(buildDir, "plugin.so")
	cmd := exec.Command("go", "build", "-buildmode=plugin", "-o", soPath, ".")
	cmd.Dir = buildDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return pkg.Result{
			Err:      fmt.Errorf("go compilation failed:\n%s", strings.TrimSpace(string(out))),
			ExitCode: 1,
		}
	}

	// Load plugin
	p, err := plugin.Open(soPath)
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("go: plugin load: %w", err), ExitCode: 1}
	}

	// Install bridge
	if r.BridgeFn != nil {
		if sym, err := p.Lookup("SetBridge"); err == nil {
			sym.(func(func(string, string) string))(r.BridgeFn)
		}
	}

	// Override os.Args for file execution
	if filePath != "" {
		savedArgs := os.Args
		os.Args = append([]string{filePath}, args...)
		defer func() { os.Args = savedArgs }()
	}

	// Look up entrypoint
	sym, err := p.Lookup(entrypoint)
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("go: symbol %q not found: %w", entrypoint, err), ExitCode: 1}
	}

	// Capture stdout
	origStdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("go: pipe: %w", err), ExitCode: 1}
	}
	os.Stdout = pw

	// Channel to collect captured output
	outCh := make(chan string, 1)
	go func() {
		buf, _ := io.ReadAll(pr)
		outCh <- string(buf)
	}()

	// Call the entrypoint, recovering from panics
	var callResult string
	var callErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				callErr = fmt.Errorf("panic: %v", r)
			}
		}()
		switch fn := sym.(type) {
		case func():
			fn()
		case func() string:
			callResult = fn()
		default:
			callErr = fmt.Errorf("unexpected symbol type %T for %s", sym, entrypoint)
		}
	}()

	// Restore stdout and read captured output
	pw.Close()
	os.Stdout = origStdout
	captured := <-outCh
	pr.Close()

	if callErr != nil {
		// Include any output produced before the error
		return pkg.Result{Output: captured, Err: callErr, ExitCode: 1}
	}

	output := captured
	if callResult != "" {
		output = callResult
	}

	return pkg.Result{Output: output}
}

// LoadPlugin loads a pre-compiled Go plugin (.so) and registers its exported
// functions. The plugin must be built with the same Go version as OmniVM.
// Plugin functions are callable via Eval as "pluginname.FuncName(args)".
func (r *Runtime) LoadPlugin(path string) error {
	p, err := plugin.Open(path)
	if err != nil {
		return fmt.Errorf("go: plugin load %s: %w", path, err)
	}

	// Install bridge if available
	if r.BridgeFn != nil {
		if sym, lookupErr := p.Lookup("SetBridge"); lookupErr == nil {
			sym.(func(func(string, string) string))(r.BridgeFn)
		}
	}

	// Derive plugin name from filename: /path/to/sessvalidator.so → sessvalidator
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	if r.loadedPlugins == nil {
		r.loadedPlugins = make(map[string]*plugin.Plugin)
	}
	r.loadedPlugins[name] = p

	// Call Init() if the plugin exports it (for one-time setup)
	if sym, lookupErr := p.Lookup("Init"); lookupErr == nil {
		if initFn, ok := sym.(func()); ok {
			initFn()
		}
	}

	return nil
}

// CallPlugin calls an exported function on a loaded plugin by name.
// funcName is "pluginname.FuncName", args is the string argument.
// Returns the string result or an error.
func (r *Runtime) CallPlugin(pluginName, funcName, args string) (string, error) {
	p, ok := r.loadedPlugins[pluginName]
	if !ok {
		return "", fmt.Errorf("go: plugin %q not loaded", pluginName)
	}

	sym, err := p.Lookup(funcName)
	if err != nil {
		return "", fmt.Errorf("go: %s.%s: %w", pluginName, funcName, err)
	}

	// Call with panic recovery
	var result string
	var callErr error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				callErr = fmt.Errorf("panic in %s.%s: %v", pluginName, funcName, rec)
			}
		}()
		switch fn := sym.(type) {
		case func(string) string:
			result = fn(args)
		case func() string:
			result = fn()
		case func():
			fn()
		default:
			callErr = fmt.Errorf("go: %s.%s has unsupported type %T", pluginName, funcName, sym)
		}
	}()

	return result, callErr
}

// transformMain renames func main() to func Main() using the Go AST.
func transformMain(src string) (string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("parse error: %w", err)
	}

	found := false
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "main" && fn.Recv == nil {
			fn.Name.Name = "Main"
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("no func main() found")
	}

	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, f); err != nil {
		return "", fmt.Errorf("print error: %w", err)
	}
	return buf.String(), nil
}

// wrapSnippet wraps a code snippet in a plugin-compatible source file.
func wrapSnippet(code string) string {
	if strings.Contains(code, "package ") {
		return code
	}
	return fmt.Sprintf(`package main

import "fmt"

var _ = fmt.Println

func Run() {
%s
}
`, code)
}

// wrapEval wraps an expression in a function that returns its string representation.
func wrapEval(code string) string {
	return fmt.Sprintf(`package main

import "fmt"

func Eval() string {
	return fmt.Sprintf("%%v", %s)
}
`, code)
}

// parsePluginCall parses "pluginname.FuncName(args)" into its components.
// Returns (pluginName, funcName, args, ok).
func parsePluginCall(code string) (string, string, string, bool) {
	code = strings.TrimSpace(code)

	// Find the dot separating plugin name from function name
	dot := strings.IndexByte(code, '.')
	if dot <= 0 {
		return "", "", "", false
	}
	pluginName := code[:dot]

	// Find the opening paren
	rest := code[dot+1:]
	paren := strings.IndexByte(rest, '(')
	if paren <= 0 {
		return "", "", "", false
	}
	funcName := rest[:paren]

	// Extract args (everything between first '(' and last ')')
	if code[len(code)-1] != ')' {
		return "", "", "", false
	}
	argsStr := rest[paren+1 : len(rest)-1]

	// Strip surrounding quotes from single-argument string calls
	argsStr = strings.TrimSpace(argsStr)
	if len(argsStr) >= 2 && argsStr[0] == '"' && argsStr[len(argsStr)-1] == '"' {
		// Unescape basic Go string
		argsStr = argsStr[1 : len(argsStr)-1]
		argsStr = strings.ReplaceAll(argsStr, `\"`, `"`)
		argsStr = strings.ReplaceAll(argsStr, `\\`, `\`)
	}

	return pluginName, funcName, argsStr, true
}

const bridgeShimSource = `package main

var _omnivm_bridge func(string, string) string

// SetBridge is called by the host to install the cross-runtime bridge.
func SetBridge(fn func(string, string) string) {
	_omnivm_bridge = fn
}

type _omnivm_t struct{}

// OmniVM provides cross-runtime bridge access.
// Call OmniVM.Call("python", "1+1") from Go to invoke other runtimes.
var OmniVM _omnivm_t

func (o _omnivm_t) Call(runtime, code string) string {
	if _omnivm_bridge == nil {
		return "ERR:bridge not initialized"
	}
	return _omnivm_bridge(runtime, code)
}
`
