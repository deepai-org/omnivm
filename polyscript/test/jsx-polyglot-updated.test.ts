// JSX with PolyScript Multi-Paradigm Features Tests
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';

// Import test helpers
import {
  verifyJSXElement,
  verifyJSXFragment,
  verifyGenericType,
  verifyFunctionDecl,
  verifyChannelSend,
  verifyChannelReceive,
  findInAST,
  findAllInAST
} from './helpers/ast-verifiers';

import {
  findJSXElements,
  findJSXFragments,
  findGenericTypes,
  findAllAngleBracketUsages,
  analyzeAngleBrackets
} from './helpers/pattern-matchers';

import { getReturnValue } from './helpers/ast-compat';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('JSX with PolyScript Multi-Paradigm Features (UPDATED)', () => {
    it('should parse JSX with Python list comprehension', () => {
        const code = `
const TodoList = ({ items }) => (
    <ul>
        {[<li key={i}>{item}</li> for item, i in items if item.active]}
    </ul>
)`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify arrow function with JSX
        expect(ast.body).toHaveLength(1);
        const decl = ast.body[0] as AST.ConstDecl;
        expect(decl.kind).toBe('ConstDecl');
        expect(decl.names[0].name).toBe('TodoList');
        
        const arrow = decl.values[0] as AST.Lambda;
        expect(arrow.kind).toBe('Lambda');
        
        // Verify JSX ul element
        const ulElement = arrow.body as AST.JSXElement;
        verifyJSXElement(ulElement, 'ul');
        
        // Should have comprehension with JSX inside
        expect(ulElement.children.length).toBeGreaterThan(0);
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        const tagNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        
        expect(tagNames).toContain('ul');
        expect(tagNames).toContain('li');
        
        // Verify no angle bracket confusion
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(2); // ul and li
    });

    it('should parse JSX with Go channels', () => {
        const code = `
function AsyncComponent() {
    const ch = make(chan<JSX.Element>)
    
    go async () => {
        const data = await fetch('/api')
        ch <- <DataView data={data} />
    }
    
    return <div>{<- ch}</div>
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify function structure
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        verifyFunctionDecl(func, 'AsyncComponent');
        
        const body = func.body as AST.Block;
        expect(body.statements.length).toBeGreaterThanOrEqual(3);
        
        // Find channel type: chan<JSX.Element>
        const genericTypes = findGenericTypes(ast);
        const chanType = genericTypes.find(g => 
            g.base.kind === 'Identifier' && g.base.name === 'chan'
        );
        expect(chanType).toBeDefined();
        if (chanType) {
            expect(chanType.args.length).toBe(1);
            // JSX.Element is represented as a type
            const arg = chanType.args[0];
            // For now, just check it exists
            expect(arg).toBeDefined();
        }
        
        // Find channel operations
        const usage = findAllAngleBracketUsages(ast);
        
        // Should have channel send: ch <- <DataView />
        expect(usage.channels.sends.length).toBeGreaterThan(0);
        
        // Should have channel receive: <- ch
        expect(usage.channels.receives.length).toBeGreaterThan(0);
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        const tagNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        
        expect(tagNames).toContain('DataView');
        expect(tagNames).toContain('div');
        
        // Verify angle bracket disambiguation
        const stats = analyzeAngleBrackets(ast);
        expect(stats.jsxCount).toBe(2);
        expect(stats.channelCount).toBe(2);
        expect(stats.genericCount).toBeGreaterThanOrEqual(1); // chan<JSX.Element>
    });

    it('should parse JSX with Ruby blocks', () => {
        const code = `
def render_list(items)
    <ul>
        {items.each do |item|
            <li>{item.name}</li>
        end}
    </ul>
end`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify Ruby def with JSX
        expect(ast.body).toHaveLength(1);
        const def = ast.body[0] as AST.FuncDecl;
        expect(def.kind).toBe('FuncDecl');
        expect(def.name.name).toBe('render_list');
        expect(def.params.length).toBe(1);
        
        // Verify JSX elements
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(2);
        
        const ul = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'ul';
        });
        verifyJSXElement(ul!, 'ul');
        
        const li = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'li';
        });
        verifyJSXElement(li!, 'li');
        
        // Verify no angle bracket confusion
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.jsx.elements.length).toBe(2);
    });

    it('should parse JSX with pattern matching', () => {
        const code = `
function StatusIcon({ status }) {
    return match status {
        case 'loading' => <Spinner />
        case 'success' => <CheckIcon color="green" />
        case 'error' => <ErrorIcon color="red" />
        default => <QuestionIcon />
    }
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify function with match returning JSX
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        verifyFunctionDecl(func, 'StatusIcon');
        
        const body = func.body as AST.Block;
        const returnStmt = body.statements[0] as AST.Return;
        expect(returnStmt.kind).toBe('Return');
        
        const returnValue = getReturnValue(returnStmt);
        const match = returnValue as any;
        expect(match.kind).toBe('Match');
        expect(match.expr.name).toBe('status');
        expect(match.arms.length).toBe(4);
        
        // Each arm should have JSX element as result
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(4);
        
        const tagNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        
        expect(tagNames).toContain('Spinner');
        expect(tagNames).toContain('CheckIcon');
        expect(tagNames).toContain('ErrorIcon');
        expect(tagNames).toContain('QuestionIcon');
        
        // CheckIcon should have color prop
        const checkIcon = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'CheckIcon';
        });
        expect(checkIcon!.openingElement.attributes.length).toBe(1);
    });

    it('should parse JSX with Python decorators', () => {
        const code = `
@observer
@inject('store')
class TodoView:
    def render(self):
        return (
            <div>
                <h1>{self.store.title}</h1>
                {self.store.todos.map(todo => 
                    <TodoItem key={todo.id} {...todo} />
                )}
            </div>
        )`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify decorated class with JSX method
        expect(ast.body).toHaveLength(1);
        const classDecl = ast.body[0] as AST.ClassDecl;
        expect(classDecl.kind).toBe('ClassDecl');
        expect(classDecl.name.name).toBe('TodoView');
        
        // Verify decorators
        expect(classDecl.decorators).toBeDefined();
        expect(classDecl.decorators!.length).toBe(2);
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        const tagNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        
        expect(tagNames).toContain('div');
        expect(tagNames).toContain('h1');
        expect(tagNames).toContain('TodoItem');
        
        // TodoItem should have spread attributes
        const todoItem = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'TodoItem';
        });
        const hasSpread = todoItem!.openingElement.attributes.some(
            attr => attr.kind === 'JSXSpreadAttribute'
        );
        expect(hasSpread).toBe(true);
    });

    it('should parse JSX with C# properties', () => {
        const code = `
class Component {
    public string Title { get; set; }
    
    public Render() => (
        <div>
            <h1>{this.Title}</h1>
        </div>
    )
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify class with JSX method
        expect(ast.body).toHaveLength(1);
        const classDecl = ast.body[0] as AST.ClassDecl;
        expect(classDecl.kind).toBe('ClassDecl');
        expect(classDecl.name.name).toBe('Component');
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(2);
        
        verifyJSXElement(jsxElements[0], 'div');
        verifyJSXElement(jsxElements[1], 'h1');
        
        // h1 should have expression container child
        const h1 = jsxElements[1];
        const hasExprContainer = h1.children.some(
            child => child.kind === 'JSXExpressionContainer'
        );
        expect(hasExprContainer).toBe(true);
    });

    it('should parse JSX with Rust Option', () => {
        const code = `
fn render_user(user: Option<User>) -> JSX.Element {
    match user {
        Some(u) => <UserCard name={u.name} avatar={u.avatar} />,
        None => <GuestCard />
    }
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify function with Option type
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        verifyFunctionDecl(func, 'render_user', { paramCount: 1 });
        
        // Verify parameter type: Option<User>
        const paramType = func.params[0].type;
        verifyGenericType(paramType, 'Option', 1);
        
        // Verify match expression
        const body = func.body as AST.Block;
        const stmt = body.statements[0];
        // Match expressions should have kind 'Match'
        if (stmt.kind === 'Match') {
            const match = stmt as AST.Match;
            expect(match.arms.length).toBe(2);
        } else {
            // Fallback for switch-based implementation
            expect(stmt.kind).toBe('Switch');
        }
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(2);
        
        const userCard = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'UserCard';
        });
        verifyJSXElement(userCard!, 'UserCard', { attributeCount: 2 });
        
        const guestCard = jsxElements.find(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' && name.name === 'GuestCard';
        });
        verifyJSXElement(guestCard!, 'GuestCard', { selfClosing: true });
        
        // Verify angle bracket usage
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.generics.length).toBe(1); // Option<User>
        expect(usage.jsx.elements.length).toBe(2);
    });

    it('should parse JSX with PHP variables', () => {
        const code = `
function renderTemplate($title, $items) {
    return (
        <div>
            <h1>{$title}</h1>
            {$items->map($item => 
                <li>{$item->name}</li>
            )}
        </div>
    )
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify function with PHP-style parameters
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        verifyFunctionDecl(func, 'renderTemplate', { paramCount: 2 });
        
        // Parameters should have $ prefix
        expect(func.params[0].name.kind).toBe('Identifier');
        expect((func.params[0].name as AST.Identifier).name).toBe('$title');
        expect(func.params[1].name.kind).toBe('Identifier');
        expect((func.params[1].name as AST.Identifier).name).toBe('$items');
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(3);
        
        verifyJSXElement(jsxElements[0], 'div');
        verifyJSXElement(jsxElements[1], 'h1');
        verifyJSXElement(jsxElements[2], 'li');
    });

    it('should parse JSX with multiple paradigms', () => {
        const code = `
// Mix of Python, Go, and JavaScript
async def render_async_data():
    ch := make(chan<Data>)
    
    go fetch_data(ch)
    
    data := <- ch
    items = [item for item in data if item.visible]
    
    return (
        <>
            {items.map(item => (
                <Card key={item.id}>
                    {match item.type {
                        case 'text' => <TextContent {...item} />
                        case 'image' => <ImageContent src={item.url} />
                        default => <DefaultContent />
                    }}
                </Card>
            ))}
        </>
    )`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify async Python function
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        expect(func.kind).toBe('FuncDecl');
        expect(func.name.name).toBe('render_async_data');
        expect(func.async).toBe(true);
        
        // Find channel operations
        const usage = findAllAngleBracketUsages(ast);
        expect(usage.channels.receives.length).toBeGreaterThan(0);
        
        // Find generic type: chan<Data>
        const chanType = findGenericTypes(ast).find(g =>
            g.base.kind === 'Identifier' && g.base.name === 'chan'
        );
        expect(chanType).toBeDefined();
        
        // Find JSX fragment
        const fragments = findJSXFragments(ast);
        expect(fragments.length).toBe(1);
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        const tagNames = jsxElements.map(el => {
            const name = el.openingElement.name;
            return name.kind === 'JSXIdentifier' ? name.name : '';
        });
        
        expect(tagNames).toContain('Card');
        expect(tagNames).toContain('TextContent');
        expect(tagNames).toContain('ImageContent');
        expect(tagNames).toContain('DefaultContent');
        
        // Verify angle bracket statistics
        const stats = analyzeAngleBrackets(ast);
        expect(stats.jsxCount).toBe(5); // 1 fragment + 4 elements
        expect(stats.genericCount).toBeGreaterThanOrEqual(1); // chan<Data>
        expect(stats.channelCount).toBeGreaterThanOrEqual(1); // <- ch
    });

    it('should parse JSX with defer and using', () => {
        const code = `
function ResourceComponent() {
    using file = openFile('data.json')
    defer cleanup()
    
    const data = JSON.parse(file.read())
    
    return (
        <DataTable>
            {data.rows.map(row => 
                <TableRow key={row.id} data={row} />
            )}
        </DataTable>
    )
}`;
        
        const ast = parseCode(code);
        
        // ✅ STRONG: Verify function with using and defer
        expect(ast.body).toHaveLength(1);
        const func = ast.body[0] as AST.FuncDecl;
        verifyFunctionDecl(func, 'ResourceComponent');
        
        const body = func.body as AST.Block;
        
        // Find using statement
        const usingStmt = body.statements.find(s => s.kind === 'Using');
        expect(usingStmt).toBeDefined();
        
        // Find defer statement
        const deferStmt = body.statements.find(s => s.kind === 'Defer');
        expect(deferStmt).toBeDefined();
        
        // Find return with JSX
        const returnStmt = body.statements.find(s => s.kind === 'Return') as AST.Return;
        expect(returnStmt).toBeDefined();
        
        // Find JSX elements
        const jsxElements = findJSXElements(ast);
        expect(jsxElements.length).toBe(2);
        
        verifyJSXElement(jsxElements[0], 'DataTable');
        verifyJSXElement(jsxElements[1], 'TableRow', { attributeCount: 2 });
        
        // TableRow should have key and data props
        const tableRow = jsxElements[1];
        const attrs = tableRow.openingElement.attributes.filter(
            a => a.kind === 'JSXAttribute'
        ) as AST.JSXNormalAttribute[];
        const attrNames = attrs.map(a => a.name.name);
        expect(attrNames).toContain('key');
        expect(attrNames).toContain('data');
    });
});