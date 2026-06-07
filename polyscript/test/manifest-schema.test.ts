import { DISPATCH_MANIFEST_SCHEMA, MANIFEST_SCHEMA_VERSION } from '../src/codegen-omnivm/manifest-schema';
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import { RuntimeResolver } from '../src/runtime-resolver';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';

function manifestFor(code: string) {
  const tokens = new Lexer(code).tokenize();
  const ast = new Parser(tokens, code).parse();
  const annotated = new RuntimeResolver().resolve(ast, code);
  return new ManifestCodeGenerator().generate(annotated);
}

describe('Dispatch Manifest Schema', () => {
  test('schema pins the manifest version and required top-level fields', () => {
    expect(DISPATCH_MANIFEST_SCHEMA.properties.version.const).toBe(MANIFEST_SCHEMA_VERSION);
    expect(DISPATCH_MANIFEST_SCHEMA.required).toEqual(['version', 'defaultRuntime', 'ops']);
  });

  test('schema enumerates every manifest op kind', () => {
    const defs = DISPATCH_MANIFEST_SCHEMA.$defs as any;
    const opRefs = defs.ManifestOp.oneOf.map((entry: any) => entry.$ref.split('/').pop());
    expect(opRefs).toEqual([
      'ExecOp',
      'EvalOp',
      'ExecCompiledOp',
      'EvalCompiledOp',
      'DeclareOp',
      'AssignOp',
      'FuncDefOp',
      'ReturnOp',
      'IfOp',
      'LoopOp',
      'TryOp',
      'ThrowOp',
      'ParallelOp',
      'ConcatOp',
      'ImportOp',
      'NativeOp',
      'ChanOp',
      'SelectOp',
      'SpawnOp',
      'ResourceOp',
      'TableOp',
      'JobOp',
      'YieldOp',
      'AwaitOp',
    ]);
  });

  test('generated manifests satisfy the schema top-level contract', () => {
    const manifest = manifestFor('const ch = make(1)\nch <- 1\nclose(ch)');
    expect(manifest.version).toBe(DISPATCH_MANIFEST_SCHEMA.properties.version.const);
    for (const required of DISPATCH_MANIFEST_SCHEMA.required) {
      expect(manifest).toHaveProperty(required);
    }
    expect(Array.isArray(manifest.ops)).toBe(true);
  });

  test('job schema includes cancellation action and cleanup code', () => {
    const defs = DISPATCH_MANIFEST_SCHEMA.$defs as any;
    expect(defs.JobOp.properties.action.enum).toEqual(['enqueue', 'complete', 'wait', 'cancel']);
    expect(defs.JobOp.properties.code.type).toBe('string');
  });

  test('loop schema includes async foreach metadata', () => {
    const defs = DISPATCH_MANIFEST_SCHEMA.$defs as any;
    expect(defs.LoopOp.properties.mode.enum).toEqual(['while', 'for', 'infinite', 'foreach']);
    expect(defs.LoopOp.properties.await.type).toBe('boolean');
    expect(defs.LoopOp.properties.variable.type).toBe('string');
  });
});
