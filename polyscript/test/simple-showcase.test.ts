import { describe, test, expect } from '@jest/globals';
import { Lexer, TokenType } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Simple Showcase Test', () => {
  test('parses simple async function', () => {
    const code = `
async function test() {
  return 42
}`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });
  
  test('parses function with string concat', () => {
    const code = `
function test() {
  throw new Error("Failed: " + message)
}`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });
});