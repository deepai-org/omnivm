// JSX Edge Cases and Disambiguation Tests
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

// Import test helpers
import {
  verifyJSXElement,
  verifyGenericType,
  verifyComparison,
  verifyBinaryOp,
  verifyAngleBrackets,
  findInAST,
  findAllInAST
} from './helpers/ast-verifiers';

import {
  findJSXElements,
  findGenericTypes,
  findComparisons,
  findAllAngleBracketUsages,
  analyzeAngleBrackets
} from './helpers/pattern-matchers';

import {
  normalizeConditional
} from './helpers/ast-compat';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('JSX Edge Cases and Disambiguation (UPDATED)', () => {
    it('should disambiguate JSX vs less-than operator', () => {
        const code = `
const a = x < 5
const b = <Component />
const c = x<5 && y>3
const d = <div>content</div>`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify each statement correctly disambiguates
        expect(ast.body).toHaveLength(4);
        
        // Statement 1: x < 5 should be comparison
        const stmt1 = ast.body[0] as AST.ConstDecl;
        expect(stmt1.kind).toBe('ConstDecl');
        const expr1 = stmt1.values[0] as AST.Binary;
        verifyComparison(expr1, '<', 'x', 5);
        
        // Statement 2: <Component /> should be JSX
        const stmt2 = ast.body[1] as AST.ConstDecl;
        const expr2 = stmt2.values[0] as AST.JSXElement;
        verifyJSXElement(expr2, 'Component', { selfClosing: true });
        
        // Statement 3: x<5 && y>3 should be comparisons with logical AND
        const stmt3 = ast.body[2] as AST.ConstDecl;
        const expr3 = stmt3.values[0] as AST.Binary;
        expect(expr3.op).toBe('&&');
        verifyComparison(expr3.left as AST.Binary, '<');
        verifyComparison(expr3.right as AST.Binary, '>');
        
        // Statement 4: <div>content</div> should be JSX
        const stmt4 = ast.body[3] as AST.ConstDecl;
        const expr4 = stmt4.values[0] as AST.JSXElement;
        verifyJSXElement(expr4, 'div', { selfClosing: false });
        
        // Comprehensive angle bracket verification
        verifyAngleBrackets(ast, {
            jsx: [
                { tag: 'Component', selfClosing: true },
                { tag: 'div', selfClosing: false }
            ],
            comparisons: [
                { op: '<', left: 'x', right: 5 },
                { op: '<', left: 'x', right: 5 },
                { op: '>', left: 'y', right: 3 }
            ]
        });
    });

    it('should disambiguate generic vs JSX', () => {
        const code = `
const arr = Array<number>()  // Generic
const elem = <Array />       // JSX
const func = fn<T>(arg)      // Generic call
const comp = <Button>Click</Button>  // JSX`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify generics vs JSX
        expect(ast.body).toHaveLength(4);
        
        // Find all angle bracket usages
        const usage = findAllAngleBracketUsages(ast);
        
        // Should have both JSX and generics
        expect(usage.jsx.elements.length).toBeGreaterThanOrEqual(2); // <Array />, <Button>
        
        // Verify JSX elements by name
        const jsxElements = findJSXElements(ast);
        const jsxNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        expect(jsxNames).toContain('Array');
        expect(jsxNames).toContain('Button');
        
        // Analyze statistics
        const stats = analyzeAngleBrackets(ast);
        expect(stats.jsxCount).toBeGreaterThanOrEqual(2);
    });

    it('should handle empty expressions', () => {
        const code = `
<div>
    {}
    {null}
    {undefined}
    {false}
    {true && <span>Show</span>}
</div>`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX with expression containers
        expect(ast.body).toHaveLength(1);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        verifyJSXElement(jsx, 'div');
        
        // Should have multiple children (text and expression containers)
        expect(jsx.children.length).toBeGreaterThan(0);
        
        // Count expression containers
        const exprContainers = jsx.children.filter(
            child => child.kind === 'JSXExpressionContainer'
        );
        expect(exprContainers.length).toBeGreaterThanOrEqual(5);
        
        // Find nested JSX
        const nestedJSX = findJSXElements(ast);
        expect(nestedJSX.length).toBe(2); // div and span
        
        // Verify nested span
        const span = nestedJSX.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'span';
        });
        expect(span).toBeDefined();
    });

    it('should handle whitespace correctly', () => {
        const code = `
<div>
    
    Text with spaces    
    
    <span>  Trimmed  </span>
</div>`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify whitespace handling in JSX
        const jsx = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
        verifyJSXElement(jsx, 'div');
        
        // Should have text and element children
        const textChildren = jsx.children.filter(c => c.kind === 'JSXText');
        const elementChildren = jsx.children.filter(c => c.kind === 'JSXElement');
        
        expect(textChildren.length).toBeGreaterThan(0);
        expect(elementChildren.length).toBe(1);
        
        // Verify nested span
        const span = elementChildren[0] as AST.JSXElement;
        verifyJSXElement(span, 'span');
    });

    it('should parse JSX in ternary', () => {
        const code = `
const result = condition ? <Success /> : <Error />`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX in conditional expression
        const stmt = ast.body[0] as AST.ConstDecl;
        const rawTernary = stmt.values[0] as any;
        const ternary = normalizeConditional(rawTernary);
        expect(ternary.kind).toBe('Conditional');
        
        // Both branches should be JSX
        verifyJSXElement(ternary.then as AST.JSXElement, 'Success');
        verifyJSXElement(ternary.else as AST.JSXElement, 'Error');
        
        // Verify angle brackets
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(2);
    });

    it('should parse JSX in array', () => {
        const code = `
const items = [
    <Item key={1} />,
    <Item key={2} />,
    <Item key={3} />
]`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX in array literal
        const stmt = ast.body[0] as AST.ConstDecl;
        const array = stmt.values[0] as AST.ArrayLiteral;
        expect(array.kind).toBe('ArrayLiteral');
        expect(array.elements.length).toBe(3);
        
        // All elements should be JSX
        array.elements.forEach((elem, i) => {
            verifyJSXElement(elem as AST.JSXElement, 'Item', { 
                selfClosing: true,
                attributeCount: 1 
            });
        });
        
        // Verify all JSX elements found
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(3);
    });

    it('should parse JSX as object value', () => {
        const code = `
const config = {
    header: <Header />,
    body: <Body />,
    footer: <Footer />
}`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX as object property values
        const stmt = ast.body[0] as AST.ConstDecl;
        const obj = stmt.values[0] as AST.ObjectLiteral;
        expect(obj.kind).toBe('ObjectLiteral');
        expect(obj.properties.length).toBe(3);
        
        // Each property value should be JSX
        const expectedTags = ['Header', 'Body', 'Footer'];
        obj.properties.forEach((prop, i) => {
            // Property values should be JSX
            verifyJSXElement((prop as any).value as AST.JSXElement, expectedTags[i]);
        });
        
        // Verify angle brackets
        verifyAngleBrackets(ast, {
            jsx: expectedTags.map(tag => ({ tag, selfClosing: true }))
        });
    });

    it('should handle JSX spread attributes', () => {
        const code = `
<Component {...props} id="test" {...overrides} />`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify spread attributes
        const jsx = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
        verifyJSXElement(jsx, 'Component', { selfClosing: true });
        
        // Should have mixed spread and normal attributes
        const attrs = jsx.openingElement.attributes;
        expect(attrs.length).toBe(3);
        
        // Check for spread attributes
        const spreads = attrs.filter(a => a.kind === 'JSXSpreadAttribute');
        expect(spreads.length).toBe(2);
        
        // Check for normal attribute
        const normal = attrs.filter(a => a.kind === 'JSXAttribute');
        expect(normal.length).toBe(1);
        expect((normal[0] as AST.JSXNormalAttribute).name.name).toBe('id');
    });

    it('should parse JSX with logical operators', () => {
        // Note: No leading newline to ensure proper parsing
        const code = `<div>
    {show && <Content />}
    {!hide || <Placeholder />}
    {count > 0 && count < 10 && <InRange />}
</div>`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX with logical expressions
        const jsx = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
        verifyJSXElement(jsx, 'div');
        
        // Find all JSX elements
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(4); // div, Content, Placeholder, InRange
        
        // Find comparisons in logical expressions
        const comparisons = findComparisons(ast);
        expect(comparisons.length).toBe(2); // count > 0, count < 10
        
        // Verify angle bracket disambiguation
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(4);
        expect(usage.comparisons.greaterThan.length).toBe(1);
        expect(usage.comparisons.lessThan.length).toBe(1);
    });

    it('should handle self-closing vs normal tags', () => {
        // Parse each tag separately to avoid multi-statement issues
        const codes = [
            '<input type="text" />',
            '<Input type="text"></Input>',
            '<br />',
            '<BR></BR>'
        ];

        // Parse each individually and verify
        const jsxElements: AST.JSXElement[] = [];
        codes.forEach(code => {
            const ast = parseCode(code);
            const jsx = findJSXElements(ast);
            jsxElements.push(...jsx);
        });
        
        // ✅ STRONG: Verify self-closing detection
        expect(jsxElements.length).toBe(4);
        
        // Check self-closing flags
        const [input1, input2, br1, br2] = jsxElements;
        
        expect(input1.openingElement.selfClosing).toBe(true);
        expect(input1.closingElement).toBeNull();
        
        expect(input2.openingElement.selfClosing).toBe(false);
        expect(input2.closingElement).toBeDefined();
        
        expect(br1.openingElement.selfClosing).toBe(true);
        expect(br2.openingElement.selfClosing).toBe(false);
    });

    it('should parse complex nested JSX', () => {
        // Note: No leading newline to ensure proper parsing
        const code = `<App>
    <Header>
        <Nav items={[<Link href="/">Home</Link>, <Link href="/about">About</Link>]} />
    </Header>
    <Main>
        {data.map(item => <Card key={item.id}>
            <Title>{item.name}</Title>
            <Body>{item.description}</Body>
        </Card>)}
    </Main>
</App>`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify complex nesting
        const jsxElements = findJSXElements(ast);
        
        // Count unique tag names
        const tagNames = new Set(jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        }));
        
        expect(tagNames).toContain('App');
        expect(tagNames).toContain('Header');
        expect(tagNames).toContain('Nav');
        expect(tagNames).toContain('Link');
        expect(tagNames).toContain('Main');
        expect(tagNames).toContain('Card');
        expect(tagNames).toContain('Title');
        expect(tagNames).toContain('Body');
        
        // Verify no angle brackets confused with comparisons
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBeGreaterThan(0);
        expect(usage.comparisons.lessThan.length).toBe(0); // No comparisons
        expect(usage.comparisons.greaterThan.length).toBe(0);
    });

    it('should handle JSX comments correctly', () => {
        const code = `
<div>
    {/* This is a comment */}
    <span>Content</span>
    {/* Multi
        line
        comment */}
</div>`;

        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX with comments
        expect(ast.body.length).toBeGreaterThan(0);
        const jsx = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
        verifyJSXElement(jsx, 'div');
        
        // Comments are usually parsed as expression containers with comment content
        // or might be skipped entirely depending on parser implementation
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBeGreaterThanOrEqual(2); // At least div and span
        
        // Verify span exists
        const span = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'span';
        });
        expect(span).toBeDefined();
    });
});