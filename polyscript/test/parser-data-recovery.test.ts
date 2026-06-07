import { describe, test, expect } from '@jest/globals';
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Parser Data Recovery - No Discarded Syntax', () => {
  describe('Destructuring in Short Declarations', () => {
    test('parses array destructuring in short declaration', () => {
      const ast = parseCode('[a, b] := arr');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ShortDecl;
      expect(decl.kind).toBe('ShortDecl');
      expect(decl.targets).toBeDefined();
      expect(decl.targets![0].kind).toBe('ArrayPattern');
      const pattern = decl.targets![0] as AST.ArrayPattern;
      expect(pattern.elements).toHaveLength(2);
      expect((pattern.elements[0] as AST.Identifier).name).toBe('a');
      expect((pattern.elements[1] as AST.Identifier).name).toBe('b');
    });

    test('parses object destructuring in short declaration', () => {
      const ast = parseCode('{name, age} := person');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ShortDecl;
      expect(decl.kind).toBe('ShortDecl');
      expect(decl.targets).toBeDefined();
      expect(decl.targets![0].kind).toBe('ObjectPattern');
      const pattern = decl.targets![0] as AST.ObjectPattern;
      expect(pattern.properties).toHaveLength(2);
    });

    test('parses nested array destructuring', () => {
      const ast = parseCode('[a, [b, c]] := nested');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ShortDecl;
      expect(decl.kind).toBe('ShortDecl');
      expect(decl.targets).toBeDefined();
      const pattern = decl.targets![0] as AST.ArrayPattern;
      expect(pattern.elements).toHaveLength(2);
      expect((pattern.elements[0] as AST.Identifier).name).toBe('a');
      expect((pattern.elements[1] as AST.ArrayPattern).kind).toBe('ArrayPattern');
    });

    test('parses array rest and default destructuring in const declaration', () => {
      const ast = parseCode('const [first, fallback = {"items": "fallback"}, ...rest] = rows');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ConstDecl;
      expect(decl.kind).toBe('ConstDecl');
      expect(decl.destructurePattern?.kind).toBe('ArrayPattern');
      const pattern = decl.destructurePattern as AST.ArrayPattern;
      expect(pattern.elements).toHaveLength(3);
      expect((pattern.elements[0] as AST.Identifier).name).toBe('first');
      const fallback = pattern.elements[1] as AST.ArrayPatternElement;
      expect(fallback.kind).toBe('ArrayPatternElement');
      expect((fallback.value as AST.Identifier).name).toBe('fallback');
      expect(fallback.defaultValue?.kind).toBe('ObjectLiteral');
      const rest = pattern.elements[2] as AST.ArrayPatternElement;
      expect(rest.kind).toBe('ArrayPatternElement');
      expect(rest.rest).toBe(true);
      expect((rest.value as AST.Identifier).name).toBe('rest');
      expect(decl.names.map(name => name.name)).toEqual(['first', 'fallback', 'rest']);
    });

    test('parses object destructuring with renaming', () => {
      const ast = parseCode('{name: userName, id: userId} := user');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ShortDecl;
      const pattern = decl.targets![0] as AST.ObjectPattern;
      expect(pattern.properties).toHaveLength(2);
      expect((pattern.properties[0].value as AST.Identifier).name).toBe('userName');
      expect((pattern.properties[1].value as AST.Identifier).name).toBe('userId');
    });
  });

  describe('Optional Calls', () => {
    test('parses optional calls on collision-heavy member fields', () => {
      const ast = parseCode('const called = payload.then?.("manual")');
      expect(ast.body).toHaveLength(1);
      const decl = ast.body[0] as AST.ConstDecl;
      expect(decl.kind).toBe('ConstDecl');
      expect(decl.values).toHaveLength(1);
      const call = decl.values[0] as AST.Call;
      expect(call.kind).toBe('Call');
      expect(call.optional).toBe(true);
      const callee = call.callee as AST.Member;
      expect(callee.kind).toBe('Member');
      expect(callee.property.name).toBe('then');
    });

    test('keeps following const declarations after optional calls as declarations', () => {
      const ast = parseCode(`const called = payload.then?.("manual") ?? "missing"
const missing_called = missing_payload.then?.("manual") ?? "missing"
const item_count = missing_payload.items?.length ?? 0`);
      expect(ast.body).toHaveLength(3);
      expect(ast.body.map((node: any) => node.kind)).toEqual(['ConstDecl', 'ConstDecl', 'ConstDecl']);
      expect((ast.body[1] as AST.ConstDecl).names[0].name).toBe('missing_called');
      expect((ast.body[2] as AST.ConstDecl).names[0].name).toBe('item_count');
    });
  });

  describe('Where Clauses in Impl Blocks', () => {
    test('parses simple where clause', () => {
      const code = `impl<T> Container<T> where T: Clone {
        fn clone_first(&self) -> T {
          self.items[0].clone()
        }
      }`;
      const ast = parseCode(code);
      const impl = ast.body[0] as AST.ImplDecl;
      expect(impl.kind).toBe('ImplDecl');
      expect(impl.whereClause).toBeDefined();
      expect(impl.whereClause!.constraints).toHaveLength(1);
      expect(impl.whereClause!.constraints[0].bounds).toHaveLength(1);
    });

    test('parses multiple constraints in where clause', () => {
      const code = `impl<T, U> Pair<T, U> 
      where 
        T: Display + Debug,
        U: Clone + Send
      {
        fn show(&self) { }
      }`;
      const ast = parseCode(code);
      const impl = ast.body[0] as AST.ImplDecl;
      expect(impl.whereClause).toBeDefined();
      expect(impl.whereClause!.constraints).toHaveLength(2);
      expect(impl.whereClause!.constraints[0].bounds).toHaveLength(2);
      expect(impl.whereClause!.constraints[1].bounds).toHaveLength(2);
    });

    test('parses trait impl with where clause', () => {
      const code = `impl<T> Display for Container<T> 
      where T: Display {
        fn fmt() {}
      }`;
      const ast = parseCode(code);
      const impl = ast.body[0] as AST.ImplDecl;
      expect(impl.trait).toBeDefined();
      expect(impl.whereClause).toBeDefined();
      expect(impl.whereClause!.constraints).toHaveLength(1);
    });
  });

  describe('Impl Block Member Types', () => {
    test('parses associated types in impl blocks', () => {
      const code = `impl<T> Iterator for Container<T> {
        type Item = T;
        type IntoIter = Vec<T>;
      }`;
      const ast = parseCode(code);
      const impl = ast.body[0] as AST.ImplDecl;
      expect(impl.members).toHaveLength(2);
      expect(impl.members[0].kind).toBe('AssociatedType');
      expect(impl.members[0].name!.name).toBe('Item');
      expect(impl.members[1].kind).toBe('AssociatedType');
      expect(impl.members[1].name!.name).toBe('IntoIter');
    });

    test('parses associated constants in impl blocks', () => {
      const code = `impl MyTrait for MyStruct {
        const MAX_SIZE: usize = 1024;
        const DEFAULT_VALUE: i32 = 42;
      }`;
      const ast = parseCode(code);
      const impl = ast.body[0] as AST.ImplDecl;
      expect(impl.members).toHaveLength(2);
      expect(impl.members[0].kind).toBe('AssociatedConst');
      expect(impl.members[0].name!.name).toBe('MAX_SIZE');
      expect(impl.members[0].value).toBeDefined();
      expect(impl.members[1].kind).toBe('AssociatedConst');
      expect(impl.members[1].name!.name).toBe('DEFAULT_VALUE');
    });

    test('parses const fn in impl blocks', () => {
      const code = `impl Math {
        const fn square(x: i32) -> i32 {
          x * x
        }
        pub const fn cube(x: i32) -> i32 {
          x * x * x
        }
      }`;
      const ast = parseCode(code);
      const impl = ast.body[0] as AST.ImplDecl;
      expect(impl.members).toHaveLength(2);
      expect(impl.members[0].kind).toBe('Method');
      expect(impl.members[0].name!.name).toBe('square');
      expect(impl.members[0].isConst).toBe(true);
      expect(impl.members[1].kind).toBe('Method');
      expect(impl.members[1].name!.name).toBe('cube');
      expect(impl.members[1].isConst).toBe(true);
      expect(impl.members[1].visibility).toBe('public');
    });

    test('preserves unknown impl members', () => {
      const code = `impl MyType {
        macro_rules! define_methods {
          () => {}
        }
      }`;
      const ast = parseCode(code);
      const impl = ast.body[0] as AST.ImplDecl;
      expect(impl.members.length).toBeGreaterThan(0);
      // Unknown members should be preserved, not discarded
      const unknownMember = impl.members.find(m => m.kind === 'Unknown');
      expect(unknownMember).toBeDefined();
    });
  });

  describe('Property Accessors', () => {
    test('parses TypeScript-style getters and setters', () => {
      const code = `class Container {
        get value(): T {
          return this._value;
        }
        set value(v: T) {
          this._value = v;
        }
      }`;
      const ast = parseCode(code);
      const cls = ast.body[0] as AST.ClassDecl;
      expect(cls.members).toHaveLength(2);
      expect(cls.members[0].kind).toBe('Getter');
      expect(cls.members[0].name!.name).toBe('value');
      expect(cls.members[1].kind).toBe('Setter');
      expect(cls.members[1].name!.name).toBe('value');
    });

    test('distinguishes between get method and getter', () => {
      const code = `class Container {
        get(index: int): T {
          return this.items[index];
        }
        get value(): T {
          return this._value;
        }
      }`;
      const ast = parseCode(code);
      const cls = ast.body[0] as AST.ClassDecl;
      expect(cls.members).toHaveLength(2);
      expect(cls.members[0].kind).toBe('Method');
      expect(cls.members[0].name!.name).toBe('get');
      expect(cls.members[1].kind).toBe('Getter');
      expect(cls.members[1].name!.name).toBe('value');
    });

    test('parses C#-style property with accessor bodies', () => {
      const code = `class Person {
        public string Name {
          get { return _name; }
          set { _name = value; }
        }
      }`;
      const ast = parseCode(code);
      const cls = ast.body[0] as AST.ClassDecl;
      expect(cls.members).toHaveLength(1);
      expect(cls.members[0].kind).toBe('Property');
      const prop = cls.members[0] as AST.ClassMember;
      expect(prop.name!.name).toBe('Name');
      expect(prop.getter).toBeDefined();
      expect(prop.setter).toBeDefined();
    });
  });

  describe('Function Type Parameter Names', () => {
    test('preserves parameter names in function types', () => {
      const code = `type Handler = (event: Event, context: Context) => void;`;
      const ast = parseCode(code);
      const decl = ast.body[0] as AST.TypeDecl;
      const funcType = decl.definition as AST.FuncType;
      expect(funcType.params).toHaveLength(2);
      expect((funcType.params[0].name as AST.Identifier).name).toBe('event');
      expect((funcType.params[1].name as AST.Identifier).name).toBe('context');
    });

    test('parses optional parameters in function types', () => {
      const code = `type Callback = (error?: Error, result?: T) => void;`;
      const ast = parseCode(code);
      const decl = ast.body[0] as AST.TypeDecl;
      const funcType = decl.definition as AST.FuncType;
      expect(funcType.params).toHaveLength(2);
      expect((funcType.params[0].name as AST.Identifier).name).toBe('error');
      expect(funcType.params[0].optional).toBe(true);
      expect((funcType.params[1].name as AST.Identifier).name).toBe('result');
      expect(funcType.params[1].optional).toBe(true);
    });
  });

  describe('Method Signatures in Classes', () => {
    test('parses method signatures without implementation', () => {
      const code = `class Shape {
        area(): number;
        perimeter(): number;
      }`;
      const ast = parseCode(code);
      const cls = ast.body[0] as AST.ClassDecl;
      expect(cls.members).toHaveLength(2);
      cls.members.forEach(member => {
        expect(member.kind).toBe('Method');
        expect(member.body).toBeUndefined();
      });
    });

    test('parses interface method signatures', () => {
      const code = `interface Calculator {
        add(a: number, b: number): number;
        subtract(x: number, y: number): number;
      }`;
      const ast = parseCode(code);
      const iface = ast.body[0] as AST.InterfaceDecl;
      expect(iface.members).toHaveLength(2);
      expect(iface.members[0].params).toHaveLength(2);
      expect((iface.members[0].params![0].name as AST.Identifier).name).toBe('a');
      expect((iface.members[0].params![1].name as AST.Identifier).name).toBe('b');
    });
  });

  describe('Decorators', () => {
    test('parses function decorators', () => {
      const code = `@deprecated
      @log
      function oldMethod() {}`;
      const ast = parseCode(code);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.decorators).toHaveLength(2);
      expect(((func.decorators![0] as AST.Decorator).name as AST.Identifier).name).toBe('deprecated');
      expect(((func.decorators![1] as AST.Decorator).name as AST.Identifier).name).toBe('log');
    });

    test('parses parameter decorators', () => {
      const code = `function validate(@NotNull name: string, @Range(0, 100) age: number) {}`;
      const ast = parseCode(code);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.params[0].decorators).toHaveLength(1);
      expect(func.params[1].decorators).toHaveLength(1);
    });

    test('parses class member decorators', () => {
      const code = `class Component {
        @Input() title: string;
        @Output() clicked = new EventEmitter();
      }`;
      const ast = parseCode(code);
      const cls = ast.body[0] as AST.ClassDecl;
      expect(cls.members[0].decorators).toHaveLength(1);
      expect(cls.members[1].decorators).toHaveLength(1);
    });
  });

  describe('Import/Export Specifiers', () => {
    test('stores destructured import specifiers', () => {
      const code = `import { foo, bar as baz } from 'module';`;
      const ast = parseCode(code);
      const imp = ast.body[0] as AST.ImportDecl;
      expect(imp.kind).toBe('ImportDecl');
      expect(imp.specifiers).toHaveLength(2);
      expect(imp.specifiers![0].imported).toBe('foo');
      expect(imp.specifiers![0].local).toBe('foo');
      expect(imp.specifiers![1].imported).toBe('bar');
      expect(imp.specifiers![1].local).toBe('baz');
    });

    test('stores default export flag', () => {
      const code = `export default class MyClass {}`;
      const ast = parseCode(code);
      const exp = ast.body[0] as AST.ExportDecl;
      expect(exp.kind).toBe('ExportDecl');
      expect(exp.isDefault).toBe(true);
    });

    test('stores export specifiers', () => {
      const code = `export { foo, bar as baz };`;
      const ast = parseCode(code);
      const exp = ast.body[0] as AST.ExportDecl;
      expect(exp.specifiers).toHaveLength(2);
      expect((exp.specifiers![0].local as AST.Identifier).name).toBe('foo');
      expect((exp.specifiers![1].local as AST.Identifier).name).toBe('bar');
      expect((exp.specifiers![1].exported as AST.Identifier).name).toBe('baz');
    });
  });

  describe('Object Type Literals', () => {
    test('parses object type literals with modifiers', () => {
      const code = `type Config = {
        readonly name: string;
        port?: number;
        debug: boolean;
      };`;
      const ast = parseCode(code);
      const decl = ast.body[0] as AST.TypeDecl;
      const objType = decl.definition as AST.ObjectType;
      expect(objType.kind).toBe('ObjectType');
      expect(objType.properties).toHaveLength(3);
      expect(objType.properties[0].readonly).toBe(true);
      expect(objType.properties[1].optional).toBe(true);
    });
  });

  describe('Computed Object Properties', () => {
    test('stores computed property expressions', () => {
      const code = `const obj = {
        [key]: value,
        [Symbol.iterator]: function* () {}
      };`;
      const ast = parseCode(code);
      const varDecl = ast.body[0] as AST.VarDecl;
      const objLit = varDecl.values![0] as AST.ObjectLiteral;
      expect(objLit.properties[0].computed).toBe(true);
      expect(objLit.properties[0].key.kind).toBe('Identifier');
      expect(objLit.properties[1].computed).toBe(true);
    });
  });

  describe('List Comprehensions', () => {
    test('parses list comprehension with filter', () => {
      const code = `[x * 2 for x in numbers if x > 0]`;
      const ast = parseCode(code);
      const expr = (ast.body[0] as AST.ExprStmt).expr as AST.ListComprehension;
      expect(expr.kind).toBe('ListComprehension');
      expect(expr.expression).toBeDefined();
      expect(expr.targets[0].name).toBe('x');
      expect(expr.iterable).toBeDefined();
      expect(expr.filter).toBeDefined();
    });

    test('parses list comprehension with multiple targets', () => {
      const code = `[value for key, value in items]`;
      const ast = parseCode(code);
      const expr = (ast.body[0] as AST.ExprStmt).expr as AST.ListComprehension;
      expect(expr.targets).toHaveLength(2);
      expect(expr.targets![0].name).toBe('key');
      expect(expr.targets![1].name).toBe('value');
    });
  });

  describe('Destructuring in Parameters', () => {
    test('parses array destructuring in function parameters', () => {
      const code = `function sum([a, b]: number[]) { return a + b; }`;
      const ast = parseCode(code);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.params[0].name).toBeDefined();
      expect(func.params[0].name!.kind).toBe('ArrayPattern');
    });

    test('parses object destructuring in function parameters', () => {
      const code = `function greet({name, age}: Person) { }`;
      const ast = parseCode(code);
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.params[0].name).toBeDefined();
      expect(func.params[0].name!.kind).toBe('ObjectPattern');
    });
  });
});
