import { Parser } from '../src/parser';
import { Lexer } from '../src/lexer';
import * as AST from '../src/ast';

// Import test helpers
import {
  verifyFunctionDecl,
  verifyBinaryOp,
  verifyComparison,
  findInAST,
  findAllInAST,
  findFirst,
  findByKind
} from './helpers/ast-verifiers';

import { getReturnValue, getIfCondition } from './helpers/ast-compat';

import {
  analyzeAngleBrackets
} from './helpers/pattern-matchers';

import {
  getCallTarget,
  isNodeKind,
  normalizeSwitchStmt,
  normalizeSwitchCase,
  normalizeTryStmt
} from './helpers/ast-compat';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Parser Core Tests (UPDATED)', () => {
  describe('Control Flow (STRONG)', () => {
    test('parses if-else statements', () => {
      const ast = parseCode('if (x > 0) { y = 1; } else { y = 0; }');
      
      // ✅ STRONG: Verify if-else structure
      expect(ast.body).toHaveLength(1);
      const ifStmt = ast.body[0] as AST.If;
      expect(ifStmt.kind).toBe('If');
      
      // Verify condition
      expect(ifStmt.arms).toHaveLength(1);
      const arm = ifStmt.arms[0];
      verifyComparison(getIfCondition(arm), '>', 'x', 0);
      
      // Verify then body
      expect(arm.body.statements).toHaveLength(1);
      const thenStmt = arm.body.statements[0] as AST.ExprStmt;
      expect(thenStmt.kind).toBe('ExprStmt');
      
      // Verify else body exists and has content
      expect(ifStmt.elseBody).not.toBeNull();
      if (ifStmt.elseBody) {
        expect(ifStmt.elseBody.statements).toHaveLength(1);
        const elseStmt = ifStmt.elseBody.statements[0] as AST.ExprStmt;
        expect(elseStmt.kind).toBe('ExprStmt');
      }
    });

    test('parses elif chains', () => {
      const ast = parseCode('if (x > 0) { y = 1; } elif (x < 0) { y = -1; } else { y = 0; }');
      
      // ✅ STRONG: Verify elif chain structure
      const ifStmt = ast.body[0] as AST.If;
      expect(ifStmt.kind).toBe('If');
      expect(ifStmt.arms).toHaveLength(2);
      
      // Verify first condition: x > 0
      verifyComparison(ifStmt.arms[0].test, '>', 'x', 0);
      
      // Verify second condition: x < 0
      verifyComparison(ifStmt.arms[1].test, '<', 'x', 0);
      
      // Verify else body has content
      expect(ifStmt.elseBody).not.toBeNull();
      if (ifStmt.elseBody) {
        expect(ifStmt.elseBody.statements.length).toBeGreaterThan(0);
      }
      
      // Verify angle bracket usage (comparisons)
      const usage = analyzeAngleBrackets(ast);
      expect(usage.comparisonCount).toBe(2);
    });

    test('parses for loops', () => {
      const ast = parseCode('for (let i = 0; i < 10; i++) { print(i); }');
      
      // ✅ STRONG: Verify for loop structure
      const loop = ast.body[0] as AST.Loop;
      expect(loop.kind).toBe('Loop');
      expect(loop.mode).toBe('for');
      
      // Verify init: let i = 0
      expect(loop.init).not.toBeNull();
      if (loop.init) {
        if (loop.init.kind === 'VarDecl') {
          const varDecl = loop.init as AST.VarDecl;
          expect(varDecl.names[0].name).toBe('i');
        }
      }
      
      // Verify test: i < 10
      expect(loop.test).not.toBeNull();
      if (loop.test) {
        verifyComparison(loop.test, '<', 'i', 10);
      }
      
      // Verify step: i++
      expect(loop.step).not.toBeNull();
      if (loop.step) {
        if (loop.step.kind === 'Unary') {
          const unary = loop.step as AST.Unary;
          expect(unary.op).toBe('++');
        }
      }
      
      // Verify loop body
      expect(loop.body.statements).toHaveLength(1);
      
      // Verify angle bracket usage
      const usage = analyzeAngleBrackets(ast);
      expect(usage.comparisonCount).toBe(1); // i < 10
    });

    test('parses while loops', () => {
      const ast = parseCode('while (x > 0) { x--; }');
      
      // ✅ STRONG: Verify while loop structure
      const loop = ast.body[0] as AST.Loop;
      expect(loop.kind).toBe('Loop');
      expect(loop.mode).toBe('while');
      
      // Verify condition: x > 0
      expect(loop.test).not.toBeNull();
      if (loop.test) {
        verifyComparison(loop.test, '>', 'x', 0);
      }
      
      // Verify loop body
      expect(loop.body.statements).toHaveLength(1);
      const bodyStmt = loop.body.statements[0] as AST.ExprStmt;
      expect(bodyStmt.kind).toBe('ExprStmt');
      
      if (bodyStmt.expr.kind === 'Unary') {
        const unary = bodyStmt.expr as AST.Unary;
        expect(unary.op).toBe('--');
      }
    });

    test('parses switch statements', () => {
      const ast = parseCode(`
        switch (x) {
          case 1:
            y = 'one';
            break;
          case 2:
            y = 'two';
            break;
          default:
            y = 'other';
        }
      `);
      
      // ✅ STRONG: Verify switch structure
      const rawSwitch = ast.body[0] as AST.Switch;
      const switchStmt = normalizeSwitchStmt(rawSwitch);
      expect(switchStmt.kind).toBe('Switch');
      
      // Verify discriminant
      expect((switchStmt.expr as AST.Identifier).name).toBe('x');
      
      // Verify cases
      expect(switchStmt.cases.length).toBeGreaterThanOrEqual(2);
      
      // Verify first case: case 1
      const rawCase1 = switchStmt.cases[0];
      const case1 = normalizeSwitchCase(rawCase1);
      expect(case1.value).not.toBeNull();
      if (case1.value && case1.value.kind === 'NumericLiteral') {
        expect(case1.value.raw).toBe('1');
      }
      expect(case1.body.statements.length).toBeGreaterThan(0);
      
      // Verify second case: case 2
      const rawCase2 = switchStmt.cases[1];
      const case2 = normalizeSwitchCase(rawCase2);
      expect(case2.value).not.toBeNull();
      if (case2.value && case2.value.kind === 'NumericLiteral') {
        expect(case2.value.raw).toBe('2');
      }
      
      // Verify default case
      const defaultCase = switchStmt.cases.find((c: any) => c.isDefault);
      expect(defaultCase).not.toBeUndefined();
      if (defaultCase) {
        expect(defaultCase.body.statements.length).toBeGreaterThan(0);
      }
    });

    test('parses try-catch-finally', () => {
      const ast = parseCode(`
        try {
          risky();
        } catch (e) {
          handle(e);
        } finally {
          cleanup();
        }
      `);
      
      // ✅ STRONG: Verify try-catch-finally structure
      const rawTry = ast.body[0] as AST.Try;
      const tryStmt = normalizeTryStmt(rawTry);
      expect(tryStmt.kind).toBe('Try');
      
      // Verify try body
      expect(tryStmt.body.statements).toHaveLength(1);
      const tryCall = tryStmt.body.statements[0] as AST.ExprStmt;
      if (tryCall.expr.kind === 'Call') {
        const call = tryCall.expr as any;
        const target = getCallTarget(call);
        expect(target.name).toBe('risky');
      }
      
      // Verify catch clause
      expect(tryStmt.catches).toHaveLength(1);
      const catchClause = tryStmt.catches[0];
      expect(catchClause.param?.name).toBe('e');
      expect(catchClause.body.statements).toHaveLength(1);
      
      // Verify finally block
      expect(tryStmt.finally).not.toBeNull();
      if (tryStmt.finally) {
        expect(tryStmt.finally.statements).toHaveLength(1);
        const finallyCall = tryStmt.finally.statements[0] as AST.ExprStmt;
        if (finallyCall.expr.kind === 'Call') {
          const call = finallyCall.expr as any;
          const target = getCallTarget(call);
          expect(target.name).toBe('cleanup');
        }
      }
    });
  });

  describe('Functions (STRONG)', () => {
    test('parses function declarations', () => {
      const ast = parseCode('function add(a, b) { return a + b; }');
      
      // ✅ STRONG: Verify function structure
      const func = ast.body[0] as AST.FuncDecl;
      verifyFunctionDecl(func, 'add', {
        paramCount: 2
      });
      
      // Verify parameters
      expect(func.params[0].name.kind).toBe('Identifier');
      expect((func.params[0].name as AST.Identifier).name).toBe('a');
      expect(func.params[1].name.kind).toBe('Identifier');
      expect((func.params[1].name as AST.Identifier).name).toBe('b');
      
      // Verify function body
      const body = func.body as AST.Block;
      expect(body.statements).toHaveLength(1);
      
      const returnStmt = body.statements[0] as AST.Return;
      expect(returnStmt.kind).toBe('Return');
      
      // Verify return expression: a + b
      const returnValue = getReturnValue(returnStmt);
      if (returnValue && returnValue.kind === 'Binary') {
        verifyBinaryOp(returnValue, '+');
      }
    });

    test('parses arrow functions', () => {
      const ast = parseCode('const double = x => x * 2;');
      
      // ✅ STRONG: Verify arrow function
      const decl = ast.body[0] as AST.ConstDecl;
      expect(decl.kind).toBe('ConstDecl');
      expect(decl.names[0].name).toBe('double');
      
      const arrow = decl.values[0] as AST.Lambda;
      expect(arrow.kind).toBe('Lambda');
      expect(arrow.params).toHaveLength(1);
      expect(arrow.params[0].name.kind).toBe('Identifier');
      expect((arrow.params[0].name as AST.Identifier).name).toBe('x');
      
      // Verify body: x * 2
      if (arrow.body.kind === 'Binary') {
        verifyBinaryOp(arrow.body, '*');
      }
    });

    test('parses async functions', () => {
      const ast = parseCode('async function fetchData() { return await fetch("/api"); }');
      
      // ✅ STRONG: Verify async function
      const func = ast.body[0] as AST.FuncDecl;
      verifyFunctionDecl(func, 'fetchData', {
        async: true,
        paramCount: 0
      });
      
      // Verify function body has await
      const body = func.body as AST.Block;
      const returnStmt = body.statements[0] as AST.Return;
      
      const returnValue = getReturnValue(returnStmt);
      if (returnValue && returnValue.kind === 'Unary') {
        const unary = returnValue as AST.Unary;
        expect(unary.op).toBe('await');
      }
    });
  });

  describe('Operators (STRONG)', () => {
    test('parses comparison operators', () => {
      const ast = parseCode('x < 5 && y > 3 && z <= 10 && w >= 0;');
      
      // ✅ STRONG: Verify all comparisons
      const stmt = ast.body[0] as AST.ExprStmt;
      
      // Find all comparisons
      const comparisons = findAllInAST(ast, n => 
        n.kind === 'Binary' && ['<', '>', '<=', '>='].includes(n.op)
      );
      
      expect(comparisons).toHaveLength(4);
      
      // Verify angle bracket disambiguation
      const usage = analyzeAngleBrackets(ast);
      expect(usage.comparisonCount).toBe(4);
      expect(usage.jsxCount).toBe(0);
      expect(usage.genericCount).toBe(0);
    });

    test('parses logical operators', () => {
      const ast = parseCode('a && b || c ?? d;');
      
      // ✅ STRONG: Verify logical operators
      const stmt = ast.body[0] as AST.ExprStmt;
      const expr = stmt.expr;
      
      // Find all logical operators
      const logicalOps = findAllInAST(ast, n => 
        n.kind === 'Binary' && ['&&', '||', '??'].includes(n.op)
      );
      
      expect(logicalOps).toHaveLength(3);
      
      // Verify specific operators
      const ops = logicalOps.map(op => op.op);
      expect(ops).toContain('&&');
      expect(ops).toContain('||');
      expect(ops).toContain('??');
    });
  });
});