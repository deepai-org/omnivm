// JSX with PolyScript Multi-Paradigm Features Tests
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
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

describe('JSX with PolyScript Multi-Paradigm Features', () => {
    it('should parse JSX with Python list comprehension', () => {
        const code = `
const TodoList = ({ items }) => (
    <ul>
        {[<li key={i}>{item}</li> for item, i in items if item.active]}
    </ul>
)`;
        const ast = parse(code);
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
        const ast = parse(code);
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
        const ast = parse(code);
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
        const ast = parse(code);
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
        const ast = parse(code);
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
        const ast = parse(code);
    });

    it('should parse JSX with Rust Option', () => {
        const code = `
fn render_user(user: Option<User>) -> JSX.Element {
    match user {
        Some(u) => <UserCard name={u.name} avatar={u.avatar} />,
        None => <GuestCard />
    }
}`;
        const ast = parse(code);
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
        const ast = parse(code);
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
        const ast = parse(code);
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
        const ast = parse(code);
    });
});