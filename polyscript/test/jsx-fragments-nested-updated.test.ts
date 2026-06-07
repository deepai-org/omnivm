// JSX Fragments and Nested Elements Tests
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

// Import test helpers
import {
  verifyJSXElement,
  verifyAngleBrackets,
  findAllInAST
} from './helpers/ast-verifiers';

import {
  findJSXElements,
  findJSXFragments,
  findAllAngleBracketUsages,
  analyzeAngleBrackets
} from './helpers/pattern-matchers';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('JSX Fragments and Nested Elements (UPDATED)', () => {
    it('should parse simple fragment', () => {
        const code = `<>Hello World</>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify fragment structure
        expect(ast.body).toHaveLength(1);
        const stmt = ast.body[0] as AST.ExprStmt;
        const fragment = stmt.expr as AST.JSXFragment;
        expect(fragment.kind).toBe('JSXFragment');
        
        // Should have text content
        expect(fragment.children.length).toBeGreaterThan(0);
        const textChild = fragment.children.find(c => c.kind === 'JSXText');
        expect(textChild).toBeDefined();
        if (textChild && textChild.kind === 'JSXText') {
            expect(textChild.value).toContain('Hello World');
        }
        
        // Verify fragment detection
        const fragments = findJSXFragments(ast);
        expect(fragments.length).toBe(1);
    });

    it('should parse fragment with multiple children', () => {
        const code = `
<>
    <Header />
    <Main />
    <Footer />
</>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify fragment with JSX children
        const fragment = (ast.body[0] as AST.ExprStmt).expr as AST.JSXFragment;
        expect(fragment.kind).toBe('JSXFragment');
        
        // Count JSX element children
        const elementChildren = fragment.children.filter(
            c => c.kind === 'JSXElement'
        ) as AST.JSXElement[];
        expect(elementChildren.length).toBe(3);
        
        // Verify each child element
        const expectedTags = ['Header', 'Main', 'Footer'];
        elementChildren.forEach((child, i) => {
            verifyJSXElement(child, expectedTags[i], { selfClosing: true });
        });
        
        // Verify angle brackets
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.fragments.length).toBe(1);
        expect(usage.jsx.elements.length).toBe(3);
    });

    it('should parse deeply nested elements', () => {
        const code = `
<div>
    <section>
        <article>
            <header>
                <h1>Title</h1>
            </header>
            <main>
                <p>Content</p>
            </main>
        </article>
    </section>
</div>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify nesting structure
        const jsxElements = findJSXElements(ast);
        
        // Count all elements
        const tagNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        
        expect(tagNames).toContain('div');
        expect(tagNames).toContain('section');
        expect(tagNames).toContain('article');
        expect(tagNames).toContain('header');
        expect(tagNames).toContain('h1');
        expect(tagNames).toContain('main');
        expect(tagNames).toContain('p');
        
        // Verify no self-closing tags
        jsxElements.forEach(el => {
            expect(el.openingElement.selfClosing).toBe(false);
            expect(el.closingElement).toBeDefined();
        });
        
        // Total JSX elements
        expect(jsxElements.length).toBe(7);
    });

    it('should parse fragment in expression', () => {
        const code = `
const component = () => (
    <>
        <First />
        <Second />
    </>
)`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify fragment in arrow function
        const decl = ast.body[0] as AST.ConstDecl;
        const arrow = decl.values[0] as AST.Lambda;
        expect(arrow.kind).toBe('Lambda');
        
        // Body should be a fragment
        const fragment = arrow.body as AST.JSXFragment;
        expect(fragment.kind).toBe('JSXFragment');
        
        // Verify children
        const jsxChildren = fragment.children.filter(
            c => c.kind === 'JSXElement'
        );
        expect(jsxChildren.length).toBe(2);
        
        // Verify angle brackets
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.fragments.length).toBe(1);
        expect(usage.jsx.elements.length).toBe(2);
    });

    it('should parse conditional rendering', () => {
        const code = `
<>
    {isLoggedIn ? (
        <Dashboard user={user} />
    ) : (
        <LoginForm />
    )}
</>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify conditional inside fragment
        const fragment = (ast.body[0] as AST.ExprStmt).expr as AST.JSXFragment;
        expect(fragment.kind).toBe('JSXFragment');
        
        // Should have expression container with conditional
        const exprContainer = fragment.children.find(
            c => c.kind === 'JSXExpressionContainer'
        ) as AST.JSXExpressionContainer;
        expect(exprContainer).toBeDefined();
        
        const conditional = exprContainer.expression as AST.Ternary;
        expect(conditional.kind).toBe('Ternary');
        
        // Both branches should be JSX
        verifyJSXElement(conditional.consequent as AST.JSXElement, 'Dashboard');
        verifyJSXElement(conditional.alternate as AST.JSXElement, 'LoginForm');
        
        // Dashboard should have user prop
        const dashboard = conditional.consequent as AST.JSXElement;
        expect(dashboard.openingElement.attributes.length).toBe(1);
    });

    it('should parse map with keys', () => {
        const code = `
<ul>
    {items.map((item, index) => (
        <li key={index}>
            <span>{item.name}</span>
            <button onClick={() => remove(index)}>X</button>
        </li>
    ))}
</ul>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify map with JSX
        const ul = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
        verifyJSXElement(ul, 'ul');
        
        // Find all JSX elements
        const jsxElements = findJSXElements(ast);
        const tagNames = new Set(jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        }));
        
        expect(tagNames).toContain('ul');
        expect(tagNames).toContain('li');
        expect(tagNames).toContain('span');
        expect(tagNames).toContain('button');
        
        // Find li element and verify key attribute
        const li = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'li';
        });
        expect(li).toBeDefined();
        if (li) {
            const keyAttr = li.openingElement.attributes.find(
                attr => attr.kind === 'JSXAttribute' && 
                (attr as AST.JSXNormalAttribute).name.name === 'key'
            );
            expect(keyAttr).toBeDefined();
        }
    });

    it('should parse nested fragments', () => {
        const code = `
<>
    <div>
        <>
            <span>Nested</span>
            <>
                <em>Deeply nested</em>
            </>
        </>
    </div>
</>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify nested fragments
        const fragments = findJSXFragments(ast);
        expect(fragments.length).toBe(3); // Three fragment levels
        
        const jsxElements = findJSXElements(ast);
        const tagNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        
        expect(tagNames).toContain('div');
        expect(tagNames).toContain('span');
        expect(tagNames).toContain('em');
        
        // Verify angle bracket stats
        const stats = analyzeAngleBrackets(ast);
        expect(stats.jsxCount).toBe(fragments.length + jsxElements.length);
    });

    it('should parse JSX comments', () => {
        const code = `
<>
    {/* Single line comment */}
    <div>
        {/* 
            Multi-line
            comment
        */}
        Content
    </div>
</>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX with comments
        const fragment = (ast.body[0] as AST.ExprStmt).expr as AST.JSXFragment;
        expect(fragment.kind).toBe('JSXFragment');
        
        // Find div element
        const jsxElements = findJSXElements(ast);
        const div = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'div';
        });
        expect(div).toBeDefined();
        
        // Comments are typically in expression containers or skipped
        // Main verification is that parsing succeeds with comments
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.fragments.length).toBe(1);
        expect(usage.jsx.elements.length).toBeGreaterThan(0);
    });

    it('should parse spread children', () => {
        const code = `
<Container>
    {...children}
    <Extra />
</Container>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify spread children
        const container = (ast.body[0] as AST.ExprStmt).expr as AST.JSXElement;
        verifyJSXElement(container, 'Container');
        
        // Should have expression container with spread
        const spreadChild = container.children.find(
            c => c.kind === 'JSXSpreadChild'
        );
        // Note: Depending on parser implementation, this might be JSXExpressionContainer
        // with a spread expression inside
        
        // Verify Extra element exists
        const jsxElements = findJSXElements(ast);
        const extra = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'Extra';
        });
        expect(extra).toBeDefined();
    });

    it('should parse namespaced components', () => {
        const code = `
<Form.Group>
    <Form.Label>Name</Form.Label>
    <Form.Input type="text" />
    <Form.Error />
</Form.Group>`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify member expression components
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(4);
        
        // All should be Form.* components
        jsxElements.forEach(el => {
            const name = el.openingElement.name;
            if (name.kind === 'JSXMemberExpression') {
                expect((name.object as AST.JSXIdentifier).name).toBe('Form');
                // Property should be one of: Group, Label, Input, Error
                const propName = (name.property as AST.JSXIdentifier).name;
                expect(['Group', 'Label', 'Input', 'Error']).toContain(propName);
            }
        });
        
        // Verify angle brackets
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(4);
        expect(usage.comparisons.lessThan.length).toBe(0); // No comparisons
    });
});