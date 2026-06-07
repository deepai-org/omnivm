import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

// Import compatibility helpers
import { normalizeVarDecl } from './helpers/ast-compat';

// Import helpers
import {
  verifyJSXElement,
  verifyGenericType,
  verifyBinaryOp,
  verifyComparison,
  verifyChannelSend,
  verifyChannelReceive,
  verifyAngleBrackets,
  verifyFunctionDecl,
  findInAST,
  findAllInAST
} from './helpers/ast-verifiers';

import {
  findJSXElements,
  findGenericTypes,
  findComparisons,
  findChannelOperations,
  findAllAngleBracketUsages,
  analyzeAngleBrackets,
  findByKind,
  findFirst
} from './helpers/pattern-matchers';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Parser - Advanced Polyglot Patterns (UPDATED)', () => {
  
  test('parses extreme operator mixing and chaining', () => {
    const code = `
# Mixing all operator styles
result := x ?? y || z && w <=> v << 2 >> 1 >>> 3
chan := make(<-chan int)
value := <-chan ?? throw new Error("no value")
ptr := &obj.*.field?.[index]
spread := [...arr1, ...arr2, ...?optionalArr]
pipeline := data |> filter |> map |> reduce
range := 1..10
destructured := {x, y, ...rest} = obj
`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Verify structure
    expect(ast.body.length).toBeGreaterThanOrEqual(8);
    
    // Verify first statement 
    const stmt1 = normalizeVarDecl(ast.body[0]);
    expect(stmt1.kind).toBe('VarDecl');
    expect(stmt1.names[0].name).toBe('result');
    
    // Find and verify shift operators
    const allBinary = findAllInAST(ast, n => n.kind === 'Binary') as AST.Binary[];
    const shifts = allBinary.filter(n => ['<<', '>>', '>>>'].includes(n.op));
    expect(shifts.length).toBe(3);
    // Check that all three shift operators are present (order may vary based on parsing)
    const shiftOps = shifts.map(s => s.op).sort();
    expect(shiftOps).toEqual(['<<', '>>', '>>>'].sort());
    
    // Find channel operations
    const channelOps = findChannelOperations(ast);
    expect(channelOps.receives.length).toBeGreaterThan(0);
    
    // Analyze angle brackets
    const usage = findAllAngleBracketUsages(ast);
    expect(usage.shifts.leftShift.length).toBe(1);
    expect(usage.shifts.rightShift.length).toBe(1);
    expect(usage.shifts.unsignedRightShift.length).toBe(1);
    expect(usage.channels.receives.length).toBeGreaterThan(0);
  });

  test('parses mixed async/concurrent patterns', () => {
    const code = `
# Mixing async/await, goroutines, and channels
async fn processStream<T>(input: Stream<T>) -> Result<Vec<T>, Error> {
  results := []
  ch := make(chan T, 100)
  
  // Spawn multiple workers
  for i := 0; i < 10; i++ {
    go async () => {
      while item := <-ch {
        processed := await transform(item)
        results.push(processed)
      }
    }()
  }
  
  // Feed the channel
  async for await (const item of input) {
    select {
      case ch <- item:
        continue
      default:
        await sleep(100)
    }
  }
  
  return Ok(results)
}
`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Complete verification
    
    // 1. Verify function exists
    expect(ast.body).toHaveLength(1);
    const func = ast.body[0] as AST.FuncDecl;
    expect(func.kind).toBe('FuncDecl');
    expect(func.name.name).toBe('processStream');
    expect(func.async).toBe(true);
    
    // 2. Verify generic parameters
    expect(func.genericParams).toBeDefined();
    expect(func.genericParams).toHaveLength(1);
    expect(func.genericParams![0].name).toBe('T');
    
    // 3. Verify parameter type is generic
    const param = func.params[0];
    expect(param.type).toBeDefined();
    const paramType = param.type as AST.GenericType;
    expect(paramType.kind).toBe('GenericType');
    expect((paramType.base as AST.Identifier).name).toBe('Stream');
    expect(paramType.args).toHaveLength(1);
    
    // 4. Verify return type is generic
    expect(func.returnType).toBeDefined();
    const returnType = func.returnType as AST.GenericType;
    expect(returnType.kind).toBe('GenericType');
    expect((returnType.base as AST.Identifier).name).toBe('Result');
    expect(returnType.args).toHaveLength(2);
    
    // Verify nested Vec<T>
    const vecType = returnType.args[0] as AST.GenericType;
    expect(vecType.kind).toBe('GenericType');
    expect((vecType.base as AST.Identifier).name).toBe('Vec');
    
    // 5. Find for loop in body
    const loops = findAllInAST(func.body, n => n.kind === 'Loop') as AST.Loop[];
    expect(loops.length).toBeGreaterThan(0);
    
    // Find comparison in loop condition
    const comparisons = findComparisons(ast);
    expect(comparisons.length).toBeGreaterThan(0);
    
    // Verify i < 10 comparison
    const lessThan = comparisons.find(c => 
      c.op === '<' && 
      c.left.kind === 'Identifier' && 
      (c.left as AST.Identifier).name === 'i'
    );
    expect(lessThan).toBeDefined();
    
    // 6. Find channel operations
    const channelOps = findChannelOperations(ast);
    expect(channelOps.receives.length).toBeGreaterThan(0);
    expect(channelOps.sends.length).toBeGreaterThan(0);
    
    // 7. Comprehensive angle bracket verification
    const usage = findAllAngleBracketUsages(ast);
    expect(usage.generics.length).toBe(3); // Stream<T>, Result<...>, Vec<T>
    expect(usage.comparisons.lessThan.length).toBe(1); // i < 10
    expect(usage.channels.sends.length).toBe(1); // ch <- item
    expect(usage.channels.receives.length).toBe(1); // <-ch
    
    // 8. Analyze statistics
    const stats = analyzeAngleBrackets(ast);
    expect(stats.genericCount).toBe(3);
    expect(stats.comparisonCount).toBe(1);
    expect(stats.channelCount).toBe(2);
    expect(stats.totalUsages).toBe(6);
  });

  test('parses complex pattern matching across languages', () => {
    const code = `
match value {
  Some(x) if x > 0 => {
    case x when
      1..10) "single digit"
      11..99) "double digit"
      _) "large"
    esac
  }
  None => "empty"
  Ok(result) => match result.type {
    Pattern::Regex(r) => r.test(input)
    Pattern::Glob(g) => g.match(input)
    _ => false
  }
  Err(e) => throw e
}
`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Verify structure
    expect(ast.body).toHaveLength(1);
    
    // Find comparison operators
    const comparisons = findComparisons(ast);
    expect(comparisons.length).toBeGreaterThanOrEqual(1);
    
    // Should have x > 0 comparison
    const greaterThan = comparisons.find(c => c.op === '>');
    expect(greaterThan).toBeDefined();
    
    // Verify angle bracket usage
    const usage = findAllAngleBracketUsages(ast);
    expect(usage.comparisons.greaterThan.length).toBe(1);
  });

  test('verifies all angle bracket types in mixed code', () => {
    const code = `
// Test all angle bracket uses
function test<T>() {
  // JSX
  const element = <Button onClick={() => console.log("clicked")} />;
  
  // Generics
  const arr: Array<string> = [];
  const map: Map<string, number> = new Map();
  
  // Comparisons
  if (x < 5 && y > 10) {
    console.log("in range");
  }
  
  // Channels (Go-style)
  const value = <-ch;
  ch <- newValue;
  
  // Shift operators
  const shifted = bits << 2 >> 1;
}
`;

    const ast = parseCode(code);
    
    // Use comprehensive verifier
    verifyAngleBrackets(ast, {
      jsx: [
        { tag: 'Button', selfClosing: true }
      ],
      generics: [
        { base: 'Array', argCount: 1 },
        { base: 'Map', argCount: 2 }
      ],
      comparisons: [
        { op: '<', left: 'x', right: 5 },
        { op: '>', left: 'y', right: 10 }
      ],
      channels: {
        sends: 1,
        receives: 1
      }
    });
    
    // Analyze all angle brackets
    const stats = analyzeAngleBrackets(ast);
    expect(stats.jsxCount).toBe(1);
    expect(stats.genericCount).toBe(2);
    expect(stats.comparisonCount).toBe(2);
    expect(stats.channelCount).toBe(2);
    expect(stats.shiftCount).toBe(2);
    
    // Verify summary
    expect(stats.summary).toContain('1 JSX');
    expect(stats.summary).toContain('2 generic');
    expect(stats.summary).toContain('2 comparison');
    expect(stats.summary).toContain('2 channel');
    expect(stats.summary).toContain('2 shift');
  });
});