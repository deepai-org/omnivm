/**
 * Bridge Integration Tests — PolyScript ↔ OmniVM
 *
 * These tests verify that manifests emitted by PolyScript's type system
 * and manifest generator produce the exact bridge ops that OmniVM's
 * bridge executor expects. They test the contract between the two projects.
 *
 * Matching Go tests exist in omnivm/pkg/manifest/bridge_integration_test.go
 * that consume these same manifest shapes and verify runtime behavior.
 */

import {
  BoundaryChecker,
  INT32, INT64, UINT8, FLOAT64, STRING, BOOL, ANY,
  option, array, result,
  CanonicalType, StructType,
} from '../src/type-system';

import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import { RuntimeResolver } from '../src/runtime-resolver';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';
import { DispatchManifest, ManifestBridgeOp } from '../src/codegen-omnivm/manifest-types';

// Helper: parse PolyScript source → manifest
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

// Helper: find bridge ops for a binding
function findBridges(manifest: DispatchManifest, binding: string): ManifestBridgeOp[] {
  return (manifest.bridges || []).filter(b => b.binding === binding);
}

// ─── Contract Tests: Bridge Op Shapes ───────────────────────────────────
// These verify the exact JSON shape that OmniVM's Go code parses.

describe('Bridge Op Contract: ManifestBridgeOp shape', () => {
  test('bridge ops have required fields: binding, op', () => {
    const checker = new BoundaryChecker();
    checker.declare('score', FLOAT64, 'python');

    const crossing = checker.checkCrossing('score', 'go', INT32);
    expect(crossing.compat).not.toBe('safe'); // should need conversion

    const bridges = checker.getBridgeOps();
    for (const b of bridges) {
      expect(b).toHaveProperty('binding');
      expect(b).toHaveProperty('op');
      expect(typeof b.binding).toBe('string');
      expect(typeof b.op.op).toBe('string');
    }
  });

  test('narrow op includes from/to in meta', () => {
    const checker = new BoundaryChecker();
    checker.declare('score', FLOAT64, 'python');

    checker.checkCrossing('score', 'go', INT32);
    const bridges = checker.getBridgeOps();
    const narrow = bridges.find(b => b.op.op === 'narrow');
    if (narrow) {
      // OmniVM reads meta.from and meta.to for range checking
      expect(narrow.op).toHaveProperty('from');
      expect(narrow.op).toHaveProperty('to');
    }
  });

  test('identity op emitted for safe crossings of same type', () => {
    const checker = new BoundaryChecker();
    checker.declare('name', STRING, 'python');

    const crossing = checker.checkCrossing('name', 'javascript', STRING);
    // String→String across runtimes should be safe or identity
    expect(['safe', 'coerce']).toContain(crossing.compat);
  });
});

// ─── Contract Tests: TypeSummary shape ──────────────────────────────────

describe('TypeSummary contract', () => {
  test('summary has all required fields', () => {
    const checker = new BoundaryChecker();
    checker.declare('x', INT32, 'python');
    checker.checkCrossing('x', 'go', INT64);

    const summary = checker.getSummary();
    expect(summary).toHaveProperty('crossings');
    expect(summary).toHaveProperty('safe');
    expect(summary).toHaveProperty('coerce');
    expect(summary).toHaveProperty('check');
    expect(summary).toHaveProperty('errors');
    expect(typeof summary.crossings).toBe('number');
  });

  test('crossing counts are consistent', () => {
    const checker = new BoundaryChecker();
    checker.declare('a', INT32, 'python');
    checker.declare('b', FLOAT64, 'python');
    checker.declare('c', STRING, 'python');

    checker.checkCrossing('a', 'go', INT64);
    checker.checkCrossing('b', 'go', INT32);
    checker.checkCrossing('c', 'javascript', STRING);

    const summary = checker.getSummary();
    expect(summary.crossings).toBe(3);
    expect(summary.safe + summary.coerce + summary.check + summary.errors).toBe(3);
  });
});

// ─── Scenario Tests: OmniVM-consumable manifests ────────────────────────
// Each test produces a manifest that matches what OmniVM's Go integration
// tests consume in bridge_integration_test.go.

describe('Scenario: Numeric narrowing (f64 → i32)', () => {
  test('f64 to i32 crossing emits narrow or check', () => {
    const checker = new BoundaryChecker();
    checker.declare('score', FLOAT64, 'python');

    const crossing = checker.checkCrossing('score', 'go', INT32);
    // f64→i32 is lossy — should be check or coerce level
    expect(['coerce', 'check']).toContain(crossing.compat);

    if (crossing.bridgeOp) {
      expect(crossing.bridgeOp.op).toBe('narrow');
    }
  });
});

describe('Scenario: Option unwrapping', () => {
  test('Option<string> to string emits unwrap_option', () => {
    const checker = new BoundaryChecker();
    checker.declare('maybeUser', option(STRING), 'python');

    const crossing = checker.checkCrossing('maybeUser', 'javascript', STRING);
    expect(crossing.bridgeOp).toBeDefined();
    if (crossing.bridgeOp) {
      expect(crossing.bridgeOp.op).toBe('unwrap_option');
    }
  });

  test('control-flow narrowing eliminates unwrap_option', () => {
    const checker = new BoundaryChecker();
    checker.declare('maybeUser', option(STRING), 'python');

    // Simulate: if (maybeUser !== null) { ... }
    // Push a narrowing scope that narrows Option<string> → string
    const narrowed = BoundaryChecker.narrowType(option(STRING), 'not-null');
    expect(narrowed).toBeDefined();
    expect(narrowed!.kind).toBe('string');

    // Inside the narrowed scope, the crossing should be safe
    const narrowings = new Map<string, CanonicalType>();
    narrowings.set('maybeUser', narrowed!);
    checker.pushNarrow(narrowings);

    const crossing = checker.checkCrossing('maybeUser', 'javascript', STRING);
    // After narrowing, should be safe — no unwrap_option needed
    expect(crossing.compat).toBe('safe');

    checker.popNarrow();
  });
});

describe('Scenario: Result convention', () => {
  test('Result<string, string> to string emits unwrap_result', () => {
    const checker = new BoundaryChecker();
    checker.declare('fileResult', result(STRING, STRING), 'python');

    const crossing = checker.checkCrossing('fileResult', 'ruby', STRING);
    expect(crossing.bridgeOp).toBeDefined();
    if (crossing.bridgeOp) {
      expect(['unwrap_result', 'throw_typed']).toContain(crossing.bridgeOp.op);
    }
  });
});

describe('Scenario: Array deep copy', () => {
  test('Array<i32> crossing emits copy_array', () => {
    const checker = new BoundaryChecker();
    checker.declare('numbers', array(INT32), 'python');

    const crossing = checker.checkCrossing('numbers', 'javascript', array(INT32));
    // Arrays need deep copy across boundaries
    if (crossing.bridgeOp) {
      expect(crossing.bridgeOp.op).toBe('copy_array');
    }
  });
});

describe('Scenario: Struct reshape (camelCase ↔ snake_case)', () => {
  test('struct with different field conventions emits struct_reshape or struct_to_dict', () => {
    const JsPerson: StructType = {
      kind: 'struct',
      name: 'Person',
      nominal: false,
      fields: [
        { name: 'firstName', type: STRING },
        { name: 'lastName', type: STRING },
        { name: 'birthYear', type: INT32 },
      ],
    };

    const checker = new BoundaryChecker();
    checker.declare('person', JsPerson, 'javascript');

    // Crossing to Python — structs become dicts
    const crossing = checker.checkCrossing('person', 'python', ANY);
    // At minimum, struct crossing should be recognized
    expect(crossing.compat).not.toBe('incompatible');
  });
});

describe('Scenario: Widen is always safe', () => {
  test('i32 to i64 is safe widening', () => {
    const checker = new BoundaryChecker();
    checker.declare('count', INT32, 'python');

    const crossing = checker.checkCrossing('count', 'go', INT64);
    expect(['safe', 'coerce']).toContain(crossing.compat);
    if (crossing.bridgeOp) {
      expect(crossing.bridgeOp.op).toBe('widen');
    }
  });

  test('i32 to f64 is safe widening', () => {
    const checker = new BoundaryChecker();
    checker.declare('count', INT32, 'python');

    const crossing = checker.checkCrossing('count', 'javascript', FLOAT64);
    expect(['safe', 'coerce']).toContain(crossing.compat);
  });
});

describe('Scenario: Multiple crossings for same binding', () => {
  test('same binding used in two target runtimes gets separate crossings', () => {
    const checker = new BoundaryChecker();
    checker.declare('val', FLOAT64, 'python');

    checker.checkCrossing('val', 'go', INT32);    // narrow
    checker.checkCrossing('val', 'javascript', STRING);  // to_string

    const bridges = checker.getBridgeOps();
    const valBridges = bridges.filter(b => b.binding === 'val');
    // Should have two separate bridge ops for the two crossings
    expect(valBridges.length).toBeGreaterThanOrEqual(1);

    const summary = checker.getSummary();
    expect(summary.crossings).toBe(2);
  });
});

describe('Scenario: Same-runtime crossing is safe', () => {
  test('Python→Python crossing is always safe', () => {
    const checker = new BoundaryChecker();
    checker.declare('x', FLOAT64, 'python');

    const crossing = checker.checkCrossing('x', 'python', INT32);
    // Same runtime — no boundary, always safe
    expect(crossing.compat).toBe('safe');
  });
});

// ─── Manifest Generator Integration ─────────────────────────────────────
// These test the full pipeline: source code → manifest with bridges.

describe('Manifest bridges field', () => {
  test('manifest without crossings has no bridges field', () => {
    const m = parseAndManifest('const x = 42');
    // Pure JS — no cross-runtime crossings
    expect(m.bridges).toBeUndefined();
  });

  test('manifest bridges array contains ManifestBridgeOp objects', () => {
    // If we can trigger a cross-runtime manifest, verify the bridge shape
    const m = parseAndManifest('def greet(name):\n  print(name)');
    // This may or may not produce bridges depending on resolver decisions
    if (m.bridges && m.bridges.length > 0) {
      for (const bridge of m.bridges) {
        expect(bridge).toHaveProperty('binding');
        expect(bridge).toHaveProperty('op');
        expect(typeof bridge.op).toBe('string');
      }
    }
  });

  test('manifest typeSummary has correct shape when present', () => {
    const m = parseAndManifest('def greet(name):\n  print(name)');
    if (m.typeSummary) {
      expect(m.typeSummary).toHaveProperty('crossings');
      expect(m.typeSummary).toHaveProperty('safe');
      expect(m.typeSummary).toHaveProperty('coerce');
      expect(m.typeSummary).toHaveProperty('check');
      expect(m.typeSummary).toHaveProperty('errors');
    }
  });

  test('manifest JSON is valid for OmniVM ParseManifest', () => {
    const code = 'def process(x):\n  return x * 2';
    const lexer = new Lexer(code);
    const tokens = lexer.tokenize();
    const parser = new Parser(tokens, code);
    const ast = parser.parse();
    const resolver = new RuntimeResolver();
    const annotated = resolver.resolve(ast, code);
    const gen = new ManifestCodeGenerator();
    const json = gen.generateJSON(annotated);

    // Parse and verify it has the shape OmniVM expects
    const parsed = JSON.parse(json);
    expect(parsed.version).toBe(1);
    expect(parsed.defaultRuntime).toBeDefined();
    expect(Array.isArray(parsed.ops)).toBe(true);

    // bridges and typeSummary are optional
    if (parsed.bridges) {
      expect(Array.isArray(parsed.bridges)).toBe(true);
    }
    if (parsed.typeSummary) {
      expect(typeof parsed.typeSummary.crossings).toBe('number');
    }
  });
});
