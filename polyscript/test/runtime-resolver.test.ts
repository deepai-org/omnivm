import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import { RuntimeResolver, OmniRuntime, RuntimeAffinity, MarshalKind } from '../src/runtime-resolver';
import { lookupMethodAffinity, lookupBuiltinAffinity, lookupGlobalAffinity } from '../src/runtime-resolver/method-tables';
import { analyzeImportPath, analyzeBareImport } from '../src/runtime-resolver/import-analyzer';
import { SymbolTable } from '../src/runtime-resolver/symbol-table';
import { computeBridgeCost, majorityRuntime, totalBridgeCost } from '../src/runtime-resolver/cost-model';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  return parser.parse();
}

function resolve(code: string) {
  const ast = parseCode(code);
  const resolver = new RuntimeResolver();
  return resolver.resolve(ast, code);
}

// --- Parser Enrichment Tests ---

describe('Parser Enrichment', () => {
  test('FuncDecl captures declKeyword: "function"', () => {
    const ast = parseCode('function greet(name) { return name }');
    const func = ast.body[0] as AST.FuncDecl;
    expect(func.kind).toBe('FuncDecl');
    expect(func.declKeyword).toBe('function');
  });

  test('FuncDecl captures declKeyword: "def"', () => {
    const ast = parseCode('def greet(name):\n  return name');
    const func = ast.body[0] as AST.FuncDecl;
    expect(func.kind).toBe('FuncDecl');
    expect(func.declKeyword).toBe('def');
  });

  test('Python-shaped class resolves to Python', () => {
    const result = resolve('class Session:\n  def __enter__(self):\n    return self\n  def __exit__(self, exc_type, exc, tb):\n    pass');
    const cls = result.program.body[0] as AST.ClassDecl;
    expect(cls.kind).toBe('ClassDecl');
    expect(result.affinityMap.get(cls)?.runtime).toBe(OmniRuntime.Python);
  });

  test('FuncDecl captures declKeyword: "fn"', () => {
    // Note: "func" is not a keyword in the lexer, so it doesn't parse as a top-level
    // function declaration. We test with "fn" which IS a keyword.
    const ast = parseCode('fn main() { }');
    const func = ast.body[0] as AST.FuncDecl;
    expect(func.kind).toBe('FuncDecl');
    expect(func.declKeyword).toBe('fn');
  });

  test('FuncDecl captures declKeyword: "fn"', () => {
    const ast = parseCode('fn compute(x: i32) -> i32 { x * 2 }');
    const func = ast.body[0] as AST.FuncDecl;
    expect(func.kind).toBe('FuncDecl');
    expect(func.declKeyword).toBe('fn');
  });

  test('Match captures style: "rust" for brace-style', () => {
    const ast = parseCode('match x { 1 => "one", _ => "other" }');
    // Match can appear as ExprStmt or direct
    const node = ast.body[0];
    let match: AST.Match;
    if (node.kind === 'ExprStmt') {
      match = (node as AST.ExprStmt).expr as AST.Match;
    } else {
      match = node as AST.Match;
    }
    expect(match.kind).toBe('Match');
    expect(match.style).toBe('rust');
  });

  test('Program captures runtimeDirective from // @runtime comment', () => {
    const code = '// @runtime python\nx = 42';
    // Parser must receive source text to extract directives from comments
    const ast = parseCode(code);
    expect(ast.runtimeDirective).toBe('python');
  });

  test('Program has no runtimeDirective without comment', () => {
    const ast = parseCode('x = 42');
    expect(ast.runtimeDirective).toBeUndefined();
  });

  test('@py() parses as RuntimeTag expression', () => {
    const ast = parseCode('x = @py(len(items))');
    // Look for RuntimeTag in the expression tree
    const stmt = ast.body[0] as any;
    const expr = stmt.expr || stmt;
    // Find RuntimeTag somewhere in the tree
    function findRuntimeTag(node: any): AST.RuntimeTag | null {
      if (!node || typeof node !== 'object') return null;
      if (node.kind === 'RuntimeTag') return node;
      for (const key of Object.keys(node)) {
        const result = findRuntimeTag(node[key]);
        if (result) return result;
      }
      return null;
    }
    const tag = findRuntimeTag(expr);
    expect(tag).not.toBeNull();
    expect(tag!.runtime).toBe('py');
  });

  test('@js() parses as RuntimeTag expression', () => {
    const ast = parseCode('result = @js(fetch("/api"))');
    function findRuntimeTag(node: any): AST.RuntimeTag | null {
      if (!node || typeof node !== 'object') return null;
      if (node.kind === 'RuntimeTag') return node;
      for (const key of Object.keys(node)) {
        const result = findRuntimeTag(node[key]);
        if (result) return result;
      }
      return null;
    }
    const stmt = ast.body[0] as any;
    const tag = findRuntimeTag(stmt);
    expect(tag).not.toBeNull();
    expect(tag!.runtime).toBe('js');
  });

  test('@go() parses as RuntimeTag expression', () => {
    const ast = parseCode('result = @go(fmt.Sprintf("hello"))');
    function findRuntimeTag(node: any): AST.RuntimeTag | null {
      if (!node || typeof node !== 'object') return null;
      if (node.kind === 'RuntimeTag') return node;
      for (const key of Object.keys(node)) {
        const result = findRuntimeTag(node[key]);
        if (result) return result;
      }
      return null;
    }
    const stmt = ast.body[0] as any;
    const tag = findRuntimeTag(stmt);
    expect(tag).not.toBeNull();
    expect(tag!.runtime).toBe('go');
  });
});

describe('Lambda Runtime Syntax', () => {
  test('Python lambda syntax resolves to Python', () => {
    const result = resolve('view = lambda request: JsonResponse({"path": request.path})');
    const stmt = result.program.body[0] as AST.ExprStmt;
    const assign = stmt.expr as AST.Assign;
    const lambda = assign.right as AST.Lambda;

    expect(result.affinityMap.get(lambda)?.runtime).toBe(OmniRuntime.Python);
    expect(result.affinityMap.get(assign)?.runtime).toBe(OmniRuntime.Python);
  });

  test('JavaScript arrow syntax resolves to JavaScript', () => {
    const result = resolve('const view = (request) => JsonResponse({"path": request.path})');
    const decl = result.program.body[0] as AST.ConstDecl;
    const lambda = decl.values[0] as AST.Lambda;

    expect(result.affinityMap.get(lambda)?.runtime).toBe(OmniRuntime.JavaScript);
  });
});

// --- Method & Builtin Tables ---

describe('Method Tables', () => {
  test('.upper() maps to Python', () => {
    expect(lookupMethodAffinity('upper')).toBe(OmniRuntime.Python);
  });

  test('.map() maps to JavaScript', () => {
    expect(lookupMethodAffinity('map')).toBe(OmniRuntime.JavaScript);
  });

  test('.each() maps to Ruby', () => {
    expect(lookupMethodAffinity('each')).toBe(OmniRuntime.Ruby);
  });

  test('.println maps to Java', () => {
    expect(lookupMethodAffinity('println')).toBe(OmniRuntime.Java);
  });

  test('ambiguous methods return undefined', () => {
    expect(lookupMethodAffinity('split')).toBeUndefined();
    expect(lookupMethodAffinity('join')).toBeUndefined();
    expect(lookupMethodAffinity('sort')).toBeUndefined();
  });

  test('collision-prone ecosystem field names return undefined', () => {
    expect(lookupMethodAffinity('then')).toBeUndefined();
    expect(lookupMethodAffinity('items')).toBeUndefined();
    expect(lookupMethodAffinity('keys')).toBeUndefined();
    expect(lookupMethodAffinity('values')).toBeUndefined();
    expect(lookupMethodAffinity('entries')).toBeUndefined();
    expect(lookupMethodAffinity('count')).toBeUndefined();
    expect(lookupMethodAffinity('get')).toBeUndefined();
    expect(lookupMethodAffinity('close')).toBeUndefined();
    expect(lookupMethodAffinity('length')).toBeUndefined();
  });

  test('unknown methods return undefined', () => {
    expect(lookupMethodAffinity('myCustomMethod')).toBeUndefined();
  });
});

describe('Builtin Tables', () => {
  test('len() maps to Python', () => {
    expect(lookupBuiltinAffinity('len')).toBe(OmniRuntime.Python);
  });

  test('isinstance() maps to Python', () => {
    expect(lookupBuiltinAffinity('isinstance')).toBe(OmniRuntime.Python);
  });

  test('make() maps to Go', () => {
    expect(lookupBuiltinAffinity('make')).toBe(OmniRuntime.Go);
  });

  test('close() remains a Go builtin', () => {
    expect(lookupBuiltinAffinity('close')).toBe(OmniRuntime.Go);
  });

  test('require() maps to JavaScript', () => {
    expect(lookupBuiltinAffinity('require')).toBe(OmniRuntime.JavaScript);
  });

  test('puts maps to Ruby', () => {
    expect(lookupBuiltinAffinity('puts')).toBe(OmniRuntime.Ruby);
  });

  test('System maps to Java', () => {
    expect(lookupBuiltinAffinity('System')).toBe(OmniRuntime.Java);
  });

  test('Java package roots map to Java globals', () => {
    expect(lookupGlobalAffinity('java')).toBe(OmniRuntime.Java);
    expect(lookupGlobalAffinity('org')).toBe(OmniRuntime.Java);
    expect(lookupGlobalAffinity('com')).toBe(OmniRuntime.Java);
    expect(lookupGlobalAffinity('okhttp3')).toBeUndefined();
    expect(lookupGlobalAffinity('reactor')).toBeUndefined();
    expect(lookupGlobalAffinity('kotlinx')).toBeUndefined();
    expect(lookupGlobalAffinity('io')).toBeUndefined();
  });

  test('JavaScript globals map to JavaScript globals', () => {
    expect(lookupGlobalAffinity('Array')).toBe(OmniRuntime.JavaScript);
    expect(lookupGlobalAffinity('JSON')).toBe(OmniRuntime.JavaScript);
    expect(lookupGlobalAffinity('AbortController')).toBe(OmniRuntime.JavaScript);
    expect(lookupGlobalAffinity('AbortSignal')).toBe(OmniRuntime.JavaScript);
    expect(lookupGlobalAffinity('Worker')).toBe(OmniRuntime.JavaScript);
    expect(lookupGlobalAffinity('ReadableStream')).toBe(OmniRuntime.JavaScript);
  });

  test('call-only builtins are not global roots', () => {
    expect(lookupGlobalAffinity('list')).toBeUndefined();
    expect(lookupGlobalAffinity('len')).toBeUndefined();
    expect(lookupGlobalAffinity('new')).toBeUndefined();
  });
});

// --- Import Analysis ---

describe('Import Analysis', () => {
  test('"fmt" infers Go', () => {
    const result = analyzeImportPath('fmt');
    expect(result).toBeDefined();
    expect(result!.runtime).toBe(OmniRuntime.Go);
  });

  test('"node:stream" infers JavaScript', () => {
    const result = analyzeImportPath('node:stream');
    expect(result).toBeDefined();
    expect(result!.runtime).toBe(OmniRuntime.JavaScript);
  });

  test('"os" bare import infers Python', () => {
    const result = analyzeBareImport('os');
    expect(result).toBeDefined();
    expect(result!.runtime).toBe(OmniRuntime.Python);
  });

  test('"java.util" infers Java', () => {
    const result = analyzeImportPath('java.util');
    expect(result).toBeDefined();
    expect(result!.runtime).toBe(OmniRuntime.Java);
  });

  test('ambiguous stdlib import paths honor preferred syntax runtime', () => {
    expect(analyzeImportPath('http')!.runtime).toBe(OmniRuntime.Go);
    expect(analyzeImportPath('http', { preferredRuntime: OmniRuntime.JavaScript })!.runtime).toBe(OmniRuntime.JavaScript);
    expect(analyzeImportPath('http', { preferredRuntime: OmniRuntime.Python })!.runtime).toBe(OmniRuntime.Python);
  });

  test('./relative/path.js infers JavaScript', () => {
    const result = analyzeImportPath('./relative/path.js');
    expect(result).toBeDefined();
    expect(result!.runtime).toBe(OmniRuntime.JavaScript);
  });

  test('github.com/user/repo infers Go', () => {
    const result = analyzeImportPath('github.com/user/repo');
    expect(result).toBeDefined();
    expect(result!.runtime).toBe(OmniRuntime.Go);
  });

  test('@scope/package infers JavaScript', () => {
    const result = analyzeImportPath('@scope/package');
    expect(result).toBeDefined();
    expect(result!.runtime).toBe(OmniRuntime.JavaScript);
  });

  test('third-party ecosystem package aliases do not infer owning runtimes', () => {
    expect(analyzeImportPath('django')).toBeUndefined();
    expect(analyzeImportPath('fastapi')).toBeUndefined();
    expect(analyzeImportPath('sqlalchemy')).toBeUndefined();
    expect(analyzeImportPath('pandas')).toBeUndefined();
    expect(analyzeImportPath('numpy')).toBeUndefined();
    expect(analyzeImportPath('react-dom/server')).toBeUndefined();
    expect(analyzeImportPath('active_record')).toBeUndefined();
    expect(analyzeImportPath('dry/validation')).toBeUndefined();
    expect(analyzeImportPath('pyarrow')).toBeUndefined();
    expect(analyzeImportPath('polars')).toBeUndefined();
    expect(analyzeImportPath('bullmq')).toBeUndefined();
    expect(analyzeImportPath('duckdb')).toBeUndefined();
    expect(analyzeImportPath('reactor.core')).toBeUndefined();
    expect(analyzeImportPath('@prisma/client')!.runtime).toBe(OmniRuntime.JavaScript);
    expect(analyzeImportPath('go.uber.org/zap')!.runtime).toBe(OmniRuntime.Go);
    expect(analyzeImportPath('log/slog')!.runtime).toBe(OmniRuntime.Go);
  });

  test('real-world compatibility package imports require explicit runtime evidence', () => {
    for (const importPath of [
      'starlette.requests',
      'uvicorn',
      'werkzeug.serving',
      'anyio',
      'asyncpg',
      'psycopg.rows',
      'marshmallow',
      'jsonschema.validators',
      'boto3',
      'botocore.stub',
      'pymongo.cursor',
      'google.api_core.page_iterator',
      'google.protobuf.descriptor_pb2',
      'jax.numpy',
      'cupy',
      'undici',
      'undici/types',
      'busboy',
      'multer',
      'body-parser',
      'koa-bodyparser',
      'rack/mock',
      'rackup/handler/webrick',
      'active_record/relation',
      'action_dispatch',
      'reactor.core.publisher.Flux',
      'io.reactivex.rxjava3.core.Flowable',
      'kotlinx.coroutines.Job',
    ]) {
      expect(analyzeImportPath(importPath)).toBeUndefined();
    }
    expect(analyzeImportPath('node:stream')!.runtime).toBe(OmniRuntime.JavaScript);
    expect(analyzeImportPath('node:stream/web')!.runtime).toBe(OmniRuntime.JavaScript);
    expect(analyzeImportPath('com.google.common.util.concurrent.ListenableFuture')!.runtime).toBe(OmniRuntime.Java);
    expect(analyzeImportPath('jakarta.validation')!.runtime).toBe(OmniRuntime.Java);
    expect(analyzeImportPath('jakarta.validation.ConstraintViolationException')!.runtime).toBe(OmniRuntime.Java);
    expect(analyzeImportPath('io.unknown')?.runtime).not.toBe(OmniRuntime.Java);
  });

  test('unknown module returns undefined', () => {
    const result = analyzeImportPath('completely_unknown_module_xyz');
    expect(result).toBeUndefined();
  });
});

describe('Import syntax coverage', () => {
  function importOps(code: string) {
    const ast = parseCode(code);
    const resolver = new RuntimeResolver();
    const annotated = resolver.resolve(ast, code);
    const nodes: Array<AST.Import | AST.ImportDecl> = [];

    for (const node of annotated.program.body) {
      if (node.kind === 'Import' || node.kind === 'ImportDecl') {
        nodes.push(node);
      } else if (node.kind === 'GroupedImport') {
        nodes.push(...node.imports);
      }
    }

    return nodes.map(node => ({
      node,
      runtime: annotated.affinityMap.get(node)?.runtime,
      confidence: annotated.affinityMap.get(node)?.confidence,
    }));
  }

  test('Python imports cover dotted packages, aliases, and from-import aliases', () => {
    const imports = importOps(`
import package_name.submodule as pkg
from package_name.submodule import factory as make_factory
`);

    expect(imports).toHaveLength(2);
    expect(imports.map(item => item.runtime)).toEqual([OmniRuntime.Python, OmniRuntime.Python]);
    expect((imports[0].node as AST.Import).path).toBe('package_name.submodule');
    expect(((imports[0].node as AST.Import).alias as AST.Identifier).name).toBe('pkg');
    expect((imports[1].node as AST.ImportDecl).path).toBe('package_name.submodule');
    expect((imports[1].node as AST.ImportDecl).specifiers).toEqual([
      { imported: 'factory', local: 'make_factory' },
    ]);
  });

  test('JavaScript imports cover scoped packages, subpaths, side effects, and namespace imports', () => {
    const imports = importOps(`
import client from "@scope/pkg-name/subpath"
import { render as mount } from "pkg-name/render"
import * as tools from "pkg-name/tools"
import "pkg-name/register"
`);

    expect(imports).toHaveLength(4);
    expect(imports.map(item => item.runtime)).toEqual([
      OmniRuntime.JavaScript,
      OmniRuntime.JavaScript,
      OmniRuntime.JavaScript,
      OmniRuntime.JavaScript,
    ]);
    expect((imports[0].node as AST.ImportDecl).defaultImport?.name).toBe('client');
    expect((imports[1].node as AST.ImportDecl).specifiers).toEqual([{ imported: 'render', local: 'mount' }]);
    expect((imports[2].node as AST.ImportDecl).namespaceImport?.name).toBe('tools');
    expect((imports[3].node as AST.Import).path).toBe('pkg-name/register');
  });

  test('Go imports cover quoted domain paths, aliases, grouped imports, and dashed path segments', () => {
    const imports = importOps(`
import (
  "github.com/acme/pkg-name/subpkg"
  tools "example.com/org/tool-kit"
)
`);

    expect(imports).toHaveLength(2);
    expect(imports.map(item => item.runtime)).toEqual([OmniRuntime.Go, OmniRuntime.Go]);
    expect((imports[0].node as AST.Import).path).toBe('github.com/acme/pkg-name/subpkg');
    expect((imports[1].node as AST.Import).path).toBe('example.com/org/tool-kit');
    expect((imports[1].node as AST.Import).alias?.name).toBe('tools');
  });

  test('Ruby require imports cover gem names and slash subpaths', () => {
    const imports = importOps(`
require "dry/validation"
require "active_record"
require "my-gem/subpath"
`);

    expect(imports).toHaveLength(3);
    expect(imports.map(item => item.runtime)).toEqual([
      OmniRuntime.Ruby,
      OmniRuntime.Ruby,
      OmniRuntime.Ruby,
    ]);
    expect(imports.map(item => (item.node as AST.Import).path)).toEqual([
      'dry/validation',
      'active_record',
      'my-gem/subpath',
    ]);
  });

  test('Java imports cover class, static, and wildcard imports', () => {
    const imports = importOps(`
import java.util.concurrent.CompletableFuture
import static java.util.concurrent.TimeUnit.SECONDS
import java.util.*
`);

    expect(imports).toHaveLength(3);
    expect(imports.map(item => item.runtime)).toEqual([
      OmniRuntime.Java,
      OmniRuntime.Java,
      OmniRuntime.Java,
    ]);
    expect((imports[0].node as AST.Import).path).toBe('java.util.concurrent.CompletableFuture');
    expect((imports[1].node as AST.Import).path).toBe('static java.util.concurrent.TimeUnit.SECONDS');
    expect((imports[2].node as AST.Import).path).toBe('java.util.*');
  });
});

// --- Symbol Table ---

describe('Symbol Table', () => {
  test('define and lookup in same scope', () => {
    const table = new SymbolTable();
    table.define('x', {
      name: 'x',
      affinity: { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
    });
    const entry = table.lookup('x');
    expect(entry).toBeDefined();
    expect(entry!.affinity.runtime).toBe(OmniRuntime.Python);
  });

  test('nested scope shadows outer', () => {
    const table = new SymbolTable();
    table.define('x', {
      name: 'x',
      affinity: { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
    });
    table.pushScope();
    table.define('x', {
      name: 'x',
      affinity: { runtime: OmniRuntime.JavaScript, confidence: 'definite', evidence: [] },
    });
    expect(table.lookup('x')!.affinity.runtime).toBe(OmniRuntime.JavaScript);
    table.popScope();
    expect(table.lookup('x')!.affinity.runtime).toBe(OmniRuntime.Python);
  });

  test('inner scope can see outer scope variables', () => {
    const table = new SymbolTable();
    table.define('outer', {
      name: 'outer',
      affinity: { runtime: OmniRuntime.Go, confidence: 'definite', evidence: [] },
    });
    table.pushScope();
    expect(table.lookup('outer')).toBeDefined();
    expect(table.lookup('outer')!.affinity.runtime).toBe(OmniRuntime.Go);
    table.popScope();
  });

  test('getScopeAffinity returns majority runtime', () => {
    const table = new SymbolTable();
    table.define('a', {
      name: 'a',
      affinity: { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
    });
    table.define('b', {
      name: 'b',
      affinity: { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
    });
    table.define('c', {
      name: 'c',
      affinity: { runtime: OmniRuntime.JavaScript, confidence: 'definite', evidence: [] },
    });
    const scopeAff = table.getScopeAffinity();
    expect(scopeAff).toBeDefined();
    expect(scopeAff!.runtime).toBe(OmniRuntime.Python);
  });
});

// --- Cost Model ---

describe('Cost Model', () => {
  test('primitive bridge cost is 1', () => {
    expect(computeBridgeCost(MarshalKind.Primitive)).toBe(1);
  });

  test('callback bridge cost is 100', () => {
    expect(computeBridgeCost(MarshalKind.Callback)).toBe(100);
  });

  test('async bridge cost is 200', () => {
    expect(computeBridgeCost(MarshalKind.AsyncBridge)).toBe(200);
  });

  test('majorityRuntime returns runtime when >90% share one', () => {
    const affinities: RuntimeAffinity[] = [
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.JavaScript, confidence: 'fallback', evidence: [] },
    ];
    expect(majorityRuntime(affinities)).toBe(OmniRuntime.Python);
  });

  test('majorityRuntime returns undefined when split', () => {
    const affinities: RuntimeAffinity[] = [
      { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] },
      { runtime: OmniRuntime.JavaScript, confidence: 'definite', evidence: [] },
    ];
    expect(majorityRuntime(affinities)).toBeUndefined();
  });

  test('totalBridgeCost sums all bridge costs', () => {
    const bridges = [
      { from: OmniRuntime.Python, to: OmniRuntime.JavaScript, marshalKind: MarshalKind.Primitive, cost: 1 },
      { from: OmniRuntime.Go, to: OmniRuntime.JavaScript, marshalKind: MarshalKind.Array, cost: 10 },
    ];
    expect(totalBridgeCost(bridges)).toBe(11);
  });
});

// --- Runtime Resolver Integration ---

describe('Runtime Resolver', () => {
  test('ListComprehension resolves to Python', () => {
    const result = resolve('[x * 2 for x in items]');
    const firstNode = result.program.body[0];
    const affinity = result.affinityMap.get(firstNode);
    // ListComprehension could be wrapped in ExprStmt
    if (firstNode.kind === 'ExprStmt') {
      const expr = (firstNode as AST.ExprStmt).expr;
      const exprAff = result.affinityMap.get(expr);
      if (exprAff) {
        expect(exprAff.runtime).toBe(OmniRuntime.Python);
        expect(exprAff.confidence).toBe('definite');
      }
    }
  });

  test('Pass statement resolves to Python', () => {
    const result = resolve('pass');
    const node = result.program.body[0];
    const aff = result.affinityMap.get(node);
    expect(aff).toBeDefined();
    expect(aff!.runtime).toBe(OmniRuntime.Python);
  });

  test('JSX resolves to JavaScript', () => {
    const result = resolve('<div>Hello</div>');
    const node = result.program.body[0];
    let jsxNode: AST.Expr | undefined;
    if (node.kind === 'ExprStmt') {
      jsxNode = (node as AST.ExprStmt).expr;
    }
    if (jsxNode) {
      const aff = result.affinityMap.get(jsxNode);
      expect(aff).toBeDefined();
      expect(aff!.runtime).toBe(OmniRuntime.JavaScript);
    }
  });

  test('fn keyword infers Rust', () => {
    const result = resolve('fn compute(x: i32) -> i32 { x * 2 }');
    const node = result.program.body[0] as AST.FuncDecl;
    const aff = result.affinityMap.get(node);
    expect(aff).toBeDefined();
    expect(aff!.runtime).toBe(OmniRuntime.Rust);
  });

  test('def keyword infers Python', () => {
    const result = resolve('def greet(name):\n  print(name)');
    const node = result.program.body[0] as AST.FuncDecl;
    const aff = result.affinityMap.get(node);
    expect(aff).toBeDefined();
    expect(aff!.runtime).toBe(OmniRuntime.Python);
  });

  test('function keyword infers JavaScript', () => {
    const result = resolve('function greet(name) { console.log(name) }');
    const node = result.program.body[0] as AST.FuncDecl;
    const aff = result.affinityMap.get(node);
    expect(aff).toBeDefined();
    expect(aff!.runtime).toBe(OmniRuntime.JavaScript);
  });

  test('file-level @runtime directive sets default', () => {
    const code = '// @runtime python\nx = 42';
    // Parser receives source to extract directives from comments
    const result = resolve(code);
    expect(result.defaultRuntime).toBe(OmniRuntime.Python);
  });

  test('fallback chain defaults to JavaScript', () => {
    const result = resolve('x = 42');
    expect(result.defaultRuntime).toBe(OmniRuntime.JavaScript);
  });

  test('scope inheritance: statements inside Python def inherit Python', () => {
    const result = resolve('def greet(name):\n  print(name)');
    const func = result.program.body[0] as AST.FuncDecl;
    const funcAff = result.affinityMap.get(func);
    expect(funcAff).toBeDefined();
    expect(funcAff!.runtime).toBe(OmniRuntime.Python);
  });

  test('@py() inline marker overrides scope affinity', () => {
    const code = 'result = @py(len(items))';
    const result = resolve(code);

    function findRuntimeTag(node: any): AST.RuntimeTag | null {
      if (!node || typeof node !== 'object') return null;
      if (node.kind === 'RuntimeTag') return node;
      for (const key of Object.keys(node)) {
        if (Array.isArray(node[key])) {
          for (const item of node[key]) {
            const found = findRuntimeTag(item);
            if (found) return found;
          }
        } else {
          const found = findRuntimeTag(node[key]);
          if (found) return found;
        }
      }
      return null;
    }

    const tag = findRuntimeTag(result.program.body[0]);
    if (tag) {
      const aff = result.affinityMap.get(tag);
      expect(aff).toBeDefined();
      expect(aff!.runtime).toBe(OmniRuntime.Python);
      expect(aff!.confidence).toBe('definite');
    }
  });

  test('resolver produces annotated tree', () => {
    const result = resolve('function hello() { return 42 }');
    expect(result.root).toBeDefined();
    expect(result.root.node).toBe(result.program);
    expect(result.root.children.length).toBeGreaterThan(0);
  });

  test('affinityMap contains entries for all visited nodes', () => {
    const result = resolve('function hello() { return 42 }');
    expect(result.affinityMap.size).toBeGreaterThan(0);
  });

  test('async function gets async flag', () => {
    const result = resolve('async function fetchData() { return await fetch("/api") }');
    const func = result.program.body[0] as AST.FuncDecl;
    const aff = result.affinityMap.get(func);
    expect(aff).toBeDefined();
    // async functions should propagate async flag
    expect(aff!.async).toBe(true);
  });
});

// --- Import-to-Usage Propagation ---

describe('Import-to-Usage Propagation', () => {
  test('import os → os.path.join() resolves to Python', () => {
    const result = resolve('import os\nos.path.join("/tmp", "file")');
    // Find the Call node
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call') {
        expect(aff.runtime).toBe(OmniRuntime.Python);
      }
    }
  });

  test('Python-style third-party imports propagate by syntax, not package-name tables', () => {
    for (const code of [
      'import django\ndjango.setup()',
      'import fastapi\nfastapi.FastAPI()',
      'from fastapi import FastAPI\nFastAPI()',
      'import sqlalchemy\nsqlalchemy.create_engine(url)',
      'from sqlalchemy import create_engine, text\ncreate_engine(url)\ntext("select 1")',
      'import pandas\npandas.DataFrame([])',
      'import numpy\nnumpy.array([1, 2, 3])',
      'import pyarrow\npyarrow.array([1, 2, 3])',
      'from pyarrow import array\narray([1, 2, 3])',
    ]) {
      const result = resolve(code);
      for (const [node, aff] of result.affinityMap) {
        if (node.kind === 'Call') {
          expect(aff.runtime).toBe(OmniRuntime.Python);
        }
      }
    }
  });

  test('unknown bare imports use Python import syntax provenance', () => {
    const result = resolve('import react');
    const node = result.program.body[0];
    const aff = result.affinityMap.get(node);
    expect(aff?.runtime).toBe(OmniRuntime.Python);
    expect(aff?.confidence).toBe("definite");
    expect(aff?.evidence.some(e => e.type === "syntax" && e.detail.includes("Python import syntax"))).toBe(true);
  });

  test('assigned variable inherits import runtime through chain', () => {
    const result = resolve('import os\npath = os.path\npath.join("/tmp", "file")');
    // All calls should be Python
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call') {
        expect(aff.runtime).toBe(OmniRuntime.Python);
      }
      if (node.kind === 'Member') {
        expect(aff.runtime).toBe(OmniRuntime.Python);
      }
    }
  });

  test('method table does NOT override import provenance', () => {
    // .map() is in the JS method table, but files came from os.listdir() (Python)
    const result = resolve('import os\nfiles = os.listdir("/tmp")\nfiles.map(f => f)');
    // Find the .map Member node
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Member' && (node as AST.Member).property.name === 'map') {
        expect(aff.runtime).toBe(OmniRuntime.Python);
      }
    }
  });

  test('method table DOES apply when object has no import provenance', () => {
    const result = resolve('items.map(x => x * 2)');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Member' && (node as AST.Member).property.name === 'map') {
        expect(aff.runtime).toBe(OmniRuntime.JavaScript);
      }
    }
  });

  test('collision-prone method names do not override file runtime without object provenance', () => {
    const result = resolve('// @runtime python\nrow.then(callback)\nrow.count()\nrow.keys()\nrow.values()\nrow.entries()\nrow.close()');
    const seen = new Set<string>();

    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Member') {
        const name = (node as AST.Member).property.name;
        if (['then', 'count', 'keys', 'values', 'entries', 'close'].includes(name)) {
          seen.add(name);
          expect(aff.runtime).toBe(OmniRuntime.Python);
        }
      }
    }

    expect(seen).toEqual(new Set(['then', 'count', 'keys', 'values', 'entries', 'close']));
  });

  test('Java collection method names infer Java without conversion wrappers', () => {
    const result = resolve('payload.keySet()\npayload.entrySet()\npayload.getOrDefault("missing", "fallback")');
    const seen = new Set<string>();

    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Member') {
        const name = (node as AST.Member).property.name;
        if (['keySet', 'entrySet', 'getOrDefault'].includes(name)) {
          seen.add(name);
          expect(aff.runtime).toBe(OmniRuntime.Java);
        }
      }
    }

    expect(seen).toEqual(new Set(['keySet', 'entrySet', 'getOrDefault']));
  });

  test('aliased stdlib import propagates: import os as pyos', () => {
    const result = resolve('import os as pyos\npyos.getcwd()');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call') {
        expect(aff.runtime).toBe(OmniRuntime.Python);
      }
    }
  });

  test('Java dotted class imports bind simple class names in mixed files', () => {
    const result = resolve([
      'import java.util.concurrent.CompletableFuture',
      'import java.util.ArrayList',
      'import java.util.HashMap',
      'const future = CompletableFuture.completedFuture("ok")',
      'const list = new ArrayList()',
      'const map = new HashMap()',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call' || node.kind === 'NewExpr')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Java,
      OmniRuntime.Java,
      OmniRuntime.Java,
    ]);
  });

  test('third-party Java class imports bind simple names by class syntax', () => {
    const result = resolve([
      'import io.reactivex.rxjava3.core.Flowable',
      'import reactor.core.publisher.Flux',
      'const flowable = Flowable.just("alpha")',
      'const flux = Flux.just("beta")',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Java,
      OmniRuntime.Java,
    ]);

    const importRuntimes = result.program.body
      .filter((node): node is AST.Import => node.kind === 'Import')
      .map(node => result.affinityMap.get(node));
    expect(importRuntimes.every(aff =>
      aff?.runtime === OmniRuntime.Java &&
      aff.evidence.some(e => e.type === "syntax" && e.detail.includes("Java class import syntax"))
    )).toBe(true);
  });

  test('explicit third-party Java imports bind fully qualified class usage', () => {
    const result = resolve([
      'import reactor.core.publisher.Flux',
      'const flux = reactor.core.publisher.Flux.just("beta")',
      'const first = flux.collectList().block().get(0)',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Java,
      OmniRuntime.Java,
      OmniRuntime.Java,
      OmniRuntime.Java,
    ]);
  });

  test('third-party Java wildcard imports keep following class usage in Java by syntax', () => {
    const result = resolve([
      'import io.reactivex.rxjava3.core.*',
      'const flowable = Flowable.just("alpha")',
    ].join('\n'));
    const importNode = result.program.body.find((node): node is AST.Import => node.kind === 'Import');
    expect(importNode?.path).toBe('io.reactivex.rxjava3.core.*');

    const importAffinity = importNode ? result.affinityMap.get(importNode) : undefined;
    expect(importAffinity?.runtime).toBe(OmniRuntime.Java);
    expect(importAffinity?.evidence.some(e =>
      e.type === "syntax" && e.detail.includes("Java wildcard import syntax")
    )).toBe(true);

    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);
    expect(callRuntimes).toEqual([OmniRuntime.Java]);
  });

  test('Java static imports bind imported member names by syntax', () => {
    const result = resolve([
      'import static java.util.concurrent.TimeUnit.SECONDS',
      'import static reactor.core.publisher.Mono.just',
      'const timeout_unit = SECONDS',
      'const value = future.get(1, SECONDS)',
      'const mono = just("ok")',
    ].join('\n'));
    const importNode = result.program.body.find((node): node is AST.Import =>
      node.kind === 'Import' && node.path.includes('TimeUnit')
    );
    expect(importNode?.path).toBe('static java.util.concurrent.TimeUnit.SECONDS');
    expect(importNode?.alias?.name).toBe('SECONDS');

    const importAffinity = importNode ? result.affinityMap.get(importNode) : undefined;
    expect(importAffinity?.runtime).toBe(OmniRuntime.Java);
    expect(importAffinity?.evidence.some(e =>
      e.type === "syntax" && e.detail.includes("Java static import syntax")
    )).toBe(true);

    const useRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Identifier' && (node as AST.Identifier).name === 'SECONDS')
      .map(([, aff]) => aff.runtime);
    expect(useRuntimes.length).toBeGreaterThanOrEqual(2);
    expect(useRuntimes.every(runtime => runtime === OmniRuntime.Java)).toBe(true);

    const justImport = result.program.body.find((node): node is AST.Import =>
      node.kind === 'Import' && node.path.includes('reactor.core.publisher.Mono.just')
    );
    expect(justImport?.alias?.name).toBe('just');

    const justCall = [...result.affinityMap].find(([node]) =>
      node.kind === 'Call' && (node as AST.Call).callee.kind === 'Identifier' &&
      ((node as AST.Call).callee as AST.Identifier).name === 'just'
    );
    expect(justCall?.[1].runtime).toBe(OmniRuntime.Java);
  });

  test('Java static wildcard imports stay single Java imports without stray expressions', () => {
    const result = resolve([
      'import static org.junit.Assert.*',
      'assertEquals(1, count)',
    ].join('\n'));
    expect(result.program.body.map(node => node.kind)).toEqual(['Import', 'ExprStmt']);

    const importNode = result.program.body[0] as AST.Import;
    expect(importNode.path).toBe('static org.junit.Assert.*');
    expect(result.affinityMap.get(importNode)?.runtime).toBe(OmniRuntime.Java);

    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);
    expect(callRuntimes).toEqual([OmniRuntime.Java]);
  });

  test('Ruby Fiber core global infers Ruby without surrounding scope hints', () => {
    const result = resolve('const fiber_id = Fiber.current.object_id');
    const fiberOps = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Member' || node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(fiberOps).toContain(OmniRuntime.Ruby);
    expect(fiberOps.every(runtime => runtime === OmniRuntime.Ruby)).toBe(true);
  });

  test('Ruby Thread.current qualified core API beats bare Java Thread root', () => {
    const rubyResult = resolve('const thread_id = Thread.current.object_id');
    const rubyRuntimes = [...rubyResult.affinityMap]
      .filter(([node]) => node.kind === 'Member')
      .map(([, aff]) => aff.runtime);

    expect(rubyRuntimes).toContain(OmniRuntime.Ruby);
    expect(rubyRuntimes.every(runtime => runtime === OmniRuntime.Ruby)).toBe(true);

    const javaResult = resolve('const current = Thread.currentThread()');
    const javaRuntimes = [...javaResult.affinityMap]
      .filter(([node]) => node.kind === 'Member' || node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(javaRuntimes).toContain(OmniRuntime.Java);
    expect(javaRuntimes.every(runtime => runtime === OmniRuntime.Java)).toBe(true);
  });

  test('Java typed catch binds handler param and body to Java', () => {
    const code = [
      'try {',
      '  failJava()',
      '} catch (RuntimeException java_err) {',
      '  const java_error_summary = java_err.getRuntime() + java_err.getOriginRuntime() + java_err.getDetails()',
      '}',
    ].join('\n');
    const result = resolve(code);
    const tryNode = result.program.body.find((node): node is AST.Try => node.kind === 'Try');
    const catchClause = tryNode?.catches[0];

    expect(catchClause).toBeDefined();
    expect(result.affinityMap.get(catchClause!.body)?.runtime).toBe(OmniRuntime.Java);
    expect(result.affinityMap.get(catchClause!.param!)?.runtime).toBe(OmniRuntime.Java);

    const javaErrRuntimes = [...result.affinityMap]
      .filter(([node]) =>
        node.kind !== 'Try' &&
        node.kind !== 'Block' &&
        node.span &&
        code.slice(node.span.start, node.span.end).includes('java_err')
      )
      .map(([, aff]) => aff.runtime);

    expect(javaErrRuntimes).toContain(OmniRuntime.Java);
    expect(javaErrRuntimes.every(runtime => runtime === OmniRuntime.Java)).toBe(true);
  });

  test('Java qualified and multi-catch clauses preserve try structure and bind handler to Java', () => {
    for (const code of [
      [
        'try {',
        '  failJava()',
        '} catch (java.io.IOException err) {',
        '  const msg = err.getMessage()',
        '}',
      ].join('\n'),
      [
        'try {',
        '  failJava()',
        '} catch (IOException | SQLException err) {',
        '  const msg = err.getMessage()',
        '}',
      ].join('\n'),
    ]) {
      const result = resolve(code);
      expect(result.program.body).toHaveLength(1);
      const tryNode = result.program.body[0] as AST.Try;
      expect(tryNode.kind).toBe('Try');
      expect(tryNode.catches).toHaveLength(1);
      expect(tryNode.catches[0].param?.name).toBe('err');
      expect(result.affinityMap.get(tryNode.catches[0].body)?.runtime).toBe(OmniRuntime.Java);
      expect(result.affinityMap.get(tryNode.catches[0].param!)?.runtime).toBe(OmniRuntime.Java);

      const catchBodyRuntimes = [...result.affinityMap]
        .filter(([node]) =>
          ['Identifier', 'Member', 'Call', 'ConstDecl'].includes(node.kind) &&
          node.span &&
          code.slice(node.span.start, node.span.end).includes('err.getMessage')
        )
        .map(([, aff]) => aff.runtime);
      expect(catchBodyRuntimes.length).toBeGreaterThan(0);
      expect(catchBodyRuntimes.every(runtime => runtime === OmniRuntime.Java)).toBe(true);
    }
  });

  test('typed catch syntax does not steal Python except or TypeScript-style catch', () => {
    const pythonCode = [
      'try:',
      '  fail_java()',
      'except RuntimeException as py_err:',
      '  py_error_summary = py_err.getRuntime()',
    ].join('\n');
    const pythonResult = resolve(pythonCode);
    const pythonTry = pythonResult.program.body.find((node): node is AST.Try => node.kind === 'Try');
    expect(pythonResult.affinityMap.get(pythonTry!.catches[0].body)?.runtime).toBe(OmniRuntime.Python);
    expect(pythonResult.affinityMap.get(pythonTry!.catches[0].param!)?.runtime).toBe(OmniRuntime.Python);

    const tsCode = [
      'try {',
      '  failJs()',
      '} catch (err: Error) {',
      '  const js_error_summary = err.message',
      '}',
    ].join('\n');
    const tsResult = resolve(tsCode);
    const tsTry = tsResult.program.body.find((node): node is AST.Try => node.kind === 'Try');
    expect(tsResult.affinityMap.get(tsTry!.catches[0].body)?.runtime).toBe(OmniRuntime.JavaScript);
    expect(tsResult.affinityMap.get(tsTry!.catches[0].param!)?.runtime).toBe(OmniRuntime.JavaScript);
  });

  test('Python dotted stdlib imports bind package roots in mixed files', () => {
    const result = resolve([
      'import http.client',
      'import email.message',
      'const connection = http.client.HTTPConnection("example.test")',
      'const message = email.message.EmailMessage()',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Python,
      OmniRuntime.Python,
    ]);
  });

  test('JS import { useState } from "react" → useState() is JS', () => {
    const result = resolve('import { useState } from "react"\nuseState(0)');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call') {
        expect(aff.runtime).toBe(OmniRuntime.JavaScript);
      }
    }
  });

  test('Node node: builtin imports resolve to JavaScript in mixed files', () => {
    const result = resolve('import os\nimport { Readable } from "node:stream"\nconst stream = Readable.from(["chunk"])');
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([OmniRuntime.JavaScript]);
  });

  test('Go stdlib slash imports keep Go provenance even with default-import sugar', () => {
    const result = resolve([
      'import os',
      'import http from "net/http"',
      'import { Readable } from "node:stream"',
      'const handler = http.HandlerFunc(go_handler)',
      'const stream = Readable.from(["chunk"])',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Go,
      OmniRuntime.JavaScript,
    ]);
  });

  test('ambiguous stdlib imports use source syntax over module-name defaults', () => {
    const result = resolve([
      'import django',
      'import http from "http"',
      'from http import client',
      'const server = http.createServer(() => null)',
      'const connection = client.HTTPConnection("example.com")',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.JavaScript,
      OmniRuntime.Python,
    ]);
  });

  test('ambiguous Redis imports use source syntax in mixed files', () => {
    const result = resolve([
      'import redis from "redis"',
      'import redis as pyredis',
      'const jsClient = redis.createClient()',
      'const pyClient = pyredis.Redis()',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.JavaScript,
      OmniRuntime.Python,
    ]);
  });

  test('tensor and SDK pager imports propagate from Python import syntax without package heuristics', () => {
    const result = resolve([
      'import jax.numpy as jnp',
      'import boto3',
      'import pymongo',
      'const tensor = jnp.array([1, 2, 3])',
      'const s3 = boto3.client("s3")',
      'const mongo = pymongo.MongoClient("mongodb://example.test")',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Python,
      OmniRuntime.Python,
      OmniRuntime.Python,
    ]);
  });

  test('ASGI server support imports propagate from Python import syntax without package heuristics', () => {
    const result = resolve([
      'import uvicorn',
      'from werkzeug.serving import make_server',
      'import anyio',
      'const config = uvicorn.Config(app)',
      'const server = make_server("127.0.0.1", 0, app)',
      'const group = anyio.create_task_group()',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Python,
      OmniRuntime.Python,
      OmniRuntime.Python,
    ]);
  });

  test('Node upload and body parser imports propagate JavaScript runtime', () => {
    const result = resolve([
      'import Busboy from "busboy"',
      'import multer from "multer"',
      'import bodyParser from "body-parser"',
      'const parser = Busboy({ headers })',
      'const upload = multer({ storage: multer.memoryStorage() })',
      'const json = bodyParser.json()',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.JavaScript,
      OmniRuntime.JavaScript,
      OmniRuntime.JavaScript,
      OmniRuntime.JavaScript,
    ]);
  });

  test('Rackup and ActionDispatch imports do not propagate without explicit runtime evidence', () => {
    const result = resolve([
      'import Rackup from "rackup/handler/webrick"',
      'import ActionDispatch from "action_dispatch"',
      'const handler = Rackup.get("webrick")',
      'const response = ActionDispatch.Response.new(200)',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.JavaScript,
      OmniRuntime.JavaScript,
    ]);
  });

  test('Ruby constant paths infer Ruby without package-name heuristics', () => {
    const result = resolve([
      'import Rack from "rack"',
      'import ActiveRecord from "active_record"',
      'const response = Rack::Response.new("hello", 200).finish',
      'const table = ActiveRecord::Base.connection.quote_table_name("users")',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes.length).toBeGreaterThanOrEqual(2);
    expect(callRuntimes.every(runtime => runtime === OmniRuntime.Ruby)).toBe(true);

    const importRuntimes = result.program.body
      .filter((node): node is AST.ImportDecl => node.kind === 'ImportDecl')
      .map(node => result.affinityMap.get(node)?.runtime);
    expect(importRuntimes).toEqual([
      OmniRuntime.Ruby,
      OmniRuntime.Ruby,
    ]);
  });

  test('unknown default imports can adopt later Ruby constant-path syntax', () => {
    const result = resolve([
      'import ActiveRecord from "active_record"',
      'import React from "react"',
      'const cast = ActiveRecord::Type::Integer.new.cast(value.to_s)',
      'const view = React.createElement("div", null, cast)',
    ].join('\n'));

    const importRuntimes = result.program.body
      .filter((node): node is AST.ImportDecl => node.kind === 'ImportDecl')
      .map(node => result.affinityMap.get(node)?.runtime);

    expect(importRuntimes).toEqual([
      OmniRuntime.Ruby,
      OmniRuntime.JavaScript,
    ]);
  });

  test('Ruby require syntax drives gem imports without package-name heuristics', () => {
    const result = resolve([
      'require "active_record"',
      'require "action_dispatch"',
      'const table = ActiveRecord::Base.connection.quote_table_name("users")',
      'const response = ActionDispatch::Response.new(200)',
    ].join('\n'));

    const importRuntimes = result.program.body
      .filter((node): node is AST.Import => node.kind === 'Import')
      .map(node => result.affinityMap.get(node));
    expect(importRuntimes).toHaveLength(2);
    expect(importRuntimes.every(aff =>
      aff?.runtime === OmniRuntime.Ruby &&
      aff.evidence.some(e => e.type === "syntax" && e.detail.includes("Ruby require syntax"))
    )).toBe(true);

    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);
    expect(callRuntimes.length).toBeGreaterThanOrEqual(2);
    expect(callRuntimes.every(runtime => runtime === OmniRuntime.Ruby)).toBe(true);
  });

  test('Protobuf from-imports propagate from Python syntax without package heuristics', () => {
    const result = resolve([
      'from google.protobuf import descriptor_pb2',
      'from google.protobuf import message_factory',
      'const proto = descriptor_pb2.FileDescriptorProto()',
      'const message = message_factory.GetMessageClass(descriptor)()',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([
      OmniRuntime.Python,
      OmniRuntime.Python,
      OmniRuntime.Python,
    ]);
  });

  test('Guava future imports bind Java class names in mixed files', () => {
    const result = resolve([
      'import com.google.common.util.concurrent.ListenableFuture',
      'import com.google.common.util.concurrent.SettableFuture',
      'const future = SettableFuture.create()',
      'const done = future instanceof ListenableFuture',
    ].join('\n'));
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([OmniRuntime.Java]);
  });

  test('quoted Go stdlib imports can beat Python bare import defaults', () => {
    const result = resolve('import "os"\nconst cwd = os.Getwd()');
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(callRuntimes).toEqual([OmniRuntime.Go]);
  });

  test('unknown JavaScript side-effect imports infer JavaScript from syntax', () => {
    expect(analyzeImportPath('react-dom/server')).toBeUndefined();

    const result = resolve('import "react-dom/server"');
    const importNode = result.program.body[0] as AST.Import;
    const affinity = result.affinityMap.get(importNode);

    expect(affinity?.runtime).toBe(OmniRuntime.JavaScript);
    expect(affinity?.evidence).toContainEqual({
      type: "syntax",
      detail: "JavaScript side-effect import syntax",
    });
  });

  test('unknown domain-like quoted imports bind Go packages from syntax', () => {
    expect(analyzeImportPath('example.com/private/pkg')).toBeUndefined();

    const result = resolve('import "example.com/private/pkg"\nconst client = pkg.NewClient()');
    const importNode = result.program.body[0] as AST.Import;
    const importAffinity = result.affinityMap.get(importNode);
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, aff]) => aff.runtime);

    expect(importAffinity?.runtime).toBe(OmniRuntime.Go);
    expect(importAffinity?.evidence).toContainEqual({
      type: "syntax",
      detail: "Go quoted import syntax",
    });
    expect(callRuntimes).toEqual([OmniRuntime.Go]);
  });
});

// --- Global Root Propagation ---

describe('Global Root Propagation', () => {
  test('Java package member chains resolve to Java without tags', () => {
    const result = resolve('const n = org.jsoup.Jsoup.parse("<a>x</a>").select("a").size()');
    const decl = result.program.body[0] as AST.ConstDecl;
    const aff = result.affinityMap.get(decl);

    expect(aff?.runtime).toBe(OmniRuntime.Java);
    for (const [node, nodeAff] of result.affinityMap) {
      if (node.kind === 'Call') {
        expect(nodeAff.runtime).toBe(OmniRuntime.Java);
      }
    }
  });

  test('qualified Java constructors resolve to Java without tags', () => {
    const result = resolve('const json = new com.google.gson.Gson().toJson(java.util.List.of(1))');
    const decl = result.program.body[0] as AST.ConstDecl;
    const aff = result.affinityMap.get(decl);

    expect(aff?.runtime).toBe(OmniRuntime.Java);
    const newExprRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'NewExpr')
      .map(([, nodeAff]) => nodeAff.runtime);
    expect(newExprRuntimes).toEqual([OmniRuntime.Java]);
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, nodeAff]) => nodeAff.runtime);
    expect(callRuntimes).toContain(OmniRuntime.Java);
    expect(callRuntimes).not.toContain(OmniRuntime.Go);
  });

  test('reactive Java package roots do not resolve without tags', () => {
    const result = resolve('const flux = reactor.core.publisher.Flux.just("alpha")\nconst job = kotlinx.coroutines.Job()');
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, nodeAff]) => nodeAff.runtime);

    expect(callRuntimes).toEqual([OmniRuntime.JavaScript, OmniRuntime.JavaScript]);
  });

  test('qualified io Java package prefixes resolve without making io global', () => {
    const result = resolve('const flowable = io.reactivex.rxjava3.core.Flowable.just("alpha")\nconst channel = io.grpc.ManagedChannelBuilder.forTarget("localhost").usePlaintext().build()\nconst buffer = io.netty.buffer.Unpooled.buffer(1)');
    const callRuntimes = [...result.affinityMap]
      .filter(([node]) => node.kind === 'Call')
      .map(([, nodeAff]) => nodeAff.runtime);

    expect(callRuntimes.every(runtime => runtime === OmniRuntime.Java)).toBe(true);
    expect(callRuntimes.length).toBeGreaterThanOrEqual(3);
  });

  test('JavaScript global callee dominates fallback arguments', () => {
    const result = resolve('const attempts = Array.from(retry_out)');
    const decl = result.program.body[0] as AST.ConstDecl;
    const aff = result.affinityMap.get(decl);

    expect(aff?.runtime).toBe(OmniRuntime.JavaScript);
    for (const [node, nodeAff] of result.affinityMap) {
      if (node.kind === 'Call') {
        expect(nodeAff.runtime).toBe(OmniRuntime.JavaScript);
      }
    }
  });

  test('JavaScript cancellation and stream globals stay JavaScript in mixed files', () => {
    const result = resolve([
      'def open_transaction():',
      '  return engine.begin()',
      'const signal = AbortSignal.timeout(1000)',
      'const controller = new AbortController()',
      'const worker = new Worker("worker.js")',
      'const stream = new ReadableStream()',
    ].join('\n'));

    const runtimeForCallMember = (objectName: string, propertyName: string) => {
      for (const [node, nodeAff] of result.affinityMap) {
        if (
          node.kind === 'Call' &&
          node.callee.kind === 'Member' &&
          node.callee.object.kind === 'Identifier' &&
          node.callee.object.name === objectName &&
          node.callee.property.name === propertyName
        ) {
          return nodeAff.runtime;
        }
      }
      return undefined;
    };
    const runtimeForNewIdentifier = (name: string) => {
      for (const [node, nodeAff] of result.affinityMap) {
        if (
          node.kind === 'NewExpr' &&
          node.callee.kind === 'Identifier' &&
          node.callee.name === name
        ) {
          return nodeAff.runtime;
        }
      }
      return undefined;
    };

    expect(runtimeForCallMember('engine', 'begin')).toBe(OmniRuntime.Python);
    expect(runtimeForCallMember('AbortSignal', 'timeout')).toBe(OmniRuntime.JavaScript);
    expect(runtimeForNewIdentifier('AbortController')).toBe(OmniRuntime.JavaScript);
    expect(runtimeForNewIdentifier('Worker')).toBe(OmniRuntime.JavaScript);
    expect(runtimeForNewIdentifier('ReadableStream')).toBe(OmniRuntime.JavaScript);
  });

  test('explains runtime inference evidence for a resolved node', () => {
    const ast = parseCode('const loud = files.map(f => f.toUpperCase())');
    const resolver = new RuntimeResolver();
    const result = resolver.resolve(ast, 'const loud = files.map(f => f.toUpperCase())');
    let mapCall: AST.Call | undefined;
    for (const [node] of result.affinityMap) {
      if (
        node.kind === 'Call' &&
        node.callee.kind === 'Member' &&
        node.callee.property.name === 'map'
      ) {
        mapCall = node;
      }
    }

    expect(mapCall).toBeDefined();
    const explanation = resolver.explainRuntimeForNode(result, mapCall!);
    expect(explanation).toContain('selected javascript');
    expect(explanation).toContain('syntax: syntactic dominance');
  });
});

// --- Syntactic Dominance Tests ---

describe('Syntactic Dominance', () => {
  test('Lambda gets definite JS affinity with syntax evidence in Pass 1', () => {
    const result = resolve('const fn = x => x * 2');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Lambda') {
        expect(aff.runtime).toBe(OmniRuntime.JavaScript);
        expect(aff.confidence).toBe('definite');
        expect(aff.evidence.some(e => e.type === 'syntax')).toBe(true);
      }
    }
  });

  test('RegexLiteral gets definite JS affinity with syntax evidence in Pass 1', () => {
    const result = resolve('const re = /pattern/g');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'RegexLiteral') {
        expect(aff.runtime).toBe(OmniRuntime.JavaScript);
        expect(aff.confidence).toBe('definite');
        expect(aff.evidence.some(e => e.type === 'syntax')).toBe(true);
      }
    }
  });

  test('Binary === gets definite JS affinity with syntax evidence in Pass 1', () => {
    const result = resolve('const eq = x === y');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Binary' && (node as AST.Binary).op === '===') {
        expect(aff.runtime).toBe(OmniRuntime.JavaScript);
        expect(aff.confidence).toBe('definite');
        expect(aff.evidence.some(e => e.type === 'syntax')).toBe(true);
      }
    }
  });

  test('optional chaining over Python provenance stays JavaScript syntax', () => {
    const result = resolve('import os\npayload = os.environ\nconst called = payload.then?.("manual") ?? payload.items?.length ?? "missing"');
    const optionalCall = [...result.affinityMap].find(([node]) =>
      node.kind === 'Call' && (node as AST.Call).optional
    );
    expect(optionalCall?.[1].runtime).toBe(OmniRuntime.JavaScript);
    expect(optionalCall?.[1].confidence).toBe('definite');
    expect(optionalCall?.[1].evidence.some(e => e.type === 'syntax')).toBe(true);

    const optionalMember = [...result.affinityMap].find(([node]) =>
      node.kind === 'Member' && (node as AST.Member).optional
    );
    expect(optionalMember?.[1].runtime).toBe(OmniRuntime.JavaScript);
    expect(optionalMember?.[1].confidence).toBe('definite');
  });

  test('.map(x => x.name) on Python object → JS via syntax dominance', () => {
    const result = resolve('import os\nfiles = os.listdir("/tmp")\nfiles.map(f => f.toUpperCase())');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call' &&
          (node as AST.Call).callee.kind === 'Member' &&
          ((node as AST.Call).callee as AST.Member).property.name === 'map') {
        expect(aff.runtime).toBe(OmniRuntime.JavaScript);
      }
    }
  });

  test('.customMethod(x => x) on Python object → JS via syntax dominance', () => {
    const result = resolve('import os\nfiles = os.listdir("/tmp")\nfiles.customMethod(f => f)');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call' &&
          (node as AST.Call).callee.kind === 'Member' &&
          ((node as AST.Call).callee as AST.Member).property.name === 'customMethod') {
        expect(aff.runtime).toBe(OmniRuntime.JavaScript);
      }
    }
  });

  test('.append(1) on Python object → Python (no syntax evidence, provenance wins)', () => {
    const result = resolve('import os\nfiles = os.listdir("/tmp")\nfiles.append(1)');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call' &&
          (node as AST.Call).callee.kind === 'Member' &&
          ((node as AST.Call).callee as AST.Member).property.name === 'append') {
        expect(aff.runtime).toBe(OmniRuntime.Python);
      }
    }
  });

  test('neutral aggregate assignment adopts owner runtime from later mutable method use', () => {
    const result = resolve(`
closed = []

def row_stream():
  try:
    yield {"items": 1}
  finally:
    closed.append("closed")
`);

    let closedAssignment: AST.Assign | undefined;
    let closedArray: AST.ArrayLiteral | undefined;
    let appendCall: AST.Call | undefined;
    for (const [node] of result.affinityMap) {
      if (node.kind === 'Assign' &&
          node.left.kind === 'Identifier' &&
          node.left.name === 'closed') {
        closedAssignment = node;
        if (node.right.kind === 'ArrayLiteral') {
          closedArray = node.right;
        }
      }
      if (node.kind === 'Call' &&
          node.callee.kind === 'Member' &&
          node.callee.property.name === 'append') {
        appendCall = node;
      }
    }

    expect(result.affinityMap.get(closedAssignment!)?.runtime).toBe(OmniRuntime.Python);
    expect(result.affinityMap.get(closedArray!)?.runtime).toBe(OmniRuntime.Python);
    expect(result.affinityMap.get(appendCall!)?.runtime).toBe(OmniRuntime.Python);
  });

  test('neutral aggregate assignment adopts owner runtime from later call argument use', () => {
    const result = resolve(`
def rank_user(user):
  return user["id"]

sample = {"id": "u-42"}
result = rank_user(sample)
`);

    let sampleAssignment: AST.Assign | undefined;
    let sampleObject: AST.ObjectLiteral | undefined;
    let sampleArgument: AST.Identifier | undefined;
    for (const [node] of result.affinityMap) {
      if (node.kind === 'Assign' &&
          node.left.kind === 'Identifier' &&
          node.left.name === 'sample') {
        sampleAssignment = node;
        if (node.right.kind === 'ObjectLiteral') {
          sampleObject = node.right;
        }
      }
      if (node.kind === 'Call' &&
          node.callee.kind === 'Identifier' &&
          node.callee.name === 'rank_user' &&
          node.args[0]?.kind === 'Identifier') {
        sampleArgument = node.args[0];
      }
    }

    expect(result.affinityMap.get(sampleAssignment!)?.runtime).toBe(OmniRuntime.Python);
    expect(result.affinityMap.get(sampleObject!)?.runtime).toBe(OmniRuntime.Python);
    expect(result.affinityMap.get(sampleArgument!)?.runtime).toBe(OmniRuntime.Python);
  });

  test('syntax collision: both JS and Python syntax in args → callee provenance wins', () => {
    // Use a helper wrapping a comprehension as a separate argument.
    // wrap() is a Python-sourced call with a Lambda (JS) and a comprehension (Python) as separate args.
    const result = resolve('import os\nos.process([y for y in z], x => x)');
    for (const [node, aff] of result.affinityMap) {
      if (node.kind === 'Call' &&
          (node as AST.Call).callee.kind === 'Member' &&
          ((node as AST.Call).callee as AST.Member).property.name === 'process') {
        // Collision: Python ListComprehension + JS Lambda → callee provenance (Python) breaks tie
        expect(aff.runtime).toBe(OmniRuntime.Python);
      }
    }
  });
});
