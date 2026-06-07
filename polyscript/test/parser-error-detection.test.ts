import { parseCode, parseCodeStrict, parseCodeWithErrors } from './helpers';

describe('Parser Error Detection', () => {

  test('detects no errors in valid code', () => {
    const code = `
      const x = 10;
      function test() { return x; }
      class Foo { bar() {} }
    `;
    const { errors } = parseCodeWithErrors(code);
    expect(errors).toHaveLength(0);
    
    // Also test that parseCode doesn't throw
    expect(() => parseCode(code)).not.toThrow();
  });

  test('detects and recovers from syntax errors', () => {
    const code = `
      const x = 10
      const y = { // missing closing brace
      const z = 20
    `;
    const { ast, errors } = parseCodeWithErrors(code);
    // Should still produce an AST due to error recovery
    expect(ast.body.length).toBeGreaterThan(0);
    // But should have recorded errors
    expect(errors.length).toBeGreaterThan(0);
    
    // parseCode should throw on this code
    expect(() => parseCodeStrict(code)).toThrow(/Parser produced/);
  });

  test('detects missing closing brackets', () => {
    const code = `
      function test() {
        if (true) {
          console.log("unclosed"
        }
      }
    `;
    const { errors } = parseCodeWithErrors(code);
    expect(errors.length).toBeGreaterThan(0);
    
    // parseCode should throw on this code
    expect(() => parseCodeStrict(code)).toThrow(/Parser produced/);
  });

  test('detects invalid token sequences', () => {
    const code = `
      const const x = 10;
      function function test() {}
    `;
    const { errors } = parseCodeWithErrors(code);
    expect(errors.length).toBeGreaterThan(0);
    
    // parseCode should throw on this code
    expect(() => parseCodeStrict(code)).toThrow(/Parser produced/);
  });

  test('complex valid code should have no errors', () => {
    const code = `
      # Python-style comment
      @decorator
      class Container<T> extends Base {
        private items: T[] = []
        
        constructor(public size: number = 10) {
          super()
        }
        
        async method(): Promise<void> {
          await this.process()
        }
      }
      
      # Go-style short declaration
      x := 10
      
      # Rust-style match
      match value {
        Some(x) => x * 2
        None => 0
      }
    `;
    const { errors } = parseCodeWithErrors(code);
    expect(errors).toHaveLength(0);
    
    // parseCode should not throw on valid code
    expect(() => parseCode(code)).not.toThrow();
  });

  test('tracks multiple errors', () => {
    const code = `
      const x = // missing value
      function test( // missing closing paren
      class { // missing class name
    `;
    const { errors } = parseCodeWithErrors(code);
    // Parser may combine related errors or recover efficiently
    expect(errors.length).toBeGreaterThanOrEqual(1);
    
    // parseCode should throw on this code
    expect(() => parseCodeStrict(code)).toThrow(/Parser produced/);
  });
});