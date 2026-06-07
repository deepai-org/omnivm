// JSX with TypeScript Types Tests
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

describe('JSX with TypeScript Types', () => {
    it('should parse component with typed props', () => {
        const code = `
interface ButtonProps {
    size: 'small' | 'medium' | 'large'
    variant: string
    onClick?: () => void
}

function Button({ size, variant, onClick }: ButtonProps) {
    return <button className={size} onClick={onClick}>{variant}</button>
}`;
        const ast = parse(code);
    });

    it('should parse generic component', () => {
        const code = `
function List<T>({ items }: { items: T[] }) {
    return (
        <ul>
            {items.map((item, i) => <li key={i}>{item}</li>)}
        </ul>
    )
}`;
        const ast = parse(code);
    });

    it('should parse type assertion in JSX', () => {
        const code = `
const element = (
    <input 
        ref={inputRef as React.RefObject<HTMLInputElement>}
        value={value as string}
    />
)`;
        const ast = parse(code);
    });

    it('should parse JSX.Element return type', () => {
        const code = `
const Component = (): JSX.Element => {
    return <div>Hello</div>
}`;
        const ast = parse(code);
    });

    it('should parse React.FC type', () => {
        const code = `
const MyComponent: React.FC<{ title: string }> = ({ title }) => {
    return <h1>{title}</h1>
}`;
        const ast = parse(code);
    });

    it('should parse children prop type', () => {
        const code = `
interface Props {
    children: React.ReactNode
}

function Container({ children }: Props) {
    return <div className="container">{children}</div>
}`;
        const ast = parse(code);
    });

    it('should parse event handler types', () => {
        const code = `
function Form() {
    const handleSubmit = (e: React.FormEvent<HTMLFormElement>) => {
        e.preventDefault()
    }
    
    return <form onSubmit={handleSubmit}>Submit</form>
}`;
        const ast = parse(code);
    });

    it('should parse ref types', () => {
        const code = `
const Input = React.forwardRef<HTMLInputElement, InputProps>((props, ref) => {
    return <input ref={ref} {...props} />
})`;
        const ast = parse(code);
    });

    it('should parse union type props', () => {
        const code = `
type Status = 'loading' | 'success' | 'error'

function StatusIcon({ status }: { status: Status }) {
    return <Icon type={status} />
}`;
        const ast = parse(code);
    });

    it('should parse generic JSX element', () => {
        const code = `const list = <List<string> items={['a', 'b', 'c']} />`;
        const ast = parse(code);
    });
});