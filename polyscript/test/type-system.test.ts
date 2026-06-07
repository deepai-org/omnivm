import {
  BoundaryChecker,
  checkCompatibility,
  typeToString,
  INT32, INT64, FLOAT64, FLOAT32, STRING, BOOL, VOID, ANY, NULL, NEVER,
  option, array, map, func, async_, result,
  CanonicalType, StructType, FuncType, InterfaceType,
} from '../src/type-system';

describe('Canonical Type System', () => {
  describe('Primitive compatibility', () => {
    test('same types are safe', () => {
      expect(checkCompatibility(INT32, INT32).compat).toBe('safe');
      expect(checkCompatibility(STRING, STRING).compat).toBe('safe');
      expect(checkCompatibility(BOOL, BOOL).compat).toBe('safe');
    });

    test('integer widening is coercion', () => {
      expect(checkCompatibility(INT32, INT64).compat).toBe('coerce');
    });

    test('integer narrowing requires check', () => {
      expect(checkCompatibility(INT64, INT32).compat).toBe('check');
    });

    test('int to float64 is coercion (lossless for i32)', () => {
      expect(checkCompatibility(INT32, FLOAT64).compat).toBe('coerce');
    });

    test('float to int requires check (truncation)', () => {
      expect(checkCompatibility(FLOAT64, INT32).compat).toBe('check');
    });

    test('anything flows to any', () => {
      expect(checkCompatibility(INT32, ANY).compat).toBe('safe');
      expect(checkCompatibility(STRING, ANY).compat).toBe('safe');
    });

    test('any flows to anything with coercion', () => {
      expect(checkCompatibility(ANY, INT32).compat).toBe('coerce');
    });

    test('never flows to anything', () => {
      expect(checkCompatibility(NEVER, INT32).compat).toBe('safe');
      expect(checkCompatibility(NEVER, STRING).compat).toBe('safe');
    });

    test('nothing flows to never', () => {
      expect(checkCompatibility(INT32, NEVER).compat).toBe('incompatible');
    });
  });

  describe('Option / Nullable', () => {
    test('T flows into Option<T> (wrapping)', () => {
      const r = checkCompatibility(INT32, option(INT32));
      expect(r.compat).toBe('safe');
      expect(r.bridgeOp?.op).toBe('wrap_option');
    });

    test('Option<T> to T requires check (may be null)', () => {
      const r = checkCompatibility(option(STRING), STRING);
      expect(r.compat).toBe('check');
      expect(r.bridgeOp?.op).toBe('unwrap_option');
    });

    test('null flows into Option<T>', () => {
      expect(checkCompatibility(NULL, option(INT32)).compat).toBe('safe');
    });
  });

  describe('Result / Error handling', () => {
    test('T flows into Result<T, E> (wrapping in Ok)', () => {
      const r = checkCompatibility(INT32, result(INT32, STRING));
      expect(r.compat).toBe('safe');
      expect(r.bridgeOp?.op).toBe('wrap_result');
    });

    test('Result<T, E> to T requires check (may be error)', () => {
      const r = checkCompatibility(result(INT32, STRING), INT32);
      expect(r.compat).toBe('check');
      expect(r.bridgeOp?.op).toBe('unwrap_result');
    });
  });

  describe('Collections', () => {
    test('Array<i32> flows into Array<i64> with coercion', () => {
      const r = checkCompatibility(array(INT32), array(INT64));
      expect(r.compat).toBe('coerce');
    });

    test('Array<string> to Array<int> requires check (element parsing)', () => {
      expect(checkCompatibility(array(STRING), array(INT32)).compat).toBe('check');
    });

    test('Map<string, i32> into Map<string, f64> coerces', () => {
      const r = checkCompatibility(map(STRING, INT32), map(STRING, FLOAT64));
      expect(r.compat).toBe('coerce');
    });
  });

  describe('Async', () => {
    test('Async<T> to T needs await', () => {
      const r = checkCompatibility(async_(STRING), STRING);
      expect(r.compat).toBe('coerce');
      expect(r.bridgeOp?.op).toBe('await_resolve');
    });

    test('T flows into Async<T> (already resolved)', () => {
      expect(checkCompatibility(STRING, async_(STRING)).compat).toBe('safe');
    });
  });

  describe('Functions', () => {
    test('compatible function signatures', () => {
      const f1 = func([INT32, STRING], BOOL);
      const f2 = func([INT32, STRING], BOOL);
      expect(checkCompatibility(f1, f2).compat).toBe('safe');
    });

    test('covariant return type', () => {
      const f1 = func([], INT32);
      const f2 = func([], FLOAT64);
      expect(checkCompatibility(f1, f2).compat).toBe('coerce');
    });
  });

  describe('Structs — structural vs nominal', () => {
    test('structural: superset satisfies subset', () => {
      const from: StructType = {
        kind: 'struct', nominal: false,
        fields: [
          { name: 'x', type: INT32 },
          { name: 'y', type: INT32 },
          { name: 'z', type: INT32 },
        ],
      };
      const to: StructType = {
        kind: 'struct', nominal: false,
        fields: [
          { name: 'x', type: INT32 },
          { name: 'y', type: INT32 },
        ],
      };
      expect(checkCompatibility(from, to).compat).toBe('safe');
    });

    test('structural: missing field is incompatible', () => {
      const from: StructType = {
        kind: 'struct', nominal: false,
        fields: [{ name: 'x', type: INT32 }],
      };
      const to: StructType = {
        kind: 'struct', nominal: false,
        fields: [
          { name: 'x', type: INT32 },
          { name: 'y', type: INT32 },
        ],
      };
      expect(checkCompatibility(from, to).compat).toBe('incompatible');
    });

    test('nominal: same name/origin is safe', () => {
      const from: StructType = { kind: 'struct', name: 'Point', nominal: true, origin: 'rust', fields: [] };
      const to: StructType = { kind: 'struct', name: 'Point', nominal: true, origin: 'rust', fields: [] };
      expect(checkCompatibility(from, to).compat).toBe('safe');
    });

    test('nominal: different name is incompatible', () => {
      const from: StructType = { kind: 'struct', name: 'Point', nominal: true, origin: 'rust', fields: [] };
      const to: StructType = { kind: 'struct', name: 'Vec2', nominal: true, origin: 'rust', fields: [] };
      expect(checkCompatibility(from, to).compat).toBe('incompatible');
    });
  });

  describe('String conversions', () => {
    test('string to int requires parse check', () => {
      const r = checkCompatibility(STRING, INT32);
      expect(r.compat).toBe('check');
      expect(r.bridgeOp?.op).toBe('parse_int');
    });

    test('int to string is coercion', () => {
      const r = checkCompatibility(INT32, STRING);
      expect(r.compat).toBe('coerce');
      expect(r.bridgeOp?.op).toBe('to_string');
    });
  });

  describe('Union types', () => {
    test('member type flows into union', () => {
      const union: CanonicalType = { kind: 'union', members: [INT32, STRING] };
      expect(checkCompatibility(INT32, union).compat).toBe('safe');
      expect(checkCompatibility(STRING, union).compat).toBe('safe');
    });

    test('bool coerces into union containing string (via to_string)', () => {
      const union: CanonicalType = { kind: 'union', members: [INT32, STRING] };
      expect(checkCompatibility(BOOL, union).compat).toBe('coerce');
    });
  });
});

describe('BoundaryChecker integration', () => {
  test('detects safe crossing (same types different runtimes)', () => {
    const checker = new BoundaryChecker();
    checker.declare('files', array(STRING), 'python');
    const result = checker.checkCrossing('files', 'javascript', array(STRING));
    expect(result.compat).toBe('safe');
    expect(checker.getErrors()).toHaveLength(0);
  });

  test('detects type error at boundary', () => {
    const checker = new BoundaryChecker();
    checker.declare('count', INT32, 'go');
    checker.checkCrossing('count', 'javascript', array(STRING));
    expect(checker.getErrors()).toHaveLength(1);
    expect(checker.getErrors()[0].message).toContain('cannot convert');
  });

  test('detects coercion needed (i32 from Go → f64 in JS)', () => {
    const checker = new BoundaryChecker();
    checker.declare('count', INT32, 'go');
    const result = checker.checkCrossing('count', 'javascript', FLOAT64);
    expect(result.compat).toBe('coerce');
    expect(checker.getErrors()).toHaveLength(0);
  });

  test('same runtime is always safe (no crossing)', () => {
    const checker = new BoundaryChecker();
    checker.declare('x', INT32, 'javascript');
    const result = checker.checkCrossing('x', 'javascript', STRING);
    expect(result.compat).toBe('safe'); // No crossing, trust the runtime
  });

  test('function call crossing checks arg types', () => {
    const checker = new BoundaryChecker();
    const sortFunc: FuncType = {
      kind: 'func',
      params: [{ type: array(STRING) }, { name: 'reverse', type: BOOL, optional: true }],
      returns: array(STRING),
    };
    checker.declare('sorted', sortFunc, 'python');
    const results = checker.checkCallCrossing('sorted', 'javascript', [array(STRING), BOOL]);
    expect(results.every(r => r.compat === 'safe')).toBe(true);
  });

  test('function call with incompatible arg (array where int expected)', () => {
    const checker = new BoundaryChecker();
    const addFunc: FuncType = {
      kind: 'func',
      params: [{ type: INT32 }, { type: INT32 }],
      returns: INT32,
    };
    checker.declare('add', addFunc, 'rust');
    checker.checkCallCrossing('add', 'javascript', [array(STRING), INT32]);
    expect(checker.getErrors()).toHaveLength(1);
    expect(checker.getErrors()[0].message).toContain('Arg 0');
  });

  test('summary counts crossings correctly', () => {
    const checker = new BoundaryChecker();
    checker.declare('a', INT32, 'go');
    checker.declare('b', STRING, 'python');
    checker.declare('c', FLOAT64, 'javascript');

    checker.checkCrossing('a', 'javascript', INT32);     // safe
    checker.checkCrossing('a', 'javascript', FLOAT64);   // coerce
    checker.checkCrossing('b', 'rust', INT32);           // check (parse)
    checker.checkCrossing('c', 'go', array(STRING));     // incompatible

    const summary = checker.getSummary();
    expect(summary.crossings).toBe(4);
    expect(summary.safe).toBe(1);
    expect(summary.coerce).toBe(1);
    expect(summary.check).toBe(1);
    expect(summary.errors).toBe(1);
  });

  test('bridge ops are collected for manifest enrichment', () => {
    const checker = new BoundaryChecker();
    checker.declare('name', option(STRING), 'rust');
    checker.checkCrossing('name', 'javascript', STRING);
    const ops = checker.getBridgeOps();
    expect(ops).toHaveLength(1);
    expect(ops[0].op.op).toBe('unwrap_option');
  });
});

describe('typeToString', () => {
  test('renders primitives', () => {
    expect(typeToString(INT32)).toBe('i32');
    expect(typeToString(FLOAT64)).toBe('f64');
    expect(typeToString(STRING)).toBe('string');
  });

  test('renders complex types', () => {
    expect(typeToString(option(INT32))).toBe('Option<i32>');
    expect(typeToString(array(STRING))).toBe('Array<string>');
    expect(typeToString(result(INT32, STRING))).toBe('Result<i32, string>');
    expect(typeToString(func([INT32, STRING], BOOL))).toBe('(i32, string) => bool');
  });
});
