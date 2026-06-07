import { OmniRuntime, RuntimeAffinity, AffinityEvidence } from './types';

/**
 * Known standard/platform module → runtime mappings.
 */
const PYTHON_MODULES = new Set([
  "os", "sys", "math", "json", "re", "datetime", "collections", "itertools",
  "functools", "pathlib", "typing", "dataclasses", "abc", "enum", "io",
  "logging", "unittest", "asyncio", "pickle",
  "subprocess", "threading", "multiprocessing", "socket", "http",
  "urllib", "email", "csv", "xml", "html", "hashlib", "hmac",
  "secrets", "random", "statistics", "decimal", "fractions",
  "copy", "pprint", "textwrap", "difflib", "struct", "codecs",
  "unicodedata", "locale", "gettext", "argparse", "configparser",
  "contextlib", "inspect", "traceback", "warnings", "atexit",
  "signal", "time", "calendar", "sched", "queue", "heapq", "bisect",
  "array", "weakref", "types", "importlib", "pkgutil", "zipimport",
  "compileall", "dis", "ast", "symtable", "token", "keyword",
  "linecache", "tokenize", "tabnanny", "pyclbr",
]);

const PYTHON_PACKAGE_ROOTS = new Set([
  "collections", "concurrent", "ctypes", "distutils", "email", "encodings",
  "html", "http", "importlib", "json", "lib2to3", "logging", "multiprocessing",
  "os", "pydoc_data", "site-packages", "sqlite3", "test", "tkinter",
  "unittest", "urllib", "venv", "wsgiref", "xml", "xmlrpc",
]);

const GO_MODULES = new Set([
  "fmt", "os", "io", "net", "http", "strings", "strconv", "math",
  "sort", "sync", "context", "time", "encoding", "encoding/json",
  "encoding/xml", "encoding/csv", "encoding/base64", "encoding/binary",
  "database/sql", "crypto", "crypto/hmac", "crypto/sha256", "crypto/md5", "crypto/rand",
  "encoding/hex",
  "path", "path/filepath", "regexp", "log", "errors", "reflect",
  "unsafe", "runtime", "testing", "flag", "bufio", "bytes",
  "container/list", "container/heap", "container/ring",
  "html/template", "text/template", "text/scanner",
  "net/http", "net/url", "net/rpc", "net/smtp",
  "os/exec", "os/signal", "os/user",
  "io/ioutil", "io/fs",
  "sync/atomic", "log/slog",
  "image", "image/png", "image/jpeg",
  "archive/tar", "archive/zip",
  "compress/gzip", "compress/zlib",
  "go/ast", "go/parser", "go/token",
  "github.com",  // Go module paths often start with domain
]);

const JS_MODULES = new Set([
  "fs", "path", "http", "https", "crypto", "stream", "events",
  "child_process", "cluster", "os", "url", "querystring",
  "util", "assert", "buffer", "zlib", "tls", "net", "dns",
  "readline", "repl", "vm", "worker_threads", "perf_hooks",
]);

const RUBY_MODULES = new Set([
  "json", "yaml", "csv", "erb", "haml", "slim",
  "webrick", "net/http", "uri", "set", "time", "date", "pathname",
  "stringio", "tempfile", "fileutils", "securerandom", "digest",
]);

const JAVA_MODULES = new Set([
  "java.lang", "java.util", "java.io", "java.nio", "java.net",
  "java.math", "java.time", "java.text", "java.sql", "java.security",
  "java.util.concurrent", "java.util.stream", "java.util.function",
  "java.util.regex", "java.util.logging",
]);

/**
 * Analyze an import path and infer the runtime affinity.
 */
export function analyzeImportPath(
  path: string,
  options?: { preferredRuntime?: OmniRuntime },
): RuntimeAffinity | undefined {
  const evidence: AffinityEvidence = { type: "import", detail: `import "${path}"` };

  if (options?.preferredRuntime === OmniRuntime.JavaScript &&
      (path.startsWith("node:") || JS_MODULES.has(path) || [...JS_MODULES].some(mod => path.startsWith(`${mod}/`)))) {
    return { runtime: OmniRuntime.JavaScript, confidence: "inferred", evidence: [evidence] };
  }
  if (options?.preferredRuntime === OmniRuntime.Python &&
      (PYTHON_MODULES.has(path) || [...PYTHON_PACKAGE_ROOTS].some(mod => path.startsWith(`${mod}.`)))) {
    return { runtime: OmniRuntime.Python, confidence: "inferred", evidence: [evidence] };
  }
  if (options?.preferredRuntime === OmniRuntime.Go &&
      (GO_MODULES.has(path) || path.startsWith("github.com/") || path.startsWith("golang.org/") ||
      path.startsWith("go.uber.org/") || path.startsWith("google.golang.org/"))) {
    return { runtime: OmniRuntime.Go, confidence: "inferred", evidence: [evidence] };
  }

  // Go module paths: quoted strings with / and often domain-like prefixes
  if (path.startsWith("github.com/") || path.startsWith("golang.org/") ||
      path.startsWith("go.uber.org/") ||
      path.startsWith("google.golang.org/")) {
    return { runtime: OmniRuntime.Go, confidence: "definite", evidence: [evidence] };
  }

  // Java package paths: dotted with standard Java/JVM prefixes. Keep `io.*`
  // specific via JAVA_MODULES so Python/Go `io` usage is not claimed as Java.
  if (/^(java|javax|org|com|jakarta)\.[a-z]/.test(path)) {
    return { runtime: OmniRuntime.Java, confidence: "definite", evidence: [evidence] };
  }
  if (JAVA_MODULES.has(path) || [...JAVA_MODULES].some(mod => path.startsWith(`${mod}.`))) {
    return { runtime: OmniRuntime.Java, confidence: "definite", evidence: [evidence] };
  }

  // Go standard library: short unquoted names that match known Go packages
  if (GO_MODULES.has(path)) {
    return { runtime: OmniRuntime.Go, confidence: "inferred", evidence: [evidence] };
  }

  // Python modules
  if (PYTHON_MODULES.has(path)) {
    return { runtime: OmniRuntime.Python, confidence: "inferred", evidence: [evidence] };
  }
  if ([...PYTHON_PACKAGE_ROOTS].some(mod => path.startsWith(`${mod}.`))) {
    return { runtime: OmniRuntime.Python, confidence: "inferred", evidence: [evidence] };
  }

  // Modern Node.js builtin imports, e.g. node:stream or node:stream/web.
  if (path.startsWith("node:")) {
    return { runtime: OmniRuntime.JavaScript, confidence: "inferred", evidence: [evidence] };
  }

  // JS modules (npm-style)
  if (JS_MODULES.has(path) || [...JS_MODULES].some(mod => path.startsWith(`${mod}/`))) {
    return { runtime: OmniRuntime.JavaScript, confidence: "inferred", evidence: [evidence] };
  }
  // Relative imports with .js/.ts/.jsx/.tsx extension
  if (/\.(js|ts|jsx|tsx|mjs|cjs)$/.test(path) || path.startsWith("./") || path.startsWith("../")) {
    return { runtime: OmniRuntime.JavaScript, confidence: "inferred", evidence: [evidence] };
  }
  // Scoped npm packages
  if (path.startsWith("@") && path.includes("/")) {
    return { runtime: OmniRuntime.JavaScript, confidence: "inferred", evidence: [evidence] };
  }

  // Ruby gems (require with dash/underscore naming)
  if (RUBY_MODULES.has(path) || [...RUBY_MODULES].some(mod => path.startsWith(`${mod}/`))) {
    return { runtime: OmniRuntime.Ruby, confidence: "inferred", evidence: [evidence] };
  }

  return undefined;
}

/**
 * Analyze a bare import name (without quotes) for runtime affinity.
 * Python uses `import os`, Go uses `import "fmt"`.
 */
export function analyzeBareImport(name: string): RuntimeAffinity | undefined {
  const evidence: AffinityEvidence = { type: "import", detail: `import ${name}` };

  if (PYTHON_MODULES.has(name)) {
    return { runtime: OmniRuntime.Python, confidence: "inferred", evidence: [evidence] };
  }

  if (GO_MODULES.has(name)) {
    return { runtime: OmniRuntime.Go, confidence: "inferred", evidence: [evidence] };
  }

  if (JS_MODULES.has(name)) {
    return { runtime: OmniRuntime.JavaScript, confidence: "inferred", evidence: [evidence] };
  }

  if (RUBY_MODULES.has(name)) {
    return { runtime: OmniRuntime.Ruby, confidence: "inferred", evidence: [evidence] };
  }

  if (JAVA_MODULES.has(name) || [...JAVA_MODULES].some(mod => name.startsWith(`${mod}.`))) {
    return { runtime: OmniRuntime.Java, confidence: "inferred", evidence: [evidence] };
  }

  return undefined;
}
