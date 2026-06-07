import { Lexer, TokenType, Token } from '../src/lexer';

function tokenize(input: string): Token[] {
  return new Lexer(input).tokenize();
}

describe('Lexer Fuzz: No Crashes', () => {
  test('empty string', () => {
    const tokens = tokenize('');
    expect(tokens.length).toBeGreaterThanOrEqual(1); // at least EOF
    expect(tokens[tokens.length - 1].type).toBe(TokenType.EOF);
  });

  test('single letter', () => {
    expect(() => tokenize('a')).not.toThrow();
  });

  test('single digit', () => {
    expect(() => tokenize('7')).not.toThrow();
  });

  test('single operator char', () => {
    expect(() => tokenize('+')).not.toThrow();
  });

  test('single whitespace', () => {
    expect(() => tokenize(' ')).not.toThrow();
  });

  test('null byte', () => {
    expect(() => tokenize('\0')).not.toThrow();
  });

  test('unicode character', () => {
    expect(() => tokenize('\u{1F600}')).not.toThrow();
  });

  test('unterminated string', () => {
    expect(() => tokenize('"hello')).not.toThrow();
  });

  test('unterminated template literal', () => {
    expect(() => tokenize('`hello ${world')).not.toThrow();
  });

  test('unterminated regex', () => {
    expect(() => tokenize('= /unclosed')).not.toThrow();
  });

  test('unterminated block comment', () => {
    expect(() => tokenize('/* never closed')).not.toThrow();
  });

  test('very long line (10000 chars)', () => {
    const long = 'x' + '+x'.repeat(5000);
    expect(() => tokenize(long)).not.toThrow();
  });

  test('only whitespace', () => {
    expect(() => tokenize('     \t\t   ')).not.toThrow();
  });

  test('only newlines', () => {
    expect(() => tokenize('\n\n\n\n\n')).not.toThrow();
  });

  test('binary-looking data', () => {
    const binary = String.fromCharCode(...Array.from({ length: 256 }, (_, i) => i));
    expect(() => tokenize(binary)).not.toThrow();
  });

  test('nested template literals 5 deep', () => {
    expect(() => tokenize('`a${`b${`c${`d${`e`}`}`}`}`')).not.toThrow();
  });
});

describe('Lexer Fuzz: No Infinite Loops', () => {
  const TIMEOUT = 2000;

  test('many angle brackets', () => {
    expect(() => tokenize('<<<<<<<<<<<<')).not.toThrow();
  }, TIMEOUT);

  test('many sigils', () => {
    expect(() => tokenize('$$$$$$$$$$$$')).not.toThrow();
  }, TIMEOUT);

  test('unbalanced open braces', () => {
    expect(() => tokenize('{{{{{{{{{{')).not.toThrow();
  }, TIMEOUT);

  test('unbalanced close braces', () => {
    expect(() => tokenize('}}}}}}}}}}')).not.toThrow();
  }, TIMEOUT);

  test('unterminated block comment with /*', () => {
    expect(() => tokenize('/*')).not.toThrow();
  }, TIMEOUT);

  test('string with 1000 escaped quotes', () => {
    const s = '"' + '\\"'.repeat(1000) + '"';
    expect(() => tokenize(s)).not.toThrow();
  }, TIMEOUT);

  test('1000 newlines', () => {
    expect(() => tokenize('\n'.repeat(1000))).not.toThrow();
  }, TIMEOUT);

  test('mix of all operator chars', () => {
    expect(() => tokenize('+-*/%=!<>&|^~?.,:;@#$\\[]{}()')).not.toThrow();
  }, TIMEOUT);
});

describe('Lexer Fuzz: Deterministic', () => {
  test('identical tokens for same simple input', () => {
    const input = 'const x = 42 + y;';
    const a = tokenize(input);
    const b = tokenize(input);
    expect(a.length).toBe(b.length);
    for (let i = 0; i < a.length; i++) {
      expect(a[i].type).toBe(b[i].type);
      expect(a[i].value).toBe(b[i].value);
      expect(a[i].start).toBe(b[i].start);
      expect(a[i].end).toBe(b[i].end);
    }
  });

  test('identical tokens for complex input', () => {
    const input = 'return <Foo bar={x => x / 2} />;';
    const a = tokenize(input);
    const b = tokenize(input);
    expect(a.length).toBe(b.length);
    for (let i = 0; i < a.length; i++) {
      expect(a[i].type).toBe(b[i].type);
      expect(a[i].value).toBe(b[i].value);
    }
  });

  test('identical tokens for multiline input', () => {
    const input = 'if (x) {\n  return /regex/g\n}\nelse {\n  y / z\n}';
    const a = tokenize(input);
    const b = tokenize(input);
    expect(a.length).toBe(b.length);
    for (let i = 0; i < a.length; i++) {
      expect(a[i].type).toBe(b[i].type);
      expect(a[i].value).toBe(b[i].value);
    }
  });
});

describe('Lexer Fuzz: Span and Position Monotonicity', () => {
  const testInputs = [
    'const x = 42;',
    'function foo(a, b) { return a + b; }',
    'x\n.y\n.z',
    'return /regex/g + "string" + `template ${x}`',
    'if (a < b) { c > d }',
  ];

  test('token.start >= 0 and token.end >= token.start', () => {
    for (const input of testInputs) {
      const tokens = tokenize(input);
      for (const t of tokens) {
        expect(t.start).toBeGreaterThanOrEqual(0);
        expect(t.end).toBeGreaterThanOrEqual(t.start);
      }
    }
  });

  test('token.end <= source.length', () => {
    for (const input of testInputs) {
      const tokens = tokenize(input);
      for (const t of tokens) {
        expect(t.end).toBeLessThanOrEqual(input.length);
      }
    }
  });

  test('line numbers are monotonically non-decreasing', () => {
    for (const input of testInputs) {
      const tokens = tokenize(input);
      for (let i = 1; i < tokens.length; i++) {
        expect(tokens[i].line).toBeGreaterThanOrEqual(tokens[i - 1].line);
      }
    }
  });

  test('tokens on same line have increasing column', () => {
    for (const input of testInputs) {
      const tokens = tokenize(input).filter(
        t => t.type !== TokenType.VirtualSemi && t.type !== TokenType.EOF
      );
      for (let i = 1; i < tokens.length; i++) {
        if (tokens[i].line === tokens[i - 1].line) {
          expect(tokens[i].column).toBeGreaterThan(tokens[i - 1].column);
        }
      }
    }
  });

  test('non-virtual token starts are non-decreasing', () => {
    for (const input of testInputs) {
      const tokens = tokenize(input).filter(
        t => t.type !== TokenType.VirtualSemi
      );
      for (let i = 1; i < tokens.length; i++) {
        expect(tokens[i].start).toBeGreaterThanOrEqual(tokens[i - 1].start);
      }
    }
  });
});
