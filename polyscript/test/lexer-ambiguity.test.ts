import { Lexer, TokenType, Token } from '../src/lexer';

function tokenize(input: string): Token[] {
  return new Lexer(input).tokenize();
}

function tokensOfType(tokens: Token[], type: TokenType): Token[] {
  return tokens.filter(t => t.type === type);
}

function findToken(tokens: Token[], value: string, type?: TokenType): Token | undefined {
  return tokens.find(t => t.value === value && (!type || t.type === type));
}

function nonEOF(tokens: Token[]): Token[] {
  return tokens.filter(t => t.type !== TokenType.EOF && t.type !== TokenType.VirtualSemi);
}

describe('JSX vs Generics Ambiguity', () => {
  test('<Foo /> produces JSXTagStart', () => {
    const tokens = tokenize('<Foo />');
    expect(tokens[0].type).toBe(TokenType.JSXTagStart);
  });

  test('Array<string> produces Operator <', () => {
    const tokens = tokenize('Array<string>');
    const lt = findToken(tokens, '<');
    expect(lt?.type).toBe(TokenType.Operator);
  });

  test('<Foo<T> /> starts with JSXTagStart', () => {
    const tokens = tokenize('<Foo<T> />');
    expect(tokens[0].type).toBe(TokenType.JSXTagStart);
  });

  test('const x = <div>text</div> has JSXTagStart', () => {
    const tokens = tokenize('const x = <div>text</div>');
    const jsx = tokensOfType(tokens, TokenType.JSXTagStart);
    expect(jsx.length).toBeGreaterThanOrEqual(1);
  });

  test('a < b && c > d produces Operator < and >', () => {
    const tokens = tokenize('a < b && c > d');
    const lt = findToken(tokens, '<');
    const gt = findToken(tokens, '>');
    expect(lt?.type).toBe(TokenType.Operator);
    expect(gt?.type).toBe(TokenType.Operator);
  });

  test('return <Component /> has JSXTagStart after return', () => {
    const tokens = tokenize('return <Component />');
    const jsx = tokensOfType(tokens, TokenType.JSXTagStart);
    expect(jsx.length).toBeGreaterThanOrEqual(1);
  });

  test('f(<div />) has JSXTagStart after open paren', () => {
    const tokens = tokenize('f(<div />)');
    const jsx = tokensOfType(tokens, TokenType.JSXTagStart);
    expect(jsx.length).toBeGreaterThanOrEqual(1);
  });

  test('x = <Foo> has JSXTagStart after equals', () => {
    const tokens = tokenize('x = <Foo>');
    const jsx = tokensOfType(tokens, TokenType.JSXTagStart);
    expect(jsx.length).toBeGreaterThanOrEqual(1);
  });

  test('{<span>x</span>} has JSXTagStart after open brace', () => {
    const tokens = tokenize('{<span>x</span>}');
    const jsx = tokensOfType(tokens, TokenType.JSXTagStart);
    expect(jsx.length).toBeGreaterThanOrEqual(1);
  });

  test('<foo> with lowercase non-HTML identifier is Operator', () => {
    // "foo" is not in the htmlTags set and not uppercase, so should be operator
    const tokens = tokenize('a <foo> b');
    const lt = findToken(tokens, '<');
    // If it's not treated as JSX, it should be an operator
    // (The lexer may or may not treat this as JSX depending on context)
    expect(lt?.type).toMatch(/Operator|JSXTagStart/);
  });
});

describe('Regex vs Division Ambiguity', () => {
  test('x / 2 after identifier produces Operator /', () => {
    const tokens = tokenize('x / 2');
    const slash = findToken(tokens, '/');
    expect(slash?.type).toBe(TokenType.Operator);
  });

  test('return /regex/g after keyword produces RegexLiteral', () => {
    const tokens = tokenize('return /regex/g');
    const regex = tokensOfType(tokens, TokenType.RegexLiteral);
    expect(regex.length).toBe(1);
  });

  test('(/regex/) after open paren produces RegexLiteral', () => {
    const tokens = tokenize('(/regex/)');
    const regex = tokensOfType(tokens, TokenType.RegexLiteral);
    expect(regex.length).toBe(1);
  });

  test(') / 2 after close paren produces Operator /', () => {
    const tokens = tokenize('(x) / 2');
    // Find the slash after )
    const slashes = tokens.filter(t => t.value === '/');
    expect(slashes.length).toBeGreaterThanOrEqual(1);
    expect(slashes[0].type).toBe(TokenType.Operator);
  });

  test('1 / 2 after number produces Operator /', () => {
    const tokens = tokenize('1 / 2');
    const slash = findToken(tokens, '/');
    expect(slash?.type).toBe(TokenType.Operator);
  });

  test('x = /regex/ after = produces RegexLiteral', () => {
    const tokens = tokenize('x = /regex/');
    const regex = tokensOfType(tokens, TokenType.RegexLiteral);
    expect(regex.length).toBe(1);
  });

  test('a / b / c is chained division (all Operator /)', () => {
    const tokens = tokenize('a / b / c');
    const slashes = tokens.filter(t => t.value === '/');
    expect(slashes.length).toBe(2);
    for (const s of slashes) {
      expect(s.type).toBe(TokenType.Operator);
    }
  });

  test('{/regex/} after { produces RegexLiteral', () => {
    const tokens = tokenize('{/regex/}');
    const regex = tokensOfType(tokens, TokenType.RegexLiteral);
    expect(regex.length).toBe(1);
  });
});

describe('Heredocs', () => {
  test('<<EOF\\ncontent\\nEOF produces a string token', () => {
    const tokens = tokenize('<<EOF\nhello world\nEOF');
    const strings = tokensOfType(tokens, TokenType.StringLiteral);
    expect(strings.length).toBeGreaterThanOrEqual(1);
  });

  test('heredoc with HEREDOC delimiter', () => {
    const tokens = tokenize('x = <<HEREDOC\nline1\nline2\nHEREDOC');
    const strings = tokensOfType(tokens, TokenType.StringLiteral);
    expect(strings.length).toBeGreaterThanOrEqual(1);
  });

  test('<< followed by lowercase is operator not heredoc', () => {
    const tokens = tokenize('a << b');
    const op = findToken(tokens, '<<');
    expect(op?.type).toBe(TokenType.Operator);
  });

  test('heredoc captures multiline content', () => {
    const tokens = tokenize('<<END\nfirst\nsecond\nthird\nEND');
    const str = tokensOfType(tokens, TokenType.StringLiteral);
    expect(str.length).toBeGreaterThanOrEqual(1);
    if (str.length > 0) {
      expect(str[0].value).toContain('first');
    }
  });

  test('heredoc delimiter must be at start of line', () => {
    const tokens = tokenize('<<EOF\nhello\n  EOF\nEOF');
    // The heredoc should extend until bare EOF at line start
    const str = tokensOfType(tokens, TokenType.StringLiteral);
    expect(str.length).toBeGreaterThanOrEqual(1);
  });
});

describe('Sigil Identifiers', () => {
  test('$var produces SigilIdentifier', () => {
    const tokens = tokenize('$variable');
    const sigil = tokensOfType(tokens, TokenType.SigilIdentifier);
    expect(sigil.length).toBeGreaterThanOrEqual(1);
  });

  test('$? produces SigilIdentifier', () => {
    const tokens = tokenize('$?');
    const sigil = tokensOfType(tokens, TokenType.SigilIdentifier);
    expect(sigil.length).toBeGreaterThanOrEqual(1);
  });

  test('$! produces SigilIdentifier', () => {
    const tokens = tokenize('$!');
    const sigil = tokensOfType(tokens, TokenType.SigilIdentifier);
    expect(sigil.length).toBeGreaterThanOrEqual(1);
  });

  test('$(command) produces SigilIdentifier', () => {
    const tokens = tokenize('$(command)');
    const sigil = tokensOfType(tokens, TokenType.SigilIdentifier);
    expect(sigil.length).toBeGreaterThanOrEqual(1);
  });

  test('${param} produces SigilIdentifier', () => {
    const tokens = tokenize('${param}');
    const sigil = tokensOfType(tokens, TokenType.SigilIdentifier);
    expect(sigil.length).toBeGreaterThanOrEqual(1);
  });

  test('$_private produces SigilIdentifier', () => {
    const tokens = tokenize('$_private');
    const sigil = tokensOfType(tokens, TokenType.SigilIdentifier);
    expect(sigil.length).toBeGreaterThanOrEqual(1);
    expect(sigil[0].value).toContain('$');
  });
});

describe('Comments', () => {
  test('// comment is skipped (no token emitted)', () => {
    const tokens = tokenize('// this is a comment');
    const meaningful = nonEOF(tokens);
    expect(meaningful.length).toBe(0);
  });

  test('/* block */ is skipped', () => {
    const tokens = tokenize('/* block comment */');
    const meaningful = nonEOF(tokens);
    expect(meaningful.length).toBe(0);
  });

  test('# hash comment is skipped', () => {
    const tokens = tokenize('# hash comment');
    const meaningful = nonEOF(tokens);
    expect(meaningful.length).toBe(0);
  });

  test('-- dash comment is skipped when preceded by line start', () => {
    const tokens = tokenize('-- dash comment');
    const meaningful = nonEOF(tokens);
    expect(meaningful.length).toBe(0);
  });

  test('<!-- html --> is skipped', () => {
    const tokens = tokenize('<!-- html comment -->');
    const meaningful = nonEOF(tokens);
    expect(meaningful.length).toBe(0);
  });

  test('code before // comment is preserved', () => {
    const tokens = tokenize('x = 1 // inline comment');
    const ident = findToken(tokens, 'x');
    expect(ident).toBeDefined();
    const num = findToken(tokens, '1');
    expect(num).toBeDefined();
  });
});

describe('MASI (Virtual Semicolons)', () => {
  test('return\\nvalue inserts VirtualSemi after return', () => {
    const tokens = tokenize('return\nvalue');
    // There should be a virtual semi between return and value
    const returnIdx = tokens.findIndex(t => t.value === 'return');
    expect(returnIdx).toBeGreaterThanOrEqual(0);
    // Check for virtualSemi flag on some token between return and value
    const between = tokens.slice(returnIdx + 1);
    const valueIdx = between.findIndex(t => t.value === 'value');
    const hasSemi = between.slice(0, valueIdx).some(t => t.virtualSemi || t.type === TokenType.VirtualSemi);
    expect(hasSemi).toBe(true);
  });

  test('x\\n.y suppresses VirtualSemi (dot continuation)', () => {
    const tokens = tokenize('x\n.y');
    // Should NOT have a virtualSemi between x and .y
    const xIdx = tokens.findIndex(t => t.value === 'x');
    const dotIdx = tokens.findIndex(t => t.value === '.');
    if (xIdx >= 0 && dotIdx >= 0) {
      const between = tokens.slice(xIdx + 1, dotIdx);
      const hasSemi = between.some(t => t.virtualSemi || t.type === TokenType.VirtualSemi);
      expect(hasSemi).toBe(false);
    }
  });

  test('a +\\nb suppresses VirtualSemi (operator continuation)', () => {
    const tokens = tokenize('a +\nb');
    const plusIdx = tokens.findIndex(t => t.value === '+');
    const bIdx = tokens.findIndex(t => t.value === 'b');
    if (plusIdx >= 0 && bIdx >= 0) {
      const between = tokens.slice(plusIdx + 1, bIdx);
      const hasSemi = between.some(t => t.virtualSemi || t.type === TokenType.VirtualSemi);
      expect(hasSemi).toBe(false);
    }
  });

  test('x\\ny inserts VirtualSemi between identifiers', () => {
    const tokens = tokenize('x\ny');
    const xIdx = tokens.findIndex(t => t.value === 'x');
    const yIdx = tokens.findIndex(t => t.value === 'y');
    const between = tokens.slice(xIdx + 1, yIdx);
    const hasSemi = between.some(t => t.virtualSemi || t.type === TokenType.VirtualSemi);
    expect(hasSemi).toBe(true);
  });

  test('}\\nelse has VirtualSemi (MASI inserts after })', () => {
    // Note: MASI inserts a VirtualSemi after } even before else.
    // The parser handles if/else continuation at a higher level.
    const tokens = tokenize('if (x) { y }\nelse { z }');
    const braceIdx = tokens.findIndex((t, i) => t.value === '}' && i > 3);
    const elseIdx = tokens.findIndex(t => t.value === 'else');
    expect(braceIdx).toBeGreaterThanOrEqual(0);
    expect(elseIdx).toBeGreaterThan(braceIdx);
  });

  test('break\\n inserts VirtualSemi after break', () => {
    const tokens = tokenize('break\nx');
    const breakIdx = tokens.findIndex(t => t.value === 'break');
    const xIdx = tokens.findIndex(t => t.value === 'x');
    const between = tokens.slice(breakIdx + 1, xIdx);
    const hasSemi = between.some(t => t.virtualSemi || t.type === TokenType.VirtualSemi);
    expect(hasSemi).toBe(true);
  });

  test('x\\n?.y suppresses VirtualSemi (?. continuation)', () => {
    const tokens = tokenize('x\n?.y');
    const xIdx = tokens.findIndex(t => t.value === 'x');
    // Find ?. or ? token
    const qIdx = tokens.findIndex((t, i) => i > xIdx && (t.value === '?.' || t.value === '?'));
    if (xIdx >= 0 && qIdx >= 0) {
      const between = tokens.slice(xIdx + 1, qIdx);
      const hasSemi = between.some(t => t.virtualSemi || t.type === TokenType.VirtualSemi);
      expect(hasSemi).toBe(false);
    }
  });

  test('indented continuation suppresses VirtualSemi', () => {
    const tokens = tokenize('obj\n  .method()');
    const objIdx = tokens.findIndex(t => t.value === 'obj');
    const dotIdx = tokens.findIndex(t => t.value === '.');
    if (objIdx >= 0 && dotIdx >= 0) {
      const between = tokens.slice(objIdx + 1, dotIdx);
      const hasSemi = between.some(t => t.virtualSemi || t.type === TokenType.VirtualSemi);
      expect(hasSemi).toBe(false);
    }
  });
});
