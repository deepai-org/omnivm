/**
 * Example: How to Update Weak Tests
 * 
 * This shows the before/after for updating the 
 * "parses mixed async/concurrent patterns" test
 */

import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import {
  verifyFunctionDecl,
  verifyGenericType,
  verifyComparison,
  verifyChannelSend,
  verifyChannelReceive,
  findInAST,
  verifyAngleBrackets
} from './helpers/ast-verifiers';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('BEFORE: Weak Test (Current)', () => {
  test('parses mixed async/concurrent patterns', () => {
    const code = `
async fn processStream<T>(input: Stream<T>) -> Result<Vec<T>, Error> {
  results := []
  ch := make(chan T, 100)
  
  for i := 0; i < 10; i++ {
    go async () => {
      while item := <-ch {
        processed := await transform(item)
        results.push(processed)
      }
    }()
  }
  
  ch <- item
  return Ok(results)
}`;

    const ast = parseCode(code);
    
    // ❌ WEAK: Only checks that something was parsed!
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
    
    // That's it! No verification of:
    // - Generic types (Stream<T>, Result<Vec<T>, Error>)
    // - Comparison operator (i < 10)
    // - Channel operations (<-ch, ch <- item)
    // - Function structure
  });
});

describe('AFTER: Strong Test (Updated)', () => {
  test('parses mixed async/concurrent patterns', () => {
    const code = `
async fn processStream<T>(input: Stream<T>) -> Result<Vec<T>, Error> {
  results := []
  ch := make(chan T, 100)
  
  for i := 0; i < 10; i++ {
    go async () => {
      while item := <-ch {
        processed := await transform(item)
        results.push(processed)
      }
    }()
  }
  
  ch <- item
  return Ok(results)
}`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Verify complete AST structure
    
    // 1. Verify function declaration
    const func = ast.body[0] as AST.FuncDecl;
    verifyFunctionDecl(func, 'processStream', {
      async: true,
      genericParams: ['T'],
      paramCount: 1
    });
    
    // 2. Verify parameter type: Stream<T>
    const paramType = func.params[0].type;
    verifyGenericType(paramType, 'Stream', 1);
    expect(paramType.args[0].name).toBe('T');
    
    // 3. Verify return type: Result<Vec<T>, Error>
    const returnType = func.returnType;
    verifyGenericType(returnType, 'Result', 2);
    
    // Verify nested generic Vec<T>
    const vecType = returnType.args[0];
    verifyGenericType(vecType, 'Vec', 1);
    expect(vecType.args[0].name).toBe('T');
    
    // Verify Error type
    expect(returnType.args[1].name).toBe('Error');
    
    // 4. Verify function body
    const body = func.body as AST.Block;
    expect(body.statements.length).toBeGreaterThanOrEqual(4);
    
    // 5. Verify for loop with comparison
    const forLoop = findInAST(body, n => n.kind === 'For') as AST.For;
    expect(forLoop).toBeDefined();
    
    // Verify loop condition: i < 10
    verifyComparison(forLoop.condition, '<', 'i', 10);
    
    // 6. Verify go routine
    const goStmt = findInAST(forLoop.body, n => n.kind === 'Go');
    expect(goStmt).toBeDefined();
    
    // 7. Verify channel receive: <-ch
    const channelReceive = findInAST(body, n => 
      n.kind === 'Unary' && n.op === '<-'
    );
    verifyChannelReceive(channelReceive, 'ch');
    
    // 8. Verify channel send: ch <- item
    const channelSend = findInAST(body, n => 
      n.kind === 'Binary' && n.op === '<-' && n.left?.name === 'ch'
    );
    verifyChannelSend(channelSend, 'ch', 'item');
    
    // 9. Use compound verifier for all angle brackets
    verifyAngleBrackets(ast, {
      generics: [
        { base: 'Stream', argCount: 1 },
        { base: 'Result', argCount: 2 },
        { base: 'Vec', argCount: 1 }
      ],
      comparisons: [
        { op: '<', left: 'i', right: 10 }
      ],
      channels: {
        sends: 1,
        receives: 1
      }
    });
  });
});

describe('ALTERNATIVE: Using Pattern-Based Verification', () => {
  test('verifies all angle brackets in complex code', () => {
    const code = `
function Component<T>() {
  const a = x < 5;
  const b = <Button />;
  const c: Array<string> = [];
  ch <- value;
  result := <-ch;
}`;

    const ast = parseCode(code);
    
    // Single call verifies all angle bracket uses!
    verifyAngleBrackets(ast, {
      jsx: [
        { tag: 'Button', selfClosing: true }
      ],
      generics: [
        { base: 'Array', argCount: 1 }
      ],
      comparisons: [
        { op: '<', left: 'x', right: 5 }
      ],
      channels: {
        sends: 1,
        receives: 1
      }
    });
    
    // Much cleaner and more maintainable!
  });
});

// ============= Test Migration Guide =============

/**
 * Step-by-step process for updating weak tests:
 * 
 * 1. Identify weak patterns:
 *    - expect(ast.body.length).toBeGreaterThan(...)
 *    - expect(() => parse()).not.toThrow()
 *    - expect(ast).toBeDefined()
 * 
 * 2. Analyze what needs verification:
 *    - Look for < and > in the test code
 *    - Identify JSX, generics, comparisons, channels
 * 
 * 3. Add specific verifications:
 *    - Cast to specific AST types: as AST.FuncDecl
 *    - Check node.kind for each important node
 *    - Verify operators, names, structure
 * 
 * 4. Use helpers for consistency:
 *    - verifyJSXElement() for JSX
 *    - verifyGenericType() for generics
 *    - verifyComparison() for < > operators
 *    - verifyChannel*() for channel ops
 * 
 * 5. Add compound verification:
 *    - Use verifyAngleBrackets() to check all at once
 *    - Ensures nothing is missed
 * 
 * 6. Document what's being tested:
 *    - Add comments explaining each verification
 *    - Makes tests self-documenting
 */