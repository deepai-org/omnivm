import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Comprehensive Parser Tests', () => {
  describe('Numeric Literals', () => {
    test('parses hex literals', () => {
      const ast = parseCode('0xFF;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.kind).toBe('NumericLiteral');
      expect(num.raw).toBe('0xFF');
      expect(num.base).toBe('hex');
    });

    test('parses octal literals', () => {
      const ast = parseCode('0o777;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.base).toBe('octal');
    });

    test('parses binary literals', () => {
      const ast = parseCode('0b1010;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.base).toBe('binary');
    });

    test('parses numbers with underscores', () => {
      const ast = parseCode('1_000_000;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.raw).toBe('1_000_000');
    });

    test('parses bigint literals', () => {
      const ast = parseCode('42n;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.suffix).toBe('n');
    });

    test('parses float with exponent', () => {
      const ast = parseCode('1.5e10;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.raw).toBe('1.5e10');
    });

    test('parses numbers with type suffixes', () => {
      const ast = parseCode('42L;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const num = stmt.expr as AST.NumericLiteral;
      expect(num.suffix).toBe('L');
    });
  });

  describe('String Literals', () => {
    test('parses single-quoted strings', () => {
      const ast = parseCode("'hello';");
      const stmt = ast.body[0] as AST.ExprStmt;
      const str = stmt.expr as AST.StringLiteral;
      expect(str.kind).toBe('StringLiteral');
    });

    test('parses triple-quoted strings', () => {
      const ast = parseCode('"""multi\nline""";');
      const stmt = ast.body[0] as AST.ExprStmt;
      const str = stmt.expr as AST.StringLiteral;
      expect(str.delimiter).toContain('"');
    });

    test('parses raw strings', () => {
      const ast = parseCode('r"\\n is literal";');
      const stmt = ast.body[0] as AST.ExprStmt;
      const str = stmt.expr as AST.StringLiteral;
      expect(str.flags.raw).toBe(true);
    });

    test('parses byte strings', () => {
      const ast = parseCode('b"bytes";');
      const stmt = ast.body[0] as AST.ExprStmt;
      const str = stmt.expr as AST.StringLiteral;
      expect(str.flags.bytes).toBe(true);
    });

    test('parses format strings', () => {
      const ast = parseCode('f"value: ${x}";');
      const stmt = ast.body[0] as AST.ExprStmt;
      const str = stmt.expr as AST.StringLiteral;
      expect(str.flags.format).toBe(true);
    });
  });

  describe('Operators', () => {
    test('parses index access', () => {
      const ast = parseCode('arr[0];');
      const stmt = ast.body[0] as AST.ExprStmt;
      const index = stmt.expr as AST.Index;
      expect(index.kind).toBe('Index');
    });

    test('parses postfix increment', () => {
      const ast = parseCode('x++;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const unary = stmt.expr as AST.Unary;
      expect(unary.kind).toBe('Unary');
      expect(unary.op).toBe('++');
      expect(unary.prefix).toBe(false);
    });

    test('parses prefix increment', () => {
      const ast = parseCode('++x;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const unary = stmt.expr as AST.Unary;
      expect(unary.op).toBe('++');
      expect(unary.prefix).toBe(true);
    });

    test('parses typeof operator', () => {
      const ast = parseCode('typeof x;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const unary = stmt.expr as AST.Unary;
      expect(unary.op).toBe('typeof');
    });

    test('parses void operator', () => {
      const ast = parseCode('void 0;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const unary = stmt.expr as AST.Unary;
      expect(unary.op).toBe('void');
    });

    test('parses delete operator', () => {
      const ast = parseCode('delete obj.prop;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const unary = stmt.expr as AST.Unary;
      expect(unary.op).toBe('delete');
    });

    test('parses await expression', () => {
      const ast = parseCode('await promise;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const unary = stmt.expr as AST.Unary;
      expect(unary.op).toBe('await');
    });

    test('parses in operator', () => {
      const ast = parseCode('x in obj;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const binary = stmt.expr as AST.Binary;
      expect(binary.op).toBe('in');
    });

    test('parses instanceof operator', () => {
      const ast = parseCode('x instanceof Array;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const binary = stmt.expr as AST.Binary;
      expect(binary.op).toBe('instanceof');
    });

    test('parses null coalescing operator', () => {
      const ast = parseCode('x ?? y;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const binary = stmt.expr as AST.Binary;
      expect(binary.op).toBe('??');
    });

    test('parses compound assignments', () => {
      const ast = parseCode('x += 1;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const assign = stmt.expr as AST.Assign;
      expect(assign.op).toBe('+=');
    });

    test('parses bitwise operators', () => {
      const ast = parseCode('x & y | z ^ ~w;');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBe('Binary');
    });

    test('parses shift operators', () => {
      const ast = parseCode('x << 2 >> 1 >>> 0;');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBe('Binary');
    });

    test('parses exponentiation', () => {
      const ast = parseCode('2 ** 3;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const binary = stmt.expr as AST.Binary;
      expect(binary.op).toBe('**');
    });

    test('parses regex match operator', () => {
      const ast = parseCode('str =~ /pattern/;');
      const stmt = ast.body[0] as AST.ExprStmt;
      const binary = stmt.expr as AST.Binary;
      expect(binary.op).toBe('=~');
    });
  });

  describe('Declarations', () => {
    test('parses auto variables', () => {
      const ast = parseCode('auto x = 42;');
      expect(ast.body[0].kind).toBe('VarDecl');
    });

    test('parses final constants', () => {
      const ast = parseCode('final PI = 3.14;');
      expect(ast.body[0].kind).toBe('ConstDecl');
    });

    test('parses immutable constants', () => {
      const ast = parseCode('immutable VALUE = 100;');
      expect(ast.body[0].kind).toBe('ConstDecl');
    });

    test('parses unsafe functions', () => {
      const ast = parseCode('unsafe function danger() {}');
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.unsafe).toBe(true);
    });

    test('parses multiple variable declarations', () => {
      const ast = parseCode('let x, y, z;');
      const decl = ast.body[0] as AST.VarDecl;
      expect(decl.names).toHaveLength(3);
    });

    test('parses variable declarations with type annotations', () => {
      const ast = parseCode('let x: number = 42;');
      const decl = ast.body[0] as AST.VarDecl;
      expect(decl.type).toBeDefined();
    });

    test('parses require imports', () => {
      const ast = parseCode('require "module";');
      const imp = ast.body[0] as AST.Import;
      expect(imp.kind).toBe('Import');
    });

    test('parses import with alias', () => {
      const ast = parseCode('import "module" as mod;');
      const imp = ast.body[0] as AST.Import;
      expect(imp.alias?.name).toBe('mod');
    });

    test('parses class declarations', () => {
      const ast = parseCode('class Point { x: number; y: number; }');
      const cls = ast.body[0] as AST.ClassDecl;
      expect(cls.kind).toBe('ClassDecl');
      expect(cls.name.name).toBe('Point');
    });

    test('parses interface declarations', () => {
      const ast = parseCode('interface Shape { area(): number; }');
      const iface = ast.body[0] as AST.InterfaceDecl;
      expect(iface.kind).toBe('InterfaceDecl');
    });

    test('parses enum declarations', () => {
      const ast = parseCode('enum Color { Red, Green, Blue }');
      const enumDecl = ast.body[0] as AST.EnumDecl;
      expect(enumDecl.kind).toBe('EnumDecl');
      expect(enumDecl.members).toHaveLength(3);
    });
  });

  describe('Statements', () => {
    test('parses until loops', () => {
      const ast = parseCode('until (x > 10) { x++; }');
      const loop = ast.body[0] as AST.Loop;
      expect(loop.mode).toBe('until');
    });

    test('parses infinite loops', () => {
      const ast = parseCode('loop { if (done) break; }');
      const loop = ast.body[0] as AST.Loop;
      expect(loop.mode).toBe('infinite');
    });

    test('parses for-in loops', () => {
      const ast = parseCode('for x in items { print(x); }');
      const loop = ast.body[0] as AST.Loop;
      expect(loop.mode).toBe('foreach');
    });

    test('parses labeled break', () => {
      const ast = parseCode('break outer;');
      const brk = ast.body[0] as AST.Break;
      expect(brk.label?.name).toBe('outer');
    });

    test('parses labeled continue', () => {
      const ast = parseCode('continue loop;');
      const cont = ast.body[0] as AST.Continue;
      expect(cont.label?.name).toBe('loop');
    });

    test('parses match statements', () => {
      const ast = parseCode('match x { case 1 => "one" case 2 => "two" }');
      const stmt = ast.body[0] as any; // Can be Match now
      expect(stmt.kind).toBe('Match');
      expect(stmt.arms).toHaveLength(2);
    });

    test('parses with statements', () => {
      const ast = parseCode('with resource { use(resource); }');
      const withStmt = ast.body[0] as AST.Using;
      expect(withStmt.kind).toBe('Using');
    });

    test('parses except as catch alias', () => {
      const ast = parseCode('try { risky(); } except (e) { handle(e); }');
      const tryStmt = ast.body[0] as AST.Try;
      expect(tryStmt.catches).toHaveLength(1);
    });

    test('parses rescue as catch alias', () => {
      const ast = parseCode('try { risky(); } rescue (e) { handle(e); }');
      const tryStmt = ast.body[0] as AST.Try;
      expect(tryStmt.catches).toHaveLength(1);
    });
  });

  describe('Types', () => {
    test('parses integer types', () => {
      const ast = parseCode('let x: i32;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.SimpleType;
      expect(type.id.name).toBe('i32');
    });

    test('parses unsigned types', () => {
      const ast = parseCode('let x: u64;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.SimpleType;
      expect(type.id.name).toBe('u64');
    });

    test('parses float types', () => {
      const ast = parseCode('let x: f32;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.SimpleType;
      expect(type.id.name).toBe('f32');
    });

    test('parses any type', () => {
      const ast = parseCode('let x: any;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.SimpleType;
      expect(type.id.name).toBe('any');
    });

    test('parses never type', () => {
      const ast = parseCode('let x: never;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.SimpleType;
      expect(type.id.name).toBe('never');
    });

    test('parses bytes type', () => {
      const ast = parseCode('let x: bytes;');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.SimpleType;
      expect(type.id.name).toBe('bytes');
    });

    test('parses channel types', () => {
      const ast = parseCode('let ch: chan<string>;');
      const decl = ast.body[0] as AST.VarDecl;
      
      // Normalize the type (GenericType chan<T> or ChanType)
      const { normalizeChannelType } = require('./helpers/ast-compat');
      const type = normalizeChannelType(decl.type) as AST.ChanType;
      expect(type.kind).toBe('ChanType');
      expect(type.direction).toBe('both');
      // elementType is now properly parsed as part of GenericType args
      expect(type.elementType).toBeDefined();
    });

    test('parses array type with brackets', () => {
      const ast = parseCode('let arr: Array[number];');
      const decl = ast.body[0] as AST.VarDecl;
      const type = decl.type as AST.GenericType;
      expect(type.base.name).toBe('Array');
    });
  });

  describe('Special Features', () => {
    test('handles shebang', () => {
      const ast = parseCode('#!/usr/bin/env polyscript\nlet x = 1;');
      expect(ast.body).toHaveLength(1);
    });

    test('parses undefined as null', () => {
      const ast = parseCode('undefined;');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBe('NullLiteral');
    });

    test('parses backtick identifiers', () => {
      const ast = parseCode('let `class` = 1;');
      const decl = ast.body[0] as AST.VarDecl;
      expect(decl.names[0].name).toBe('class');
    });

    test('parses new expressions', () => {
      const ast = parseCode('new Array(10);');
      const stmt = ast.body[0] as AST.ExprStmt;
      const expr = stmt.expr as AST.NewExpr;
      expect(expr.kind).toBe('NewExpr');
      expect((expr.callee as AST.Identifier).name).toBe('Array');
      expect(expr.args).toHaveLength(1);
    });

    test('parses new expressions before chained member calls', () => {
      const ast = parseCode('new com.example.Widget().render();');
      const stmt = ast.body[0] as AST.ExprStmt;
      const call = stmt.expr as AST.Call;
      expect(call.kind).toBe('Call');
      expect(call.callee.kind).toBe('Member');
      const member = call.callee as AST.Member;
      expect(member.property.name).toBe('render');
      expect(member.object.kind).toBe('NewExpr');
      const newExpr = member.object as AST.NewExpr;
      expect(newExpr.callee.kind).toBe('Member');
    });

    test('parses generic constructor type arguments on NewExpr', () => {
      const ast = parseCode('new Map<string, number>();');
      const stmt = ast.body[0] as AST.ExprStmt;
      const expr = stmt.expr as AST.NewExpr;
      expect(expr.kind).toBe('NewExpr');
      expect(expr.typeArgs).toBeDefined();
      expect(expr.typeArgs).toHaveLength(2);
    });

    test('parses parenthesized constructor callee', () => {
      const ast = parseCode('new (factory())();');
      const stmt = ast.body[0] as AST.ExprStmt;
      const expr = stmt.expr as AST.NewExpr;
      expect(expr.kind).toBe('NewExpr');
      expect(expr.callee.kind).toBe('Call');
      expect(expr.args).toHaveLength(0);
    });

    test('handles multiple statements on one line', () => {
      const ast = parseCode('x = 1; y = 2; z = 3;');
      expect(ast.body).toHaveLength(3);
    });

    test('parses go expression', () => {
      const ast = parseCode('go doWork();');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBeDefined();
    });

    test('parses yield expression', () => {
      const ast = parseCode('yield value;');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBeDefined();
    });

    test('parses throw statement', () => {
      const ast = parseCode('throw new Error("oops");');
      expect(ast.body[0].kind).toBeDefined();
    });

    test('parses export statement', () => {
      const ast = parseCode('export function foo() {}');
      expect(ast.body[0].kind).toBeDefined();
    });

    test('parses package declaration', () => {
      const ast = parseCode('package main;');
      expect(ast.body[0].kind).toBeDefined();
    });
  });

  describe('Block Delimiters', () => {
    test('parses indent-based blocks', () => {
      const ast = parseCode('if x > 0:\n  y = 1\n  z = 2');
      const ifStmt = ast.body[0] as AST.If;
      expect(ifStmt.arms[0].body.statements).toHaveLength(2);
    });

    test('parses keyword blocks with do/done', () => {
      const ast = parseCode('while true do\n  echo "loop"\ndone');
      const loop = ast.body[0] as AST.Loop;
      expect(loop.body.statements).toHaveLength(1);
    });

    test('parses keyword blocks with begin/end', () => {
      const ast = parseCode('begin\n  x = 1\nend');
      expect(ast.body[0].kind).toBe('Block');
    });

    test('parses bash-style if/fi', () => {
      const ast = parseCode('if [ "$x" = "1" ]; then\n  echo "one"\nfi');
      const ifStmt = ast.body[0] as AST.If;
      expect(ifStmt.kind).toBe('If');
    });

    test('parses case/esac', () => {
      const ast = parseCode('case $x in\n  1) echo "one";;\n  2) echo "two";;\nesac');
      const switchStmt = ast.body[0] as AST.Switch;
      expect(switchStmt.kind).toBe('Switch');
    });
  });

  describe('Comments', () => {
    test('handles HTML comments', () => {
      const ast = parseCode('<!-- comment -->\nx = 1;');
      expect(ast.body).toHaveLength(1);
    });

    test('handles hash comments', () => {
      const ast = parseCode('# comment\nx = 1;');
      expect(ast.body).toHaveLength(1);
    });

    test('handles double-dash comments', () => {
      const ast = parseCode('-- comment\nx = 1;');
      expect(ast.body).toHaveLength(1);
    });
  });

  describe('Advanced Expressions', () => {
    test('parses spread operator', () => {
      const ast = parseCode('[...arr];');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBe('ArrayLiteral');
    });

    test('parses list comprehensions', () => {
      const ast = parseCode('[x * 2 for x in range(10)];');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBeDefined();
    });

    test('parses destructuring assignment', () => {
      const ast = parseCode('[a, b] = [1, 2];');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBe('Assign');
    });

    test('parses object destructuring', () => {
      const ast = parseCode('{x, y} = point;');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBeDefined();
    });

    test('parses channel send', () => {
      const ast = parseCode('ch <- value;');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBeDefined();
    });

    test('parses channel receive', () => {
      const ast = parseCode('<- ch;');
      const stmt = ast.body[0] as AST.ExprStmt;
      expect(stmt.expr.kind).toBeDefined();
    });
  });
});
