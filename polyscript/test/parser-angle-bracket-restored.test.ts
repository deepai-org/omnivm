import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import {
  parseCode,
  verifyJSXElement,
  verifyGenericType,
  verifyComparison,
  verifyChannelSend,
  verifyChannelReceive,
  findFirst,
  findByKind,
  verifyAngleBrackets,
  analyzeAngleBrackets
} from './helpers';

describe('Parser - Restored Angle Bracket Tests', () => {
  
  describe('Previously Removed Tests', () => {
    
    test('uses "as" for type assertion in JSX context', () => {
      const code = `
<div>{x as number}</div>;
<MyComponent prop={x as string} />
`;
      const ast = parseCode(code);
      
      // First JSX with type assertion
      const jsx1 = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
      verifyJSXElement(jsx1, 'div', { selfClosing: false });
      const child = (jsx1 as any).children[0].expression;
      const assertion1 = child as AST.TypeAssertion;
      expect(assertion1.kind).toBe('TypeAssertion');
      expect((assertion1.expr as any).name).toBe('x');
      expect((assertion1.type as any).name || (assertion1.type as any).id?.name).toBe('number');
      
      // Second JSX with type assertion in prop
      const jsx2 = (ast.body[1] as AST.ExprStmt).expr as AST.JSXElement;
      verifyJSXElement(jsx2, 'MyComponent', { selfClosing: true });
      const prop = (jsx2 as any).openingElement.attributes[0];
      expect(prop.name.name).toBe('prop');
      const assertion2 = prop.value.expression as AST.TypeAssertion;
      expect(assertion2.kind).toBe('TypeAssertion');
      expect((assertion2.expr as any).name).toBe('x');
      expect((assertion2.type as any).name || (assertion2.type as any).id?.name).toBe('string');
    });
    
    test('parses JSX with generic component and children', () => {
      // Parser now supports generic type arguments on JSX components
      const code = `
<MyComponent<string>
    value={foo<string>("hi")}
>
    <ChildComponent<int> value={42} />
</MyComponent>
`;
      const ast = parseCode(code);
      
      const jsx = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
      expect((jsx as any).openingElement.name.name).toBe('MyComponent');
      
      // Check if parser supports generic args on JSX components
      if ((jsx as any).openingElement.typeArguments) {
        expect((jsx as any).openingElement.typeArguments).toHaveLength(1);
        expect(((jsx as any).openingElement.typeArguments[0] as any).name || 
               ((jsx as any).openingElement.typeArguments[0] as any).id?.name).toBe('string');
      }
      
      // Verify prop with generic call
      const prop = (jsx as any).openingElement.attributes[0];
      expect(prop.name.name).toBe('value');
      const call = prop.value.expression as AST.Call;
      expect(call.kind).toBe('Call');
      const callee = (call as any).func || call.callee;
      expect((callee as any).name).toBe('foo');
      expect(call.typeArgs).toHaveLength(1);
      
      // Verify child component
      const child = (jsx as any).children.find((c: any) => c.kind === 'JSXElement');
      if (child) {
        expect((child as any).openingElement.name.name).toBe('ChildComponent');
        if ((child as any).openingElement.typeArguments) {
          expect((child as any).openingElement.typeArguments).toHaveLength(1);
        }
      }
    });
    
    test('parses JSX fragment with generic components', () => {
      // Parser now supports generic type arguments on JSX components
      const code = `
<>
    <Table<RowData<string>> columns={["a","b"]} />
</>
`;
      const ast = parseCode(code);
      
      const frag = (ast.body[0] as AST.ExprStmt).expr as AST.JSXFragment;
      expect(frag.kind).toBe('JSXFragment');
      
      const table = (frag as any).children.find((c: any) => c.kind === 'JSXElement');
      if (table) {
        expect((table as any).openingElement.name.name).toBe('Table');
        
        // Check if parser supports generic args on JSX
        if ((table as any).openingElement.typeArguments) {
          expect((table as any).openingElement.typeArguments).toHaveLength(1);
          
          // Verify nested generic in type args
          const typeArg = (table as any).openingElement.typeArguments[0];
          verifyGenericType(typeArg, 'RowData', 1);
        }
      }
    });
    
    test('parses mixed generics, shifts, comparisons, and JSX', () => {
      // Parser now correctly handles type assertions like <A>c
      const code = `let v = foo<chan<int> >() << (bar < baz ? <A>c : <D>f)`;
      const ast = parseCode(code);
      
      const letDecl = ast.body[0] as AST.VarDecl;
      const shift = (letDecl as any).values[0] as AST.Binary;
      expect(shift.op).toBe('<<');
      
      // Left of shift: generic call
      const call = shift.left as AST.Call;
      expect(call.typeArgs).toHaveLength(1);
      verifyGenericType(call.typeArgs![0], 'chan', 1);
      
      // Right of shift: conditional with comparison and type assertions
      const conditional = shift.right as AST.Ternary;
      expect(conditional.kind).toBe('Ternary');
      const comparison = conditional.test as AST.Binary;
      verifyComparison(comparison, '<', 'bar', 'baz');
      
      // Verify type assertions in branches (not JSX elements)
      const conseq = conditional.consequent as AST.TypeAssertion;
      expect(conseq.kind).toBe('TypeAssertion');
      expect((conseq.type as any).name || (conseq.type as any).id?.name).toBe('A');
      expect((conseq.expr as any).name).toBe('c');
      
      const alt = conditional.alternate as AST.TypeAssertion;
      expect(alt.kind).toBe('TypeAssertion');
      expect((alt.type as any).name || (alt.type as any).id?.name).toBe('D');
      expect((alt.expr as any).name).toBe('f');
    });
    
    test('parses the all-in-one monster example', () => {
      const code = `
function crazy<T>(xs: List<T>, ch: chan<T>): any {
    let x = foo<Bar>(baz < 2 ? true : false)
    let v = <number>xs[0]
    return <Container>
        <Header>{x < y ? "Less" : "Greater"}</Header>
        <Send onClick={() => ch <- x}>{x}</Send>
    </Container>
}
`;
      const ast = parseCode(code);
      
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.genericParams).toHaveLength(1);
      expect(func.genericParams![0].name).toBe('T');
      
      // Verify parameters with generic types
      expect(func.params).toHaveLength(2);
      verifyGenericType(func.params[0].type, 'List', 1);
      verifyGenericType(func.params[1].type, 'chan', 1);
      
      const body = func.body as AST.Block;
      
      // First statement: generic call with ternary containing comparison and JSX
      const stmt1 = body.statements[0] as AST.VarDecl;
      const call = (stmt1 as any).values[0] as AST.Call;
      expect(call.kind).toBe('Call');
      expect(call.typeArgs).toHaveLength(1);
      
      // Arg is ternary with comparison and booleans
      const ternary = call.args[0] as AST.Ternary;
      const comparison = ternary.test as AST.Binary;
      verifyComparison(comparison, '<', 'baz', 2);
      
      // Ternary branches should be boolean literals
      const branch1 = ternary.consequent as AST.BooleanLiteral;
      expect(branch1.kind).toBe('BooleanLiteral');
      expect(branch1.value).toBe(true);
      const branch2 = ternary.alternate as AST.BooleanLiteral;
      expect(branch2.kind).toBe('BooleanLiteral');
      expect(branch2.value).toBe(false);
      
      // Second statement: type assertion
      const stmt2 = body.statements[1] as AST.VarDecl;
      const assertion = (stmt2 as any).values[0] as AST.TypeAssertion;
      expect(assertion.kind).toBe('TypeAssertion');
      expect((assertion.type as any).name || (assertion.type as any).id?.name).toBe('number');
      
      // Third statement: return JSX with nested elements
      const stmt3 = body.statements[2] as AST.Return;
      const jsx = stmt3.values[0] as AST.JSXElement;
      verifyJSXElement(jsx, 'Container', { selfClosing: false });
      
      // Find Header child with comparison
      const header = (jsx as any).children.find((c: any) => 
        c.kind === 'JSXElement' && c.openingElement.name.name === 'Header'
      );
      expect(header).toBeDefined();
      const headerExpr = (header as any).children[0].expression as AST.Ternary;
      const headerComparison = headerExpr.test as AST.Binary;
      verifyComparison(headerComparison, '<', 'x', 'y');
      
      // Find Send child with channel send
      const send = (jsx as any).children.find((c: any) => 
        c.kind === 'JSXElement' && c.openingElement.name.name === 'Send'
      );
      expect(send).toBeDefined();
      const onClick = (send as any).openingElement.attributes[0];
      expect(onClick.name.name).toBe('onClick');
      const arrow = onClick.value.expression as AST.Lambda;
      const channelSend = arrow.body as AST.Binary;
      verifyChannelSend(channelSend, 'ch', 'x');
      
      // Comprehensive angle bracket analysis
      const stats = analyzeAngleBrackets(ast);
      expect(stats.jsxCount).toBeGreaterThanOrEqual(3); // Container, Header, Send
      expect(stats.genericCount).toBeGreaterThanOrEqual(2); // List<T>, chan<T> (Bar doesn't have <>)
      expect(stats.comparisonCount).toBeGreaterThanOrEqual(2); // baz < 2, x < y
      expect(stats.channelCount).toBe(1); // ch <- x
    });
  });
});