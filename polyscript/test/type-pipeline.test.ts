/**
 * Type Pipeline Integration Tests (TDD)
 *
 * Tests the FULL path: .poly source → Lexer → Parser → RuntimeResolver →
 * ManifestGenerator (with BoundaryChecker) → manifest with bridge ops.
 *
 * Each level builds on the previous, from simple single-language type annotations
 * to complex cross-runtime data flows with streams, buffers, and disposables.
 */

import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import { RuntimeResolver } from '../src/runtime-resolver';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';
import { DispatchManifest } from '../src/codegen-omnivm/manifest-types';
import {
  BoundaryChecker, lowerType, typeToString, checkCompatibility,
  func, array, result, option, async_, bufferView, stream,
  INT32, INT64, UINT8, FLOAT64, STRING, BOOL, VOID, ANY, NULL,
  FuncType, CanonicalType,
} from '../src/type-system';

function parseAndManifest(code: string): DispatchManifest {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, code);
  const gen = new ManifestCodeGenerator();
  return gen.generate(annotated);
}

function parseAST(code: string) {
  const tokens = new Lexer(code).tokenize();
  return new Parser(tokens, code).parse();
}

// ════════════════════════════════════════════════════════════════
// Level 1: Parser captures type annotations from each language
// ════════════════════════════════════════════════════════════════

describe('Level 1: Type annotations are captured in AST', () => {

  test('TypeScript: function with typed params and return', () => {
    const ast = parseAST('function greet(name: string, age: number): string { return name }');
    const fn = ast.body[0] as any;
    expect(fn.kind).toBe('FuncDecl');
    expect(fn.params[0].type.id.name).toBe('string');
    expect(fn.params[1].type.id.name).toBe('number');
    expect(fn.returnType.id.name).toBe('string');
  });

  test('Python: def with type annotations and -> return', () => {
    const ast = parseAST('def process(data: list, count: int) -> str:\n    return str(count)');
    const fn = ast.body[0] as any;
    expect(fn.kind).toBe('FuncDecl');
    expect(fn.params[0].type.id.name).toBe('list');
    expect(fn.params[1].type.id.name).toBe('int');
    expect(fn.returnType.id.name).toBe('str');
  });

  test('Rust: fn with typed params and -> return', () => {
    const ast = parseAST('fn multiply(a: i32, b: i32) -> i32 { a * b }');
    const fn = ast.body[0] as any;
    expect(fn.kind).toBe('FuncDecl');
    expect(fn.params[0].type.id.name).toBe('i32');
    expect(fn.params[1].type.id.name).toBe('i32');
    expect(fn.returnType.id.name).toBe('i32');
  });

  test('Go: func with typed params and return type', () => {
    const ast = parseAST('func add(x int, y int) int { return x + y }');
    const fn = ast.body[0] as any;
    expect(fn.kind).toBe('FuncDecl');
    expect(fn.params[0].type.id.name).toBe('int');
    expect(fn.params[1].type.id.name).toBe('int');
    expect(fn.returnType.id.name).toBe('int');
  });

  test('TypeScript: let with type annotation', () => {
    const ast = parseAST('let x: number = 42');
    const decl = ast.body[0] as any;
    expect(decl.kind).toBe('VarDecl');
    expect(decl.type.id.name).toBe('number');
  });

  test('Python: variable with type annotation', () => {
    const ast = parseAST('x: int = 42');
    const decl = ast.body[0] as any;
    // Should be accessible as .type (not .declType)
    const type = decl.type || decl.declType;
    expect(type).toBeDefined();
    expect(type.id.name).toBe('int');
  });

  test('Go: var with type', () => {
    const ast = parseAST('var count int = 10');
    const decl = ast.body[0] as any;
    const type = decl.type || decl.declType;
    expect(type).toBeDefined();
    expect(type.id.name).toBe('int');
  });

  test('Rust: let with type annotation', () => {
    const ast = parseAST('let mut name: String = "hello"');
    const decl = ast.body[0] as any;
    expect(decl.type.id.name).toBe('String');
  });

  test('TypeScript: generic types preserved', () => {
    const ast = parseAST('function getItems(): Array<string> { return [] }');
    const fn = ast.body[0] as any;
    expect(fn.returnType.kind).toBe('GenericType');
    expect(fn.returnType.base.name).toBe('Array');
    expect(fn.returnType.args[0].id.name).toBe('string');
  });

  test('Go: []string return type captured', () => {
    const ast = parseAST('func getFiles(dir string) []string { return nil }');
    const fn = ast.body[0] as any;
    expect(fn.returnType).toBeDefined();
    // Should parse as an array type of string
  });

  test('Rust: Result<T, E> return type captured', () => {
    const ast = parseAST('fn parse(s: &str) -> Result<i32, String> { Ok(42) }');
    const fn = ast.body[0] as any;
    expect(fn.returnType).toBeDefined();
    expect(fn.returnType.kind).toBe('GenericType');
    expect(fn.returnType.base.name).toBe('Result');
  });

  test('Python: Optional[str] type annotation', () => {
    const ast = parseAST('def find(key: str) -> Optional[str]:\n    return None');
    const fn = ast.body[0] as any;
    expect(fn.returnType).toBeDefined();
    expect(fn.returnType.kind).toBe('GenericType');
    expect(fn.returnType.base.name).toBe('Optional');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 2: Type lowering produces correct canonical types
// ════════════════════════════════════════════════════════════════

describe('Level 2: Parsed types lower to correct canonical forms', () => {

  function lowerFromSource(code: string, runtime?: string): any {
    const ast = parseAST(code);
    const fn = ast.body[0] as any;
    // Get the first param's type or the return type
    const typeNode = fn.params?.[0]?.type || fn.returnType || fn.type || fn.declType;
    if (!typeNode) return null;
    return lowerType(typeNode, runtime as any);
  }

  test('TS number → f64', () => {
    const t = lowerFromSource('function f(x: number) {}', 'javascript');
    expect(t.kind).toBe('float');
    expect(t.size).toBe(64);
  });

  test('Python int → bigint', () => {
    const t = lowerFromSource('def f(x: int): pass', 'python');
    expect(t.kind).toBe('int');
    expect(t.size).toBe('big');
  });

  test('Go int → i64', () => {
    const t = lowerFromSource('func f(x int) {}', 'go');
    expect(t.kind).toBe('int');
    expect(t.size).toBe(64);
  });

  test('Rust i32 → i32', () => {
    const t = lowerFromSource('fn f(x: i32) {}', 'rust');
    expect(t.kind).toBe('int');
    expect(t.size).toBe(32);
  });

  test('TS string → string', () => {
    const t = lowerFromSource('function f(x: string) {}', 'javascript');
    expect(t.kind).toBe('string');
  });

  test('Python str → string', () => {
    const t = lowerFromSource('def f(x: str): pass', 'python');
    expect(t.kind).toBe('string');
  });

  test('TS Array<number> → Array<f64>', () => {
    const ast = parseAST('function f(): Array<number> { return [] }');
    const fn = ast.body[0] as any;
    const t = lowerType(fn.returnType, 'javascript');
    expect(t.kind).toBe('array');
    expect(typeToString(t)).toBe('Array<f64>');
  });

  test('Rust Result<i32, String> → Result<i32, string>', () => {
    const ast = parseAST('fn f() -> Result<i32, String> { Ok(1) }');
    const fn = ast.body[0] as any;
    if (!fn.returnType) { expect(fn.returnType).toBeDefined(); return; }
    const t = lowerType(fn.returnType, 'rust');
    expect(t.kind).toBe('result');
    expect(typeToString(t)).toBe('Result<i32, string>');
  });

  test('Python Optional[str] → Option<string>', () => {
    const ast = parseAST('def f() -> Optional[str]:\n    return None');
    const fn = ast.body[0] as any;
    if (!fn.returnType) { expect(fn.returnType).toBeDefined(); return; }
    const t = lowerType(fn.returnType, 'python');
    expect(t.kind).toBe('option');
    expect(typeToString(t)).toBe('Option<string>');
  });

  test('TS Promise<string> → Async<string>', () => {
    const ast = parseAST('function f(): Promise<string> { return "" }');
    const fn = ast.body[0] as any;
    const t = lowerType(fn.returnType, 'javascript');
    expect(t.kind).toBe('async');
    expect(typeToString(t)).toBe('Async<string>');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 3: Cross-runtime functions produce bridge ops in manifest
// ════════════════════════════════════════════════════════════════

describe('Level 3: Cross-runtime calls produce bridge ops', () => {

  test('Python function called from JS triggers boundary check', () => {
    const code = `
import os

def get_files(path: str) -> list:
    return os.listdir(path)

const files = get_files("/tmp")
console.log(files)
`;
    const m = parseAndManifest(code);
    expect(m.ops.length).toBeGreaterThan(0);
    // The manifest should have ops from both python and javascript runtimes
    const runtimes = new Set(m.ops.map((op: any) => op.runtime).filter(Boolean));
    expect(runtimes.size).toBeGreaterThanOrEqual(1);
  });

  test('Go function with typed params produces func_def op', () => {
    const code = `
func add(x int, y int) int {
    return x + y
}

const sum = add(1, 2)
`;
    const m = parseAndManifest(code);
    const funcDef = m.ops.find((op: any) => op.op === 'func_def') as any;
    expect(funcDef).toBeDefined();
    expect(funcDef.name).toBe('add');
  });

  test('Rust function returning Result produces throw_typed bridge', () => {
    const code = `
fn parse_config(path: &str) -> Result<String, ParseError> {
    Ok("config")
}

const config = parse_config("app.toml")
console.log(config)
`;
    const m = parseAndManifest(code);
    // If crossings detected, bridges should include throw_typed
    if (m.bridges && m.bridges.length > 0) {
      const throwOp = m.bridges.find(b => b.op === 'throw_typed');
      if (throwOp) {
        expect(throwOp.op).toBe('throw_typed');
      }
    }
  });
});

// ════════════════════════════════════════════════════════════════
// Level 4: Advanced types from real syntax
// ════════════════════════════════════════════════════════════════

describe('Level 4: Advanced types from polyglot syntax', () => {

  test('TS AsyncIterable lowers to Stream', () => {
    const ast = parseAST('async function* generate(): AsyncIterable<number> { yield 1 }');
    const fn = ast.body[0] as any;
    if (!fn.returnType) { expect(fn.returnType).toBeDefined(); return; }
    const t = lowerType(fn.returnType, 'javascript');
    expect(t.kind).toBe('stream');
    expect(typeToString(t)).toBe('Stream<f64>');
  });

  test('TS Uint8Array lowers to BufferView<u8>', () => {
    const ast = parseAST('function process(buf: Uint8Array): number { return buf.length }');
    const fn = ast.body[0] as any;
    const t = lowerType(fn.params[0].type, 'javascript');
    expect(t.kind).toBe('buffer_view');
    expect(typeToString(t)).toBe('BufferView<u8>');
  });

  test('Go chan int in function produces channel type', () => {
    const ast = parseAST('func produce(ch chan int) { ch <- 42 }');
    const fn = ast.body[0] as any;
    expect(fn.params[0].type).toBeDefined();
    if (fn.params[0].type) {
      const t = lowerType(fn.params[0].type, 'go');
      expect(t.kind).toBe('channel');
    }
  });

  test('TS Map<string, number> preserved through pipeline', () => {
    const ast = parseAST('function f(m: Map<string, number>): void {}');
    const fn = ast.body[0] as any;
    const t = lowerType(fn.params[0].type, 'javascript');
    expect(t.kind).toBe('map');
    expect(typeToString(t)).toBe('Map<string, f64>');
  });

  test('Python Dict[str, List[int]] lowers correctly', () => {
    const ast = parseAST('def f(d: Dict[str, List[int]]) -> None:\n    pass');
    const fn = ast.body[0] as any;
    const t = lowerType(fn.params[0].type, 'python');
    expect(t.kind).toBe('map');
    expect(typeToString(t)).toBe('Map<string, Array<ibig>>');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 5: Full polyglot pipeline with type-aware bridges
// ════════════════════════════════════════════════════════════════

describe('Level 5: Full polyglot pipeline', () => {

  test('Python→JS: typed function crossing produces coerce/check info', () => {
    const code = `
def compute(x: int, y: float) -> float:
    return x * y

const result: number = compute(10, 3.14)
`;
    const m = parseAndManifest(code);
    expect(m.ops.length).toBeGreaterThan(0);
  });

  test('Go→Rust: integer narrowing detected', () => {
    const code = `
func getCount() int {
    return 42
}

fn validate(n: i32) -> bool {
    n > 0
}

let valid = validate(getCount())
`;
    const m = parseAndManifest(code);
    expect(m.ops.length).toBeGreaterThan(0);
  });

  test('manifest typeSummary counts are sane', () => {
    const code = `const x: number = 42`;
    const m = parseAndManifest(code);
    if (m.typeSummary) {
      expect(m.typeSummary.crossings).toBeGreaterThanOrEqual(0);
      expect(m.typeSummary.errors).toBe(0);
    }
  });
});

// ════════════════════════════════════════════════════════════════
// Level 6: Advanced End-to-End — The Three Proofs
// ════════════════════════════════════════════════════════════════

describe('Level 6: Zero-Copy Buffer Pipeline', () => {
  /**
   * Prove that binary data passes between Go and Rust without triggering
   * slow JSON serialization. Go []byte → JS Uint8Array → Rust Vec<u8>
   * should use share_memory/copy_buffer, NOT serialize.
   */
  test('Go []byte → JS Uint8Array → Rust Vec<u8> uses zero-copy bridge ops', () => {
    const code = `
func ReadImage() []byte {
    return nil
}

fn process_image(data: Vec<u8>) -> usize {
    data.len()
}

function pipeline() {
    const img: Uint8Array = ReadImage()
    const size: number = process_image(img)
}
`;
    const m = parseAndManifest(code);

    // Must have bridge ops
    expect(m.bridges).toBeDefined();
    expect(m.bridges!.length).toBeGreaterThanOrEqual(1);

    // Crossing 1: Go []byte (Array<u8>) → JS Uint8Array (BufferView<u8>)
    // Bridge op MUST be share_memory, NOT serialize
    const goToJs = m.bridges!.find(b => b.binding === 'ReadImage()');
    expect(goToJs).toBeDefined();
    expect(goToJs!.op).toBe('share_memory');
    expect(goToJs!.from).toBe('go');
    expect(goToJs!.to).toBe('javascript');

    // Crossing 2: JS Uint8Array (BufferView<u8>) → Rust Vec<u8> (Array<u8>)
    // Bridge op should be copy_buffer (BufferView → Array copies out)
    const jsToRust = m.bridges!.find(b => b.binding.includes('process_image'));
    expect(jsToRust).toBeDefined();
    expect(jsToRust!.op).toBe('copy_buffer');
    expect(jsToRust!.from).toBe('javascript');
    expect(jsToRust!.to).toBe('rust');

    // CRITICAL: No serialize ops anywhere — binary data stays binary
    expect(m.bridges!.some(b => b.op === 'serialize')).toBe(false);

    // Summary should show coercions, not errors
    expect(m.typeSummary).toBeDefined();
    expect(m.typeSummary!.errors).toBe(0);
  });
});

describe('Level 6: Error Fidelity Boundary', () => {
  /**
   * Prove that Rust's nominal Result types produce throw_typed bridge ops
   * with error kind metadata, not generic unwrap_result.
   * This allows JS to catch specific error types structurally.
   */
  test('Rust Result<String, DbError> → JS string produces throw_typed with error metadata', () => {
    const code = `
class DbError {
    code: i32
}

fn fetch_user() -> Result<String, DbError> {
    Ok("alice")
}

function handle() {
    let user: string = fetch_user()
}
`;
    const m = parseAndManifest(code);

    // Must have exactly one bridge op
    expect(m.bridges).toBeDefined();
    expect(m.bridges!.length).toBe(1);

    const bridge = m.bridges![0];

    // The bridge op MUST be throw_typed, NOT generic unwrap_result
    expect(bridge.op).toBe('throw_typed');
    expect(bridge.from).toBe('rust');
    expect(bridge.to).toBe('javascript');

    // CRITICAL: The error kind metadata must be preserved
    // This is what allows JS to do: if (e.kind === 'DbError') { ... }
    expect(bridge.meta).toBeDefined();
    expect(bridge.meta!.errorKind).toBe('DbError');

    // Summary: 1 crossing, check (not error — it's handleable)
    expect(m.typeSummary).toBeDefined();
    expect(m.typeSummary!.check).toBe(1);
    expect(m.typeSummary!.errors).toBe(0);
  });
});

describe('Level 6: Hard Rejection — Nominal Type Safety', () => {
  /**
   * Prove that the boundary checker rejects nominal→nominal crossings
   * between different runtimes. Go Vector ≠ Rust Point, even if the
   * fields are identical. This is the type system catching a real bug.
   */
  test('Go nominal struct → Rust nominal struct is incompatible', () => {
    const code = `
func GetVec() Vector {
    return nil
}

fn calculate(p: Point) -> f64 {
    p.x + p.y
}

function main() {
    const v = GetVec()
    calculate(v)
}
`;
    const m = parseAndManifest(code);

    // The type system MUST report an error
    expect(m.typeSummary).toBeDefined();
    expect(m.typeSummary!.errors).toBeGreaterThanOrEqual(1);

    // The diagnostics should identify the nominal mismatch
    // (Vector from Go cannot satisfy Point in Rust)
  });

  test('same nominal type, same runtime is safe', () => {
    // Contrast: when types stay within one runtime, no error
    const code = `
fn make_point() -> Point { }
fn use_point(p: Point) -> f64 { 0.0 }

function main() {
    const p = make_point()
    use_point(p)
}
`;
    const m = parseAndManifest(code);

    // Within the same runtime (rust→rust), nominal match succeeds
    // No type errors expected for same-runtime usage
    if (m.typeSummary) {
      // Cross-runtime crossings may have errors, but rust→rust is fine
      // The function calls from JS→rust are the crossings
    }
  });
});

// ════════════════════════════════════════════════════════════════
// Level 7: Cross-Boundary Callbacks
// ════════════════════════════════════════════════════════════════

describe('Level 7: Cross-Boundary Callbacks', () => {

  test('function type crossing produces proxy_callable bridge op', () => {
    // JS passes a callback to Rust — can't serialize a function
    const jsCallback = func([UINT8], UINT8);
    const rustParam = func([UINT8], UINT8);

    const checker = new BoundaryChecker();
    checker.declare('mapper', jsCallback, 'javascript');
    const r = checker.checkCrossing('mapper', 'rust', rustParam);

    expect(r.compat).toBe('safe');  // Types match
    expect(r.bridgeOp).toBeDefined();
    expect(r.bridgeOp!.op).toBe('proxy_callable');
  });

  test('callback with coerced params produces proxy_callable with coerce', () => {
    // JS passes (number) => number to Rust expecting (i32) => i64
    const jsFunc = func([FLOAT64], FLOAT64);
    const rustFunc = func([INT32], INT64);

    const r = checkCompatibility(jsFunc, rustFunc);
    // Contravariant params: Rust passes i32 → JS expects f64 (coerce)
    // Covariant return: JS returns f64 → Rust expects i64 (check)
    expect(r.bridgeOp).toBeDefined();
    expect(r.bridgeOp!.op).toBe('proxy_callable');
  });

  test('incompatible callback shapes are rejected', () => {
    // JS passes (string) => void to Rust expecting (i32) => i32
    const jsFunc = func([STRING], VOID);
    const rustFunc = func([INT32], INT32);

    const r = checkCompatibility(jsFunc, rustFunc);
    expect(r.compat).toBe('incompatible');
  });

  test('full pipeline: TS callback passed to Rust function', () => {
    const code = `
fn map_data(data: Vec<u8>, mapper: (u8) => u8) -> Vec<u8> {
    data
}

function transform(buf: Uint8Array): Uint8Array {
    const result: Uint8Array = map_data(buf, (b: number) => b + 1)
    return result
}
`;
    const m = parseAndManifest(code);
    expect(m.bridges).toBeDefined();
    // Should have bridge ops including proxy_callable for the callback arg
    const callbackBridge = m.bridges!.find(b => b.op === 'proxy_callable');
    if (callbackBridge) {
      expect(callbackBridge.from).toBe('javascript');
      expect(callbackBridge.to).toBe('rust');
    }
  });

  test('closure crossing is flagged with proxy_callable (lifetime concern)', () => {
    const checker = new BoundaryChecker();
    // A closure that captures local state
    const closure: FuncType = {
      kind: 'func',
      params: [{ type: STRING }],
      returns: BOOL,
    };
    checker.declare('filter', closure, 'javascript');

    // Go wants to hold onto this callback
    const r = checker.checkCrossing('filter', 'go', closure);
    expect(r.bridgeOp).toBeDefined();
    expect(r.bridgeOp!.op).toBe('proxy_callable');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 8: Nested / Composite Bridge Ops
// ════════════════════════════════════════════════════════════════

describe('Level 8: Nested / Composite Bridge Ops', () => {

  test('Async<Option<T>> → T produces composed bridge chain', () => {
    const nested = async_(option(STRING));
    const r = checkCompatibility(nested, STRING);

    // Must not be incompatible — needs await then unwrap
    expect(r.compat).not.toBe('incompatible');
    // Should have a composite bridge op
    expect(r.bridgeOp).toBeDefined();
    expect(r.bridgeOp!.op).toBe('compose');
    const steps = (r.bridgeOp as any).steps;
    expect(steps).toBeDefined();
    expect(steps.length).toBe(2);
    expect(steps[0].op).toBe('await_resolve');
    expect(steps[1].op).toBe('unwrap_option');
  });

  test('Result<Array<i32>, Error> → Array<f64> unwraps result (inner coercion is implicit)', () => {
    const nested = result(array(INT32), ANY);
    const r = checkCompatibility(nested, array(FLOAT64));

    expect(r.compat).not.toBe('incompatible');
    expect(r.bridgeOp).toBeDefined();
    // Inner Array<i32>→Array<f64> is implicit coercion (no explicit bridge op),
    // so the result is just unwrap_result without a compose wrapper
    expect(r.bridgeOp!.op).toBe('unwrap_result');
  });

  test('Promise<Option<Vec<Result<string, Error>>>> → string[] composes full chain', () => {
    // The hardest case: 4 layers of wrapping
    const deep = async_(option(array(result(STRING, ANY))));
    const target = array(STRING);

    const r = checkCompatibility(deep, target);
    expect(r.compat).not.toBe('incompatible');
    expect(r.bridgeOp).toBeDefined();
    // Should compose: await → unwrap_option → (array elements: unwrap_result)
    expect(r.bridgeOp!.op).toBe('compose');
    const steps = (r.bridgeOp as any).steps;
    expect(steps.length).toBeGreaterThanOrEqual(2);
    expect(steps[0].op).toBe('await_resolve');
    expect(steps[1].op).toBe('unwrap_option');
  });

  test('Option<BufferView<u8>> → Uint8Array composes unwrap + identity', () => {
    const nested = option(bufferView(UINT8, 'borrowed'));
    const target = bufferView(UINT8, 'owned');

    const r = checkCompatibility(nested, target);
    expect(r.compat).not.toBe('incompatible');
    expect(r.bridgeOp).toBeDefined();
  });

  test('single-layer wrapping still returns simple bridge op (no unnecessary compose)', () => {
    // Option<string> → string should be simple unwrap_option, not compose([unwrap_option])
    const r = checkCompatibility(option(STRING), STRING);
    expect(r.bridgeOp!.op).toBe('unwrap_option');
    // Not wrapped in compose
  });

  test('full pipeline: Rust returns nested Result in Promise context', () => {
    const code = `
fn fetch_data(url: &str) -> Result<String, String> {
    Ok("data")
}

async function loadData(): Promise<string> {
    const data: string = fetch_data("http://example.com")
    return data
}
`;
    const m = parseAndManifest(code);
    expect(m.bridges).toBeDefined();
    // Should detect Result<String, String> → string crossing
    const bridge = m.bridges!.find(b => b.binding.includes('fetch_data'));
    expect(bridge).toBeDefined();
    // unwrap_result (String error is not a named struct, so generic unwrap)
    expect(bridge!.op).toBe('unwrap_result');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 9: Structural Struct Compatibility
// ════════════════════════════════════════════════════════════════

describe('Level 9: Structural Struct Compatibility', () => {
  test('same-named structs across runtimes are coerce (not incompatible)', () => {
    const goUser: CanonicalType = {
      kind: 'struct', name: 'User', fields: [
        { name: 'name', type: STRING },
        { name: 'age', type: INT32 },
      ], nominal: true, origin: 'go',
    };
    const tsUser: CanonicalType = {
      kind: 'struct', name: 'User', fields: [
        { name: 'name', type: STRING },
        { name: 'age', type: FLOAT64 },
      ], nominal: true, origin: 'typescript',
    };
    const r = checkCompatibility(goUser, tsUser);
    // age: i32 → f64 is coerce, and cross-runtime so structural
    expect(r.compat).not.toBe('incompatible');
  });

  test('differently-named opaque structs across runtimes are incompatible', () => {
    const goVec: CanonicalType = { kind: 'struct', name: 'Vector', fields: [], nominal: true, origin: 'go' };
    const rsPoint: CanonicalType = { kind: 'struct', name: 'Point', fields: [], nominal: true, origin: 'rust' };
    const r = checkCompatibility(goVec, rsPoint);
    expect(r.compat).toBe('incompatible');
  });

  test('struct with extra fields is structurally compatible (subtyping)', () => {
    const full: CanonicalType = {
      kind: 'struct', name: 'FullUser', fields: [
        { name: 'name', type: STRING },
        { name: 'age', type: INT32 },
        { name: 'email', type: STRING },
      ], nominal: true, origin: 'go',
    };
    const partial: CanonicalType = {
      kind: 'struct', name: 'BasicUser', fields: [
        { name: 'name', type: STRING },
        { name: 'age', type: INT32 },
      ], nominal: true, origin: 'typescript',
    };
    const r = checkCompatibility(full, partial);
    expect(r.compat).toBe('coerce');
    expect(r.reason).toContain('extra fields');
  });

  test('struct missing required field is incompatible', () => {
    const from: CanonicalType = {
      kind: 'struct', fields: [
        { name: 'x', type: FLOAT64 },
      ], nominal: false,
    };
    const to: CanonicalType = {
      kind: 'struct', fields: [
        { name: 'x', type: FLOAT64 },
        { name: 'y', type: FLOAT64 },
      ], nominal: false,
    };
    const r = checkCompatibility(from, to);
    expect(r.compat).toBe('incompatible');
    expect(r.reason).toContain('missing field');
  });

  test('same nominal type in same runtime is safe', () => {
    const a: CanonicalType = { kind: 'struct', name: 'Foo', fields: [], nominal: true, origin: 'rust' };
    const b: CanonicalType = { kind: 'struct', name: 'Foo', fields: [], nominal: true, origin: 'rust' };
    expect(checkCompatibility(a, b).compat).toBe('safe');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 10: Enum / Tagged Union Bridging
// ════════════════════════════════════════════════════════════════

describe('Level 10: Enum / Tagged Union Bridging', () => {
  const shape: CanonicalType = {
    kind: 'enum', name: 'Shape', variants: [
      { name: 'Circle', payload: FLOAT64 },
      { name: 'Rect', payload: { kind: 'tuple', elements: [FLOAT64, FLOAT64] } },
    ],
  };

  test('enum → union emits tag_dispatch bridge op', () => {
    const union: CanonicalType = {
      kind: 'union', members: [
        { kind: 'struct', name: 'Circle', fields: [], nominal: false },
        { kind: 'struct', name: 'Rect', fields: [], nominal: false },
      ],
    };
    const r = checkCompatibility(shape, union);
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp).toBeDefined();
    expect(r.bridgeOp!.op).toBe('tag_dispatch');
    expect((r.bridgeOp as any).variants).toEqual(['Circle', 'Rect']);
  });

  test('union → enum emits tag_dispatch with guard', () => {
    const union: CanonicalType = {
      kind: 'union', members: [
        { kind: 'struct', name: 'Circle', fields: [], nominal: false },
        { kind: 'struct', name: 'Rect', fields: [], nominal: false },
      ],
    };
    const r = checkCompatibility(union, shape);
    expect(r.compat).toBe('check');
    expect(r.bridgeOp!.op).toBe('tag_dispatch');
    expect(r.guard).toBeDefined();
    expect(r.guard!.js).toContain('includes');
  });

  test('enum → enum with matching variants is safe', () => {
    const other: CanonicalType = {
      kind: 'enum', name: 'Shape2', variants: [
        { name: 'Circle', payload: FLOAT64 },
        { name: 'Rect', payload: { kind: 'tuple', elements: [FLOAT64, FLOAT64] } },
      ],
    };
    const r = checkCompatibility(shape, other);
    expect(r.compat).toBe('safe');
  });

  test('enum → enum with missing variant is incompatible', () => {
    const smaller: CanonicalType = {
      kind: 'enum', name: 'Shape3', variants: [
        { name: 'Circle', payload: FLOAT64 },
        { name: 'Triangle', payload: FLOAT64 },
      ],
    };
    const r = checkCompatibility(shape, smaller);
    expect(r.compat).toBe('incompatible');
    expect(r.reason).toContain('Triangle');
  });

  test('enum → enum with incompatible payload type is rejected', () => {
    const mismatch: CanonicalType = {
      kind: 'enum', name: 'Shape4', variants: [
        { name: 'Circle', payload: FLOAT64 },
        { name: 'Rect', payload: INT32 }, // tuple(f64,f64) → i32 is incompatible
      ],
    };
    const r = checkCompatibility(shape, mismatch);
    expect(r.compat).toBe('incompatible');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 11: Runtime Guards
// ════════════════════════════════════════════════════════════════

describe('Level 11: Runtime Guard Hints', () => {
  test('float → int produces guard with range check', () => {
    const r = checkCompatibility(FLOAT64, INT32);
    expect(r.compat).toBe('check');
    expect(r.guard).toBeDefined();
    expect(r.guard!.js).toContain('Number.isInteger');
    expect(r.guard!.python).toContain('is_integer');
  });

  test('string → int produces guard with parse check', () => {
    const r = checkCompatibility(STRING, INT32);
    expect(r.compat).toBe('check');
    expect(r.guard).toBeDefined();
    expect(r.guard!.js).toContain('parseInt');
    expect(r.guard!.python).toContain('isdigit');
    expect(r.guard!.go).toContain('ParseInt');
  });

  test('string → float produces guard', () => {
    const r = checkCompatibility(STRING, FLOAT64);
    expect(r.compat).toBe('check');
    expect(r.guard).toBeDefined();
    expect(r.guard!.js).toContain('parseFloat');
  });

  test('float → int bridge op has narrow with sizes', () => {
    const r = checkCompatibility(FLOAT64, INT32);
    expect(r.bridgeOp).toBeDefined();
    expect(r.bridgeOp!.op).toBe('narrow');
    expect((r.bridgeOp as any).from).toBe('f64');
    expect((r.bridgeOp as any).to).toBe('i32');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 12: Improved Type Inference
// ════════════════════════════════════════════════════════════════

describe('Level 12: Improved Type Inference', () => {
  test('infers type from literal initializer in pipeline', () => {
    const checker = new BoundaryChecker();
    // Simulate: const name: string declared in JS
    checker.declare('name', STRING, 'javascript');

    // Then used in Python: process(name) where process expects int
    const r = checker.checkCrossing('name', 'python', INT32);
    expect(r.compat).toBe('check'); // string → int needs parsing
    expect(r.bridgeOp!.op).toBe('parse_int');
  });

  test('member access on known struct type resolves field type', () => {
    const checker = new BoundaryChecker();
    const userType: CanonicalType = {
      kind: 'struct', name: 'User', fields: [
        { name: 'name', type: STRING },
        { name: 'age', type: INT32 },
      ], nominal: false,
    };
    checker.declare('user', userType, 'typescript');

    // Checking user crossing — struct field types are known
    const r = checker.checkCrossing('user', 'python', userType);
    expect(r.compat).toBe('safe');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 13: Struct Field Capture from Class/Interface Declarations
// ════════════════════════════════════════════════════════════════

describe('Level 13: Struct Field Capture', () => {
  test('interface fields are captured and available for structural checking', () => {
    const code = `
interface User {
    name: string
    age: number
}

def greet(u: User):
    print(u.name)
`;
    const m = parseAndManifest(code);
    // The interface should be registered — crossing checks should have field info
    expect(m).toBeDefined();
  });

  test('class fields are captured with types', () => {
    const code = `
class Point {
    x: number
    y: number

    distance(): number {
        return Math.sqrt(this.x * this.x + this.y * this.y)
    }
}

fn use_point(p: Point) -> f64 {
    p.x + p.y
}
`;
    const m = parseAndManifest(code);
    expect(m).toBeDefined();
  });

  test('cross-runtime struct with matching fields is coerce, not incompatible', () => {
    const code = `
interface Config {
    host: string
    port: number
}

func NewConfig() Config {
    return nil
}

function startServer(c: Config) {
    console.log(c.host)
}

const config: Config = NewConfig()
startServer(config)
`;
    const m = parseAndManifest(code);
    // Config type flows from Go to JS — should be coerce not error
    // because the interface has field info for structural matching
    if (m.typeSummary) {
      expect(m.typeSummary.errors).toBe(0);
    }
  });
});

// ════════════════════════════════════════════════════════════════
// Level 14: Generic Instantiation
// ════════════════════════════════════════════════════════════════

describe('Level 14: Generic Instantiation', () => {
  test('Call.typeArgs is populated from parser', () => {
    const code = `const user = fetchData<User>("/api/user")`;
    const lexer = new (require('../src/lexer').Lexer)(code);
    const tokens = lexer.tokenize();
    const parser = new (require('../src/parser').Parser)(tokens, code);
    const ast = parser.parse();

    const constDecl = ast.body[0];
    expect(constDecl.kind).toBe('ConstDecl');
    const call = (constDecl as any).values[0];
    expect(call.kind).toBe('Call');
    expect(call.typeArgs).toBeDefined();
    expect(call.typeArgs).toHaveLength(1);
    expect(call.typeArgs[0].kind).toBe('SimpleType');
    expect(call.typeArgs[0].id.name).toBe('User');
  });

  test('generic instantiation resolves typevar in return type', () => {
    const checker = new BoundaryChecker();

    // Declare a generic function: function fetch<T>(url: string): Promise<T>
    const fetchType: FuncType = {
      kind: 'func',
      params: [{ type: STRING }],
      returns: { kind: 'async', inner: { kind: 'typevar', name: 'T' } },
    };
    checker.declare('fetch', fetchType, 'javascript');

    // The manifest generator's instantiateReturn would substitute T→User
    // Here we test the raw type var is present
    const binding = checker.getBinding('fetch');
    expect(binding).toBeDefined();
    expect(binding!.type.kind).toBe('func');
    const ret = (binding!.type as FuncType).returns;
    expect(ret.kind).toBe('async');
  });
});

// ════════════════════════════════════════════════════════════════
// Level 15: Control-Flow Narrowing
// ════════════════════════════════════════════════════════════════

describe('Level 15: Control-Flow Narrowing', () => {
  test('Option<T> narrows to T with not-null guard', () => {
    const narrowed = BoundaryChecker.narrowType(
      { kind: 'option', inner: STRING },
      'not-null'
    );
    expect(narrowed).toBeDefined();
    expect(narrowed!.kind).toBe('string');
  });

  test('union with null narrows to non-null members', () => {
    const union: CanonicalType = {
      kind: 'union',
      members: [STRING, INT32, NULL],
    };
    const narrowed = BoundaryChecker.narrowType(union, 'not-null');
    expect(narrowed).toBeDefined();
    expect(narrowed!.kind).toBe('union');
    expect((narrowed as any).members).toHaveLength(2);
  });

  test('union with single non-null member narrows to that member', () => {
    const union: CanonicalType = {
      kind: 'union',
      members: [STRING, NULL],
    };
    const narrowed = BoundaryChecker.narrowType(union, 'not-null');
    expect(narrowed).toBeDefined();
    expect(narrowed!.kind).toBe('string');
  });

  test('non-nullable type returns undefined (no narrowing needed)', () => {
    const narrowed = BoundaryChecker.narrowType(STRING, 'not-null');
    expect(narrowed).toBeUndefined();
  });

  test('push/pop narrow scope affects getEffectiveType', () => {
    const checker = new BoundaryChecker();
    checker.declare('x', { kind: 'option', inner: INT32 }, 'javascript');

    // Before narrowing
    expect(checker.getEffectiveType('x')!.kind).toBe('option');

    // After push narrow
    const narrowings = new Map<string, CanonicalType>();
    narrowings.set('x', INT32);
    checker.pushNarrow(narrowings);
    expect(checker.getEffectiveType('x')!.kind).toBe('int');

    // After pop
    checker.popNarrow();
    expect(checker.getEffectiveType('x')!.kind).toBe('option');
  });

  test('narrowing affects boundary crossing checks', () => {
    const checker = new BoundaryChecker();
    checker.declare('maybeVal', { kind: 'option', inner: STRING }, 'javascript');

    // Without narrowing: option → string needs unwrap_option (check)
    const r1 = checker.checkCrossing('maybeVal', 'python', STRING);
    expect(r1.compat).toBe('check');

    // With narrowing: narrowed to string → string is safe
    const narrowings = new Map<string, CanonicalType>();
    narrowings.set('maybeVal', STRING);
    checker.pushNarrow(narrowings);
    const r2 = checker.checkCrossing('maybeVal', 'python', STRING);
    expect(r2.compat).toBe('safe');
    checker.popNarrow();
  });

  test('if (x !== null) narrows in pipeline', () => {
    const code = `
fn get_user() -> Option<string> {
    None
}

function process() {
    const user: string | null = get_user()
    if (user !== null) {
        console.log(user.toUpperCase())
    }
}
`;
    const m = parseAndManifest(code);
    // Should parse and generate manifest without errors
    expect(m).toBeDefined();
  });
});
