/**
 * Rust language support — step 3 of docs/rust-runtime-design.md.
 *
 * Covers Pass 1 definite-Rust evidence, the ambiguity resolutions from the
 * design table (=>, let, ::, struct, panic!), Rust import provenance, the
 * @rs runtime tag, and emitRustFuncDef codegen against the north-star
 * example (examples/rust-review-service.poly).
 */

import * as fs from 'fs';
import * as path from 'path';
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import { RuntimeResolver, OmniRuntime } from '../src/runtime-resolver';
import { lookupMethodAffinity, lookupBuiltinAffinity, lookupGlobalAffinity } from '../src/runtime-resolver/method-tables';
import { analyzeBareImport, analyzeImportPath, RUST_MODULES } from '../src/runtime-resolver/import-analyzer';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';
import { DispatchManifest, ManifestOp, FuncDefOp, EvalOp, ExecOp } from '../src/codegen-omnivm/manifest-types';

function parseCode(code: string): { ast: AST.Program; errors: unknown[] } {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  return { ast, errors: parser.getErrors() };
}

function resolve(code: string) {
  const { ast } = parseCode(code);
  const resolver = new RuntimeResolver();
  return resolver.resolve(ast, code);
}

function affinityOfKind(result: ReturnType<typeof resolve>, kind: string) {
  const node = result.program.body.find(n => n.kind === kind);
  return node ? result.affinityMap.get(node) : undefined;
}

function allAffinities(result: ReturnType<typeof resolve>) {
  return [...result.affinityMap.entries()];
}

function compileManifest(code: string): DispatchManifest {
  const { ast, errors } = parseCode(code);
  expect(errors).toEqual([]);
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, code);
  const gen = new ManifestCodeGenerator();
  return gen.generate(annotated);
}

// ─── Pass 1: definite Rust evidence ────────────────────────────────

describe('Rust evidence: fn keyword', () => {
  test('fn declaration is definite Rust', () => {
    const result = resolve('fn add(a: i64, b: i64) -> i64 {\n  a + b\n}');
    const aff = affinityOfKind(result, 'FuncDecl');
    expect(aff?.runtime).toBe(OmniRuntime.Rust);
    expect(aff?.confidence).toBe('definite');
  });

  test('async fn declaration is Rust and parses async', () => {
    const { ast } = parseCode('async fn fetch_user(id: i64) -> Result<String, reqwest::Error> {\n  Ok(id)\n}');
    const fn = ast.body[0] as AST.FuncDecl;
    expect(fn.kind).toBe('FuncDecl');
    expect(fn.declKeyword).toBe('fn');
    expect(fn.async).toBe(true);
  });

  test('pub fn parses and is Rust', () => {
    const result = resolve('pub fn add(a: i64, b: i64) -> i64 {\n  a + b\n}');
    const aff = affinityOfKind(result, 'FuncDecl');
    expect(aff?.runtime).toBe(OmniRuntime.Rust);
  });
});

describe('Rust evidence: macro invocations ident!(', () => {
  test('println!(...) is definite Rust', () => {
    const result = resolve('println!("hello")');
    const call = allAffinities(result).find(([node]) => node.kind === 'Call');
    expect(call?.[1].runtime).toBe(OmniRuntime.Rust);
    expect(call?.[1].confidence).toBe('definite');
  });

  test('format! and vec! are Rust', () => {
    const result = resolve('fn f() {\n  let v = vec![1, 2];\n  let s = format!("x {y}");\n}');
    const calls = allAffinities(result)
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);
    expect(calls).toContain(OmniRuntime.Rust);
  });

  test('panic!( is Rust; bare panic( stays Go', () => {
    const macroResult = resolve('panic!("boom")');
    const macroCall = allAffinities(macroResult).find(([node]) => node.kind === 'Call');
    expect(macroCall?.[1].runtime).toBe(OmniRuntime.Rust);

    const goResult = resolve('panic("boom")');
    const goCall = allAffinities(goResult).find(([node]) => node.kind === 'Call');
    expect(goCall?.[1].runtime).toBe(OmniRuntime.Go);
  });
});

describe('Rust evidence: postfix .await', () => {
  test('.await postfix is definite Rust', () => {
    const result = resolve('fn f() {\n  let body = client.send().await?;\n}');
    const awaitNode = allAffinities(result).find(([node, aff]) =>
      node.kind === 'Unary' && (node as AST.Unary).op === 'await' && !(node as AST.Unary).prefix);
    expect(awaitNode?.[1].runtime).toBe(OmniRuntime.Rust);
    expect(awaitNode?.[1].confidence).toBe('definite');
  });

  test('JS prefix await stays JavaScript', () => {
    const result = resolve('async function f() {\n  const x = await fetch("/api")\n}');
    const fnAff = affinityOfKind(result, 'FuncDecl');
    expect(fnAff?.runtime).toBe(OmniRuntime.JavaScript);
  });
});

describe('Rust evidence: let forms', () => {
  test('let mut is definite Rust', () => {
    const result = resolve('fn f() {\n  let mut count = 0;\n}');
    const varDecl = allAffinities(result).find(([node]) => node.kind === 'VarDecl');
    expect(varDecl?.[1].runtime).toBe(OmniRuntime.Rust);
    expect(varDecl?.[1].confidence).toBe('definite');
  });

  test('let with Rust primitive type annotation is definite Rust', () => {
    const result = resolve('let x: i32 = 5');
    const varDecl = allAffinities(result).find(([node]) => node.kind === 'VarDecl');
    expect(varDecl?.[1].runtime).toBe(OmniRuntime.Rust);
    expect(varDecl?.[1].confidence).toBe('definite');
  });

  test('bare let stays ambiguous (not claimed by Rust)', () => {
    const result = resolve('let x = 5');
    const varDecl = allAffinities(result).find(([node]) => node.kind === 'VarDecl');
    expect(varDecl?.[1]?.runtime).not.toBe(OmniRuntime.Rust);
  });

  test('TS-typed let is not claimed by Rust', () => {
    const result = resolve('let x: number = 5');
    const varDecl = allAffinities(result).find(([node]) => node.kind === 'VarDecl');
    expect(varDecl?.[1]?.runtime).not.toBe(OmniRuntime.Rust);
  });
});

describe('Rust evidence: &mut and closures', () => {
  test('&mut borrow is definite Rust', () => {
    const result = resolve('fn f() {\n  process(&mut data);\n}');
    const borrow = allAffinities(result).find(([node]) =>
      node.kind === 'Unary' && (node as AST.Unary).op === '&mut');
    expect(borrow?.[1].runtime).toBe(OmniRuntime.Rust);
  });

  test('|x| closure parses as a Rust lambda', () => {
    const result = resolve('let doubled = items.map(|x| x * 2);');
    const lambda = allAffinities(result).find(([node]) => node.kind === 'Lambda');
    expect(lambda?.[1].runtime).toBe(OmniRuntime.Rust);
  });
});

// ─── Ambiguity resolutions ─────────────────────────────────────────

describe('Ambiguity: => contextual (match anchor)', () => {
  test('=> inside a Rust-style match body does not create JS affinity', () => {
    const code = 'fn grade(s: f64) -> i32 {\n  match s {\n    x if x > 0.5 => 1,\n    _ => 0,\n  }\n}';
    const result = resolve(code);
    const fnAff = affinityOfKind(result, 'FuncDecl');
    expect(fnAff?.runtime).toBe(OmniRuntime.Rust);
    const jsArrowClaims = allAffinities(result).filter(([, aff]) =>
      aff.runtime === OmniRuntime.JavaScript &&
      aff.evidence.some(e => e.detail.includes('arrow function')));
    expect(jsArrowClaims).toEqual([]);
  });

  test('closure containing a match with => arms stays Rust', () => {
    const code = 'let labels = xs.map(|r| match r.score {\n  s if s > 0.9 => 1,\n  _ => 0,\n});';
    const result = resolve(code);
    const lambda = allAffinities(result).find(([node]) => node.kind === 'Lambda');
    expect(lambda?.[1].runtime).toBe(OmniRuntime.Rust);
  });

  test('=> outside match is still a definite JS arrow', () => {
    const result = resolve('const f = (x) => x + 1');
    const lambda = allAffinities(result).find(([node]) => node.kind === 'Lambda');
    expect(lambda?.[1].runtime).toBe(OmniRuntime.JavaScript);
    expect(lambda?.[1].confidence).toBe('definite');
  });
});

describe('Ambiguity: :: paths', () => {
  test('lowercase::lowercase path is Rust-leaning', () => {
    const result = resolve('let handle = tokio::spawn(work());');
    const member = allAffinities(result).find(([node]) => node.kind === 'Member');
    expect(member?.[1].runtime).toBe(OmniRuntime.Rust);
  });

  test('Vec::new() resolves to Rust', () => {
    const result = resolve('let v = Vec::new();');
    const call = allAffinities(result).find(([node]) => node.kind === 'Call');
    expect(call?.[1].runtime).toBe(OmniRuntime.Rust);
  });

  test('use-imported type with :: resolves to Rust', () => {
    const result = resolve('use reqwest::Client;\nconst http = Client::new()');
    const call = allAffinities(result).find(([node]) => node.kind === 'Call');
    expect(call?.[1].runtime).toBe(OmniRuntime.Rust);
  });

  test('Constant-cased :: path without Rust context stays Ruby', () => {
    const result = resolve('const response = Rack::Response.new("hello", 200)');
    const rubyClaims = allAffinities(result).filter(([, aff]) => aff.runtime === OmniRuntime.Ruby);
    expect(rubyClaims.length).toBeGreaterThan(0);
  });
});

describe('Ambiguity: struct keyword order', () => {
  test('struct X { is definite Rust', () => {
    const result = resolve('struct Review {\n  user_id: i64,\n  text: String,\n}');
    const aff = affinityOfKind(result, 'TypeDecl');
    expect(aff?.runtime).toBe(OmniRuntime.Rust);
    expect(aff?.confidence).toBe('definite');
    const decl = result.program.body[0] as AST.TypeDecl;
    expect(decl.structDecl).toBe(true);
    expect(decl.name.name).toBe('Review');
  });

  test('enum with data-carrying variants is definite Rust', () => {
    const result = resolve('enum Verdict {\n  Approved { score: f64 },\n  NeedsHuman,\n}');
    const aff = affinityOfKind(result, 'EnumDecl');
    expect(aff?.runtime).toBe(OmniRuntime.Rust);
    expect(aff?.confidence).toBe('definite');
  });

  test('plain enum is not claimed by Rust', () => {
    const result = resolve('enum Color {\n  Red,\n  Green,\n}');
    const aff = affinityOfKind(result, 'EnumDecl');
    expect(aff?.runtime).not.toBe(OmniRuntime.Rust);
  });
});

// ─── Imports ───────────────────────────────────────────────────────

describe('Rust import provenance', () => {
  test('use crate::path; is Rust import evidence', () => {
    const result = resolve('use polars::prelude::*;');
    const aff = affinityOfKind(result, 'Import');
    expect(aff?.runtime).toBe(OmniRuntime.Rust);
  });

  test('grouped use serde::{Serialize, Deserialize}; is Rust and binds specifiers', () => {
    const result = resolve('use serde::{Serialize, Deserialize};\nconst x = Serialize');
    const aff = affinityOfKind(result, 'ImportDecl');
    expect(aff?.runtime).toBe(OmniRuntime.Rust);
    expect(aff?.confidence).toBe('definite');
    const identifier = allAffinities(result).find(([node]) =>
      node.kind === 'Identifier' && (node as AST.Identifier).name === 'Serialize');
    expect(identifier?.[1].runtime).toBe(OmniRuntime.Rust);
  });

  test('RUST_MODULES contains the design-doc crate roots', () => {
    for (const root of ['std', 'core', 'alloc', 'tokio', 'serde', 'serde_json', 'polars',
                        'arrow', 'reqwest', 'hyper', 'axum', 'rayon', 'regex', 'chrono',
                        'anyhow', 'thiserror', 'itertools', 'futures', 'crossbeam',
                        'ndarray', 'candle', 'nalgebra']) {
      expect(RUST_MODULES.has(root)).toBe(true);
    }
  });

  test('analyzeBareImport recognizes a::b paths as Rust', () => {
    expect(analyzeBareImport('tokio::time::sleep')?.runtime).toBe(OmniRuntime.Rust);
    expect(analyzeBareImport('reqwest::Client')?.runtime).toBe(OmniRuntime.Rust);
    expect(analyzeImportPath('serde_json::Value')?.runtime).toBe(OmniRuntime.Rust);
  });

  test('Ruby Constant::Constant include paths are not claimed as Rust', () => {
    expect(analyzeBareImport('ActiveRecord::Base')).toBeUndefined();
  });

  test('use import of a known crate is definite', () => {
    expect(analyzeBareImport('tokio::main')?.confidence).toBe('definite');
  });
});

// ─── Builtin / method affinity tables ──────────────────────────────

describe('Rust method tables', () => {
  test('Some/Ok/Err are Rust builtins', () => {
    expect(lookupBuiltinAffinity('Some')).toBe(OmniRuntime.Rust);
    expect(lookupBuiltinAffinity('Ok')).toBe(OmniRuntime.Rust);
    expect(lookupBuiltinAffinity('Err')).toBe(OmniRuntime.Rust);
  });

  test('.unwrap() and .expect() are Rust method affinity', () => {
    expect(lookupMethodAffinity('unwrap')).toBe(OmniRuntime.Rust);
    expect(lookupMethodAffinity('expect')).toBe(OmniRuntime.Rust);
  });

  test('Vec, tokio, serde_json are Rust global roots', () => {
    expect(lookupGlobalAffinity('Vec')).toBe(OmniRuntime.Rust);
    expect(lookupGlobalAffinity('tokio')).toBe(OmniRuntime.Rust);
    expect(lookupGlobalAffinity('serde_json')).toBe(OmniRuntime.Rust);
  });

  test('Some(value) call resolves to Rust', () => {
    const result = resolve('fn f() {\n  return Some(42);\n}');
    const call = allAffinities(result).find(([node]) =>
      node.kind === 'Call' && (node as AST.Call).callee.kind === 'Identifier');
    expect(call?.[1].runtime).toBe(OmniRuntime.Rust);
  });
});

// ─── @rs runtime tag ───────────────────────────────────────────────

describe('@rs runtime tag', () => {
  test('@rs(expr) parses as a RuntimeTag', () => {
    const { ast, errors } = parseCode('const x = @rs(compute())');
    expect(errors).toEqual([]);
    const decl = ast.body[0] as AST.ConstDecl;
    expect(decl.values[0].kind).toBe('RuntimeTag');
    expect((decl.values[0] as AST.RuntimeTag).runtime).toBe('rs');
  });

  test('@rs(expr) resolves to Rust with definite confidence', () => {
    const result = resolve('const x = @rs(compute())');
    const tag = allAffinities(result).find(([node]) => node.kind === 'RuntimeTag');
    expect(tag?.[1].runtime).toBe(OmniRuntime.Rust);
    expect(tag?.[1].confidence).toBe('definite');
  });
});

// ─── Codegen: emitRustFuncDef and the north-star example ───────────

describe('emitRustFuncDef', () => {
  // The fns below carry a poly call site: only fns referenced from OUTSIDE
  // the Rust unit are exported (shim + `exports` entry + func_def op).
  test('a Rust fn called from poly emits a func_def with the full unit and shim', () => {
    const m = compileManifest('fn add(a: i64, b: i64) -> i64 {\n  a + b\n}\nconst total = add(2, 3)\nprint(total)');
    const funcDefs = m.ops.filter(op => op.op === 'func_def') as FuncDefOp[];
    expect(funcDefs).toHaveLength(1);
    const def = funcDefs[0];
    expect(def.name).toBe('add');
    expect(def.bodyRuntime).toBe('rust');
    expect(def.params.map(p => p.name)).toEqual(['a', 'b']);
    expect(def.exports).toEqual(['add']);
    expect(def.source).toContain('fn add(a: i64, b: i64) -> i64');
    expect(def.source).toContain('omnivm::export_fn!(OmniVMCall_add, add, 2);');
    expect(def.async).toBeUndefined();
  });

  test('async fn emits async func_def with export_async_fn shim', () => {
    const m = compileManifest('async fn ping(url: String) -> String {\n  url\n}\nconst pong = ping("hi")\nprint(pong)');
    const def = m.ops.find(op => op.op === 'func_def') as FuncDefOp;
    expect(def.async).toBe(true);
    expect(def.source).toContain('async fn ping');
    expect(def.source).toContain('omnivm::export_async_fn!(OmniVMCall_ping, ping, 1);');
  });

  test('a Rust fn with no outside references is internal-only: no func_def, no shim', () => {
    const m = compileManifest('fn helper(a: i64) -> i64 {\n  a * 2\n}\nprint("done")');
    expect(m.ops.filter(op => op.op === 'func_def')).toEqual([]);
  });
});

describe('north-star example: rust-review-service.poly', () => {
  const examplePath = path.join(__dirname, '..', 'examples', 'rust-review-service.poly');
  const source = fs.readFileSync(examplePath, 'utf-8');
  let manifest: DispatchManifest;
  let funcDefs: FuncDefOp[];

  beforeAll(() => {
    manifest = compileManifest(source);
    funcDefs = manifest.ops.filter(op => op.op === 'func_def') as FuncDefOp[];
  });

  test('compiles without parse errors or error diagnostics', () => {
    const errors = (manifest.diagnostics || []).filter(d => d.severity === 'error');
    expect(errors).toEqual([]);
  });

  test('enrich/classify/heavy_stats become rust func_def ops', () => {
    const names = funcDefs.map(d => d.name).sort();
    expect(names).toEqual(['classify', 'enrich', 'heavy_stats']);
    for (const def of funcDefs) {
      expect(def.bodyRuntime).toBe('rust');
    }
    const enrich = funcDefs.find(d => d.name === 'enrich')!;
    expect(enrich.async).toBe(true);
    expect(enrich.params.map(p => p.name)).toEqual(['user_id']);
  });

  test('every rust func_def carries the SAME full unit source and exports', () => {
    const sources = new Set(funcDefs.map(d => d.source));
    expect(sources.size).toBe(1);
    for (const def of funcDefs) {
      expect(def.exports).toEqual(['enrich', 'classify', 'heavy_stats']);
    }
  });

  test('the unit source contains items, statics, fns, and shim macros', () => {
    const unit = funcDefs[0].source!;
    // use statements
    expect(unit).toContain('use polars::prelude::*;');
    expect(unit).toContain('use reqwest::Client;');
    expect(unit).toContain('use rayon::prelude::*;');
    expect(unit).toContain('use serde::{Serialize, Deserialize};');
    // structs/enums with attributes preserved
    expect(unit).toContain('#[derive(Serialize, Deserialize)]');
    expect(unit).toContain('struct Review {');
    expect(unit).toContain('#[serde(tag = "type")]');
    expect(unit).toContain('enum Verdict {');
    expect(unit).toContain('Approved { score: f64 }');
    // const http -> LazyLock static, author's name preserved
    expect(unit).toContain('static http: std::sync::LazyLock<Client> = std::sync::LazyLock::new(|| Client::new());');
    // fns verbatim, async restored
    expect(unit).toContain('async fn enrich(user_id: i64) -> Result<String, reqwest::Error> {');
    expect(unit).toContain('fn classify(frame: DataFrame) -> Vec<Verdict> {');
    expect(unit).toContain('fn heavy_stats(frame: DataFrame) -> DataFrame {');
    expect(unit).toContain('.send().await?');
    // export shims, one per fn, async vs sync
    expect(unit).toContain('omnivm::export_async_fn!(OmniVMCall_enrich, enrich, 1);');
    expect(unit).toContain('omnivm::export_fn!(OmniVMCall_classify, classify, (df));');
    expect(unit).toContain('omnivm::export_fn!(OmniVMCall_heavy_stats, heavy_stats, (df));');
  });

  test('module-level Rust items emit no standalone exec ops', () => {
    expect(manifest.ops.filter(op => op.op === 'exec_compiled')).toEqual([]);
    // use-imports are absorbed into the unit, not emitted as import ops
    const importOps = manifest.ops.filter(op => op.op === 'import') as Array<ManifestOp & { path?: string }>;
    expect(importOps.map(op => op.path).sort()).toEqual(['express', 'pandas']);
  });

  test('the pandas lines are python ops', () => {
    const pythonOps = manifest.ops.filter(op =>
      (op as EvalOp).runtime === 'python' &&
      ((op as EvalOp).code || '').includes('read_parquet'));
    expect(pythonOps.length).toBe(1);
    expect((pythonOps[0] as EvalOp).bind).toBe('df');
  });

  test('heavy_stats/classify calls are orchestration evals into rust', () => {
    const rustEvals = manifest.ops.filter(op =>
      op.op === 'eval' && (op as EvalOp).runtime === 'rust') as EvalOp[];
    const codes = rustEvals.map(op => op.code);
    expect(codes).toContain('heavy_stats(df)');
    expect(codes).toContain('classify(df)');
    for (const op of rustEvals) {
      expect(op.captures).toHaveProperty('df');
    }
  });

  test('the express lines are javascript ops; await enrich(...) stays a JS-side stub call', () => {
    const jsOps = manifest.ops.filter(op =>
      (op as ExecOp).runtime === 'javascript') as Array<EvalOp | ExecOp>;
    const appGet = jsOps.find(op => (op.code || '').includes('app.get'));
    expect(appGet).toBeDefined();
    expect(appGet!.code).toContain('await enrich(parseInt(req.params.id))');
    expect((appGet as ExecOp).captures).toHaveProperty('enrich');
    expect((appGet as ExecOp).captures).toHaveProperty('verdicts');
    const listen = jsOps.find(op => (op.code || '').includes('app.listen(3000)'));
    expect(listen).toBeDefined();
    const express = jsOps.find(op => (op.code || '').includes('express()'));
    expect(express).toBeDefined();
  });
});

// ─── Registry-sweep fixes: raw-scanned item anchors ─────────────────
//
// Each describe below pins one failure group from the Rust registry sweep
// (scripts/rust-registry-sweep.js): macro-invocation items, module-level
// attributes (inner + multi-line outer), lifetime-typed consts, unsafe impl
// with where clauses, and pub consts of user-defined types.

import { scanTopLevelRustItems } from '../src/rust-item-scanner';

function rustItems(code: string): AST.RustItem[] {
  const { ast, errors } = parseCode(code);
  expect(errors).toEqual([]);
  return ast.body.filter((n): n is AST.RustItem => n.kind === 'RustItem');
}

describe('Rust items inside macro invocations (sweep group 1)', () => {
  test('pin_project! { ... } is one opaque macro-call item', () => {
    const code = [
      'pin_project! {',
      '    /// A future which can be remotely short-circuited.',
      '    #[derive(Debug, Clone)]',
      '    pub struct Abortable<T> {',
      '        #[pin]',
      '        task: T,',
      '    }',
      '}',
      'print("done")',
    ].join('\n');
    const items = rustItems(code);
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('macro-call');
    expect(items[0].text).toContain('pub struct Abortable<T>');
    // The struct must NOT leak into the union grammar.
    const { ast } = parseCode(code);
    expect(ast.body.map(n => n.kind)).toEqual(['RustItem', 'Echo']);
  });

  test('path-qualified cfg_if-style invocation with braces', () => {
    const code = 'cfg_if::cfg_if! {\n    if #[cfg(unix)] {\n        fn imp() {}\n    }\n}\nprint("x")';
    const items = rustItems(code);
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('macro-call');
  });

  test('paren-delimited invocation requires the trailing semicolon', () => {
    const withSemi = rustItems('delegate_all!(\n    Flatten<F>(flatten::Flatten<F>)\n);\nprint("x")');
    expect(withSemi).toHaveLength(1);
    expect(withSemi[0].itemKind).toBe('macro-call');
    expect(withSemi[0].text.endsWith(';')).toBe(true);

    // No trailing `;` — not an item-position macro: stays in the union
    // grammar (panic!("boom") keeps resolving as a Rust-affine Call).
    const { ast } = parseCode('panic!("boom")');
    expect(ast.body.some(n => n.kind === 'RustItem')).toBe(false);
  });

  test('macro-call items flow verbatim into the rust unit', () => {
    const code = 'use pin_project_lite::pin_project;\n\npin_project! {\n    pub struct Fuse<Fut> {\n        #[pin]\n        inner: Option<Fut>,\n    }\n}\n\nfn poke() -> i64 { 1 }\n\nprint(poke())';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    const resolver = new RuntimeResolver();
    const annotated = resolver.resolve(ast, code);
    const gen = new ManifestCodeGenerator();
    gen.generate(annotated);
    const unit = (gen as unknown as { rustUnit?: { source: string } }).rustUnit;
    expect(unit).toBeDefined();
    expect(unit!.source).toContain('pin_project! {');
    expect(unit!.source).toContain('pub struct Fuse<Fut>');
    // The scanner and the parser agree byte-for-byte.
    const scanned = scanTopLevelRustItems(code.slice(0, code.indexOf('print(')));
    expect(scanned.map(i => i.itemKind)).toEqual(['use', 'macro-call', 'fn']);
    for (const item of scanned) {
      expect(unit!.source).toContain(item.text);
    }
  });
});

describe('Module-level attributes as items (sweep group 2)', () => {
  test('multi-line inner attribute #![doc(...)] is module trivia, not tokens', () => {
    const code = [
      '#![doc(test(',
      '    no_crate_inject,',
      '    attr(deny(warnings), allow(dead_code, unused_variables))',
      '))]',
      '#![no_std]',
      '',
      'fn imp() -> i64 { 1 }',
      'print("done")',
    ].join('\n');
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    // The inner attributes vanish as module trivia; the fn parses normally
    // (union FuncDecl or raw-scanned item) and the tail survives.
    expect(ast.body).toHaveLength(2);
    expect(['FuncDecl', 'RustItem']).toContain(ast.body[0].kind);
    expect(ast.body[1].kind).toBe('Echo');
  });

  test('multi-line outer #[cfg_attr(...)] walks back onto the next item', () => {
    const code = [
      '#[derive(PartialEq, Eq, Copy, Clone, Debug)]',
      '#[cfg_attr(',
      '    any(feature = "rkyv", feature = "rkyv-16"),',
      '    derive(Archive, Deserialize, Serialize)',
      ')]',
      'pub enum Month {',
      '    January = 0,',
      '}',
      'print("done")',
    ].join('\n');
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    expect(ast.body[ast.body.length - 1].kind).toBe('Echo');
    // The raw scanner attaches BOTH attribute lines (incl. the multi-line
    // group) to the enum item, so the verbatim slice keeps them.
    const scanned = scanTopLevelRustItems(code.slice(0, code.indexOf('print(')));
    expect(scanned).toHaveLength(1);
    expect(scanned[0].itemKind).toBe('enum');
    expect(scanned[0].text).toContain('#[cfg_attr(');
    expect(scanned[0].text).toContain('#[derive(PartialEq, Eq, Copy, Clone, Debug)]');
  });

  test('attribute with a multi-line string argument is consumed wholly', () => {
    const code = [
      '#[deprecated = "',
      'This macro has no effect.',
      '"]',
      '#[doc(hidden)]',
      'macro_rules! noop { () => {}; }',
      'print("done")',
    ].join('\n');
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    const items = ast.body.filter((n): n is AST.RustItem => n.kind === 'RustItem');
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('macro');
  });

  test('python hash comments are untouched (single-line discipline)', () => {
    const code = '# [TODO] revisit this\n# plain comment\nx = 1\nprint(x)';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    expect(ast.body.some(n => n.kind === 'RustItem')).toBe(false);
  });
});

describe('Lifetime in const type (sweep group 3)', () => {
  test("const X: &'static [(char, char)] = ... is a raw-scanned const", () => {
    const code = "pub const PAIRS: &'static [(char, char)] = &[('a', 'b'), ('c', 'd')];\nprint(\"done\")";
    const items = rustItems(code);
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('const');
    expect(items[0].name).toBe('PAIRS');
    expect(items[0].text).toBe("pub const PAIRS: &'static [(char, char)] = &[('a', 'b'), ('c', 'd')];");
  });

  test('bare const with &Type reference type is Rust-only', () => {
    const items = rustItems("const FRAGMENT: &AsciiSet = &CONTROLS.add(b' ');\nprint(\"x\")");
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('const');
  });

  test('bare const with ::-path initializer is Rust-only', () => {
    const items = rustItems('const FINAL: StateID = StateID::ZERO;\nprint("x")');
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('const');
  });

  test('TS-style consts stay in the union grammar', () => {
    for (const code of [
      'const x: number = 5;',
      'const greeting: string = "hello";',
      "const sep: string = '::';",
    ]) {
      const { ast, errors } = parseCode(code);
      expect(errors).toEqual([]);
      expect(ast.body.some(n => n.kind === 'RustItem')).toBe(false);
    }
  });
});

describe('unsafe impl ... where ... {} (sweep group 4)', () => {
  test('unsafe impl with a where clause is one opaque impl item', () => {
    const code = 'unsafe impl Send for A where A: Sized {}\nprint("done")';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    const items = ast.body.filter((n): n is AST.RustItem => n.kind === 'RustItem');
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('impl');
    expect(items[0].text).toBe('unsafe impl Send for A where A: Sized {}');
    expect(ast.body[ast.body.length - 1].kind).toBe('Echo');
  });

  test('unsafe trait declaration anchors too', () => {
    const code = 'unsafe trait Zeroable {}\nprint("done")';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    const last = ast.body[ast.body.length - 1];
    expect(last.kind).toBe('Echo');
  });
});

describe('pub const with user-defined type (sweep group 5)', () => {
  test('pub const NAME: UserType = ... is unconditionally Rust', () => {
    const code = 'pub const STANDARD: Alphabet = Alphabet { chars: 0 };\nprint("done")';
    const items = rustItems(code);
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('const');
    expect(items[0].name).toBe('STANDARD');
  });

  test('pub(crate) const works the same', () => {
    const items = rustItems('pub(crate) const USERINFO: AsciiSet = X;\nprint("x")');
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('const');
  });
});

describe('Tail-swallow regressions (sweep group 6)', () => {
  test('fn with lifetime in where clause does not absorb following items', () => {
    const code = [
      'pub fn lazy<F, R>(f: F) -> Lazy<F>',
      'where',
      "    F: FnOnce(&mut Context<'_>) -> R,",
      '{',
      '    assert_future::<R, _>(Lazy { f: Some(f) })',
      '}',
      '',
      'print("done")',
    ].join('\n');
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    expect(ast.body.map(n => n.kind)).toEqual(['RustItem', 'Echo']);
  });

  test('nested generics << do not scan as a heredoc', () => {
    const code = 'fn collect<I>(iter: I) -> Vec<<I as IntoIterator>::Item> {\n    iter.into_iter().collect()\n}\nprint("done")';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    expect(ast.body[ast.body.length - 1].kind).toBe('Echo');
  });

  test('bash heredocs still lex as one string', () => {
    const code = 'cat <<EOF\nhello there\nEOF\necho done';
    const tokens = new Lexer(code).tokenize();
    const heredoc = tokens.find(t => String(t.value).startsWith('<<EOF'));
    expect(heredoc).toBeDefined();
    expect(String(heredoc!.value)).toContain('hello there');
  });

  test('fn new<E> generic does not open JSX mode', () => {
    const code = 'impl Error {\n    pub fn new<E>(error: E) -> Self {\n        Error { e: error }\n    }\n}\n\nfn after() -> i64 { 2 }\nprint("done")';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    expect(ast.body[ast.body.length - 1].kind).toBe('Echo');
  });

  test('qualified path <A as B>::C in a body neither hangs nor swallows', () => {
    const code = 'fn seeded() -> X {\n    let mut seed = <XorShiftRng as SeedableRng>::Seed::default();\n    seed\n}\nprint("done")';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    expect(ast.body[ast.body.length - 1].kind).toBe('Echo');
  });

  test('prefix-less use group `use { ... };` is one use item', () => {
    const code = 'use {\n    csv_core::{Reader as CoreReader},\n    serde_core::de::DeserializeOwned,\n};\nprint("done")';
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    const items = ast.body.filter((n): n is AST.RustItem => n.kind === 'RustItem');
    expect(items).toHaveLength(1);
    expect(items[0].itemKind).toBe('use');
  });
});

// ─── Rust diagnostic source map ────────────────────────────────────

describe('Rust unit source map (rustc diagnostics -> .poly lines)', () => {
  const code = [
    'import polars as pl',          // poly line 1
    '',                             // 2
    'def summarize(rows):',         // 3
    '    return len(rows)',         // 4
    '',                             // 5
    'use regex::Regex;',            // 6
    '',                             // 7
    '#[derive(Clone)]',             // 8
    'struct Token {',               // 9
    '    word: String,',            // 10
    '}',                            // 11
    '',                             // 12
    'fn tokenize(text: String) -> Vec<String> {',  // 13
    '    let re = Regex::new(r"\\w+").unwrap();',  // 14
    '    re.find_iter(&text).map(|m| m.as_str().to_string()).collect()', // 15
    '}',                            // 16
    '',                             // 17
    'result = tokenize("hello")',   // 18
    'print(summarize(result))',     // 19
  ].join('\n');

  function rustFuncDef(manifest: DispatchManifest): FuncDefOp {
    const fd = manifest.ops.find(
      (op): op is FuncDefOp => op.op === 'func_def' && (op as FuncDefOp).bodyRuntime === OmniRuntime.Rust,
    );
    expect(fd).toBeDefined();
    return fd!;
  }

  test('verbatim items map unit_line -> poly_line; glue gets no entries', () => {
    const fd = rustFuncDef(compileManifest(code));
    expect(fd.source).toBeDefined();
    expect(fd.source_map).toEqual([
      { unit_line: 1, poly_line: 6, lines: 1 },   // use regex::Regex;
      { unit_line: 3, poly_line: 8, lines: 4 },   // #[derive] struct Token
      { unit_line: 8, poly_line: 13, lines: 4 },  // fn tokenize
    ]);
    // Cross-check every entry: the unit slice IS the poly slice.
    const unitLines = fd.source!.split('\n');
    const polyLines = code.split('\n');
    for (const entry of fd.source_map!) {
      for (let i = 0; i < entry.lines; i++) {
        expect(unitLines[entry.unit_line - 1 + i]).toBe(polyLines[entry.poly_line - 1 + i]);
      }
    }
    // The export shim is generated glue: it must sit past every mapped extent.
    const shimLine = unitLines.findIndex(l => l.includes('export_fn!')) + 1;
    expect(shimLine).toBeGreaterThan(0);
    for (const entry of fd.source_map!) {
      expect(shimLine).toBeGreaterThanOrEqual(entry.unit_line + entry.lines);
    }
  });

  test('poly_file flows from generate options into the op', () => {
    const { ast, errors } = parseCode(code);
    expect(errors).toEqual([]);
    const resolver = new RuntimeResolver();
    const annotated = resolver.resolve(ast, code);
    const gen = new ManifestCodeGenerator();
    const fd = rustFuncDef(gen.generate(annotated, { sourceFile: 'review.poly' }));
    expect(fd.poly_file).toBe('review.poly');
    expect(fd.source_map?.length).toBeGreaterThan(0);
  });

  test('no sourceFile option -> no poly_file, map still present', () => {
    const fd = rustFuncDef(compileManifest(code));
    expect(fd.poly_file).toBeUndefined();
    expect(fd.source_map?.length).toBeGreaterThan(0);
  });
});
