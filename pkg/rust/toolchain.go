package rust

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ABIRev is the bridge ABI revision this host speaks. It must match
// omnivm_rs::abi::ABI_REV; it is part of every artifact cache key, so a host
// upgrade invalidates incompatible artifacts instead of silently loading them
// (the same-toolchain invariant).
const ABIRev = 1

const pluginCacheDir = "/tmp/omnivm-plugins"

var (
	toolchainOnce sync.Once
	toolchain     *Toolchain
	toolchainErr  error
)

// Toolchain holds resolved paths for the pinned Rust toolchain and the
// omnivm cargo workspace shipped with the image.
//
// Everything builds inside ONE cargo workspace (WorkspaceDir): the support
// dylib, the prelude, and every generated unit. One Cargo.lock and one unit
// graph mean cargo assigns omnivm_rs the same -C metadata for every build —
// a unit can never silently rebuild the support dylib with symbol hashes
// that differ from the copy already dlopen'd in this process.
type Toolchain struct {
	CargoBin     string
	RustcVersion string
	// WorkspaceDir is the cargo workspace root (runtime/rust in the tree,
	// /opt/omnivm-rust in the image). It must be writable: generated units
	// build as transient workspace members under units/.
	WorkspaceDir string
	// CrateDir is the omnivm support crate (WorkspaceDir/omnivm).
	CrateDir string
	// TargetDir is the shared cargo target dir (support dylib + prelude +
	// units), so a unit using only prelude crates compiles user code only.
	TargetDir string
	// RustLibDir is the sysroot directory holding libstd-*.so for dynamic
	// linking (prefer-dynamic).
	RustLibDir string
	// SupportDylib is the built libomnivm_rs.so path.
	SupportDylib string
	// LockHash is the SHA256 of the workspace Cargo.lock — part of the
	// cache key so image upgrades reset the cache by design.
	LockHash string
	// supportHash is supportSourceHash() captured ONCE at init: unit builds
	// mutate the live Cargo.lock (transient member edges), so re-reading it
	// per key would cascade recompiles.
	supportHash string

	// buildMu serializes unit builds (cargo locks the target dir anyway;
	// this keeps member churn in units/ orderly).
	buildMu sync.Mutex
}

// GetToolchain locates (and on first use, builds) the Rust support toolchain.
func GetToolchain() (*Toolchain, error) {
	toolchainOnce.Do(func() {
		toolchain, toolchainErr = initToolchain()
	})
	return toolchain, toolchainErr
}

func initToolchain() (*Toolchain, error) {
	cargo, err := exec.LookPath("cargo")
	if err != nil {
		return nil, fmt.Errorf("rust: cargo not found in PATH: %w", err)
	}

	rustcOut, err := exec.Command("rustc", "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("rust: rustc --version: %w", err)
	}
	rustcVersion := strings.TrimSpace(string(rustcOut))

	sysrootOut, err := exec.Command("rustc", "--print", "sysroot").Output()
	if err != nil {
		return nil, fmt.Errorf("rust: rustc --print sysroot: %w", err)
	}
	sysroot := strings.TrimSpace(string(sysrootOut))

	hostOut, err := exec.Command("rustc", "--print", "host-tuple").Output()
	host := ""
	if err == nil {
		host = strings.TrimSpace(string(hostOut))
	} else {
		// Older rustc: derive from version output ("host: ..." via -vV).
		vv, vvErr := exec.Command("rustc", "-vV").Output()
		if vvErr != nil {
			return nil, fmt.Errorf("rust: cannot determine host tuple: %w", vvErr)
		}
		for _, line := range strings.Split(string(vv), "\n") {
			if rest, ok := strings.CutPrefix(line, "host: "); ok {
				host = strings.TrimSpace(rest)
			}
		}
	}
	rustLibDir := filepath.Join(sysroot, "lib", "rustlib", host, "lib")

	workspaceDir, err := findWorkspaceDir()
	if err != nil {
		return nil, err
	}

	targetDir := os.Getenv("OMNIVM_RUST_TARGET_DIR")
	if targetDir == "" {
		targetDir = filepath.Join(workspaceDir, "target")
	}

	tc := &Toolchain{
		CargoBin:     cargo,
		RustcVersion: rustcVersion,
		WorkspaceDir: workspaceDir,
		CrateDir:     filepath.Join(workspaceDir, "omnivm"),
		TargetDir:    targetDir,
		RustLibDir:   rustLibDir,
	}

	if err := tc.ensureSupportDylib(); err != nil {
		return nil, err
	}

	lock, err := os.ReadFile(filepath.Join(workspaceDir, "Cargo.lock"))
	if err != nil {
		return nil, fmt.Errorf("rust: workspace Cargo.lock: %w", err)
	}
	sum := sha256.Sum256(lock)
	tc.LockHash = hex.EncodeToString(sum[:])
	tc.supportHash = tc.supportSourceHash()

	return tc, nil
}

func findWorkspaceDir() (string, error) {
	if dir := os.Getenv("OMNIVM_RUST_WORKSPACE_DIR"); dir != "" {
		return dir, nil
	}
	candidates := []string{"/opt/omnivm-rust"}
	// Development fallback: locate runtime/rust relative to this file.
	if _, thisFile, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Join(filepath.Dir(thisFile), "..", "..", "runtime", "rust"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, "runtime", "rust"),
			filepath.Join(wd, "..", "..", "runtime", "rust"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "omnivm", "Cargo.toml")); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("rust: omnivm workspace not found (set OMNIVM_RUST_WORKSPACE_DIR)")
}

// RustFlags is the one canonical flag set for every Rust build sharing the
// target dir (support dylib, prelude, units). It MUST be identical across
// them: cargo fingerprints include RUSTFLAGS, so divergent flags would
// rebuild omnivm_rs with new symbol hashes while the old inode is already
// loaded in the process. The Dockerfile prebuild uses the same construction.
func (tc *Toolchain) RustFlags() string {
	return strings.Join([]string{
		"-C", "prefer-dynamic",
		"-C", "link-arg=-Wl,-rpath," + filepath.Join(tc.TargetDir, "release"),
		"-C", "link-arg=-Wl,-rpath," + filepath.Join(tc.TargetDir, "release", "deps"),
		"-C", "link-arg=-Wl,-rpath," + tc.RustLibDir,
	}, " ")
}

// withBuildLock serializes workspace builds ACROSS PROCESSES (prefork
// workers share /opt/omnivm-rust): transient unit members must not be
// visible to another process's --workspace invocation mid-lifecycle, and
// cargo's own target-dir lock does not cover member dirs.
func (tc *Toolchain) withBuildLock(fn func() error) error {
	lockPath := filepath.Join(tc.WorkspaceDir, ".omnivm-build.flock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		// Read-only workspace: fall back to the in-process mutex only.
		return fn()
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fn()
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (tc *Toolchain) cargoEnv() []string {
	return append(os.Environ(),
		"CARGO_TARGET_DIR="+tc.TargetDir,
		"RUSTFLAGS="+tc.RustFlags(),
	)
}

// ensureSupportDylib builds libomnivm_rs.so. The build always runs (a no-op
// when the image prebuild is fresh) and always from the workspace root:
//   - freshness BEFORE first dlopen matters because rebuilding the dylib
//     after it is loaded leaves the process on the stale copy silently
//     (content changes do not change Rust symbol hashes);
//   - --workspace matters because cargo resolver-2 feature unification is
//     computed per build invocation — a narrower package selection would
//     rebuild omnivm_rs with different feature sets (and symbol hashes) than
//     the image prebuild. The prelude member pins the feature union.
// supportSourceHash fingerprints the support crate by CONTENT (sources +
// manifest + lockfile). Cargo's own mtime fingerprinting has proven
// unreliable when sources are refreshed over a prebuilt image target dir
// (observed: identical containers disagreeing, and the prelude rebuilding
// against a stale omnivm_rs rlib), so freshness is decided here.
func (tc *Toolchain) supportSourceHash() string {
	h := sha256.New()
	paths := []string{
		filepath.Join(tc.WorkspaceDir, "Cargo.lock"),
		filepath.Join(tc.CrateDir, "Cargo.toml"),
	}
	srcDir := filepath.Join(tc.CrateDir, "src")
	if entries, err := os.ReadDir(srcDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".rs") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			paths = append(paths, filepath.Join(srcDir, n))
		}
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// supportSourcesNewerThan reports whether any support-crate source file is
// newer than the given artifact (the no-stamp freshness heuristic).
func (tc *Toolchain) supportSourcesNewerThan(artifact string) bool {
	st, err := os.Stat(artifact)
	if err != nil {
		return true
	}
	built := st.ModTime()
	paths := []string{filepath.Join(tc.WorkspaceDir, "Cargo.lock"), filepath.Join(tc.CrateDir, "Cargo.toml")}
	if entries, err := os.ReadDir(filepath.Join(tc.CrateDir, "src")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				paths = append(paths, filepath.Join(tc.CrateDir, "src", e.Name()))
			}
		}
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(built) {
			return true
		}
	}
	return false
}

func (tc *Toolchain) ensureSupportDylib() error {
	dylib := filepath.Join(tc.TargetDir, "release", "libomnivm_rs.so")
	stamp := filepath.Join(tc.TargetDir, ".omnivm-support-src-hash")
	current := tc.supportSourceHash()
	var out []byte
	forced := false
	err := tc.withBuildLock(func() error {
		old, readErr := os.ReadFile(stamp)
		if readErr != nil {
			// No stamp (image prebuild, fresh checkout): force only when
			// sources are demonstrably newer than the built dylib —
			// pristine images keep their fast path.
			forced = tc.supportSourcesNewerThan(dylib)
		} else if strings.TrimSpace(string(old)) != current {
			// Content changed: force the recompile cargo's mtime
			// fingerprinting sometimes misses over prebuilt target dirs.
			forced = true
		}
		if forced {
			// `cargo clean -p` has proven unreliable here; removing the
			// fingerprint dirs directly guarantees re-evaluation.
			if matches, _ := filepath.Glob(filepath.Join(tc.TargetDir, "release", ".fingerprint", "omnivm_rs-*")); matches != nil {
				for _, m := range matches {
					os.RemoveAll(m)
				}
			}
		}
		cmd := exec.Command(tc.CargoBin, "build", "--release", "--workspace")
		cmd.Dir = tc.WorkspaceDir
		cmd.Env = tc.cargoEnv()
		started := time.Now()
		var buildErr error
		out, buildErr = cmd.CombinedOutput()
		fmt.Fprintf(os.Stderr, "[rust] support dylib build: forced=%v err=%v took=%s\n", forced, buildErr != nil, time.Since(started).Round(time.Millisecond))
		if buildErr == nil {
			_ = os.WriteFile(stamp, []byte(current+"\n"), 0o644)
		}
		return buildErr
	})
	if err != nil {
		// A read-only workspace with a prebuilt dylib is still usable — but
		// say so loudly: a silently-stale dylib turns every content change
		// into a phantom old-semantics bug.
		if _, statErr := os.Stat(dylib); statErr == nil {
			fmt.Fprintf(os.Stderr, "[rust] WARNING: support dylib rebuild failed; using prebuilt %s\n[rust] build error: %s\n",
				dylib, strings.TrimSpace(string(out)))
			tc.SupportDylib = dylib
			return nil
		}
		return fmt.Errorf("rust: building omnivm support dylib: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if _, err := os.Stat(dylib); err != nil {
		return fmt.Errorf("rust: support dylib missing after build: %w", err)
	}
	tc.SupportDylib = dylib
	return nil
}

// UnitCacheKey computes the artifact cache key: source + bridge ABI revision
// + toolchain + prelude lockfile. Same scheme as the Go plugin cache.
func (tc *Toolchain) UnitCacheKey(source string, exports []string) string {
	sorted := append([]string(nil), exports...)
	sort.Strings(sorted)
	material := strings.Join([]string{
		"rust",
		fmt.Sprintf("abi:%d", ABIRev),
		tc.RustcVersion,
		tc.LockHash,
		// Support-crate CONTENT: a unit .so links against omnivm_rs symbols,
		// so a crate change must invalidate cached units (stale artifacts
		// dlopen-fail with undefined symbols — found during the
		// boundary-generics round). Captured at init: see supportHash.
		tc.supportHash,
		strings.Join(sorted, ","),
		source,
	}, "\x00")
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// pinnedCrates maps crate roots inferred from `use` statements to pinned
// dependency specs. The prelude policy: rustls over native-tls throughout
// (native-tls drags in openssl-sys probing the system), and a deliberate
// feature union so commonly-needed features are "free" (no cold rebuild).
var pinnedCrates = map[string]string{
	"serde":      `serde = { version = "1", features = ["derive"] }`,
	"serde_json": `serde_json = "1"`,
	"tokio":      ``, // re-exported by the omnivm crate; one runtime per process
	"reqwest":    `reqwest = { version = "0.12", default-features = false, features = ["json", "rustls-tls"] }`,
	"rayon":      `rayon = "1"`,
	"polars":     `polars = { version = "0.46", features = ["lazy", "serde", "ipc", "ipc_streaming"] }`,
	"anyhow":     `anyhow = "1"`,
	"thiserror":  `thiserror = "2"`,
	"itertools":  `itertools = "0.14"`,
	"regex":      `regex = "1"`,
	"chrono":     `chrono = { version = "0.4", features = ["serde"] }`,
	"futures":    `futures = "0.3"`,
	"rand":       `rand = "0.9"`,
	"once_cell":  `once_cell = "1"`,
	"ndarray":    `ndarray = "0.16"`,
	"arrow":      `arrow = "55"`,
	"axum":       `axum = "0.8"`,
	"hyper":      `hyper = { version = "1", features = ["full"] }`,
	"bytes":      `bytes = "1"`,
	"url":        `url = "2"`,
	"uuid":       `uuid = { version = "1", features = ["v4", "serde"] }`,
	"base64":     `base64 = "0.22"`,
	"sha2":       `sha2 = "0.10"`,
	"csv":        `csv = "1"`,
	"sqlx":       `sqlx = { version = "0.8", default-features = false, features = ["runtime-tokio", "sqlite", "macros"] }`,
	"tower":      `tower = "0.5"`,
}

// crate roots that never become inferred Cargo dependencies: language
// builtins, the omnivm crate itself, tokio (re-exported by omnivm so one
// runtime exists per process), and serde/serde_json (always declared by the
// generated Cargo.toml).
var builtinRoots = map[string]bool{
	"std": true, "core": true, "alloc": true,
	"crate": true, "self": true, "super": true,
	"omnivm": true, "tokio": true,
	"serde": true, "serde_json": true,
}

var useRootRE = regexp.MustCompile(`(?m)^\s*(?:pub\s+)?use\s+(?:::)?([A-Za-z_][A-Za-z0-9_]*)`)

var tokioPathRE = regexp.MustCompile(`\btokio::`)

// injectRuntimeAliases makes real-world `use tokio::...` / `tokio::spawn`
// paths resolve against the omnivm re-export (ONE tokio per process — the
// version is pinned by the image): a crate-root alias is enough under
// uniform paths. Same for the `log` facade.
func injectRuntimeAliases(source string) string {
	var aliases []string
	if tokioPathRE.MatchString(source) && !strings.Contains(source, "use omnivm::tokio") {
		aliases = append(aliases, "use omnivm::tokio;")
	}
	if regexp.MustCompile("\\blog::").MatchString(source) && !strings.Contains(source, "use omnivm::log") {
		aliases = append(aliases, "use omnivm::log;")
	}
	if strings.Contains(source, "futures_core::") && !strings.Contains(source, "use omnivm::futures_core") {
		aliases = append(aliases, "use omnivm::futures_core;")
	}
	// `alloc` is a builtin root but not in the edition-2021 extern prelude.
	if regexp.MustCompile(`\balloc::`).MatchString(source) && !strings.Contains(source, "extern crate alloc") {
		aliases = append(aliases, "extern crate alloc;")
	}
	if len(aliases) == 0 {
		return source
	}
	return "// injected by omnivm: these crates are re-exported by the support dylib\n" +
		"#[allow(unused_imports)]\n" + strings.Join(aliases, "\n#[allow(unused_imports)]\n") + "\n\n" + source
}

// InferDependencies maps `use` statement crate roots to Cargo.toml dependency
// lines, with the crates.io hyphen/underscore mapping and versions pinned by
// the in-image table. Mirrors Go import inference.
var localModRE = regexp.MustCompile(`(?m)^\s*(?:#\[[^\]]*\]\s*)*(?:pub(?:\([^)]*\))?\s+)?mod\s+([A-Za-z_][A-Za-z0-9_]*)`)

func InferDependencies(source string) []string {
	// Locally-declared modules are not crates: `mod private { ... }` +
	// `use private::...` must not pin a crates.io package named "private".
	localMods := map[string]bool{}
	for _, match := range localModRE.FindAllStringSubmatch(source, -1) {
		localMods[match[1]] = true
	}
	seen := map[string]bool{}
	var deps []string
	for _, match := range useRootRE.FindAllStringSubmatch(source, -1) {
		root := match[1]
		if builtinRoots[root] || localMods[root] || seen[root] {
			continue
		}
		// `use Command::*;` / `use ParseError::EndOfStream;` import LOCAL
		// type variants — crates.io package names are lowercase, so an
		// uppercase-first root is never a crate (dogfood finding).
		if root[0] >= 'A' && root[0] <= 'Z' {
			continue
		}
		seen[root] = true
		if line, ok := pinnedCrates[root]; ok {
			if line != "" {
				deps = append(deps, line)
			}
			continue
		}
		// Unknown crate: crates.io package names use hyphens where Rust
		// paths use underscores; let cargo resolve the newest version.
		pkg := strings.ReplaceAll(root, "_", "-")
		if pkg == root {
			deps = append(deps, fmt.Sprintf("%s = \"*\"", root))
		} else {
			deps = append(deps, fmt.Sprintf("%s = { package = %q, version = \"*\" }", root, pkg))
		}
	}
	sort.Strings(deps)
	return deps
}

// enhanceCompileError adds "add the crate" style hints to rustc output.
func enhanceCompileError(out string) string {
	if strings.Contains(out, "failed to get ") || strings.Contains(out, "network failure") ||
		strings.Contains(out, "Unable to update registry") {
		out += "\nhint: this crate is not in the baked pin set and the registry is unreachable; offline images fail closed on unknown crates — add the crate to the prelude pin set or build online once"
	}
	if strings.Contains(out, "error[E0432]") || strings.Contains(out, "error[E0433]") {
		out += "\nhint: unresolved imports usually mean the crate is not in the pinned prelude; supported prelude crates: " + strings.Join(sortedPinnedCrateNames(), ", ")
	}
	// Gradual typing: a missing method/operator/index/trait-bound on
	// omnivm::Dyn means the author is treating a dynamically typed parameter
	// as a concrete native type.
	if strings.Contains(out, "Dyn") &&
		(strings.Contains(out, "error[E0599]") || strings.Contains(out, "error[E0369]") ||
			strings.Contains(out, "error[E0608]") || strings.Contains(out, "error[E0277]")) {
		out += "\nhint: this value is gradually typed (omnivm::Dyn) — annotate the parameter with a concrete type to use native methods"
	}
	return out
}

func sortedPinnedCrateNames() []string {
	names := make([]string, 0, len(pinnedCrates))
	for name := range pinnedCrates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BuildUnit compiles a complete Rust compilation unit (user items + export
// shims) into a cached cdylib and returns the artifact path. The unit builds
// as a transient member of the omnivm workspace (units/u<hash>), sharing the
// workspace lock and target dir, then the member dir is removed — the cached
// artifact under /tmp/omnivm-plugins is what gets loaded.
func (tc *Toolchain) BuildUnit(source string, exports []string) (string, error) {
	return tc.BuildUnitMapped(source, exports, nil)
}

// BuildUnitMapped is BuildUnit plus an optional source map: when the unit
// fails to compile and a map is present, rustc diagnostics are rewritten to
// point at the original .poly coordinates instead of the generated lib.rs
// (which the user has never seen). A nil/empty map keeps current behavior.
func (tc *Toolchain) BuildUnitMapped(rawSource string, exports []string, smap *SourceMap) (string, error) {
	source := injectRuntimeAliases(rawSource)
	// Lines the alias injection prepended before the op's unit source; the
	// source map's unit_line coordinates are relative to the op source.
	aliasLines := strings.Count(source[:len(source)-len(rawSource)], "\n")
	hash := tc.UnitCacheKey(source, exports)
	soPath := filepath.Join(pluginCacheDir, "rust-"+hash+".so")
	if _, err := os.Stat(soPath); err == nil {
		return soPath, nil
	}
	if err := os.MkdirAll(pluginCacheDir, 0o755); err != nil {
		return "", err
	}

	tc.buildMu.Lock()
	defer tc.buildMu.Unlock()
	if _, err := os.Stat(soPath); err == nil {
		return soPath, nil
	}

	unitName := "u" + hash[:16]
	pkgName := "omnivm-unit-" + unitName
	libName := "omnivm_" + unitName
	memberDir := filepath.Join(tc.WorkspaceDir, "units", unitName)
	err := tc.withBuildLock(func() error {
		// Another process may have published while we waited for the lock.
		if _, statErr := os.Stat(soPath); statErr == nil {
			return nil
		}
		return tc.buildUnitLocked(source, unitName, pkgName, libName, memberDir, soPath, smap, aliasLines)
	})
	if err != nil {
		return "", err
	}
	return soPath, nil
}

func (tc *Toolchain) buildUnitLocked(source, unitName, pkgName, libName, memberDir, soPath string, smap *SourceMap, aliasLines int) error {
	defer os.RemoveAll(memberDir)

	deps := InferDependencies(source)
	var depBlock strings.Builder
	for _, dep := range deps {
		depBlock.WriteString(dep)
		depBlock.WriteByte('\n')
	}

	cargoToml := fmt.Sprintf(`[package]
name = %q
version = "0.1.0"
edition = "2021"

[lib]
name = %q
crate-type = ["cdylib"]
path = "src/lib.rs"

[dependencies]
omnivm = { package = "omnivm_rs", path = "../../omnivm" }
serde = { version = "1", features = ["derive"] }
serde_json = "1"
%s`, pkgName, libName, depBlock.String())

	if err := os.MkdirAll(filepath.Join(memberDir, "src"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(memberDir, "Cargo.toml"), []byte(cargoToml), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(memberDir, "src", "lib.rs"), []byte(source), 0o644); err != nil {
		return err
	}

	// --workspace keeps feature unification identical across every build
	// (see ensureSupportDylib); the transient member is what adds this unit
	// to the set. Stale members were removed after their builds, so this
	// compiles the new unit only.
	fmt.Fprintf(os.Stderr, "[rust] compiling unit %s (cold cache)...\n", unitName)
	buildStart := time.Now()
	// With a source map, build with --message-format=json so failure
	// diagnostics carry structured spans we can rewrite to .poly coordinates
	// (the JSON goes to stdout and is discarded on success; cargo's own
	// human noise stays on stderr). Without a map: current behavior.
	mapped := smap != nil && len(smap.Entries) > 0
	args := []string{"build", "--release", "--workspace"}
	if mapped {
		args = append(args, "--message-format=json")
	}
	cmd := exec.Command(tc.CargoBin, args...)
	cmd.Dir = tc.WorkspaceDir
	cmd.Env = tc.cargoEnv()
	if mapped {
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := renderMappedCompileError(stdout.String(), stderr.String(), smap, aliasLines, source)
			return fmt.Errorf("rust compilation failed:\n%s", enhanceCompileError(strings.TrimSpace(msg)))
		}
	} else if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rust compilation failed:\n%s", enhanceCompileError(strings.TrimSpace(string(out))))
	}
	fmt.Fprintf(os.Stderr, "[rust] unit %s compiled in %s\n", unitName, time.Since(buildStart).Round(time.Millisecond))

	built := filepath.Join(tc.TargetDir, "release", "lib"+libName+".so")
	data, err := os.ReadFile(built)
	if err != nil {
		return fmt.Errorf("rust: built unit missing: %w", err)
	}
	// Atomic publish: concurrent processes never observe a partial artifact.
	tmpArtifact := soPath + ".tmp." + unitName
	if err := os.WriteFile(tmpArtifact, data, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpArtifact, soPath); err != nil {
		os.Remove(tmpArtifact)
		return err
	}
	// Record the resolution that produced this artifact (reproducibility
	// audit trail; non-pinned crates resolve once per image lifetime).
	if lock, lockErr := os.ReadFile(filepath.Join(tc.WorkspaceDir, "Cargo.lock")); lockErr == nil {
		_ = os.WriteFile(soPath+".lock", lock, 0o644)
	}
	tc.pruneArtifactCache()
	return nil
}

// Cache GC limits (overridable for tests).
var (
	// CacheMaxArtifacts bounds /tmp/omnivm-plugins rust artifacts (LRU by
	// mtime; currently-loaded units are never pruned).
	CacheMaxArtifacts = 256
)

func (tc *Toolchain) pruneArtifactCache() {
	entries, err := filepath.Glob(filepath.Join(pluginCacheDir, "rust-*.so"))
	if err != nil || len(entries) <= CacheMaxArtifacts {
		return
	}
	type aged struct {
		path string
		mod  int64
	}
	var candidates []aged
	for _, path := range entries {
		if _, loaded := loadedUnitPath(path); loaded {
			continue
		}
		st, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		candidates = append(candidates, aged{path: path, mod: st.ModTime().UnixNano()})
	}
	excess := len(entries) - CacheMaxArtifacts
	if excess <= 0 || len(candidates) == 0 {
		return
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mod < candidates[j].mod })
	for i := 0; i < excess && i < len(candidates); i++ {
		_ = os.Remove(candidates[i].path)
	}
}
