// JSX Attributes and Props Tests
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

describe('JSX Attributes and Props', () => {
    it('should parse string attributes', () => {
        const code = `<input type="text" placeholder="Enter name" />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(2);
        
        // Check type="text"
        const attr1 = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        expect(attr1.kind).toBe('JSXAttribute');
        const name1 = attr1.name as AST.JSXIdentifier;
        expect(name1.name).toBe('type');
        const value1 = attr1.value as AST.StringLiteral;
        expect(value1.kind).toBe('StringLiteral');
        expect(value1.parts[0].value).toBe('text');
        
        // Check placeholder="Enter name"
        const attr2 = jsx.openingElement.attributes[1] as AST.JSXNormalAttribute;
        const name2 = attr2.name as AST.JSXIdentifier;
        expect(name2.name).toBe('placeholder');
        const value2 = attr2.value as AST.StringLiteral;
        expect(value2.parts[0].value).toBe('Enter name');
    });

    it('should parse expression attributes', () => {
        const code = `<Button onClick={handleClick} disabled={isDisabled} />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(2);
        
        // Check onClick={handleClick}
        const attr1 = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        expect(attr1.kind).toBe('JSXAttribute');
        const name1 = attr1.name as AST.JSXIdentifier;
        expect(name1.name).toBe('onClick');
        const value1 = attr1.value as AST.JSXExpressionContainer;
        expect(value1.kind).toBe('JSXExpressionContainer');
        const expr1 = value1.expression as AST.Identifier;
        expect(expr1.kind).toBe('Identifier');
        expect(expr1.name).toBe('handleClick');
        
        // Check disabled={isDisabled}
        const attr2 = jsx.openingElement.attributes[1] as AST.JSXNormalAttribute;
        const name2 = attr2.name as AST.JSXIdentifier;
        expect(name2.name).toBe('disabled');
        const value2 = attr2.value as AST.JSXExpressionContainer;
        expect(value2.kind).toBe('JSXExpressionContainer');
    });

    it('should parse spread props', () => {
        const code = `<Component {...props} />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(1);
        
        // Check spread attribute
        const spread = jsx.openingElement.attributes[0] as AST.JSXSpreadAttribute;
        expect(spread.kind).toBe('JSXSpreadAttribute');
        const arg = spread.argument as AST.Identifier;
        expect(arg.kind).toBe('Identifier');
        expect(arg.name).toBe('props');
    });

    it('should parse mixed spread and regular props', () => {
        const code = `<Component {...defaultProps} size="large" {...overrides} />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(3);
        
        // Check first spread
        const spread1 = jsx.openingElement.attributes[0] as AST.JSXSpreadAttribute;
        expect(spread1.kind).toBe('JSXSpreadAttribute');
        
        // Check size attribute
        const attr = jsx.openingElement.attributes[1] as AST.JSXNormalAttribute;
        expect(attr.kind).toBe('JSXAttribute');
        const attrName = attr.name as AST.JSXIdentifier;
        expect(attrName.name).toBe('size');
        
        // Check second spread
        const spread2 = jsx.openingElement.attributes[2] as AST.JSXSpreadAttribute;
        expect(spread2.kind).toBe('JSXSpreadAttribute');
    });

    it('should parse boolean attributes', () => {
        const code = `<button disabled hidden autoFocus />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(3);
        
        // All boolean attributes should have null values
        jsx.openingElement.attributes.forEach((attr, i) => {
            const jsxAttr = attr as AST.JSXNormalAttribute;
            expect(jsxAttr.kind).toBe('JSXAttribute');
            expect(jsxAttr.value).toBeNull();
            
            const name = jsxAttr.name as AST.JSXIdentifier;
            if (i === 0) expect(name.name).toBe('disabled');
            if (i === 1) expect(name.name).toBe('hidden');
            if (i === 2) expect(name.name).toBe('autoFocus');
        });
    });

    it('should parse style object', () => {
        const code = `<div style={{color: 'red', fontSize: 16}} />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(1);
        
        // Check style attribute with object expression
        const attr = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        expect(attr.kind).toBe('JSXAttribute');
        const name = attr.name as AST.JSXIdentifier;
        expect(name.name).toBe('style');
        const value = attr.value as AST.JSXExpressionContainer;
        expect(value.kind).toBe('JSXExpressionContainer');
        const objExpr = value.expression as AST.ObjectLiteral;
        expect(objExpr.kind).toBe('ObjectLiteral');
        expect(objExpr.properties).toHaveLength(2);
    });

    it('should parse event handlers', () => {
        const code = `<button onClick={() => setCount(count + 1)} onMouseEnter={handleHover} />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(2);
        
        // Check onClick with arrow function
        const attr1 = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        const name1 = attr1.name as AST.JSXIdentifier;
        expect(name1.name).toBe('onClick');
        const value1 = attr1.value as AST.JSXExpressionContainer;
        expect(value1.kind).toBe('JSXExpressionContainer');
        const arrow = value1.expression as AST.Lambda;
        expect(arrow.kind).toBe('Lambda');
        
        // Check onMouseEnter with identifier
        const attr2 = jsx.openingElement.attributes[1] as AST.JSXNormalAttribute;
        const name2 = attr2.name as AST.JSXIdentifier;
        expect(name2.name).toBe('onMouseEnter');
    });

    it('should parse both className and class', () => {
        const code = `
<div className="container">
    <span class="text-bold">Both work</span>
</div>`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        
        // Check outer div has className
        const divAttr = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        const divAttrName = divAttr.name as AST.JSXIdentifier;
        expect(divAttrName.name).toBe('className');
        
        // Check inner span has class
        const childElements = jsx.children.filter(c => c.kind === 'JSXElement');
        const span = childElements[0] as AST.JSXElement;
        const spanAttr = span.openingElement.attributes[0] as AST.JSXNormalAttribute;
        const spanAttrName = spanAttr.name as AST.JSXIdentifier;
        expect(spanAttrName.name).toBe('class');
    });

    it('should parse namespaced attributes', () => {
        const code = `<svg xmlns:xlink="http://www.w3.org/1999/xlink" />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(1);
        
        // Check namespaced attribute
        const attr = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        expect(attr.kind).toBe('JSXAttribute');
        const name = attr.name as AST.JSXNamespacedName;
        expect(name.kind).toBe('JSXNamespacedName');
        expect(name.namespace.name).toBe('xmlns');
        expect(name.name.name).toBe('xlink');
    });

    it('should parse data and aria attributes', () => {
        const code = `<div data-testid="component" aria-label="Close button" />`;
        const ast = parse(code);
        
        const stmt = ast.body[0] as AST.ExprStmt;
        const jsx = stmt.expr as AST.JSXElement;
        expect(jsx.kind).toBe('JSXElement');
        expect(jsx.openingElement.attributes).toHaveLength(2);
        
        // Check data-testid
        const attr1 = jsx.openingElement.attributes[0] as AST.JSXNormalAttribute;
        const name1 = attr1.name as AST.JSXIdentifier;
        expect(name1.name).toBe('data-testid');
        const value1 = attr1.value as AST.StringLiteral;
        expect(value1.parts[0].value).toBe('component');
        
        // Check aria-label
        const attr2 = jsx.openingElement.attributes[1] as AST.JSXNormalAttribute;
        const name2 = attr2.name as AST.JSXIdentifier;
        expect(name2.name).toBe('aria-label');
        const value2 = attr2.value as AST.StringLiteral;
        expect(value2.parts[0].value).toBe('Close button');
    });
});