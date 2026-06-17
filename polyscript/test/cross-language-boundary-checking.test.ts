/**
 * Cross-language type checking — END-TO-END through the .poly → manifest path.
 *
 * These tests drive the full pipeline (Lexer → Parser → RuntimeResolver →
 * ManifestCodeGenerator), NOT the BoundaryChecker directly, to prove that the
 * cross-language boundary diagnostics computed by the BoundaryChecker are now
 * surfaced into the manifest's `diagnostics` (and therefore the CLI), and that
 * declared argument types ride the typed/zero-copy lane while still being
 * statically checked.
 *
 * Severity policy (coercion lattice → diagnostic):
 *   incompatible -> error   (genuine cross-language type mismatch; stops build)
 *   check        -> warning (lossy/fallible crossing; needs a runtime guard)
 *   coerce/safe  -> (info / none)
 */

import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import { RuntimeResolver } from '../src/runtime-resolver';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';
import {
  DispatchManifest,
  FuncDefOp,
  ManifestDiagnostic,
} from '../src/codegen-omnivm/manifest-types';
import { checkCompatibility } from '../src/type-system/coercion';
import * as C from '../src/type-system/canonical';

function compileManifest(code: string): DispatchManifest {
  const tokens = new Lexer(code).tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  expect(parser.getErrors()).toEqual([]);
  const annotated = new RuntimeResolver().resolve(ast, code);
  return new ManifestCodeGenerator().generate(annotated);
}

function boundaryDiagnostics(m: DispatchManifest): ManifestDiagnostic[] {
  return (m.diagnostics ?? []).filter(d => d.code.startsWith('boundary-type'));
}

function rustUnitSource(m: DispatchManifest): string {
  const fd = m.ops.find(
    op => op.op === 'func_def' && (op as FuncDefOp).bodyRuntime === 'rust',
  ) as FuncDefOp | undefined;
  expect(fd).toBeDefined();
  return fd!.source!;
}

// ─── 1 + 5: diagnostics are surfaced through .poly → manifest ────────────────

describe('cross-language boundary diagnostics are surfaced into the manifest', () => {
  // 5(a) — a genuine cross-language type mismatch is a hard ERROR.
  //
  // NOTE on the spec's `-> str` into `fn f(x: i64)` example: the coercion
  // lattice deliberately classifies string→int as `check` (parseable, see
  // type-system.test.ts "string to int requires parse check"), so that pair
  // surfaces as a WARNING, not an error. To prove the ERROR path with a pair
  // the lattice genuinely rejects, we use a Python `-> list[int]` flowing into
  // a Rust `i64` slot — a categorical (array → scalar) mismatch.
  test('(5a) incompatible crossing → surfaced ERROR that names both runtimes + location', () => {
    const code = [
      '# @runtime python',
      'fn need_int(x: i64) -> i64 {',
      '  x',
      '}',
      'def make_list() -> list[int]:',
      '    return [1, 2]',
      'const r = need_int(make_list())',
      'print(r)',
    ].join('\n');
    const m = compileManifest(code);
    const diags = boundaryDiagnostics(m);
    const err = diags.find(d => d.severity === 'error');
    expect(err).toBeDefined();
    expect(err!.code).toBe('boundary-type-mismatch');
    // Names BOTH runtimes.
    expect(err!.message).toContain('python');
    expect(err!.message).toContain('rust');
    // Carries a .poly location.
    expect(err!.span).toBeDefined();
    expect(err!.span!.line).toBeGreaterThan(0);
    // typeSummary counts still work.
    expect(m.typeSummary).toBeDefined();
    expect(m.typeSummary!.errors).toBeGreaterThanOrEqual(1);
  });

  // 5(b) — a lossy numeric crossing (float → i64) is a 'check' WARNING.
  test('(5b) float → i64 crossing → surfaced WARNING (check), not an error', () => {
    const code = [
      '# @runtime python',
      'fn need_int(x: i64) -> i64 {',
      '  x',
      '}',
      'def make_f() -> float:',
      '    return 1.5',
      'const r = need_int(make_f())',
      'print(r)',
    ].join('\n');
    const m = compileManifest(code);
    const diags = boundaryDiagnostics(m);
    const warn = diags.find(d => d.severity === 'warning');
    expect(warn).toBeDefined();
    expect(warn!.code).toBe('boundary-type-check');
    expect(warn!.message).toContain('python');
    expect(warn!.message).toContain('rust');
    expect(diags.some(d => d.severity === 'error')).toBe(false);
  });

  // 5(d) — compatible typed crossings produce NO boundary diagnostics.
  test('(5d) compatible typed crossing (python int → rust i64) → no boundary diagnostics', () => {
    const code = [
      '# @runtime python',
      'fn need_int(x: i64) -> i64 {',
      '  x',
      '}',
      'n: int = 7',
      'const r = need_int(n)',
      'print(r)',
    ].join('\n');
    const m = compileManifest(code);
    expect(boundaryDiagnostics(m)).toEqual([]);
    expect(m.typeSummary!.errors).toBe(0);
  });
});

// ─── 2 + 5(c): declared types as call-site evidence (typed lane) ─────────────

describe('declared argument types ride the typed lane (evidence)', () => {
  // 5(c) — a typed Python int arg into a gradual Rust fn stamps the slot i64
  // (the declaration rides the typed lane) AND is statically checked.
  test('(5c) typed python int arg stamps a gradual Rust Dyn slot to i64', () => {
    const code = [
      '# @runtime python',
      'fn scale(rating) {',
      '  rating.as_i64() * 2',
      '}',
      'n: int = 7',
      'const out = scale(n)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn scale(rating: i64)');
    expect(source).not.toContain('fn scale(rating: omnivm::Dyn)');
  });

  test('typed python str arg stamps a gradual Dyn slot to String', () => {
    const code = [
      '# @runtime python',
      'fn shout(label) {',
      '  format!("{}!", label)',
      '}',
      'msg: str = "hi"',
      'const out = shout(msg)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn shout(label: String)');
  });

  // Gradual Dyn fallback MUST still fire when there is genuinely no type.
  test('an untyped arg keeps the Dyn slot (gradual fallback preserved)', () => {
    const code = [
      '# @runtime python',
      'fn mirror(v) {',
      '  v',
      '}',
      'thing = compute()',
      'const out = mirror(thing)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn mirror(v: omnivm::Dyn)');
  });

  // A typed container arg (list[...]) is ambiguous as a Rust scalar slot, so it
  // stays Dyn — only the unambiguous scalar/df mappings stamp.
  test('a typed list[...] arg keeps Dyn (container has no clear scalar mapping)', () => {
    const code = [
      '# @runtime python',
      'fn first(items) {',
      '  items[0].clone()',
      '}',
      'xs: list[int] = [1, 2, 3]',
      'const out = first(xs)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn first(items: omnivm::Dyn)');
  });
});

// ─── 3: coercion lattice soundness (static classification) ──────────────────

describe('coercion lattice: lossy numeric conversions are check, widenings are safe', () => {
  const I32: C.IntType = { kind: 'int', size: 32, signed: true };
  const I64: C.IntType = { kind: 'int', size: 64, signed: true };
  const BIG: C.IntType = { kind: 'int', size: 'big', signed: true };

  test('float → int is check (lossy truncation), not safe', () => {
    expect(checkCompatibility(C.FLOAT64, I64).compat).toBe('check');
  });

  test('bigint → fixed-width int is check (possible overflow), not safe', () => {
    expect(checkCompatibility(BIG, I64).compat).toBe('check');
    expect(checkCompatibility(BIG, I32).compat).toBe('check');
  });

  test('wide int → narrow int is check (possible truncation)', () => {
    expect(checkCompatibility(I64, I32).compat).toBe('check');
  });

  test('lossless widenings stay safe/coerce (never check)', () => {
    // i64 → bigint is representation-preserving: safe.
    expect(checkCompatibility(I64, BIG).compat).toBe('safe');
    // small int → f64 is lossless: coerce (a conversion, but never lossy).
    expect(checkCompatibility(I32, C.FLOAT64).compat).toBe('coerce');
    // identity stays safe.
    expect(checkCompatibility(I64, I64).compat).toBe('safe');
  });
});

// ─── 4: parse gap — bare value-less Python annotation is captured ───────────

describe('parser: bare value-less Python annotation becomes a typed declaration', () => {
  function parse(code: string) {
    const tokens = new Lexer(code).tokenize();
    const parser = new Parser(tokens, code);
    const ast = parser.parse();
    expect(parser.getErrors()).toEqual([]);
    return ast;
  }

  test('`x: int` (no initializer) parses as a typed VarDecl with no values', () => {
    const ast = parse('x: int\n');
    const decl = ast.body.find((n: any) => n.kind === 'VarDecl') as any;
    expect(decl).toBeDefined();
    expect(decl.names.map((n: any) => n.name)).toEqual(['x']);
    expect(decl.type).toBeDefined();
    expect(decl.values).toBeUndefined();
  });

  test('the bare annotation is visible to the boundary checker as evidence', () => {
    // `count: int` with no initializer must still let scale() stamp i64.
    const code = [
      '# @runtime python',
      'fn scale(rating) {',
      '  rating.as_i64()',
      '}',
      'count: int',
      'const out = scale(count)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn scale(rating: i64)');
  });

  test('`x: int = 5` (with initializer) still parses with type AND value', () => {
    const ast = parse('x: int = 5\n');
    const decl = ast.body.find((n: any) => n.kind === 'VarDecl') as any;
    expect(decl).toBeDefined();
    expect(decl.type).toBeDefined();
    expect(decl.values).toBeDefined();
    expect(decl.values.length).toBe(1);
  });
});
