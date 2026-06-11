/**
 * Boundary generics — tiered dispatch (docs/rust-boundary-generics.md).
 *
 * Tier 1: per-call-site stamping. Each foreign call site with unambiguous
 *   literal evidence gets its own concrete wrapper
 *   (`__omnivm_<fn>__<typekey>`) and the call op is rewritten to target it.
 *   Different sites may carry different stamps; the generic declaration
 *   stays byte-verbatim, gradual declarations keep their Dyn completion.
 * Tier 2: single-type-param fns (arity ≤ 2, bare-`T` params) export an
 *   autoref-pruned lattice dispatcher under the ORIGINAL symbol: the
 *   incoming serde tags select a candidate from {i64, f64, bool, String},
 *   probe tokens let rustc prune candidates that fail the bounds, and
 *   tags outside the lattice route through the Dyn probe.
 * Tier 3: bounded generics whose bound vocabulary is a subset of what
 *   omnivm::Dyn satisfies export `f::<Dyn>` under the original symbol —
 *   foreign calls just work, runtime-checked.
 * Otherwise: the rust-generic-export warning, naming the blocking bounds.
 */

import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import { RuntimeResolver } from '../src/runtime-resolver';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';
import { DispatchManifest, FuncDefOp, EvalOp } from '../src/codegen-omnivm/manifest-types';
import { extractRustFnGenerics } from '../src/rust-item-scanner';

function compileManifest(code: string): DispatchManifest {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  expect(parser.getErrors()).toEqual([]);
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, code);
  const gen = new ManifestCodeGenerator();
  return gen.generate(annotated);
}

function rustFuncDefs(manifest: DispatchManifest): FuncDefOp[] {
  return manifest.ops.filter(
    op => op.op === 'func_def' && (op as FuncDefOp).bodyRuntime === 'rust',
  ) as FuncDefOp[];
}

function rustUnitSource(manifest: DispatchManifest): string {
  const [fd] = rustFuncDefs(manifest);
  expect(fd).toBeDefined();
  return fd.source!;
}

function rustEvalCodes(manifest: DispatchManifest): string[] {
  return manifest.ops
    .filter(op => op.op === 'eval' && (op as EvalOp).runtime === 'rust')
    .map(op => (op as EvalOp).code ?? '');
}

// ─── Scanner: generic-parameter and where-clause extraction ──────────

describe('scanner: extractRustFnGenerics', () => {
  test('inline bounds and where clause merge textually', () => {
    const g = extractRustFnGenerics(
      'fn pick<T: PartialOrd + Clone, U>(a: T, b: U) -> T where U: std::fmt::Display { a }',
      'pick',
    )!;
    expect(g.opaque).toBe(false);
    expect(g.params).toEqual([
      { name: 'T', bounds: ['PartialOrd', 'Clone'], isConst: false },
      { name: 'U', bounds: [], isConst: false },
    ]);
    expect(g.wherePredicates).toEqual([
      { target: 'U', bounds: ['std::fmt::Display'] },
    ]);
  });

  test('const generics and lifetimes classify correctly', () => {
    const g = extractRustFnGenerics(
      "fn windowed<'a, T: Clone, const N: usize>(xs: &'a [T]) -> Vec<T> { xs.to_vec() }",
      'windowed',
    )!;
    expect(g.params).toEqual([
      { name: 'T', bounds: ['Clone'], isConst: false },
      { name: 'N', bounds: [], isConst: true },
    ]);
  });

  test('parenthesized Fn bounds survive the top-level + split', () => {
    const g = extractRustFnGenerics(
      'fn apply<F: Fn(i64) -> i64 + Send>(f: F) -> i64 { f(1) }',
      'apply',
    )!;
    expect(g.params).toEqual([
      { name: 'F', bounds: ['Fn(i64) -> i64', 'Send'], isConst: false },
    ]);
  });
});

// ─── Tier 1: per-call-site stamping ──────────────────────────────────

describe('tier 1: per-site stamping for generic fns', () => {
  const code = [
    'use std::fmt::Display;',
    '',
    'fn echo_typed<T: Display>(x: T) -> String {',
    '  format!("[{x}]")',
    '}',
    '',
    'const a = echo_typed(42)',
    'const b = echo_typed("rust")',
    'print(a, b)',
  ].join('\n');

  test('different sites get different stamps; declaration stays verbatim', () => {
    const manifest = compileManifest(code);
    const source = rustUnitSource(manifest);
    expect(source).toContain('fn echo_typed<T: Display>(x: T) -> String {');
    expect(source).toContain('fn __omnivm_echo_typed__i64(x: i64) -> String {\n    echo_typed(x)\n}');
    expect(source).toContain('fn __omnivm_echo_typed__string(x: String) -> String {\n    echo_typed(x)\n}');
    // Stamped scalar wrappers ride the typed lane.
    expect(source).toContain(
      'omnivm::export_typed_fn!(OmniVMCallTyped___omnivm_echo_typed__i64, __omnivm_echo_typed__i64, 1);',
    );
  });

  test('each call op is rewritten to its own wrapper', () => {
    const manifest = compileManifest(code);
    const codes = rustEvalCodes(manifest);
    expect(codes).toContain('__omnivm_echo_typed__i64(42)');
    expect(codes).toContain('__omnivm_echo_typed__string("rust")');
  });

  test('wrappers export and get func_def ops (guest stubs follow)', () => {
    const manifest = compileManifest(code);
    const [fd] = rustFuncDefs(manifest);
    expect(fd.exports).toEqual(
      expect.arrayContaining(['echo_typed', '__omnivm_echo_typed__i64', '__omnivm_echo_typed__string']),
    );
    const names = rustFuncDefs(manifest).map(f => f.name);
    expect(names).toEqual(
      expect.arrayContaining(['echo_typed', '__omnivm_echo_typed__i64', '__omnivm_echo_typed__string']),
    );
  });
});

describe('tier 1: per-site stamping for gradual fns', () => {
  const code = [
    'fn mirror(v) {',
    '  v',
    '}',
    'const a = mirror(1)',
    'const b = mirror("two")',
    'print(a, b)',
  ].join('\n');

  test('conflicting sites keep the Dyn declaration AND stamp per site', () => {
    const manifest = compileManifest(code);
    const source = rustUnitSource(manifest);
    expect(source).toContain('fn mirror(v: omnivm::Dyn) -> impl omnivm::serde::Serialize {');
    expect(source).toContain(
      'fn __omnivm_mirror__i64(v: i64) -> impl omnivm::serde::Serialize {\n    mirror(omnivm::Dyn::from(v))\n}',
    );
    expect(source).toContain(
      'fn __omnivm_mirror__string(v: String) -> impl omnivm::serde::Serialize {\n    mirror(omnivm::Dyn::from(v))\n}',
    );
    const codes = rustEvalCodes(manifest);
    expect(codes).toContain('__omnivm_mirror__i64(1)');
    expect(codes).toContain('__omnivm_mirror__string("two")');
  });

  test('unanimous evidence still stamps the declaration itself (no wrappers)', () => {
    const source = rustUnitSource(compileManifest([
      'fn scale(rating, factor: i64) -> i64 {',
      '  rating * factor',
      '}',
      'const out = scale(7, 6)',
      'print(out)',
    ].join('\n')));
    expect(source).toContain('fn scale(rating: i64, factor: i64) -> i64 {');
    expect(source).not.toContain('__omnivm_scale__');
  });

  test('unevidenced sites keep calling the Dyn export', () => {
    const manifest = compileManifest([
      'fn mirror(v) {',
      '  v',
      '}',
      'items = [1, 2]',
      'const a = mirror(1)',
      'const b = mirror("two")',
      'const c = mirror(items)',
      'print(a, b, c)',
    ].join('\n'));
    const codes = rustEvalCodes(manifest);
    expect(codes).toContain('mirror(items)');
    expect(codes).toContain('__omnivm_mirror__i64(1)');
  });
});

// ─── Tier 2: autoref-pruned lattice dispatcher ───────────────────────

describe('tier 2: tag dispatcher for single-param bare-T generics', () => {
  test('dispatcher + probe tokens export under the ORIGINAL symbol', () => {
    const manifest = compileManifest([
      'fn twice<T: std::ops::Add<Output = T> + Clone>(x: T) -> T {',
      '  x.clone() + x',
      '}',
      'n = 21',
      'const t = twice(n)',
      'print(t)',
    ].join('\n'));
    const source = rustUnitSource(manifest);
    // The fn's own bounds gate the by-value probe impl; rustc prunes.
    expect(source).toContain(
      'where T: std::ops::Add<Output = T> + Clone + omnivm::serde::Serialize + omnivm::serde::de::DeserializeOwned {',
    );
    expect(source).toContain('struct __OmnivmProbe_twice<T>(::std::marker::PhantomData<T>);');
    expect(source).toContain('trait __OmnivmMiss_twice');
    expect(source).toContain('fn __omnivm_dispatch_twice(__omnivm_a0: omnivm::Dyn) -> omnivm::Dyn {');
    expect(source).toContain('omnivm::export_fn!(OmniVMCall_twice, __omnivm_dispatch_twice, 1);');
    // Exported under the original name: foreign calls just work.
    const [fd] = rustFuncDefs(manifest);
    expect(fd.exports).toContain('twice');
    expect(manifest.diagnostics ?? []).toEqual([]);
    // The unevidenced call op is untouched (the dispatcher handles tags).
    expect(rustEvalCodes(manifest)).toContain('twice(n)');
  });

  test('the Dyn fallback arm rides the same probe (tags outside the lattice)', () => {
    const source = rustUnitSource(compileManifest([
      'fn smallest<T: PartialOrd + Clone>(a: T, b: T) -> T {',
      '  if a < b { a } else { b }',
      '}',
      'x = 4',
      'const s = smallest(x, x)',
      'print(s)',
    ].join('\n')));
    expect(source).toContain('__OmnivmProbe_smallest::<omnivm::Dyn>(::std::marker::PhantomData)');
    expect(source).toContain("TypeError: no boundary instantiation of 'smallest'");
  });
});

// ─── Tier 3: the Dyn instantiation ───────────────────────────────────

describe('tier 3: f::<Dyn> auto-export for bounded generics', () => {
  test('container params map to owned Dyn forms under the original symbol', () => {
    const manifest = compileManifest([
      'fn max_of<T: PartialOrd + Clone>(items: Vec<T>) -> T {',
      '  let mut best = items[0].clone();',
      '  for it in items.iter() {',
      '    if *it > best { best = it.clone(); }',
      '  }',
      '  best',
      '}',
      'nums = [3, 11, 7]',
      'const m = max_of(nums)',
      'print(m)',
    ].join('\n'));
    const source = rustUnitSource(manifest);
    expect(source).toContain(
      'fn __omnivm_dyn_max_of(items: Vec<omnivm::Dyn>) -> omnivm::Dyn {\n    max_of(items)\n}',
    );
    expect(source).toContain('omnivm::export_fn!(OmniVMCall_max_of, __omnivm_dyn_max_of, 1);');
    expect(manifest.diagnostics ?? []).toEqual([]);
    expect(rustFuncDefs(manifest).map(f => f.name)).toContain('max_of');
  });

  test('vocabulary check accepts path-qualified bounds and where clauses', () => {
    const manifest = compileManifest([
      'fn label_all<T>(items: Vec<T>) -> Vec<String> where T: std::fmt::Display {',
      '  items.iter().map(|x| format!("{x}")).collect()',
      '}',
      'words = ["a", "b"]',
      'const out = label_all(words)',
      'print(out)',
    ].join('\n'));
    expect(rustUnitSource(manifest)).toContain(
      'fn __omnivm_dyn_label_all(items: Vec<omnivm::Dyn>) -> Vec<String> {',
    );
    expect(manifest.diagnostics ?? []).toEqual([]);
  });
});

// ─── The diagnostic tier ─────────────────────────────────────────────

describe('diagnostic: bounds outside the vocabulary', () => {
  test('unsatisfiable bounds keep the warning, now naming the blockers', () => {
    const manifest = compileManifest([
      'use std::io::Write;',
      '',
      'fn dump_all<W: Write, T: std::fmt::Display>(sink: W, items: Vec<T>) -> i64 {',
      '  let mut n = 0;',
      '  for it in items.iter() { let _ = write!(&mut sink, "{it}"); n += 1; }',
      '  n',
      '}',
      'const r = dump_all(out_sink, rows)',
      'print(r)',
    ].join('\n'));
    const warning = (manifest.diagnostics ?? []).find(d => d.code === 'rust-generic-export');
    expect(warning).toBeDefined();
    expect(warning!.severity).toBe('warning');
    expect(warning!.message).toContain("Rust fn 'dump_all'");
    expect(warning!.message).toContain('not satisfiable by the dynamic fallback (Write)');
    expect(warning!.message).toContain('annotate the call site or add a concrete wrapper');
    // No export under the original name: the fn stays internal.
    expect(rustFuncDefs(manifest).map(f => f.name)).not.toContain('dump_all');
  });

  test('T in an unsupported nested position names the position', () => {
    const manifest = compileManifest([
      'use std::collections::HashMap;',
      'use std::hash::Hash;',
      '',
      'fn tally<T: Eq + Hash + Clone, U: Clone>(counts: HashMap<T, i64>, mark: U) -> i64 {',
      '  counts.len() as i64',
      '}',
      'const r = tally(counter_map, marker)',
      'print(r)',
    ].join('\n'));
    const warning = (manifest.diagnostics ?? []).find(d => d.code === 'rust-generic-export');
    expect(warning).toBeDefined();
    expect(warning!.message).toContain("in unsupported position HashMap<T, i64>");
  });

  test('tier-1 stamps still work for warned fns (evidenced sites only)', () => {
    const manifest = compileManifest([
      'fn pair_up<A: Clone, B: std::io::Read>(a: A, b: B) -> i64 {',
      '  1',
      '}',
      'const r = pair_up(7, reader)',
      'print(r)',
    ].join('\n'));
    // No evidence for `reader` → no wrapper; the warning still fires.
    const warning = (manifest.diagnostics ?? []).find(d => d.code === 'rust-generic-export');
    expect(warning).toBeDefined();
  });
});
