/**
 * End-to-End Type System Showcase
 *
 * Demonstrates the full power of the unified type system by simulating
 * a real polyglot application where data flows between Python, JavaScript,
 * Go, and Rust — with the type checker validating every boundary crossing
 * and emitting the exact bridge operations needed.
 */

import {
  BoundaryChecker,
  checkCompatibility,
  lowerType,
  typeToString,
  // Canonical types
  INT32, INT64, UINT8, FLOAT64, STRING, BOOL, VOID, ANY, NULL, NEVER,
  option, array, map, func, async_, result, stream, bufferView, disposable,
  CanonicalType, StructType, FuncType, InterfaceType, OptionType,
  ArrayType, MapType, ResultType, AsyncType, StreamType, BufferViewType, DisposableType,
} from '../src/type-system';
import * as AST from '../src/ast';

describe('End-to-End: Polyglot Data Pipeline', () => {
  /**
   * Scenario: A web application where:
   * - Python handles data science (pandas-style processing)
   * - Go handles concurrent file I/O
   * - Rust handles parsing with strict error handling
   * - JavaScript handles the HTTP API layer
   *
   * Data flows: Go→Python→Rust→JavaScript with type checking at every boundary.
   */
  test('full pipeline: Go files → Python processing → Rust validation → JS API', () => {
    const checker = new BoundaryChecker();

    // ===== Go: Concurrent file scanner =====
    // func scanFiles(dir string) []FileInfo
    const FileInfo: StructType = {
      kind: 'struct',
      name: 'FileInfo',
      nominal: false, // structural at boundaries
      fields: [
        { name: 'path', type: STRING },
        { name: 'size', type: INT64 },
        { name: 'modified', type: INT64 },
        { name: 'isDir', type: BOOL },
      ],
    };

    const scanFiles: FuncType = {
      kind: 'func',
      params: [{ name: 'dir', type: STRING }],
      returns: array(FileInfo),
    };
    checker.declare('scanFiles', scanFiles, 'go');
    checker.declare('fileResults', array(FileInfo), 'go');

    // ===== Python: Data processing =====
    // def analyze(files: List[dict]) -> dict
    // Python sees FileInfo as a dict (structural typing at boundary)
    const PythonFileView: StructType = {
      kind: 'struct',
      nominal: false,
      fields: [
        { name: 'path', type: STRING },
        { name: 'size', type: { kind: 'int', size: 'big', signed: true } }, // Python int is bigint
        { name: 'modified', type: { kind: 'int', size: 'big', signed: true } },
      ],
    };

    const AnalysisResult: StructType = {
      kind: 'struct',
      nominal: false,
      fields: [
        { name: 'totalSize', type: { kind: 'int', size: 'big', signed: true } },
        { name: 'fileCount', type: { kind: 'int', size: 'big', signed: true } },
        { name: 'largestFile', type: option(STRING) },
        { name: 'categories', type: map(STRING, array(STRING)) },
      ],
    };

    const analyze: FuncType = {
      kind: 'func',
      params: [{ name: 'files', type: array(PythonFileView) }],
      returns: AnalysisResult,
    };
    checker.declare('analyze', analyze, 'python');

    // ===== Rust: Strict validation =====
    // fn validate(report: AnalysisReport) -> Result<ValidatedReport, ValidationError>
    const RustAnalysisReport: StructType = {
      kind: 'struct',
      name: 'AnalysisReport',
      nominal: false, // structural at boundary
      fields: [
        { name: 'totalSize', type: INT64 },
        { name: 'fileCount', type: INT32 },
        { name: 'largestFile', type: option(STRING) },
        { name: 'categories', type: map(STRING, array(STRING)) },
      ],
    };

    const ValidatedReport: StructType = {
      kind: 'struct',
      name: 'ValidatedReport',
      nominal: false,
      fields: [
        { name: 'totalSize', type: INT64 },
        { name: 'fileCount', type: INT32 },
        { name: 'largestFile', type: option(STRING) },
        { name: 'categories', type: map(STRING, array(STRING)) },
        { name: 'checksum', type: STRING },
        { name: 'valid', type: BOOL },
      ],
    };

    const ValidationError: StructType = {
      kind: 'struct',
      name: 'ValidationError',
      nominal: false,
      fields: [
        { name: 'code', type: INT32 },
        { name: 'message', type: STRING },
      ],
    };

    const validate: FuncType = {
      kind: 'func',
      params: [{ name: 'report', type: RustAnalysisReport }],
      returns: result(ValidatedReport, ValidationError),
    };
    checker.declare('validate', validate, 'rust');

    // ===== JavaScript: API response =====
    // async function respond(data: ValidatedReport): Promise<Response>
    const ApiResponse: StructType = {
      kind: 'struct',
      nominal: false,
      fields: [
        { name: 'status', type: INT32 },
        { name: 'body', type: ANY },
        { name: 'headers', type: map(STRING, STRING) },
      ],
    };

    const respond: FuncType = {
      kind: 'func',
      params: [{ name: 'data', type: ValidatedReport }],
      returns: async_(ApiResponse),
      async: true,
    };
    checker.declare('respond', respond, 'javascript');

    // ===== Now simulate the data flow =====

    // 1. Go → Python: fileResults flows to Python's analyze()
    //    Go []FileInfo → Python List[dict] (structural check)
    const crossing1 = checker.checkCrossing(
      'fileResults', 'python', array(PythonFileView)
    );
    // Go's i64 fields flow into Python's bigint — safe widening
    expect(crossing1.compat).toBe('safe');

    // 2. Python → Rust: AnalysisResult flows to Rust's validate()
    //    Python bigint → Rust i64/i32 (narrowing check needed!)
    checker.declare('analysisResult', AnalysisResult, 'python');
    const crossing2 = checker.checkCrossing(
      'analysisResult', 'rust', RustAnalysisReport
    );
    // Python bigint → Rust i64 is a check (big → fixed size)
    expect(crossing2.compat).toBe('check');

    // 3. Rust → JavaScript: Result<ValidatedReport, ValidationError> → JS
    //    JS doesn't have Result, so it must unwrap (may throw)
    checker.declare('validationResult', result(ValidatedReport, ValidationError), 'rust');
    const crossing3 = checker.checkCrossing(
      'validationResult', 'javascript', ValidatedReport
    );
    expect(crossing3.compat).toBe('check');
    expect(crossing3.bridgeOp?.op).toBe('throw_typed');

    // 4. JavaScript respond() returns Async<ApiResponse>
    //    If Go wants the result synchronously, it needs await
    checker.declare('apiResponse', async_(ApiResponse), 'javascript');
    const crossing4 = checker.checkCrossing(
      'apiResponse', 'go', ApiResponse
    );
    expect(crossing4.compat).toBe('coerce');
    expect(crossing4.bridgeOp?.op).toBe('await_resolve');

    // ===== Verify diagnostics =====
    const summary = checker.getSummary();
    expect(summary.crossings).toBe(4);
    expect(summary.errors).toBe(0); // No hard errors — everything is at least checkable
    expect(summary.safe).toBe(1);   // Go→Python (i64→bigint)
    expect(summary.coerce).toBe(1); // JS async→Go sync
    expect(summary.check).toBe(2);  // Python bigint→Rust i64, Rust Result→JS

    // Bridge ops tell the manifest generator exactly what to do
    const ops = checker.getBridgeOps();
    expect(ops.length).toBeGreaterThanOrEqual(2);
    expect(ops.some(o => o.op.op === 'throw_typed')).toBe(true);
    expect(ops.some(o => o.op.op === 'await_resolve')).toBe(true);
  });
});

describe('End-to-End: Error Handling Across Runtimes', () => {
  /**
   * The biggest headache in polyglot: how do errors cross boundaries?
   * - Rust: Result<T, E> (value-based)
   * - Go: (T, error) tuple (value-based)
   * - Python/JS/Java: exceptions (control-flow-based)
   *
   * The type system bridges these automatically.
   */
  test('Rust Result → JS exception → Python exception → Go error tuple', () => {
    const checker = new BoundaryChecker();

    // Rust function returns Result<String, ParseError>
    const ParseError: StructType = {
      kind: 'struct', name: 'ParseError', nominal: false,
      fields: [
        { name: 'line', type: INT32 },
        { name: 'message', type: STRING },
      ],
    };
    checker.declare('parseConfig', {
      kind: 'func',
      params: [{ type: STRING }],
      returns: result(STRING, ParseError),
    } as FuncType, 'rust');

    // Rust Result flowing to JS: throw_typed (Err becomes typed exception with metadata)
    checker.declare('configResult', result(STRING, ParseError), 'rust');
    const toJS = checker.checkCrossing('configResult', 'javascript', STRING);
    expect(toJS.compat).toBe('check');
    expect(toJS.bridgeOp?.op).toBe('throw_typed');
    expect((toJS.bridgeOp as any).errorKind).toBe('ParseError');

    // If JS wraps it back into a Result for Go, that's safe
    const toGo = checker.checkCrossing('configResult', 'go', result(STRING, ParseError));
    expect(toGo.compat).toBe('safe');

    // Go receiving a plain string from a Result is also a check (unwrap)
    checker.declare('goInput', result(STRING, ParseError), 'rust');
    const goUnwrap = checker.checkCrossing('goInput', 'go', STRING);
    expect(goUnwrap.compat).toBe('check');
    expect(goUnwrap.bridgeOp?.op).toBe('throw_typed');
  });
});

describe('End-to-End: Async Unification', () => {
  /**
   * Every language has different async primitives:
   * - JS: Promise<T>
   * - Rust: Future<Output=T>
   * - Python: Coroutine/Awaitable
   * - Go: channels + goroutines (no single async type)
   *
   * All canonicalize to Async<T>.
   */
  test('Promise/Future/Coroutine all unify through Async<T>', () => {
    const checker = new BoundaryChecker();

    // JS returns a Promise<User>
    const User: StructType = {
      kind: 'struct', nominal: false,
      fields: [
        { name: 'id', type: INT32 },
        { name: 'name', type: STRING },
        { name: 'email', type: option(STRING) },
      ],
    };

    checker.declare('fetchUser', {
      kind: 'func',
      params: [{ type: INT32 }],
      returns: async_(User),
      async: true,
    } as FuncType, 'javascript');

    checker.declare('userPromise', async_(User), 'javascript');

    // Python awaits it — Async<User> → User (needs await)
    const toPython = checker.checkCrossing('userPromise', 'python', User);
    expect(toPython.compat).toBe('coerce');
    expect(toPython.bridgeOp?.op).toBe('await_resolve');

    // Rust receives it as a Future — Async<User> → Async<User> (same shape, safe)
    const toRust = checker.checkCrossing('userPromise', 'rust', async_(User));
    expect(toRust.compat).toBe('safe');

    // Go receives the resolved User directly — same coerce + await
    const toGo = checker.checkCrossing('userPromise', 'go', User);
    expect(toGo.compat).toBe('coerce');
    expect(toGo.bridgeOp?.op).toBe('await_resolve');
  });
});

describe('End-to-End: Structural Duck Typing at Boundaries', () => {
  /**
   * The tolerant philosophy: if Python passes a dict with the right shape,
   * it satisfies a Rust struct — no nominal instance required.
   */
  test('Python dict satisfies Rust struct structurally', () => {
    const checker = new BoundaryChecker();

    // Python produces a dict-like object
    const pythonPoint: StructType = {
      kind: 'struct', nominal: false,
      fields: [
        { name: 'x', type: FLOAT64 },
        { name: 'y', type: FLOAT64 },
        { name: 'label', type: STRING }, // extra field — that's fine
      ],
    };

    // Rust expects a Point struct (but structural at boundary)
    const rustPoint: StructType = {
      kind: 'struct', name: 'Point', nominal: false,
      fields: [
        { name: 'x', type: FLOAT64 },
        { name: 'y', type: FLOAT64 },
      ],
    };

    checker.declare('origin', pythonPoint, 'python');
    const crossing = checker.checkCrossing('origin', 'rust', rustPoint);

    // Python has all required fields → safe (superset satisfies subset)
    expect(crossing.compat).toBe('safe');
  });

  test('missing field is caught at boundary', () => {
    const checker = new BoundaryChecker();

    const pythonPartial: StructType = {
      kind: 'struct', nominal: false,
      fields: [{ name: 'x', type: FLOAT64 }], // missing 'y'!
    };

    const rustPoint: StructType = {
      kind: 'struct', name: 'Point', nominal: false,
      fields: [
        { name: 'x', type: FLOAT64 },
        { name: 'y', type: FLOAT64 },
      ],
    };

    checker.declare('broken', pythonPartial, 'python');
    const crossing = checker.checkCrossing('broken', 'rust', rustPoint);
    expect(crossing.compat).toBe('incompatible');
    expect(checker.getErrors()[0].message).toContain('missing field: y');
  });

  test('optional fields are not required at boundary', () => {
    const checker = new BoundaryChecker();

    const minimal: StructType = {
      kind: 'struct', nominal: false,
      fields: [{ name: 'name', type: STRING }],
    };

    const full: StructType = {
      kind: 'struct', nominal: false,
      fields: [
        { name: 'name', type: STRING },
        { name: 'age', type: INT32, optional: true },
        { name: 'email', type: STRING, optional: true },
      ],
    };

    checker.declare('user', minimal, 'python');
    const crossing = checker.checkCrossing('user', 'typescript', full);
    // Only 'name' is required, and it's present → safe
    expect(crossing.compat).toBe('safe');
  });
});

describe('End-to-End: The Number Tower', () => {
  /**
   * The most common real-world crossing: numbers.
   * JS only has f64. Go has int/int64. Rust has i8-i128, u8-u128, f32, f64.
   * Python has arbitrary-precision int and f64 float.
   *
   * The type system tracks exactly what's safe and what might truncate.
   */
  test('number flows across all runtimes with correct coercions', () => {
    const checker = new BoundaryChecker();

    // Rust i32 → JS number (f64): lossless coercion
    checker.declare('rustCount', INT32, 'rust');
    const toJS = checker.checkCrossing('rustCount', 'javascript', FLOAT64);
    expect(toJS.compat).toBe('coerce');

    // JS number (f64) → Rust i32: DANGEROUS (truncation)
    checker.declare('jsValue', FLOAT64, 'javascript');
    const toRust = checker.checkCrossing('jsValue', 'rust', INT32);
    expect(toRust.compat).toBe('check');

    // Python int (bigint) → Go int64: may not fit
    const pyBigInt = { kind: 'int' as const, size: 'big' as const, signed: true };
    checker.declare('pyCount', pyBigInt, 'python');
    const toGo = checker.checkCrossing('pyCount', 'go', INT64);
    expect(toGo.compat).toBe('check');

    // Go int64 → Python int (bigint): always safe (bigint holds anything)
    checker.declare('goCount', INT64, 'go');
    const toPython = checker.checkCrossing('goCount', 'python', pyBigInt);
    expect(toPython.compat).toBe('safe');

    // Rust i32 → Go int64: safe widening
    const toGo2 = checker.checkCrossing('rustCount', 'go', INT64);
    expect(toGo2.compat).toBe('coerce');
  });
});

describe('End-to-End: Collections Crossing Boundaries', () => {
  test('array of structs from Go → Python with element coercion', () => {
    const checker = new BoundaryChecker();

    const GoRecord: StructType = {
      kind: 'struct', nominal: false,
      fields: [
        { name: 'id', type: INT64 },
        { name: 'value', type: FLOAT64 },
        { name: 'tags', type: array(STRING) },
      ],
    };

    const PyRecord: StructType = {
      kind: 'struct', nominal: false,
      fields: [
        { name: 'id', type: { kind: 'int', size: 'big', signed: true } },
        { name: 'value', type: FLOAT64 },
        { name: 'tags', type: array(STRING) },
      ],
    };

    checker.declare('records', array(GoRecord), 'go');
    const crossing = checker.checkCrossing('records', 'python', array(PyRecord));
    // Go int64 → Python bigint: safe (widening)
    expect(crossing.compat).toBe('safe');
  });

  test('Map<string, any> is the universal exchange format', () => {
    const checker = new BoundaryChecker();

    // Any language can pass a map to any other language
    checker.declare('config', map(STRING, ANY), 'javascript');
    expect(checker.checkCrossing('config', 'python', map(STRING, ANY)).compat).toBe('safe');
    expect(checker.checkCrossing('config', 'go', map(STRING, ANY)).compat).toBe('safe');
    expect(checker.checkCrossing('config', 'rust', map(STRING, ANY)).compat).toBe('safe');
  });
});

describe('End-to-End: Callbacks Across Runtimes', () => {
  test('JS callback passed to Python is type-checked', () => {
    const checker = new BoundaryChecker();

    // JS defines a comparator callback
    const comparator: FuncType = {
      kind: 'func',
      params: [{ name: 'a', type: STRING }, { name: 'b', type: STRING }],
      returns: INT32,
    };
    checker.declare('compare', comparator, 'javascript');

    // Python's sorted() expects a key function (str) → int
    const pythonKeyFn: FuncType = {
      kind: 'func',
      params: [{ type: STRING }],
      returns: INT32,
    };

    // Different arity — incompatible!
    const crossing = checker.checkCrossing('compare', 'python', pythonKeyFn);
    // The comparator takes 2 required params but Python key fn takes 1
    // This is correctly caught as incompatible (too many required params)
    expect(crossing.compat).toBe('incompatible');
  });

  test('compatible callback flows safely', () => {
    const checker = new BoundaryChecker();

    const jsMapper: FuncType = {
      kind: 'func',
      params: [{ type: STRING }],
      returns: STRING,
    };
    checker.declare('toUpper', jsMapper, 'javascript');

    const pyMapFn: FuncType = {
      kind: 'func',
      params: [{ type: STRING }],
      returns: STRING,
    };

    const crossing = checker.checkCrossing('toUpper', 'python', pyMapFn);
    expect(crossing.compat).toBe('safe');
  });
});

describe('End-to-End: Option/Nullable Unification', () => {
  /**
   * T?, Option<T>, Optional[T], T | null, *T (nil) — all the same concept.
   * The type system sees through all of them.
   */
  test('all nullable representations unify', () => {
    const checker = new BoundaryChecker();

    // Rust: Option<String>
    checker.declare('rustName', option(STRING), 'rust');

    // TypeScript expects: string | null (which lowers to Option<string>)
    const tsNullable = option(STRING);
    const crossing = checker.checkCrossing('rustName', 'typescript', tsNullable);
    expect(crossing.compat).toBe('safe');

    // Python expects: Optional[str] (same thing)
    const pyOptional = option(STRING);
    const crossing2 = checker.checkCrossing('rustName', 'python', pyOptional);
    expect(crossing2.compat).toBe('safe');

    // Go expects plain string — must unwrap (may panic on None)
    const crossing3 = checker.checkCrossing('rustName', 'go', STRING);
    expect(crossing3.compat).toBe('check');
    expect(crossing3.bridgeOp?.op).toBe('unwrap_option');

    // JS can pass a plain string INTO an Option slot — wraps automatically
    checker.declare('jsName', STRING, 'javascript');
    const crossing4 = checker.checkCrossing('jsName', 'rust', option(STRING));
    expect(crossing4.compat).toBe('safe');
    expect(crossing4.bridgeOp?.op).toBe('wrap_option');
  });
});

describe('End-to-End: AST Type Lowering', () => {
  /**
   * Demonstrates the full path from parsed type syntax → canonical type.
   */
  test('TypeScript generic lowers correctly', () => {
    // Simulates: Promise<Array<string>>
    const node: AST.GenericType = {
      kind: 'GenericType',
      base: { kind: 'Identifier', name: 'Promise', span: { line: 0, column: 0, start: 0, end: 7 } },
      args: [{
        kind: 'GenericType',
        base: { kind: 'Identifier', name: 'Array', span: { line: 0, column: 0, start: 8, end: 13 } },
        args: [{
          kind: 'SimpleType',
          id: { kind: 'Identifier', name: 'string', span: { line: 0, column: 0, start: 14, end: 20 } },
          span: { line: 0, column: 0, start: 14, end: 20 },
        }],
        span: { line: 0, column: 0, start: 8, end: 21 },
      }],
      span: { line: 0, column: 0, start: 0, end: 22 },
    };

    const lowered = lowerType(node, 'typescript');
    expect(lowered.kind).toBe('async');
    expect((lowered as AsyncType).inner.kind).toBe('array');
    expect(((lowered as AsyncType).inner as ArrayType).element).toEqual(STRING);
    expect(typeToString(lowered)).toBe('Async<Array<string>>');
  });

  test('Rust Result<i32, String> lowers correctly', () => {
    const node: AST.GenericType = {
      kind: 'GenericType',
      base: { kind: 'Identifier', name: 'Result', span: { line: 0, column: 0, start: 0, end: 6 } },
      args: [
        { kind: 'SimpleType', id: { kind: 'Identifier', name: 'i32', span: { line: 0, column: 0, start: 7, end: 10 } }, span: { line: 0, column: 0, start: 7, end: 10 } },
        { kind: 'SimpleType', id: { kind: 'Identifier', name: 'String', span: { line: 0, column: 0, start: 12, end: 18 } }, span: { line: 0, column: 0, start: 12, end: 18 } },
      ],
      span: { line: 0, column: 0, start: 0, end: 19 },
    };

    const lowered = lowerType(node, 'rust');
    expect(lowered.kind).toBe('result');
    expect((lowered as ResultType).ok).toEqual(INT32);
    expect((lowered as ResultType).err).toEqual(STRING);
    expect(typeToString(lowered)).toBe('Result<i32, string>');
  });

  test('Go chan int lowers to Channel<i64>', () => {
    const node: AST.ChanType = {
      kind: 'ChanType',
      direction: 'both',
      elementType: { kind: 'SimpleType', id: { kind: 'Identifier', name: 'int', span: { line: 0, column: 0, start: 5, end: 8 } }, span: { line: 0, column: 0, start: 5, end: 8 } },
      span: { line: 0, column: 0, start: 0, end: 8 },
    };

    const lowered = lowerType(node, 'go');
    expect(lowered.kind).toBe('channel');
    expect(typeToString(lowered)).toBe('Chan<i64>');
  });

  test('Python Optional[Dict[str, List[int]]] lowers fully', () => {
    // Optional[Dict[str, List[int]]]
    const node: AST.GenericType = {
      kind: 'GenericType',
      base: { kind: 'Identifier', name: 'Optional', span: { line: 0, column: 0, start: 0, end: 8 } },
      args: [{
        kind: 'GenericType',
        base: { kind: 'Identifier', name: 'Dict', span: { line: 0, column: 0, start: 9, end: 13 } },
        args: [
          { kind: 'SimpleType', id: { kind: 'Identifier', name: 'str', span: { line: 0, column: 0, start: 14, end: 17 } }, span: { line: 0, column: 0, start: 14, end: 17 } },
          {
            kind: 'GenericType',
            base: { kind: 'Identifier', name: 'List', span: { line: 0, column: 0, start: 19, end: 23 } },
            args: [
              { kind: 'SimpleType', id: { kind: 'Identifier', name: 'int', span: { line: 0, column: 0, start: 24, end: 27 } }, span: { line: 0, column: 0, start: 24, end: 27 } },
            ],
            span: { line: 0, column: 0, start: 19, end: 28 },
          },
        ],
        span: { line: 0, column: 0, start: 9, end: 29 },
      }],
      span: { line: 0, column: 0, start: 0, end: 30 },
    };

    const lowered = lowerType(node, 'python');
    expect(lowered.kind).toBe('option');
    const inner = (lowered as OptionType).inner;
    expect(inner.kind).toBe('map');
    expect((inner as MapType).key).toEqual(STRING);
    expect((inner as MapType).value.kind).toBe('array');
    // Python int is bigint
    expect(((inner as MapType).value as ArrayType).element).toEqual({ kind: 'int', size: 'big', signed: true });
    expect(typeToString(lowered)).toBe('Option<Map<string, Array<ibig>>>');
  });
});

// ============================================================
// Advanced Type System Features
// ============================================================

describe('Advanced: Streams (Multi-Value Async)', () => {
  test('Go channel → TS AsyncIterable via stream_proxy', () => {
    const checker = new BoundaryChecker();
    // Go: ch := make(chan string) — produces multiple values
    checker.declare('logStream', { kind: 'channel', element: STRING, direction: 'recv' }, 'go');

    // TS wants AsyncIterable<string> — a Stream
    const r = checker.checkCrossing('logStream', 'javascript', stream(STRING));
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('stream_proxy');
  });

  test('Stream<i32> → Stream<f64> coerces element types', () => {
    const r = checkCompatibility(stream(INT32), stream(FLOAT64));
    expect(r.compat).toBe('coerce'); // i32→f64 widening propagates
  });

  test('Stream → Async is a lossy check (takes first element only)', () => {
    const r = checkCompatibility(stream(STRING), async_(STRING));
    expect(r.compat).toBe('check');
    expect(r.reason).toContain('first element');
  });

  test('Channel → Async receives one value (coerce with await)', () => {
    const r = checkCompatibility(
      { kind: 'channel', element: INT32, direction: 'recv' },
      async_(INT32)
    );
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('await_resolve');
  });

  test('Stream → Channel is coerce (downgrade)', () => {
    const r = checkCompatibility(stream(STRING), { kind: 'channel', element: STRING, direction: 'both' });
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('stream_proxy');
  });

  test('backpressure mismatch produces coerce warning', () => {
    const source = stream(INT32, false);   // no backpressure
    const target = stream(INT32, true);    // wants backpressure
    const r = checkCompatibility(source, target);
    expect(r.compat).toBe('coerce');
    expect(r.reason).toContain('backpressure');
  });

  test('typeToString renders Stream', () => {
    expect(typeToString(stream(STRING))).toBe('Stream<string>');
  });
});

describe('Advanced: Zero-Copy BufferView', () => {
  test('Go []byte → Rust &[u8] via share_memory (zero-copy)', () => {
    const checker = new BoundaryChecker();
    // Go has an owned byte slice
    checker.declare('imageData', bufferView(UINT8, 'owned'), 'go');
    // Rust wants a borrowed immutable view
    const r = checker.checkCrossing('imageData', 'rust', bufferView(UINT8, 'borrowed'));
    expect(r.compat).not.toBe('incompatible');
    expect(r.bridgeOp?.op).toBe('share_memory');
  });

  test('immutable → mutable buffer requires copy', () => {
    const r = checkCompatibility(
      bufferView(UINT8, 'borrowed', false),
      bufferView(UINT8, 'borrowed', true)
    );
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('copy_buffer');
  });

  test('Array<u8> → BufferView<u8> coerces (array to view)', () => {
    const r = checkCompatibility(array(UINT8), bufferView(UINT8, 'borrowed'));
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('share_memory');
  });

  test('BufferView → Array copies out', () => {
    const r = checkCompatibility(bufferView(UINT8, 'owned'), array(UINT8));
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('copy_buffer');
  });

  test('bytes → BufferView<u8> coerces', () => {
    const r = checkCompatibility({ kind: 'bytes' }, bufferView(UINT8, 'shared'));
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('share_memory');
  });

  test('typeToString renders BufferView', () => {
    expect(typeToString(bufferView(UINT8))).toBe('BufferView<u8>');
  });

  test('lowering: Uint8Array → BufferView', () => {
    const node: AST.TypeNode = { kind: 'SimpleType', id: { kind: 'Identifier', name: 'Uint8Array', span: { start: 0, end: 0, line: 0, column: 0 } }, span: { start: 0, end: 0, line: 0, column: 0 } };
    const lowered = lowerType(node, 'javascript');
    expect(lowered.kind).toBe('buffer_view');
    expect((lowered as BufferViewType).element).toEqual(UINT8);
  });
});

describe('Advanced: Typed Exceptions (Error Fidelity)', () => {
  test('Result<T, NamedError> → T uses throw_typed with error kind', () => {
    const IoError: StructType = {
      kind: 'struct', name: 'IoError', nominal: true, origin: 'rust',
      fields: [{ name: 'code', type: INT32 }, { name: 'message', type: STRING }],
    };
    const r = checkCompatibility(result(STRING, IoError), STRING);
    expect(r.compat).toBe('check');
    expect(r.bridgeOp?.op).toBe('throw_typed');
    expect((r.bridgeOp as any).errorKind).toBe('IoError');
  });

  test('Result<T, ANY> → T uses generic unwrap_result', () => {
    // When error type is not a named struct, fall back to unwrap_result
    const r = checkCompatibility(result(STRING, ANY), STRING);
    expect(r.compat).toBe('check');
    expect(r.bridgeOp?.op).toBe('unwrap_result');
  });

  test('Result<T, anonymous struct> → T uses generic unwrap_result', () => {
    const anonError: StructType = {
      kind: 'struct', nominal: false,
      fields: [{ name: 'msg', type: STRING }],
    };
    const r = checkCompatibility(result(INT32, anonError), INT32);
    expect(r.compat).toBe('check');
    expect(r.bridgeOp?.op).toBe('unwrap_result');
  });

  test('full flow: Rust IoError → JS catch with type info', () => {
    const checker = new BoundaryChecker();
    const RustError: StructType = {
      kind: 'struct', name: 'ConnectionError', nominal: true, origin: 'rust',
      fields: [
        { name: 'host', type: STRING },
        { name: 'port', type: INT32 },
        { name: 'reason', type: STRING },
      ],
    };
    checker.declare('dbResult', result(array(STRING), RustError), 'rust');
    const r = checker.checkCrossing('dbResult', 'javascript', array(STRING));
    expect(r.compat).toBe('check');
    expect(r.bridgeOp?.op).toBe('throw_typed');
    expect((r.bridgeOp as any).errorKind).toBe('ConnectionError');

    // JS can catch: if (e.kind === 'ConnectionError') { e.host, e.port, e.reason }
    const ops = checker.getBridgeOps();
    expect(ops).toHaveLength(1);
    expect(ops[0].op.op).toBe('throw_typed');
  });
});

describe('Advanced: Cross-Boundary Resource Management (Disposable)', () => {
  test('T → Disposable<T> wraps with finalizer', () => {
    // TS passes a file handle to Go — Go needs to know when to close it
    const r = checkCompatibility(
      { kind: 'struct', name: 'FileHandle', fields: [], nominal: false },
      disposable({ kind: 'struct', name: 'FileHandle', fields: [], nominal: false }, 'close')
    );
    expect(r.compat).toBe('coerce');
    expect(r.bridgeOp?.op).toBe('attach_disposer');
    expect((r.bridgeOp as any).disposer).toBe('close');
  });

  test('Disposable<T> → T is a check (receiver must manage lifecycle)', () => {
    const r = checkCompatibility(
      disposable(STRING, 'dispose'),
      STRING
    );
    expect(r.compat).toBe('check');
    expect(r.reason).toContain('lifecycle');
  });

  test('callback proxy across boundary gets proxy_with_finalizer', () => {
    const checker = new BoundaryChecker();
    // TS passes a callback to Go as io.Reader interface
    const readerInterface: InterfaceType = {
      kind: 'interface', name: 'Reader',
      methods: [{ name: 'Read', type: func([bufferView(UINT8, 'owned', true)], INT32) }],
    };
    // Wrapping in Disposable signals Go must release the proxy when done
    checker.declare('jsReader', disposable(readerInterface, 'close'), 'javascript');
    const r = checker.checkCrossing('jsReader', 'go', disposable(readerInterface, 'close'));
    expect(r.compat).toBe('safe'); // same disposable type
  });

  test('typeToString renders Disposable', () => {
    expect(typeToString(disposable(STRING))).toBe('Disposable<string>');
  });

  test('lowering: Closer → Disposable', () => {
    const node: AST.TypeNode = { kind: 'SimpleType', id: { kind: 'Identifier', name: 'Closer', span: { start: 0, end: 0, line: 0, column: 0 } }, span: { start: 0, end: 0, line: 0, column: 0 } };
    const lowered = lowerType(node, 'go');
    expect(lowered.kind).toBe('disposable');
    expect((lowered as DisposableType).disposer).toBe('close');
  });

  test('lowering: Java closeable resources use close disposer', () => {
    for (const name of ['Closeable', 'AutoCloseable', 'java.io.Closeable', 'java.lang.AutoCloseable']) {
      const node: AST.TypeNode = { kind: 'SimpleType', id: { kind: 'Identifier', name, span: { start: 0, end: 0, line: 0, column: 0 } }, span: { start: 0, end: 0, line: 0, column: 0 } };
      const lowered = lowerType(node, 'java');
      expect(lowered.kind).toBe('disposable');
      expect((lowered as DisposableType).disposer).toBe('close');
    }
  });

  test('lowering: Disposable keeps dispose disposer', () => {
    const node: AST.TypeNode = { kind: 'SimpleType', id: { kind: 'Identifier', name: 'Disposable', span: { start: 0, end: 0, line: 0, column: 0 } }, span: { start: 0, end: 0, line: 0, column: 0 } };
    const lowered = lowerType(node, 'java');
    expect(lowered.kind).toBe('disposable');
    expect((lowered as DisposableType).disposer).toBe('dispose');
  });

  test('lowering: AsyncIterable → Stream', () => {
    const node: AST.TypeNode = {
      kind: 'GenericType',
      base: { kind: 'Identifier', name: 'AsyncIterable', span: { start: 0, end: 0, line: 0, column: 0 } },
      args: [{ kind: 'SimpleType', id: { kind: 'Identifier', name: 'string', span: { start: 0, end: 0, line: 0, column: 0 } }, span: { start: 0, end: 0, line: 0, column: 0 } }],
      span: { start: 0, end: 0, line: 0, column: 0 },
    };
    const lowered = lowerType(node, 'javascript');
    expect(lowered.kind).toBe('stream');
    expect((lowered as StreamType).element).toEqual(STRING);
  });
});
