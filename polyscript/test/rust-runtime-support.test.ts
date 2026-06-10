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
    expect(unit).toContain('fn classify(reviews: Vec<Review>) -> Vec<Verdict> {');
    expect(unit).toContain('fn heavy_stats(frame: DataFrame) -> DataFrame {');
    expect(unit).toContain('.send().await?');
    // export shims, one per fn, async vs sync
    expect(unit).toContain('omnivm::export_async_fn!(OmniVMCall_enrich, enrich, 1);');
    expect(unit).toContain('omnivm::export_fn!(OmniVMCall_classify, classify, 1);');
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
    expect(codes).toContain('classify(df.to_dict("records"))');
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
