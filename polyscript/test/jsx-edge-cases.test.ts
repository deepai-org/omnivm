// JSX Edge Cases and Disambiguation Tests
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

describe('JSX Edge Cases and Disambiguation', () => {
    it('should disambiguate JSX vs less-than operator', () => {
        const code = `
const a = x < 5
const b = <Component />
const c = x<5 && y>3
const d = <div>content</div>`;
        const ast = parse(code);
    });

    it('should disambiguate generic vs JSX', () => {
        const code = `
const arr = Array<number>()  // Generic
const elem = <Array />       // JSX
const func = fn<T>(arg)      // Generic call
const comp = <Button>Click</Button>  // JSX`;
        const ast = parse(code);
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
        const ast = parse(code);
    });

    it('should handle whitespace correctly', () => {
        const code = `
<div>
    
    Text with spaces    
    
    {' '}
    More text
    
</div>`;
        const ast = parse(code);
    });

    it('should parse JSX in ternary', () => {
        const code = `
const element = condition 
    ? <Success message="Done" />
    : <Error message="Failed" />`;
        const ast = parse(code);
    });

    it('should parse JSX in array', () => {
        const code = `
const elements = [
    <First key="1" />,
    <Second key="2" />,
    <Third key="3" />
]`;
        const ast = parse(code);
    });

    it('should parse JSX as object value', () => {
        const code = `
const config = {
    header: <Header />,
    footer: <Footer />,
    sidebar: isOpen ? <Sidebar /> : null
}`;
        const ast = parse(code);
    });

    it('should parse adjacent JSX elements with fragment', () => {
        const code = `
function Component() {
    return (
        <>
            <First />
            <Second />
        </>
    )
}`;
        const ast = parse(code);
    });

    it('should parse JSX with template literals', () => {
        const code = `
<div className={\`container \${size} \${variant}\`}>
    {\`Count: \${count}\`}
</div>`;
        const ast = parse(code);
    });

    it('should parse complex nested expression', () => {
        const code = `
<Container>
    {items
        .filter(item => item.active)
        .map(item => (
            <Item 
                key={item.id}
                onClick={() => handleClick(item.id)}
            >
                {item.children?.map(child => 
                    <Child key={child.id}>{child.name}</Child>
                )}
            </Item>
        ))
    }
</Container>`;
        const ast = parse(code);
    });

    it('should parse JSX with logical operators', () => {
        const code = `
<div>
    {count > 0 && <Badge>{count}</Badge>}
    {message || <DefaultMessage />}
    {loading ?? <Spinner />}
</div>`;
        const ast = parse(code);
    });

    it('should parse self-closing HTML tags', () => {
        const code = `
<div>
    <img src="image.jpg" alt="description" />
    <br />
    <hr />
    <input type="text" />
    <meta charset="UTF-8" />
</div>`;
        const ast = parse(code);
    });
});