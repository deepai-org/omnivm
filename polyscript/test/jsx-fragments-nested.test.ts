// JSX Fragments and Nested Elements Tests
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

describe('JSX Fragments and Nested Elements', () => {
    it('should parse simple fragment', () => {
        const code = `<>Hello World</>`;
        const ast = parse(code);
    });

    it('should parse fragment with multiple children', () => {
        const code = `
<>
    <Header />
    <Main />
    <Footer />
</>`;
        const ast = parse(code);
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
        const ast = parse(code);
    });

    it('should parse fragment in expression', () => {
        const code = `const element = <>First<br />Second</>;`;
        const ast = parse(code);
    });

    it('should parse conditional rendering', () => {
        const code = `
<div>
    {isLoading ? <Spinner /> : <Content />}
    {error && <ErrorMessage />}
</div>`;
        const ast = parse(code);
    });

    it('should parse map with keys', () => {
        const code = `
<ul>
    {items.map(item => (
        <li key={item.id}>{item.name}</li>
    ))}
</ul>`;
        const ast = parse(code);
    });

    it('should parse nested fragments', () => {
        const code = `
<>
    <div>
        <>
            <span>Nested</span>
            <span>Fragment</span>
        </>
    </div>
</>`;
        const ast = parse(code);
    });

    it('should parse JSX comments', () => {
        const code = `
<div>
    {/* This is a JSX comment */}
    <span>Content</span>
    {/* 
        Multi-line
        comment 
    */}
</div>`;
        const ast = parse(code);
    });

    it('should parse spread children', () => {
        const code = `
<Container>
    {...childElements}
</Container>`;
        const ast = parse(code);
    });

    it('should parse namespaced components', () => {
        const code = `
<Form.Group>
    <Form.Label>Name</Form.Label>
    <Form.Control type="text" />
</Form.Group>`;
        const ast = parse(code);
    });
});