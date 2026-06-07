import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import { parseCode } from './helpers';

describe('Parser', () => {
  describe('Declarations', () => {
    test('parses variable declarations', () => {
      const ast = parseCode('let x = 42;');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.VarDecl;
      expect(decl.kind).toBe('VarDecl');
      expect(decl.names[0].name).toBe('x');
      expect(decl.values?.[0].kind).toBe('NumericLiteral');
    });

    test('parses const declarations', () => {
      const ast = parseCode('const PI = 3.14;');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ConstDecl;
      expect(decl.kind).toBe('ConstDecl');
      expect(decl.names[0].name).toBe('PI');
    });

    test('parses short declarations', () => {
      const ast = parseCode('x := 10;');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ShortDecl;
      expect(decl.kind).toBe('ShortDecl');
      expect(decl.pairs![0].name.name).toBe('x');
    });

    test('parses function declarations', () => {
      const ast = parseCode('def add(a: int, b: int) -> int { return a + b; }');
      expect(ast.body).toHaveLength(1);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.kind).toBe('FuncDecl');
      expect(func.name.name).toBe('add');
      expect(func.params).toHaveLength(2);
      expect(func.returnType?.kind).toBe('SimpleType');
    });

    test('parses async function declarations', () => {
      const ast = parseCode('async function fetch() { return data; }');
      expect(ast.body).toHaveLength(1);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.async).toBe(true);
    });

    test('keeps async with statements inside async Python functions', () => {
      const ast = parseCode(`async def read_http_lines(url):
  async with httpx.AsyncClient() as client:
    await asyncio.sleep(0)
    return response.text.splitlines()
`);
      expect(ast.body).toHaveLength(1);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.kind).toBe('FuncDecl');
      expect(func.async).toBe(true);
      expect(func.body.statements).toHaveLength(1);
      const using = func.body.statements[0] as AST.Using;
      expect(using.kind).toBe('Using');
      expect(using.async).toBe(true);
      const resource = using.resource as AST.VarDecl;
      expect(resource.kind).toBe('VarDecl');
      expect(resource.names[0].name).toBe('client');
      expect(resource.values?.[0].kind).toBe('Call');
      expect(using.body.statements.map(stmt => stmt.kind)).toEqual(['ExprStmt', 'Return']);
    });

    test('parses import statements', () => {
      const ast = parseCode('import "module" as mod;');
      expect(ast.body).toHaveLength(1);
      const imp = ast.body[0] as AST.Import;
      expect(imp.kind).toBe('Import');
      expect(imp.path).toBe('module');
      expect(imp.alias?.name).toBe('mod');
    });

    test('parses type declarations', () => {
      const ast = parseCode('type ID = string;');
      expect(ast.body).toHaveLength(1);
      const type = ast.body[0] as AST.TypeDecl;
      expect(type.kind).toBe('TypeDecl');
      expect(type.name.name).toBe('ID');
    });
  });

  describe('Expressions', () => {
    test('parses numeric literals', () => {
      const ast = parseCode('42;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.kind).toBe('NumericLiteral');
      expect(num.raw).toBe('42');
    });

    test('parses string literals', () => {
      const ast = parseCode('"hello";');
      const stmt = ast.body[0] as AST.ExprStmt;
      const str = stmt.expr as AST.StringLiteral;
      expect(str.kind).toBe('StringLiteral');
    });

    test('parses binary expressions', () => {
      const ast = parseCode('1 + 2 * 3;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const expr = stmt.expr as AST.Binary;
      expect(expr.kind).toBe('Binary');
      expect(expr.op).toBe('+');
      
      // Check precedence: * should bind tighter than +
      const right = expr.right as AST.Binary;
      expect(right.kind).toBe('Binary');
      expect(right.op).toBe('*');
    });

    test('parses assignment expressions', () => {
      const ast = parseCode('x = 42;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const assign = stmt.expr as AST.Assign;
      expect(assign.kind).toBe('Assign');
      expect(assign.op).toBe('=');
    });

    test('parses ternary expressions', () => {
      const ast = parseCode('x ? y : z;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const ternary = stmt.expr as AST.Ternary;
      expect(ternary.kind).toBe('Ternary');
    });

    test('parses function calls', () => {
      const ast = parseCode('foo(1, 2, 3);');
      const stmt = ast.body[0] as AST.ExprStmt;
      const call = stmt.expr as AST.Call;
      expect(call.kind).toBe('Call');
      expect(call.args).toHaveLength(3);
    });

    test('parses member access', () => {
      const ast = parseCode('obj.prop;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const member = stmt.expr as AST.Member;
      expect(member.kind).toBe('Member');
      expect(member.property.name).toBe('prop');
    });

    test('parses optional chaining', () => {
      const ast = parseCode('obj?.prop;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const member = stmt.expr as AST.Member;
      expect(member.optional).toBe(true);
    });

    test('parses array literals', () => {
      const ast = parseCode('[1, 2, 3];');
      const stmt = ast.body[0] as AST.ExprStmt;
      const arr = stmt.expr as AST.ArrayLiteral;
      expect(arr.kind).toBe('ArrayLiteral');
      expect(arr.elements).toHaveLength(3);
    });

    test('parses object literals', () => {
      const ast = parseCode('({ x: 1, y: 2 });');
      const stmt = ast.body[0] as AST.ExprStmt;
      const obj = stmt.expr as AST.ObjectLiteral;
      expect(obj.kind).toBe('ObjectLiteral');
      expect(obj.properties).toHaveLength(2);
    });

    test('parses lambda expressions', () => {
      const ast = parseCode('(x, y) => x + y;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const lambda = stmt.expr as AST.Lambda;
      expect(lambda.kind).toBe('Lambda');
      expect(lambda.params).toHaveLength(2);
    });

    test('parses unary expressions', () => {
      const ast = parseCode('!true;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const unary = stmt.expr as AST.Unary;
      expect(unary.kind).toBe('Unary');
      expect(unary.op).toBe('!');
      expect(unary.prefix).toBe(true);
    });
  });

  describe('Statements', () => {
    test('parses if statements', () => {
      const ast = parseCode('if (x > 0) { y = 1; }');
      const ifStmt = ast.body[0] as AST.If;
      expect(ifStmt.kind).toBe('If');
      expect(ifStmt.arms).toHaveLength(1);
    });

    test('parses if-else statements', () => {
      const ast = parseCode('if (x > 0) { y = 1; } else { y = 0; }');
      const ifStmt = ast.body[0] as AST.If;
      expect(ifStmt.elseBody).toBeDefined();
    });

    test('parses elif chains', () => {
      const ast = parseCode('if (x > 0) { y = 1; } elif (x < 0) { y = -1; } else { y = 0; }');
      const ifStmt = ast.body[0] as AST.If;
      expect(ifStmt.arms).toHaveLength(2);
      expect(ifStmt.elseBody).toBeDefined();
    });

    test('parses for loops', () => {
      const ast = parseCode('for (let i = 0; i < 10; i++) { print(i); }');
      const loop = ast.body[0] as AST.Loop;
      expect(loop.kind).toBe('Loop');
      expect(loop.mode).toBe('for');
      expect(loop.init).toBeDefined();
      expect(loop.test).toBeDefined();
      expect(loop.step).toBeDefined();
    });

    test('parses while loops', () => {
      const ast = parseCode('while (x > 0) { x--; }');
      const loop = ast.body[0] as AST.Loop;
      expect(loop.mode).toBe('while');
      expect(loop.test).toBeDefined();
    });

    test('parses foreach loops', () => {
      const ast = parseCode('foreach item in items { print(item); }');
      const loop = ast.body[0] as AST.Loop;
      expect(loop.mode).toBe('foreach');
      expect(loop.variable?.kind === 'Identifier' && loop.variable.name).toBe('item');
    });

    test('preserves Python async for loops as awaited foreach loops', () => {
      const ast = parseCode(`async def consume_rows(rows):
  async for row in rows:
    await process(row)
`);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.kind).toBe('FuncDecl');
      expect(func.async).toBe(true);
      const loop = func.body.statements[0] as AST.Loop;
      expect(loop.kind).toBe('Loop');
      expect(loop.mode).toBe('foreach');
      expect(loop.await).toBe(true);
      expect(loop.variable?.kind === 'Identifier' && loop.variable.name).toBe('row');
      expect(loop.iterable?.kind).toBe('Identifier');
    });

    test('parses switch statements', () => {
      const ast = parseCode(`
        switch (x) {
          case 1: y = "one";
          case 2: y = "two";
          default: y = "other";
        }
      `);
      const switchStmt = ast.body[0] as AST.Switch;
      expect(switchStmt.kind).toBe('Switch');
      expect(switchStmt.cases).toHaveLength(2);
      expect(switchStmt.defaultCase).toBeDefined();
    });

    test('parses try-catch statements', () => {
      const ast = parseCode(`
        try {
          risky();
        } catch (e) {
          handle(e);
        }
      `);
      const tryStmt = ast.body[0] as AST.Try;
      expect(tryStmt.kind).toBe('Try');
      expect(tryStmt.catches).toHaveLength(1);
      expect(tryStmt.catches[0].param?.name).toBe('e');
    });

    test('parses try-catch-finally statements', () => {
      const ast = parseCode(`
        try {
          risky();
        } catch (e) {
          handle(e);
        } finally {
          cleanup();
        }
      `);
      const tryStmt = ast.body[0] as AST.Try;
      expect(tryStmt.finallyBody).toBeDefined();
    });

    test('parses return statements', () => {
      const ast = parseCode('return 42;');
      const ret = ast.body[0] as AST.Return;
      expect(ret.kind).toBe('Return');
      expect(ret.values).toHaveLength(1);
    });

    test('parses break statements', () => {
      const ast = parseCode('break;');
      const brk = ast.body[0] as AST.Break;
      expect(brk.kind).toBe('Break');
    });

    test('parses continue statements', () => {
      const ast = parseCode('continue;');
      const cont = ast.body[0] as AST.Continue;
      expect(cont.kind).toBe('Continue');
    });

    test('parses echo/print statements', () => {
      const ast = parseCode('echo "Hello", "World";');
      const echo = ast.body[0] as AST.Echo;
      expect(echo.kind).toBe('Echo');
      expect(echo.values).toHaveLength(2);
    });

    test('parses using statements', () => {
      const ast = parseCode('using file = open("test.txt") { read(file); }');
      const using = ast.body[0] as AST.Using;
      expect(using.kind).toBe('Using');
    });

    test('parses defer statements', () => {
      const ast = parseCode('defer { cleanup(); }');
      const defer = ast.body[0] as AST.Defer;
      expect(defer.kind).toBe('Defer');
    });
  });

  describe('Types', () => {
    test('parses simple types', () => {
      const ast = parseCode('let x: string;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.SimpleType;
      expect(type.kind).toBe('SimpleType');
      expect(type.id.name).toBe('string');
    });

    test('parses nullable types', () => {
      const ast = parseCode('let x: string?;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.NullableType;
      expect(type.kind).toBe('NullableType');
    });

    test('parses union types', () => {
      const ast = parseCode('let x: string | number;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.UnionType;
      expect(type.kind).toBe('UnionType');
      expect(type.types).toHaveLength(2);
    });

    test('parses generic types', () => {
      const ast = parseCode('let x: Array<string>;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.GenericType;
      expect(type.kind).toBe('GenericType');
      expect(type.base.name).toBe('Array');
      expect(type.args).toHaveLength(1);
    });

    test('parses function types', () => {
      const ast = parseCode('let f: (a: number, b: number) => number;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.FuncType;
      expect(type.kind).toBe('FuncType');
      expect(type.params).toHaveLength(2);
    });
  });

  describe('Special Features', () => {
    test('parses sigil identifiers', () => {
      const ast = parseCode('$var = 42;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const assign = stmt.expr as AST.Assign;
      const id = assign.left as AST.Identifier;
      expect(id.name).toBe('var');
      expect(id.originalSpelling).toBe('$var');
    });

    test('handles MASI (semicolon insertion)', () => {
      const ast = parseCode(`
        let x = 1
        let y = 2
      `);
      expect(ast.body).toHaveLength(2);
    });

    test('parses regex literals', () => {
      const ast = parseCode('/[a-z]+/gi;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const regex = stmt.expr as AST.RegexLiteral;
      expect(regex.kind).toBe('RegexLiteral');
      expect(regex.pattern).toBe('[a-z]+');
      expect(regex.flags).toBe('gi');
    });

    test('parses template literals', () => {
      const ast = parseCode('`Hello ${name}`;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const template = stmt.expr as AST.StringLiteral;
      expect(template.kind).toBe('StringLiteral');
      expect(template.delimiter).toBe('`');
    });

    test('handles operator precedence correctly', () => {
      const ast = parseCode('a + b * c ** d;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const add = stmt.expr as AST.Binary;
      expect(add.op).toBe('+');
      
      const mul = add.right as AST.Binary;
      expect(mul.op).toBe('*');
      
      const pow = mul.right as AST.Binary;
      expect(pow.op).toBe('**');
    });

    test('parses reassignment operator', () => {
      const ast = parseCode('x :=: 42;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const assign = stmt.expr as AST.Assign;
      expect(assign.op).toBe(':=:');
    });
  });

  describe('Error Recovery', () => {
    test('recovers from missing semicolon', () => {
      const lexer = new Lexer('let x = 1 let y = 2');
      const tokens = lexer.tokenize();
      const parser = new Parser(tokens);
      const ast = parser.parse();
      
      expect(ast.body).toHaveLength(2);
    });

    test('reports parse errors', () => {
      const lexer = new Lexer('let x =');
      const tokens = lexer.tokenize();
      const parser = new Parser(tokens);
      const ast = parser.parse();
      
      expect(parser.getErrors()).not.toHaveLength(0);
    });
  });
});
