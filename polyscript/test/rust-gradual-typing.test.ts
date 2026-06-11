/**
 * Rust gradual typing — untyped boundary slots, types flow in from call
 * sites.
 *
 * Scanner superset: `fn` items may omit param types and/or the return type
 * (invalid in plain rustc, so accepting them is a pure superset — valid Rust
 * must still slice byte-identical).
 *
 * Codegen completion: untyped params complete to `omnivm::Dyn` (or a type
 * stamped from unanimous call-site evidence); a missing return type on a
 * gradual fn whose body ends in an expression completes to
 * `-> impl omnivm::serde::Serialize`. Rewrites are signature-only and
 * newline-free, so line counts (and the unit source map) are preserved.
 */

import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import { RuntimeResolver } from '../src/runtime-resolver';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';
import { DispatchManifest, FuncDefOp } from '../src/codegen-omnivm/manifest-types';
import {
  extractRustFnSignatures,
  scanRustFnCompletion,
  scanRustItemAt,
} from '../src/rust-item-scanner';

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

function rustUnitSource(manifest: DispatchManifest): string {
  const fd = manifest.ops.find(
    op => op.op === 'func_def' && (op as FuncDefOp).bodyRuntime === 'rust',
  ) as FuncDefOp | undefined;
  expect(fd).toBeDefined();
  return fd!.source!;
}

// ─── Scanner superset ───────────────────────────────────────────────

describe('scanner: type-less fn params parse (gradual superset)', () => {
  test('fully untyped params and omitted return', () => {
    const [sig] = extractRustFnSignatures('fn score(review) { review["score"].as_f64() }');
    expect(sig.name).toBe('score');
    expect(sig.params).toEqual(['review']);
    expect(sig.paramTypes).toEqual(['']);
    expect(sig.untypedParams).toEqual([0]);
    expect(sig.returnType).toBe('');
  });

  test('mixed typed and untyped params', () => {
    const [sig] = extractRustFnSignatures('fn gate(a, b: i64, mut c) -> i64 { 0 }');
    expect(sig.params).toEqual(['a', 'b', 'c']);
    expect(sig.paramTypes).toEqual(['', 'i64', '']);
    expect(sig.untypedParams).toEqual([0, 2]);
    expect(sig.returnType).toBe('i64');
  });

  test('fully typed fn has no untyped slots', () => {
    const [sig] = extractRustFnSignatures(
      'fn add(a: i64, b: HashMap<String, i64>) -> i64 { a }',
    );
    expect(sig.untypedParams).toEqual([]);
  });

  test('pattern params are typed (no extractable name), not gradual', () => {
    const [sig] = extractRustFnSignatures('fn f((a, b): (i64, i64)) -> i64 { a + b }');
    expect(sig.untypedParams).toEqual([]);
  });

  test('valid Rust items still slice byte-identical', () => {
    const src = [
      '#[inline]',
      'pub fn dist(a: &[f64], b: &[f64]) -> f64 {',
      '  a.iter().zip(b).map(|(x, y)| (x - y).powi(2)).sum::<f64>().sqrt()',
      '}',
    ].join('\n');
    const item = scanRustItemAt(src, src.indexOf('pub fn'));
    expect(item).not.toBeNull();
    expect(item!.text).toBe(src);
    expect(item!.fns[0].untypedParams).toEqual([]);
  });

  test('untyped fn item slices verbatim too', () => {
    const src = 'fn pick(items, idx) {\n  items[idx].clone()\n}';
    const item = scanRustItemAt(src, 0);
    expect(item).not.toBeNull();
    expect(item!.text).toBe(src);
    expect(item!.fns[0].untypedParams).toEqual([0, 1]);
  });
});

describe('scanner: completion scan facts', () => {
  test('expression-tail body, no declared return', () => {
    const text = 'fn score(review) {\n  review["n"].as_i64() * 2\n}';
    const scan = scanRustFnCompletion(text, 'score')!;
    expect(scan.untypedParams).toEqual([0]);
    expect(scan.hasReturnType).toBe(false);
    expect(scan.bodyTailIsExpression).toBe(true);
    const [span] = scan.paramSpans;
    expect(text.slice(span.start, span.end)).toBe('review');
    expect(text[scan.paramsEnd - 1]).toBe(')');
  });

  test('statement-only body is not an expression tail', () => {
    const scan = scanRustFnCompletion('fn log_it(msg) {\n  println!("{msg}");\n}', 'log_it')!;
    expect(scan.bodyTailIsExpression).toBe(false);
  });

  test('empty body is not an expression tail', () => {
    const scan = scanRustFnCompletion('fn noop(x) { }', 'noop')!;
    expect(scan.bodyTailIsExpression).toBe(false);
  });

  test('trailing comment does not count as a tail expression', () => {
    const scan = scanRustFnCompletion(
      'fn f(x) {\n  do_thing(x);\n  // tail comment\n}', 'f')!;
    expect(scan.bodyTailIsExpression).toBe(false);
  });

  test('block-tail body (match) counts as expression', () => {
    const scan = scanRustFnCompletion(
      'fn f(x) {\n  match x.as_i64() { 0 => "z", _ => "n" }\n}', 'f')!;
    expect(scan.bodyTailIsExpression).toBe(true);
  });

  test('declared return type is detected', () => {
    const scan = scanRustFnCompletion('fn f(x) -> i64 { x.as_i64() }', 'f')!;
    expect(scan.hasReturnType).toBe(true);
  });
});

// ─── Codegen completion ─────────────────────────────────────────────

describe('codegen: gradual signature completion', () => {
  test('untyped param -> omnivm::Dyn, omitted return -> impl Serialize, body verbatim', () => {
    const code = [
      'fn top_score(reviews) {',
      '  let mut best = 0.0;',
      '  for r in reviews.iter() {',
      '    if r["score"] > best { best = r["score"].as_f64(); }',
      '  }',
      '  best',
      '}',
      '',
      'data = [{"score": 4.5}]',
      'const out = top_score(data)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain(
      'fn top_score(reviews: omnivm::Dyn) -> impl omnivm::serde::Serialize {',
    );
    // The body is byte-verbatim and the item line count is unchanged.
    expect(source).toContain('    if r["score"] > best { best = r["score"].as_f64(); }');
    const item = source.slice(source.indexOf('fn top_score'), source.indexOf('}\n\n') + 1);
    expect(item.split('\n').length).toBe(7);
    expect(source).toContain('omnivm::export_fn!(OmniVMCall_top_score, top_score, 1);');
  });

  test('statement-only body keeps a return-less signature', () => {
    const code = [
      'fn log_msg(msg) {',
      '  println!("{}", msg);',
      '}',
      'note = {"text": "hello"}',
      'const x = log_msg(note)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn log_msg(msg: omnivm::Dyn) {');
    expect(source).not.toContain('log_msg(msg: omnivm::Dyn) ->');
  });

  test('fully typed fns pass through byte-identical (no completion)', () => {
    const code = [
      'fn ping(n: i64) {',
      '  println!("{n}");',
      '}',
      'const x = ping(1)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn ping(n: i64) {');
    expect(source).not.toContain('impl omnivm::serde::Serialize');
  });

  test('source map line counts survive the rewrite', () => {
    const code = [
      'fn first(values) {',
      '  values[0].clone()',
      '}',
      'const out = first([1, 2])',
      'print(out)',
    ].join('\n');
    const manifest = compileManifest(code);
    const fd = manifest.ops.find(
      op => op.op === 'func_def' && (op as FuncDefOp).bodyRuntime === 'rust',
    ) as FuncDefOp;
    expect(fd.source_map).toBeDefined();
    const entry = fd.source_map!.find(e => e.poly_line === 1)!;
    expect(entry.lines).toBe(3); // the 3-line item, line count preserved
  });
});

describe('codegen: call-site stamping', () => {
  test('integer literal stamps i64 (and the typed lane follows)', () => {
    const code = [
      'fn scale(rating, factor: i64) -> i64 {',
      '  rating * factor',
      '}',
      'const out = scale(7, 6)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn scale(rating: i64, factor: i64) -> i64 {');
    // Stamped scalar signature qualifies for the omni_value_t typed lane.
    expect(source).toContain('omnivm::export_typed_fn!(OmniVMCallTyped_scale, scale, 2);');
  });

  test('float, string and bool literals stamp their types', () => {
    const code = [
      'fn blend(w, label, flag) {',
      '  format!("{w}{label}{flag}")',
      '}',
      'const out = blend(0.5, "x", true)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain(
      'fn blend(w: f64, label: String, flag: bool) -> impl omnivm::serde::Serialize {',
    );
  });

  test('conflicting call sites keep Dyn', () => {
    const code = [
      'fn mirror(v) {',
      '  v',
      '}',
      'const a = mirror(1)',
      'const b = mirror("two")',
      'print(a, b)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn mirror(v: omnivm::Dyn) -> impl omnivm::serde::Serialize {');
  });

  test('non-literal evidence keeps Dyn', () => {
    const code = [
      'fn mirror2(v) {',
      '  v',
      '}',
      'items = [1, 2, 3]',
      'const a = mirror2(items)',
      'print(a)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain('fn mirror2(v: omnivm::Dyn) -> impl omnivm::serde::Serialize {');
  });

  test('DataFrame provenance stamps the df lane when polars is in scope', () => {
    const code = [
      'import pandas as pd',
      'use polars::prelude::*;',
      '',
      'fn summarize(frame) {',
      '  frame.height() as i64',
      '}',
      '',
      'table = pd.read_csv("data.csv")',
      'const out = summarize(table)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain(
      'fn summarize(frame: polars::prelude::DataFrame) -> impl omnivm::serde::Serialize {',
    );
    expect(source).toContain('omnivm::export_fn!(OmniVMCall_summarize, summarize, (df));');
  });

  test('DataFrame provenance without polars in scope stays Dyn', () => {
    const code = [
      'import pandas as pd',
      '',
      'fn summarize2(frame) {',
      '  frame.len() as i64',
      '}',
      '',
      'table = pd.read_csv("data.csv")',
      'const out = summarize2(table)',
      'print(out)',
    ].join('\n');
    const source = rustUnitSource(compileManifest(code));
    expect(source).toContain(
      'fn summarize2(frame: omnivm::Dyn) -> impl omnivm::serde::Serialize {',
    );
  });
});
