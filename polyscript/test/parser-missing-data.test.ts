import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Parser - Missing AST Data Storage', () => {
  
  describe('Import/Export Data', () => {
    
    test('stores destructured import specifiers', () => {
      const code = `import { foo, bar as baz, qux } from 'module';`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const importDecl = ast.body[0] as AST.ImportDecl;
      expect(importDecl.kind).toBe('ImportDecl');
      
      // Should store the module path
      expect(importDecl.path).toBe('module');
      
      // Should store import specifiers
      expect(importDecl.specifiers).toBeDefined();
      expect(importDecl.specifiers).toHaveLength(3);
      
      // First specifier: foo
      expect(importDecl.specifiers![0].imported).toBe('foo');
      expect(importDecl.specifiers![0].local).toBe('foo');
      
      // Second specifier: bar as baz
      expect(importDecl.specifiers![1].imported).toBe('bar');
      expect(importDecl.specifiers![1].local).toBe('baz');
      
      // Third specifier: qux
      expect(importDecl.specifiers![2].imported).toBe('qux');
      expect(importDecl.specifiers![2].local).toBe('qux');
    });
    
    test('stores default export value', () => {
      const code = `export default class MyClass { }`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const exportDecl = ast.body[0] as AST.ExportDecl;
      expect(exportDecl.kind).toBe('ExportDecl');
      
      // Should store that it's a default export
      expect(exportDecl.isDefault).toBe(true);
      
      // Should store the declaration being exported
      expect(exportDecl.declaration).toBeDefined();
      const classDecl = exportDecl.declaration as AST.ClassDecl;
      expect(classDecl.kind).toBe('ClassDecl');
      expect(classDecl.name.name).toBe('MyClass');
    });
    
    test('stores destructured export specifiers', () => {
      const code = `export { foo, bar as baz } from 'module';`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const exportDecl = ast.body[0] as AST.ExportDecl;
      expect(exportDecl.kind).toBe('ExportDecl');
      
      // Should store the source module
      expect(exportDecl.source).toBe('module');
      
      // Should store export specifiers
      expect(exportDecl.specifiers).toBeDefined();
      expect(exportDecl.specifiers).toHaveLength(2);
      
      // First specifier: foo
      expect(exportDecl.specifiers![0].local.name).toBe('foo');
      expect(exportDecl.specifiers![0].exported).toBeUndefined();
      
      // Second specifier: bar as baz
      expect(exportDecl.specifiers![1].local.name).toBe('bar');
      expect(exportDecl.specifiers![1].exported!.name).toBe('baz');
    });
  });
  
  describe('Decorators', () => {
    
    test('stores function decorators', () => {
      const code = `
@deprecated
@memoize
function calculate(x: number): number {
  return x * 2;
}`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const funcDecl = ast.body[0] as AST.FuncDecl;
      expect(funcDecl.kind).toBe('FuncDecl');
      
      // Should store decorators
      expect(funcDecl.decorators).toBeDefined();
      expect(funcDecl.decorators).toHaveLength(2);
      expect(funcDecl.decorators![0].name.name).toBe('deprecated');
      expect(funcDecl.decorators![1].name.name).toBe('memoize');
    });
    
    test('stores parameter decorators', () => {
      const code = `function validate(@NotNull @Range(0, 100) value: number) { }`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const funcDecl = ast.body[0] as AST.FuncDecl;
      
      // Should store parameter decorators
      const param = funcDecl.params[0];
      expect(param.decorators).toBeDefined();
      expect(param.decorators).toHaveLength(2);
      expect(param.decorators![0].name.name).toBe('NotNull');
      expect(param.decorators![1].name.name).toBe('Range');
      expect(param.decorators![1].args).toHaveLength(1); // Parsed as single comma expression
    });
    
    test('stores class member decorators', () => {
      const code = `
class Component {
  @Input() title: string;
  @Output() click = new EventEmitter();
  
  @HostListener('click')
  onClick() { }
}`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const classDecl = ast.body[0] as AST.ClassDecl;
      
      // Property decorator
      const titleProp = classDecl.members[0];
      expect(titleProp.decorators).toBeDefined();
      expect(titleProp.decorators![0].name.name).toBe('Input');
      
      // Method decorator
      const method = classDecl.members.find(m => m.kind === 'Method');
      expect(method?.decorators).toBeDefined();
      expect(method?.decorators![0].name.name).toBe('HostListener');
      expect(method?.decorators![0].args).toHaveLength(1);
    });
  });
  
  describe('Type System', () => {
    
    test('stores object type literal structure', () => {
      const code = `type User = { name: string, age: number, active?: boolean };`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const typeDecl = ast.body[0] as AST.TypeDecl;
      
      const objType = typeDecl.definition as AST.ObjectType;
      expect(objType.kind).toBe('ObjectType');
      expect(objType.properties).toHaveLength(3);
      
      // First property
      expect(objType.properties[0].name).toBe('name');
      expect(objType.properties[0].type.kind).toBe('SimpleType');
      expect(objType.properties[0].optional).toBe(false);
      
      // Optional property
      expect(objType.properties[2].name).toBe('active');
      expect(objType.properties[2].optional).toBe(true);
    });
    
    test('stores interface method signatures', () => {
      const code = `
interface Calculator {
  add(a: number, b: number): number;
  multiply(x: number, y: number): number;
}`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const interfaceDecl = ast.body[0] as AST.InterfaceDecl;
      
      // Should store method signatures
      const addMethod = interfaceDecl.members[0] as AST.InterfaceMember;
      expect(addMethod.kind).toBe('Method');
      expect(addMethod.name.name).toBe('add');
      expect(addMethod.params).toHaveLength(2);
      expect(addMethod.returnType?.kind).toBe('SimpleType');
    });
  });
  
  describe('Advanced Expressions', () => {
    
    test('stores computed object property keys', () => {
      const code = `const obj = { [key]: value, [Symbol.iterator]: fn };`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const constDecl = ast.body[0] as AST.ConstDecl;
      const objLiteral = constDecl.values![0] as AST.ObjectLiteral;
      
      // Should store computed keys as expressions
      const prop1 = objLiteral.properties[0];
      expect(prop1.computed).toBe(true);
      expect(prop1.key.kind).toBe('Identifier');
      expect((prop1.key as AST.Identifier).name).toBe('key');
      
      const prop2 = objLiteral.properties[1];
      expect(prop2.computed).toBe(true);
      expect(prop2.key.kind).toBe('Member');
    });
    
    test('stores list comprehension structure', () => {
      const code = `const doubled = [x * 2 for x in range(10) if x % 2 == 0];`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const constDecl = ast.body[0] as AST.ConstDecl;
      const comprehension = constDecl.values![0] as AST.ListComprehension;
      
      expect(comprehension.kind).toBe('ListComprehension');
      
      // Expression part
      expect(comprehension.expression.kind).toBe('Binary');
      
      // For clause - single target
      expect(comprehension.targets).toHaveLength(1);
      expect(comprehension.targets[0].name).toBe('x');
      expect(comprehension.iterable.kind).toBe('Call');
      
      // If clause (filter)
      expect(comprehension.filter).toBeDefined();
      expect(comprehension.filter!.kind).toBe('Binary');
    });
    
    test('stores multiple targets in list comprehension', () => {
      const code = `const pairs = [[item, idx] for item, idx in enumerate(items)];`;
      const ast = parseCode(code);
      
      expect(ast.body).toHaveLength(1);
      const constDecl = ast.body[0] as AST.ConstDecl;
      const comprehension = constDecl.values![0] as AST.ListComprehension;
      
      expect(comprehension.kind).toBe('ListComprehension');
      
      // Should store both target variables
      expect(comprehension.targets).toHaveLength(2);
      expect(comprehension.targets[0].name).toBe('item');
      expect(comprehension.targets[1].name).toBe('idx');
      
      // Iterable
      expect(comprehension.iterable.kind).toBe('Call');
    });
  });
});