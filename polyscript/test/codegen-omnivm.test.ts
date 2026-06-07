import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import { RuntimeResolver, OmniRuntime } from '../src/runtime-resolver';
import { OmniVMCodeGenerator } from '../src/codegen-omnivm';
import { Emitter } from '../src/codegen-omnivm/emitter';
import { BridgeEmitter } from '../src/codegen-omnivm/bridge-emitter';
import { AsyncOrchestrator } from '../src/codegen-omnivm/async-orchestrator';
import { consolidateBlocks, isConsolidatable, isCompiledRuntime } from '../src/codegen-omnivm/runtime-blocks';
import { RuntimeAffinity, MarshalKind } from '../src/runtime-resolver/types';

function parseAndGenerate(code: string): string {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast);
  const codegen = new OmniVMCodeGenerator();
  return codegen.generate(annotated);
}

// --- Emitter Tests ---

describe('Emitter', () => {
  test('emit produces raw text', () => {
    const emitter = new Emitter();
    emitter.emit('hello');
    emitter.emit(' world');
    expect(emitter.toString()).toBe('hello world');
  });

  test('emitLine produces indented lines', () => {
    const emitter = new Emitter();
    emitter.emitLine('line 1');
    emitter.push();
    emitter.emitLine('line 2');
    emitter.pop();
    emitter.emitLine('line 3');
    expect(emitter.toString()).toBe('line 1\n  line 2\nline 3\n');
  });

  test('reset clears output', () => {
    const emitter = new Emitter();
    emitter.emitLine('hello');
    emitter.reset();
    expect(emitter.toString()).toBe('');
  });
});

// --- Bridge Emitter Tests ---

describe('BridgeEmitter', () => {
  test('emitCall generates omnivm.call()', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const result = bridge.emitCall(OmniRuntime.Python, 'print("hello")');
    expect(result).toBe('omnivm.call("python", `print("hello")`)');
  });

  test('emitCall with captures includes context object', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const result = bridge.emitCall(OmniRuntime.Python, 'process(x)', { x: 'x' });
    expect(result).toBe('omnivm.call("python", `process(x)`, { x })');
  });

  test('emitCallAsync generates omnivm.callAsync()', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const result = bridge.emitCallAsync(OmniRuntime.Python, 'await fetch_data()');
    expect(result).toBe('omnivm.callAsync("python", `await fetch_data()`)');
  });

  test('emitCall escapes backticks in code', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const result = bridge.emitCall(OmniRuntime.Ruby, 'puts `date`');
    expect(result).toContain('\\`');
  });

  test('tempVar generates unique names', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const v1 = bridge.tempVar();
    const v2 = bridge.tempVar();
    expect(v1).not.toBe(v2);
  });

  test('emitCallAsDecl emits variable declaration', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    bridge.emitCallAsDecl('result', OmniRuntime.Go, 'fmt.Sprintf("hello")');
    const output = emitter.toString();
    expect(output).toContain('const result = omnivm.call("go"');
    expect(output).toContain('fmt.Sprintf("hello")');
  });
});

// --- Async Orchestrator Tests ---

describe('AsyncOrchestrator', () => {
  test('emitSingleAsync generates await for async runtime', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const orchestrator = new AsyncOrchestrator(emitter, bridge);

    orchestrator.emitSingleAsync({
      varName: 'data',
      runtime: OmniRuntime.Python,
      code: 'fetch_data()',
      isAsync: true,
    });

    const output = emitter.toString();
    expect(output).toContain('const data = await omnivm.callAsync("python"');
  });

  test('emitSingleAsync generates sync call for Ruby', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const orchestrator = new AsyncOrchestrator(emitter, bridge);

    orchestrator.emitSingleAsync({
      varName: 'result',
      runtime: OmniRuntime.Ruby,
      code: 'compute()',
      isAsync: false,
    });

    const output = emitter.toString();
    expect(output).toContain('const result = omnivm.call("ruby"');
    expect(output).not.toContain('await');
  });

  test('emitParallelAsync generates Promise.all pattern', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const orchestrator = new AsyncOrchestrator(emitter, bridge);

    orchestrator.emitParallelAsync([
      { varName: 'py_data', runtime: OmniRuntime.Python, code: 'fetch_data()', isAsync: true },
      { varName: 'js_data', runtime: OmniRuntime.JavaScript, code: 'fetch("/api")', isAsync: true },
    ]);

    const output = emitter.toString();
    expect(output).toContain('Promise.all');
    expect(output).toContain('await');
  });

  test('emitSingleAsync uses JS directly for JavaScript runtime', () => {
    const emitter = new Emitter();
    const bridge = new BridgeEmitter(emitter);
    const orchestrator = new AsyncOrchestrator(emitter, bridge);

    orchestrator.emitSingleAsync({
      varName: 'data',
      runtime: OmniRuntime.JavaScript,
      code: 'fetch("/api")',
      isAsync: true,
    });

    const output = emitter.toString();
    expect(output).toContain('const data = await fetch("/api")');
    expect(output).not.toContain('omnivm');
  });
});

// --- Runtime Blocks ---

describe('Runtime Blocks', () => {
  test('consolidateBlocks groups adjacent same-runtime nodes', () => {
    const affinityMap = new Map<any, RuntimeAffinity>();
    const node1: any = { kind: 'ExprStmt', expr: { kind: 'Identifier', name: 'a' } };
    const node2: any = { kind: 'ExprStmt', expr: { kind: 'Identifier', name: 'b' } };
    const node3: any = { kind: 'ExprStmt', expr: { kind: 'Identifier', name: 'c' } };

    affinityMap.set(node1, { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] });
    affinityMap.set(node2, { runtime: OmniRuntime.Python, confidence: 'definite', evidence: [] });
    affinityMap.set(node3, { runtime: OmniRuntime.JavaScript, confidence: 'fallback', evidence: [] });

    const blocks = consolidateBlocks([node1, node2, node3], affinityMap);
    expect(blocks.length).toBe(2);
    expect(blocks[0].runtime).toBe(OmniRuntime.Python);
    expect(blocks[0].nodes.length).toBe(2);
    expect(blocks[1].runtime).toBe(OmniRuntime.JavaScript);
    expect(blocks[1].nodes.length).toBe(1);
  });

  test('JS nodes are not consolidated', () => {
    const affinityMap = new Map<any, RuntimeAffinity>();
    const node1: any = { kind: 'ExprStmt', expr: { kind: 'Identifier', name: 'a' } };
    const node2: any = { kind: 'ExprStmt', expr: { kind: 'Identifier', name: 'b' } };

    affinityMap.set(node1, { runtime: OmniRuntime.JavaScript, confidence: 'fallback', evidence: [] });
    affinityMap.set(node2, { runtime: OmniRuntime.JavaScript, confidence: 'fallback', evidence: [] });

    const blocks = consolidateBlocks([node1, node2], affinityMap);
    // JS nodes get their own blocks (not consolidated)
    expect(blocks.length).toBe(2);
  });

  test('isConsolidatable returns true for multi-node non-JS blocks', () => {
    expect(isConsolidatable({ runtime: OmniRuntime.Python, nodes: [{} as any, {} as any] })).toBe(true);
    expect(isConsolidatable({ runtime: OmniRuntime.JavaScript, nodes: [{} as any, {} as any] })).toBe(false);
    expect(isConsolidatable({ runtime: OmniRuntime.Python, nodes: [{} as any] })).toBe(false);
  });

  test('isCompiledRuntime returns true for Rust and C', () => {
    expect(isCompiledRuntime(OmniRuntime.Rust)).toBe(true);
    expect(isCompiledRuntime(OmniRuntime.C)).toBe(true);
    expect(isCompiledRuntime(OmniRuntime.Python)).toBe(false);
    expect(isCompiledRuntime(OmniRuntime.JavaScript)).toBe(false);
  });
});

// --- Code Generator Integration ---

describe('OmniVM Code Generator', () => {
  test('JS function emits natively', () => {
    const output = parseAndGenerate('function hello() { return 42 }');
    expect(output).toContain('function hello()');
    expect(output).toContain('return 42');
    expect(output).not.toContain('omnivm.call');
  });

  test('Rust fn uses callCompiled', () => {
    const output = parseAndGenerate('fn compute(x: i32) -> i32 { x * 2 }');
    // Rust is a compiled target — uses callCompiled instead of call
    expect(output).toContain('omnivm.callCompiled("rust"');
  });

  test('Python def wraps in omnivm.call', () => {
    const output = parseAndGenerate('def greet(name):\n  print(name)');
    expect(output).toContain('omnivm.call("python"');
  });

  test('pass statement generates Python bridge call', () => {
    const output = parseAndGenerate('pass');
    expect(output).toContain('omnivm.call("python"');
  });

  test('generated code is syntactically valid JS structure', () => {
    const output = parseAndGenerate('function hello() { return 42 }');
    // Should not contain syntax errors — basic structural check
    expect(output).toMatch(/function\s+hello\s*\(/);
    expect(output).toContain('{');
    expect(output).toContain('}');
  });

  test('const declaration emits correctly', () => {
    const output = parseAndGenerate('const x = 42');
    expect(output).toContain('const x');
    expect(output).toContain('42');
  });

  test('variable declaration emits correctly', () => {
    const output = parseAndGenerate('let y = "hello"');
    // VarDecl maps to let
    expect(output).toContain('y');
  });
});

// --- String Interpolation in Code Generator ---

describe('String Interpolation Code Generation', () => {
  test('simple string emits as JSON string', () => {
    const output = parseAndGenerate('const msg = "hello world"');
    // Should contain the string value
    expect(output).toContain('hello');
  });
});
