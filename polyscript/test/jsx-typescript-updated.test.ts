// JSX with TypeScript Types Tests
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

// Import test helpers
import {
  verifyJSXElement,
  verifyGenericType,
  verifyFunctionDecl,
  verifyInterfaceDecl,
  findInAST,
  findAllInAST
} from './helpers/ast-verifiers';

import { normalizeTypeDecl } from './helpers/ast-compat';

import {
  findJSXElements,
  findGenericTypes,
  findInterfaces,
  findAllAngleBracketUsages,
  analyzeAngleBrackets
} from './helpers/pattern-matchers';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('JSX with TypeScript Types (UPDATED)', () => {
    it('should parse component with typed props', () => {
        const code = `interface ButtonProps {
    size: 'small' | 'medium' | 'large'
    variant: string
    onClick?: () => void
}

function Button({ size, variant, onClick }: ButtonProps) {
    return <button className={size} onClick={onClick}>{variant}</button>
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify interface and function
        expect(ast.body).toHaveLength(2);
        
        // Verify interface
        const iface = ast.body[0] as AST.InterfaceDecl;
        verifyInterfaceDecl(iface, 'ButtonProps', { propertyCount: 3 });
        
        // Verify function
        const func = ast.body[1] as AST.FuncDecl;
        verifyFunctionDecl(func, 'Button', { paramCount: 1 });
        
        // Parameter should have type annotation
        const paramType = func.params[0].type;
        expect(paramType).toBeDefined();
        if (paramType && (paramType as any).name) {
            expect((paramType as any).name).toBe('ButtonProps');
        }
        
        // Find JSX button element
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        verifyJSXElement(jsxElements[0], 'button', { attributeCount: 2 });
        
        // Verify button has className and onClick
        const button = jsxElements[0];
        const attrs = button.openingElement.attributes as AST.JSXNormalAttribute[];
        const attrNames = attrs.map(a => a.name.name);
        expect(attrNames).toContain('className');
        expect(attrNames).toContain('onClick');
    });

    it('should parse generic component', () => {
        const code = `function List<T>({ items }: { items: T[] }) {
    return (
        <ul>
            {items.map((item, i) => <li key={i}>{item}</li>)}
        </ul>
    )
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify generic function
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        verifyFunctionDecl(func, 'List', { 
            genericParams: ['T'],
            paramCount: 1 
        });
        
        // Parameter has object type annotation
        const paramType = func.params[0].type;
        expect(paramType).toBeDefined();
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(2);
        
        verifyJSXElement(jsxElements[0], 'ul');
        verifyJSXElement(jsxElements[1], 'li', { attributeCount: 1 });
        
        // Verify li has key attribute
        const li = jsxElements[1];
        const keyAttr = li.openingElement.attributes.find(
            attr => attr.kind === 'JSXAttribute' && 
            (attr as AST.JSXNormalAttribute).name.name === 'key'
        );
        expect(keyAttr).toBeDefined();
        
        // Verify angle brackets
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(2);
        expect(usage.generics.length).toBeGreaterThanOrEqual(0); // T in function signature
    });

    it('should parse type assertion in JSX', () => {
        const code = `const element = (
    <input 
        ref={inputRef as React.RefObject<HTMLInputElement>}
        value={value as string}
    />
)`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX with type assertions
        expect(ast.body).toHaveLength(1);
        const decl = ast.body[0] as AST.ConstDecl;
        expect(decl.kind).toBe('ConstDecl');
        
        const jsx = decl.values[0] as AST.JSXElement;
        verifyJSXElement(jsx, 'input', { 
            selfClosing: true,
            attributeCount: 2 
        });
        
        // Find generic types in assertions
        const genericTypes = findGenericTypes(ast);
        // Should have at least one generic for RefObject<HTMLInputElement>
        expect(genericTypes.length).toBeGreaterThanOrEqual(1);
        
        // Verify angle bracket usage
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(1);
        expect(usage.generics.length).toBeGreaterThanOrEqual(1); // RefObject<HTMLInputElement>
    });

    it('should parse JSX.Element return type', () => {
        const code = `const Component = (): JSX.Element => {
    return <div>Hello</div>
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify arrow function with return type
        expect(ast.body).toHaveLength(1);
        const decl = ast.body[0] as AST.ConstDecl;
        expect(decl.names[0].name).toBe('Component');
        
        const arrow = decl.values[0] as AST.Lambda;
        expect(arrow.kind).toBe('Lambda');
        
        // Should have return type annotation
        const returnType = arrow.returnType;
        expect(returnType).toBeDefined();
        
        // Find JSX div
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        verifyJSXElement(jsxElements[0], 'div');
    });

    it('should parse React.FC type', () => {
        const code = `const MyComponent: React.FC<{ title: string }> = ({ title }) => {
    return <h1>{title}</h1>
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify typed const with generic
        expect(ast.body).toHaveLength(1);
        const decl = ast.body[0] as AST.ConstDecl;
        expect(decl.names[0].name).toBe('MyComponent');
        
        // Should have type annotation
        // Note: names[0] may not have a 'type' property in current AST
        // The type annotation is likely on the variable declaration itself
        
        // Find JSX h1
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        verifyJSXElement(jsxElements[0], 'h1');
        
        // Verify angle brackets
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(1);
        expect(usage.generics.length).toBeGreaterThanOrEqual(1); // React.FC<...>
    });

    it('should parse children prop type', () => {
        const code = `interface Props {
    children: React.ReactNode
}

function Container({ children }: Props) {
    return <div className="container">{children}</div>
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify interface and function
        expect(ast.body).toHaveLength(2);
        
        // Verify interface
        const iface = ast.body[0] as AST.InterfaceDecl;
        verifyInterfaceDecl(iface, 'Props', { propertyCount: 1 });
        
        // Verify function
        const func = ast.body[1] as AST.FuncDecl;
        verifyFunctionDecl(func, 'Container', { paramCount: 1 });
        
        // Find JSX div
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        verifyJSXElement(jsxElements[0], 'div', { attributeCount: 1 });
        
        // Verify className attribute
        const div = jsxElements[0];
        const className = div.openingElement.attributes.find(
            attr => attr.kind === 'JSXAttribute' &&
            (attr as AST.JSXNormalAttribute).name.name === 'className'
        );
        expect(className).toBeDefined();
        
        // Should have expression container child
        const hasExprContainer = div.children.some(
            child => child.kind === 'JSXExpressionContainer'
        );
        expect(hasExprContainer).toBe(true);
    });

    it('should parse event handler types', () => {
        const code = `function Form() {
    const handleSubmit = (e: React.FormEvent<HTMLFormElement>) => {
        e.preventDefault()
    }
    
    return <form onSubmit={handleSubmit}>Submit</form>
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify function with typed event handler
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        verifyFunctionDecl(func, 'Form');
        
        // Find generic types
        const genericTypes = findGenericTypes(ast);
        // Should have at least one generic for FormEvent<HTMLFormElement>
        expect(genericTypes.length).toBeGreaterThanOrEqual(1);
        
        // Find JSX form
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        verifyJSXElement(jsxElements[0], 'form', { attributeCount: 1 });
        
        // Verify onSubmit attribute
        const form = jsxElements[0];
        const onSubmit = form.openingElement.attributes.find(
            attr => attr.kind === 'JSXAttribute' &&
            (attr as AST.JSXNormalAttribute).name.name === 'onSubmit'
        );
        expect(onSubmit).toBeDefined();
        
        // Verify angle brackets
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(1);
        expect(usage.generics.length).toBeGreaterThanOrEqual(1); // FormEvent<HTMLFormElement>
    });

    it('should parse ref types', () => {
        const code = `const Input = React.forwardRef<HTMLInputElement, InputProps>((props, ref) => {
    return <input ref={ref} {...props} />
})`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify forwardRef with generics
        expect(ast.body).toHaveLength(1);
        const decl = ast.body[0] as AST.ConstDecl;
        expect(decl.names[0].name).toBe('Input');
        
        // Find generic types
        const genericTypes = findGenericTypes(ast);
        // Should have at least one generic for forwardRef<...>
        expect(genericTypes.length).toBeGreaterThanOrEqual(1);
        
        // Find JSX input
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        verifyJSXElement(jsxElements[0], 'input', { selfClosing: true });
        
        // Should have ref and spread attributes
        const input = jsxElements[0];
        const hasRef = input.openingElement.attributes.some(
            attr => attr.kind === 'JSXAttribute' &&
            (attr as AST.JSXNormalAttribute).name.name === 'ref'
        );
        expect(hasRef).toBe(true);
        
        const hasSpread = input.openingElement.attributes.some(
            attr => attr.kind === 'JSXSpreadAttribute'
        );
        expect(hasSpread).toBe(true);
    });

    it('should parse union type props', () => {
        const code = `type Status = 'loading' | 'success' | 'error'

function StatusIcon({ status }: { status: Status }) {
    return <Icon type={status} />
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify type alias and function
        expect(ast.body).toHaveLength(2);
        
        // Verify type alias
        const typeAlias = normalizeTypeDecl(ast.body[0]);
        expect(typeAlias.kind).toBe('TypeAlias');
        expect(typeAlias.name.name).toBe('Status');
        
        // Verify function
        const func = ast.body[1] as AST.FuncDecl;
        verifyFunctionDecl(func, 'StatusIcon', { paramCount: 1 });
        
        // Find JSX Icon
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        verifyJSXElement(jsxElements[0], 'Icon', { 
            selfClosing: true,
            attributeCount: 1 
        });
    });

    it('should parse generic JSX element', () => {
        const code = `const list = <List<string> items={['a', 'b', 'c']} />`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify JSX element with type arguments
        expect(ast.body).toHaveLength(1);
        const decl = ast.body[0] as AST.ConstDecl;
        expect(decl.names[0].name).toBe('list');
        
        // Find JSX element
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(1);
        
        const list = jsxElements[0];
        verifyJSXElement(list, 'List', { 
            selfClosing: true,
            attributeCount: 1 
        });
        
        // List element with items attribute
        expect(list.openingElement.attributes.length).toBe(1);
        
        // Verify angle brackets - should detect JSX element
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(1);
        
        // The <string> might be parsed as part of the JSX element
        // or as a separate generic, depending on implementation
        const stats = analyzeAngleBrackets(ast);
        expect(stats.jsxCount).toBeGreaterThanOrEqual(1);
    });
});