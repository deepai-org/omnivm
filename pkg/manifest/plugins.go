package manifest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"plugin"
	"regexp"
	"runtime"
	"strings"
)

const pluginCacheDir = "/tmp/omnivm-plugins"

// loadedPlugins tracks plugins already opened in this process.
// Go's plugin.Open panics/errors if the same .so is opened twice.
var loadedPlugins = map[string]*plugin.Plugin{}

// compileGoPlugin handles func_def ops with bodyRuntime:"go" and a source field.
// It compiles the Go source as a plugin, loads exports, and registers them
// in the executor's goFuncs registry.
func (e *Executor) compileGoPlugin(op *Op) (interface{}, error) {
	hash := sha256Hash(op.Source)
	soPath := filepath.Join(pluginCacheDir, hash+".so")

	// Check if already loaded in this process
	plug, alreadyLoaded := loadedPlugins[soPath]

	if !alreadyLoaded {
		// Check compile cache
		if _, err := os.Stat(soPath); os.IsNotExist(err) {
			if err := compilePlugin(op.Source, soPath); err != nil {
				return nil, fmt.Errorf("go plugin compile: %w", err)
			}
		}

		// Load the plugin
		var err error
		plug, err = plugin.Open(soPath)
		if err != nil {
			return nil, fmt.Errorf("go plugin open: %w", err)
		}
		loadedPlugins[soPath] = plug
	}

	// If the plugin has an Init function and requires dependencies, call it
	if len(op.Requires) > 0 {
		initSym, err := plug.Lookup("Init")
		if err == nil {
			if initFn, ok := initSym.(func(map[string]interface{})); ok {
				deps := make(map[string]interface{})
				for _, req := range op.Requires {
					if fn, ok := e.goFuncs[req]; ok {
						deps[req] = fn
					}
				}
				initFn(deps)
			}
		}
	}

	// Register exported symbols under both their Go name and the manifest func name
	for _, name := range op.Exports {
		sym, err := plug.Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("go plugin: export %q not found: %w", name, err)
		}
		e.goFuncs[name] = sym
	}

	// Also register the manifest function name → first export mapping
	// so HandleCall can find it by the manifest name (e.g. "shard_for" → ShardFor)
	if op.Name != "" && len(op.Exports) > 0 {
		if _, exists := e.goFuncs[op.Name]; !exists {
			e.goFuncs[op.Name] = e.goFuncs[op.Exports[0]]
		}
	}

	// Register stubs in each runtime so guest code can call this function
	if op.Name != "" {
		params := make([]string, len(op.Params))
		for i, p := range op.Params {
			params[i] = p.Name
		}
		fd := &FuncDef{Name: op.Name, Params: op.Params}
		if err := e.registerStubs(fd); err != nil {
			return nil, fmt.Errorf("go plugin stubs: %w", err)
		}
	}

	return nil, nil
}

// compilePlugin writes Go source to a temp directory and builds it as a plugin.
func compilePlugin(source, outputPath string) error {
	if err := os.MkdirAll(pluginCacheDir, 0o755); err != nil {
		return err
	}

	// Create temp directory for compilation
	tmpDir, err := os.MkdirTemp("", "omnivm-plugin-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Rewrite package declaration to "main" (Go plugins require package main)
	pkgRe := regexp.MustCompile(`(?m)^package\s+\w+`)
	source = pkgRe.ReplaceAllString(source, "package main")

	// Write source
	srcPath := filepath.Join(tmpDir, "plugin.go")
	if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
		return err
	}

	// Write go.mod — each plugin needs a unique module name
	// so Go's plugin system treats them as distinct packages.
	modName := fmt.Sprintf("omnivm-plugin-%s", filepath.Base(outputPath[:len(outputPath)-3]))
	goVer := strings.TrimPrefix(runtime.Version(), "go")
	if parts := strings.SplitN(goVer, ".", 3); len(parts) >= 2 {
		goVer = parts[0] + "." + parts[1]
	}
	modContent := fmt.Sprintf("module %s\n\ngo %s\n", modName, goVer)
	modPath := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(modPath, []byte(modContent), 0o644); err != nil {
		return err
	}

	// Build plugin
	cmd := exec.Command("go", "build", "-buildmode=plugin", "-o", outputPath, ".")
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %s: %w", string(out), err)
	}

	return nil
}

func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}
