// JSX Basic Elements Tests
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import { RuntimeResolver } from '../src/runtime-resolver';
import { ManifestCodeGenerator } from '../src/codegen-omnivm/manifest-generator';

/** Parse code and smoke-test the manifest pipeline */
function parse(code: string) {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  // Smoke-test manifest pipeline
  if (parser.getErrors().length === 0) {
    const resolver = new RuntimeResolver();
    const annotated = resolver.resolve(ast, code);
    const gen = new ManifestCodeGenerator();
    const manifest = gen.generate(annotated);
    JSON.stringify(manifest);
  }
  return ast;
}

describe('JSX Basic Elements', () => {
    it('should parse self-closing JSX element', () => {
        const code = `<Button />`;
        const ast = parse(code);
        
        expect(ast.body).toHaveLength(1);
        const stmt = ast.body[0] as AST.ExprStmt;
        expect(stmt.kind).toBe('ExprStmt');
        
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.selfClosing).toBe(true);
        expect(jsx.closingElement).toBeNull();
        expect(jsx.children).toHaveLength(0);
        
        const name = jsx.openingElement.name as AST.JSXIdentifier;
        expect(name.kind).toBe('JSXIdentifier');
        expect(name.name).toBe('Button');
    });

    it('should parse self-closing element with props', () => {
        const code = `<Button size="large" variant="primary" disabled />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(3);
        
        // Check first attribute (size="large")
        const attr1 = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        expect(attr1.kind).toBe('JSXAttribute');
        const name1 = attr1.name as AST.JSXIdentifier;
        expect(name1.name).toBe('size');
        const value1 = attr1.value as AST.StringLiteral;
        expect(value1.kind).toBe('StringLiteral');
        
        // Check boolean attribute (disabled)
        const attr3 = jsx.openingElement.attributes[2] as AST.JSXNormalAttribute;
        expect(attr3.kind).toBe('JSXAttribute');
        const name3 = attr3.name as AST.JSXIdentifier;
        expect(name3.name).toBe('disabled');
        expect(attr3.value).toBeNull();
    });

    it('should parse simple container element', () => {
        const code = `<div>Hello World</div>`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.selfClosing).toBe(false);
        expect(jsx.closingElement).not.toBeNull();
        
        // Check element name matches
        const openName = jsx.openingElement.name as AST.JSXIdentifier;
        const closeName = jsx.closingElement!.name as AST.JSXIdentifier;
        expect(openName.name).toBe('div');
        expect(closeName.name).toBe('div');
        
        // Check text content
        expect(jsx.children).toHaveLength(1);
        const text = jsx.children[0] as AST.JSXText;
        expect(text.kind).toBe('JSXText');
        expect(text.value.trim()).toBe('Hello World'); // Space handling in JSX text
    });

    it('should parse container with expression', () => {
        const code = `<span>{message}</span>`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        
        // Check it has expression child
        expect(jsx.children).toHaveLength(1);
        const expr = jsx.children[0] as AST.JSXExpressionContainer;
        expect(expr.kind).toBe('JSXExpressionContainer');
        const ident = expr.expression as AST.Identifier;
        expect(ident.kind).toBe('Identifier');
        expect(ident.name).toBe('message');
    });

    it('should parse multiple children', () => {
        const code = `
<div>
    <h1>Title</h1>
    <p>Paragraph text</p>
    <Button>Click me</Button>
</div>`;
        const ast = parse(code);

        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');

        // Check it has multiple child elements (plus text nodes for whitespace)
        const childElements = jsx.children.filter(c => c.kind === 'JSXElement');
        expect(childElements).toHaveLength(3);
        
        // Check first child is h1
        const h1 = childElements[0] as AST.JSXElement;
        const h1Name = h1.openingElement.name as AST.JSXIdentifier;
        expect(h1Name.name).toBe('h1');
        
        // Check second child is p
        const p = childElements[1] as AST.JSXElement;
        const pName = p.openingElement.name as AST.JSXIdentifier;
        expect(pName.name).toBe('p');
        
        // Check third child is Button
        const button = childElements[2] as AST.JSXElement;
        const buttonName = button.openingElement.name as AST.JSXIdentifier;
        expect(buttonName.name).toBe('Button');
    });

    it('should parse mixed text and expressions', () => {
        const code = `<div>Count: {count} items</div>`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        
        // Should have 3 children: text, expression, text
        expect(jsx.children.length).toBeGreaterThanOrEqual(3);
        
        // Find non-whitespace children
        const meaningfulChildren = jsx.children.filter(c => {
            if (c.kind === 'JSXText') {
                return (c as AST.JSXText).value.trim() !== '';
            }
            return true;
        });
        
        expect(meaningfulChildren).toHaveLength(3);
        
        // First is text "Count: "
        const text1 = meaningfulChildren[0] as AST.JSXText;
        expect(text1.kind).toBe('JSXText');
        expect(text1.value.trim()).toBe('Count:');
        
        // Second is expression {count}
        const expr = meaningfulChildren[1] as AST.JSXExpressionContainer;
        expect(expr.kind).toBe('JSXExpressionContainer');
        
        // Third is text " items"
        const text2 = meaningfulChildren[2] as AST.JSXText;
        expect(text2.kind).toBe('JSXText');
        expect(text2.value.trim()).toBe('items');
    });

    it('should parse JSX in function', () => {
        const code = `
function App() {
    return <div>Hello</div>
}`;
        const ast = parse(code);

        // Check function declaration
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        expect(func.kind).toBe('FuncDecl');
        expect(func.name.name).toBe('App');
        
        // Check return statement
        const body = func.body as AST.Block;
        expect(body.statements).toHaveLength(1);
        const returnStmt = body.statements[0] as AST.Return;
        expect(returnStmt.kind).toBe('Return');
        
        // Check JSX element in return
        const jsx = returnStmt.values[0] as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        const name = jsx.openingElement.name as AST.JSXIdentifier;
        expect(name.name).toBe('div');
    });

    it('should parse HTML entities', () => {
        const code = `<div>&lt;script&gt; &amp; &quot;text&quot;</div>`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        
        // Check it has text content with entities
        expect(jsx.children).toHaveLength(1);
        const text = jsx.children[0] as AST.JSXText;
        expect(text.kind).toBe('JSXText');
        // The entities should be preserved as-is in the AST
        expect(text.value).toContain('&lt;');
        expect(text.value).toContain('&gt;');
        expect(text.value).toContain('&amp;');
        expect(text.value).toContain('&quot;');
    });
});