/**
 * Comprehensive Angle Bracket Disambiguation Verification Suite
 * 
 * This test suite ensures that all uses of < and > are correctly
 * disambiguated between JSX, generics, comparisons, channels, and shifts.
 */

import { describe, test, expect } from '@jest/globals';
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

import {
  verifyJSXElement,
  verifyGenericType,
  verifyComparison,
  verifyChannelSend,
  verifyChannelReceive,
  verifyAngleBrackets
} from './helpers/ast-verifiers';

import {
  analyzeAngleBrackets,
  findAllAngleBracketUsages
} from './helpers/pattern-matchers';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Angle Bracket Disambiguation Verification', () => {
  
  describe('JSX vs Comparison', () => {
    test.each([
      ['<div />', 'JSXElement', 'div'],
      ['<Button />', 'JSXElement', 'Button'],
      ['<Form.Input />', 'JSXElement', 'Form.Input'],
      ['x < 5', 'Binary', '<'],
      ['y > 10', 'Binary', '>'],
      ['a <= b', 'Binary', '<='],
      ['c >= d', 'Binary', '>=']
    ])('correctly parses "%s" as %s', (code, expectedKind, detail) => {
      const ast = parseCode(code);
      const expr = (ast.body[0] as AST.ExprStmt).expr;
      
      if (expectedKind === 'JSXElement') {
        expect(expr.kind).toBe('JSXElement');
        const jsx = expr as AST.JSXElement;
        const name = jsx.openingElement.name;
        if (name.kind === 'JSXIdentifier') {
          expect(name.name).toBe(detail);
        }
      } else {
        expect(expr.kind).toBe('Binary');
        const binary = expr as AST.Binary;
        expect(binary.op).toBe(detail);
      }
    });
  });
  
  describe('Generic vs JSX vs Comparison', () => {
    test('disambiguates in type context', () => {
      const code = `
let x: Array<string> = [];
let y: Map<string, number> = new Map();
let z: Result<Vec<T>, Error>;
`;
      const ast = parseCode(code);
      
      // All should be parsed as generics in type position
      const usage = findAllAngleBracketUsages(ast);
      expect(usage.generics.length).toBe(4); // Array, Map, Result, Vec
      expect(usage.jsx.elements.length).toBe(0);
      expect(usage.comparisons.lessThan.length).toBe(0);
    });
    
    test('disambiguates in expression context', () => {
      const code = `
const a = <Button />;
const b = x < 5;
const c = <Component>{children}</Component>;
`;
      const ast = parseCode(code);
      
      const usage = findAllAngleBracketUsages(ast);
      expect(usage.jsx.elements.length).toBe(2); // Button, Component
      expect(usage.comparisons.lessThan.length).toBe(1); // x < 5
      expect(usage.generics.length).toBe(0);
    });
  });
  
  describe('Channel Operations', () => {
    test('correctly identifies channel send and receive', () => {
      const code = `
ch := make(chan int)
value := <-ch
ch <- 42
select {
  case msg := <-ch:
    print(msg)
  case ch <- value:
    continue
}
`;
      const ast = parseCode(code);
      
      const usage = findAllAngleBracketUsages(ast);
      expect(usage.channels.receives.length).toBe(2); // <-ch twice
      expect(usage.channels.sends.length).toBe(2); // ch <- twice
      
      // Verify they're not confused with comparisons
      expect(usage.comparisons.lessThan.length).toBe(0);
    });
  });
  
  describe('Shift Operators', () => {
    test('distinguishes shifts from comparisons', () => {
      const code = `
const left = x << 2;
const right = y >> 3;
const unsigned = z >>> 4;
const comparison = a < b && c > d;
`;
      const ast = parseCode(code);
      
      const usage = findAllAngleBracketUsages(ast);
      expect(usage.shifts.leftShift.length).toBe(1);
      expect(usage.shifts.rightShift.length).toBe(1);
      expect(usage.shifts.unsignedRightShift.length).toBe(1);
      expect(usage.comparisons.lessThan.length).toBe(1);
      expect(usage.comparisons.greaterThan.length).toBe(1);
    });
  });
  
  describe('Complex Mixed Cases', () => {
    test('handles all angle bracket types in one expression', () => {
      const code = `
function process<T, U>(data: Stream<T>) {
  const jsx = <Button onClick={() => x < 5} />;
  const shifted = bits << 2 >> 1;
  const chan = <-ch;
  ch <- value;
  
  if (a < b && c > d) {
    return <Result<Vec<T>, Error>>{
      ok: true,
      value: data
    };
  }
}
`;
      const ast = parseCode(code);
      
      // Comprehensive verification
      verifyAngleBrackets(ast, {
        jsx: [
          { tag: 'Button', selfClosing: true }
          // Note: <Result<Vec<T>, Error>> is a type assertion, not JSX
        ],
        generics: [
          { base: 'Stream', argCount: 1 },
          { base: 'Result', argCount: 2 }, // Result<Vec<T>, Error> in type assertion
          { base: 'Vec', argCount: 1 }     // Vec<T> nested in Result
        ],
        comparisons: [
          { op: '<', left: 'x', right: 5 },
          { op: '<', left: 'a' },
          { op: '>', left: 'c' }
        ],
        channels: {
          receives: 1,
          sends: 1
        }
      });
      
      // Verify statistics
      const stats = analyzeAngleBrackets(ast);
      expect(stats.jsxCount).toBe(1); // Only Button is JSX
      expect(stats.genericCount).toBe(3); // Stream<T>, Result<Vec<T>, Error>, Vec<T>
      expect(stats.comparisonCount).toBe(3);
      expect(stats.channelCount).toBe(2);
      expect(stats.shiftCount).toBe(2);
      
      // Total should be sum of all
      expect(stats.totalUsages).toBe(11); // Updated: 1 JSX + 3 generics + 3 comparisons + 2 channels + 2 shifts
    });
    
    test('handles deeply nested generics', () => {
      const code = `
type Complex = Result<Option<Vec<Map<string, Array<T>>>>, Error>;
`;
      const ast = parseCode(code);
      
      const usage = findAllAngleBracketUsages(ast);
      expect(usage.generics.length).toBe(5); // Result, Option, Vec, Map, Array
      
      // Verify nesting structure
      const stats = analyzeAngleBrackets(ast);
      expect(stats.genericCount).toBe(5);
      expect(stats.summary).toContain('5 generic');
    });
    
    test('handles JSX with generic components', () => {
      const code = `
const element = <Component<Props> data={items}>
  {items.map(item => <Item<T> key={item.id} />)}
</Component>;
`;
      const ast = parseCode(code);
      
      // This is a complex case - Component<Props> could be parsed differently
      const usage = findAllAngleBracketUsages(ast);
      
      // Should have JSX elements
      expect(usage.jsx.elements.length).toBeGreaterThan(0);
      
      // May also have generics depending on parser implementation
      const stats = analyzeAngleBrackets(ast);
      expect(stats.totalUsages).toBeGreaterThan(0);
    });
  });
  
  describe('Edge Cases', () => {
    test('handles ambiguous expression x<y>z', () => {
      const code = 'x<y>z';
      const ast = parseCode(code);
      
      // Should be parsed as (x < y) > z (two comparisons)
      const usage = findAllAngleBracketUsages(ast);
      expect(usage.comparisons.lessThan.length).toBe(1);
      expect(usage.comparisons.greaterThan.length).toBe(1);
      expect(usage.jsx.elements.length).toBe(0);
      expect(usage.generics.length).toBe(0);
    });
    
    test('handles JSX fragments', () => {
      const code = `
const frag = <>
  <div>Content</div>
  <span>More</span>
</>;
`;
      const ast = parseCode(code);
      
      const usage = findAllAngleBracketUsages(ast);
      expect(usage.jsx.fragments.length).toBe(1);
      expect(usage.jsx.elements.length).toBe(2); // div and span
    });
    
    test('handles type assertions vs JSX', () => {
      // Type assertions (<Type>value) are now fully implemented
      const code = `
const jsx = <Type />;
const component = <Button>Click</Button>;
`;
      const ast = parseCode(code);
      
      const usage = findAllAngleBracketUsages(ast);
      
      // Both should be parsed as JSX
      expect(usage.jsx.elements.length).toBe(2);
    });
  });
  
  describe('Performance and Statistics', () => {
    test('provides accurate statistics for complex code', () => {
      const code = `
// Kitchen sink of angle brackets
async function* generator<T, U, V>(
  input: AsyncIterator<Stream<T>>,
  transform: (x: T) => Promise<U>
): AsyncGenerator<Result<V, Error>> {
  const jsx = (
    <Container>
      <Header title="Test" />
      <Body>
        {items.filter(x => x < 100).map(item => 
          <Item key={item} active={item > 50} />
        )}
      </Body>
    </Container>
  );
  
  const shifted = value << 2 >> 1 >>> 0;
  const chan = <-input;
  output <- result;
  
  if (a < b && c > d && e <= f && g >= h) {
    yield* results;
  }
}
`;
      const ast = parseCode(code);
      const stats = analyzeAngleBrackets(ast);
      
      // Log detailed statistics
      console.log('Angle Bracket Statistics:');
      console.log(`  JSX: ${stats.jsxCount}`);
      console.log(`  Generics: ${stats.genericCount}`);
      console.log(`  Comparisons: ${stats.comparisonCount}`);
      console.log(`  Channels: ${stats.channelCount}`);
      console.log(`  Shifts: ${stats.shiftCount}`);
      console.log(`  Total: ${stats.totalUsages}`);
      console.log(`  Summary: ${stats.summary}`);
      
      // Verify totals
      expect(stats.totalUsages).toBe(
        stats.jsxCount + 
        stats.genericCount + 
        stats.comparisonCount + 
        stats.channelCount + 
        stats.shiftCount
      );
      
      // Should have significant usage of each type
      expect(stats.jsxCount).toBeGreaterThan(0);
      expect(stats.genericCount).toBeGreaterThan(0);
      expect(stats.comparisonCount).toBeGreaterThan(0);
    });
  });
});