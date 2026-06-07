/**
 * Main Test Helpers Export
 * 
 * Combines all test utilities for easy import in tests
 */

export * from './ast-verifiers';
export * from './pattern-matchers';

// Re-export verifiers from ast-verifiers
export {
  verifyJSXElement,
  verifyGenericType,
  verifyBinaryOp,
  verifyComparison,
  verifyChannelSend,
  verifyChannelReceive,
  verifyAngleBrackets,
  verifyFunctionDecl
} from './ast-verifiers';

// Re-export pattern matchers
export {
  findJSXElements,
  findGenericTypes,
  findComparisons,
  findChannelOperations,
  findAllAngleBracketUsages,
  analyzeAngleBrackets,
  
  // Type guards
  isJSXElement,
  isGenericType,
  isComparison,
  isChannelSend,
  isChannelReceive,
  
  // Generic finders
  findByKind,
  findFirst
} from './pattern-matchers';

import { Lexer } from '../../src/lexer';
import { Parser } from '../../src/parser';
import * as AST from '../../src/ast';
import { RuntimeResolver } from '../../src/runtime-resolver';
import { ManifestCodeGenerator } from '../../src/codegen-omnivm';

/**
 * Helper to parse code and return AST
 * Also runs the full manifest codegen pipeline as a smoke test when parsing succeeds cleanly.
 * If parser has errors, codegen is skipped (those tests are testing error recovery).
 */
export function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();

  // If parser had errors, skip codegen (those tests are testing error recovery)
  const errors = parser.getErrors();
  if (errors.length === 0) {
    // Smoke-test the manifest codegen pipeline
    const resolver = new RuntimeResolver();
    const annotated = resolver.resolve(ast, code);
    const gen = new ManifestCodeGenerator();
    const manifest = gen.generate(annotated);
    JSON.stringify(manifest); // verify JSON-serializable
  } else {
    const fs = require('fs');
    const line = JSON.stringify({
      n: errors.length,
      msg: errors.map((e: any) => `${e.message} @${e.token?.line}:${e.token?.column}`).slice(0, 5),
      code: code.replace(/\n/g, '\\n').slice(0, 250)
    }) + '\n';
    fs.appendFileSync('/tmp/polyscript-diag.jsonl', line);
  }

  return ast;
}

/**
 * Strict variant that throws on parser errors (for error-detection tests)
 */
export function parseCodeStrict(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();

  const errors = parser.getErrors();
  if (errors.length > 0) {
    const errorMessages = errors.map((e: any) =>
      `${e.message} at ${e.token?.line || 'unknown'}:${e.token?.column || 'unknown'}`
    ).join('\n  ');
    throw new Error(`Parser produced ${errors.length} error(s):\n  ${errorMessages}`);
  }

  return ast;
}

/**
 * Helper to parse code allowing errors (for error testing)
 */
export function parseCodeWithErrors(code: string): { ast: AST.Program; errors: any[] } {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  const errors = parser.getErrors();
  return { ast, errors };
}

/**
 * Helper to parse and analyze in one step
 */
export function parseAndAnalyze(code: string) {
  const ast = parseCode(code);
  const { analyzeAngleBrackets } = require('./pattern-matchers');
  const stats = analyzeAngleBrackets(ast);
  return { ast, stats };
}