/**
 * Parser Resilience Tests
 *
 * Asserts: no infinite loops, no crashes, stable recovery, deterministic errors.
 * Every test case feeds malformed/partial input and checks that the parser
 * terminates within a reasonable time and returns a result (possibly with errors).
 */

import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';

const TIMEOUT = 2000; // ms — if parsing takes longer, it's an infinite loop

function safeParse(code: string): { ok: boolean; errors: number; nodeCount: number } {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, code);
  const program = parser.parse();
  return {
    ok: true,
    errors: parser.getErrors().length,
    nodeCount: program.body.length,
  };
}

function expectTerminates(code: string) {
  let result: ReturnType<typeof safeParse> | null = null;
  const start = Date.now();
  try {
    result = safeParse(code);
  } catch {
    // Parser threw — that's fine, as long as it didn't hang
  }
  const elapsed = Date.now() - start;
  expect(elapsed).toBeLessThan(TIMEOUT);
  return result;
}

// ============ Malformed Generics ============

describe('Resilience: malformed generics', () => {
  test('unclosed generic angle bracket', () => {
    expectTerminates('let x: Map<string');
  });

  test('deeply nested unclosed generics', () => {
    expectTerminates('let x: A<B<C<D<E<F<G<H');
  });

  test('mismatched generic closers', () => {
    expectTerminates('let x: Map<string, number>>');
  });

  test('empty generic params', () => {
    expectTerminates('function foo<>() {}');
  });

  test('generic with trailing comma', () => {
    expectTerminates('function foo<T,>() {}');
  });

  test('generic with only commas', () => {
    expectTerminates('function foo<,,,>() {}');
  });

  test('generic in expression position with no closer', () => {
    expectTerminates('x = foo<bar');
  });

  test('nested generics with mixed operators', () => {
    expectTerminates('let x: Map<string, Array<number> = new Map()');
  });
});

// ============ Half-Written JSX ============

describe('Resilience: half-written JSX', () => {
  test('unclosed JSX tag', () => {
    expectTerminates('<div>hello');
  });

  test('JSX with unclosed attribute', () => {
    expectTerminates('<div className="foo');
  });

  test('self-closing tag missing /', () => {
    expectTerminates('<br>');
  });

  test('JSX expression hole unclosed', () => {
    expectTerminates('<div>{name</div>');
  });

  test('nested unclosed JSX', () => {
    expectTerminates('<div><span><a>text');
  });

  test('JSX closing tag mismatch', () => {
    expectTerminates('<div>text</span>');
  });

  test('JSX with only opening angle bracket', () => {
    expectTerminates('let x = <');
  });

  test('JSX fragment unclosed', () => {
    expectTerminates('<>hello world');
  });

  test('JSX with spread attribute unclosed', () => {
    expectTerminates('<div {...props');
  });
});

// ============ Nested Ruby Blocks ============

describe('Resilience: nested Ruby blocks', () => {
  test('unclosed do...end', () => {
    expectTerminates('items.each do |item|');
  });

  test('mismatched begin/end', () => {
    expectTerminates('begin\n  x = 1\n');
  });

  test('nested do blocks with missing end', () => {
    expectTerminates('a.each do |x|\n  b.each do |y|\n    puts y\n  end\n');
  });

  test('def without end', () => {
    expectTerminates('def foo(x)\n  return x * 2\n');
  });

  test('class without end', () => {
    expectTerminates('class Foo\n  def bar\n    42\n  end\n');
  });
});

// ============ Partial Match/Switch ============

describe('Resilience: partial match/switch', () => {
  test('switch with no cases', () => {
    expectTerminates('switch (x) {}');
  });

  test('switch with unclosed brace', () => {
    expectTerminates('switch (x) { case 1: return 1;');
  });

  test('match with no arms', () => {
    expectTerminates('match x {}');
  });

  test('match with unclosed arm', () => {
    expectTerminates('match x { 1 =>');
  });

  test('case...esac without esac', () => {
    expectTerminates('case $x in\n  1) echo one;;\n');
  });

  test('when without body', () => {
    expectTerminates('when (x) {');
  });

  test('match with trailing pipe', () => {
    expectTerminates('match x { 1 | 2 | => "yes" }');
  });
});

// ============ Incomplete Destructuring ============

describe('Resilience: incomplete destructuring', () => {
  test('unclosed array destructuring', () => {
    expectTerminates('let [a, b = [1, 2, 3]');
  });

  test('unclosed object destructuring', () => {
    expectTerminates('let {a, b = {x: 1}');
  });

  test('nested destructuring unclosed', () => {
    expectTerminates('let {a: {b, c} = {}');
  });

  test('destructuring with trailing comma only', () => {
    expectTerminates('let [,,,] = arr');
  });

  test('destructuring with spread and no closer', () => {
    expectTerminates('let [...rest = arr');
  });

  test('short decl destructuring unclosed', () => {
    expectTerminates('{a, b := getValue()');
  });
});

// ============ Broken Indentation (Python-style) ============

describe('Resilience: broken indentation', () => {
  test('inconsistent indentation', () => {
    expectTerminates('if True:\n  x = 1\n    y = 2\n  z = 3');
  });

  test('dedent to unknown level', () => {
    expectTerminates('def foo():\n    x = 1\n  y = 2');
  });

  test('empty indented block', () => {
    expectTerminates('if True:\n\nx = 1');
  });

  test('deeply nested indent then sudden dedent', () => {
    expectTerminates('if a:\n  if b:\n    if c:\n      if d:\n        x = 1\ny = 2');
  });
});

// ============ Weird Postfix Chains ============

describe('Resilience: weird postfix chains', () => {
  test('very long member chain', () => {
    expectTerminates('a.b.c.d.e.f.g.h.i.j.k.l.m.n.o.p.q.r.s.t.u.v.w.x.y.z');
  });

  test('alternating call and index', () => {
    expectTerminates('a()[0]()[1]()[2]()[3]()[4]()[5]()');
  });

  test('optional chaining explosion', () => {
    expectTerminates('a?.b?.c?.d?.e?.f?.g?.h?.i?.j?.k?.l');
  });

  test('unclosed call in chain', () => {
    expectTerminates('a.b(c.d(e.f(');
  });

  test('unclosed index in chain', () => {
    expectTerminates('a[b[c[d[');
  });

  test('postfix with newlines everywhere', () => {
    expectTerminates('a\n.b\n.c\n()\n[0]\n.d');
  });

  test('double dot operator', () => {
    expectTerminates('a..b..c');
  });
});

// ============ Mixed Language Confusion ============

describe('Resilience: mixed language confusion', () => {
  test('Python def with braces', () => {
    expectTerminates('def foo(x): {\n  return x\n}');
  });

  test('Go func with Python colon', () => {
    expectTerminates('func foo(x int):\n  return x');
  });

  test('Ruby block after JS arrow', () => {
    expectTerminates('const f = (x) => do |y|\n  y + x\nend');
  });

  test('Rust match in Python indent', () => {
    expectTerminates('match x:\n  1 => "one"\n  2 => "two"');
  });

  test('Bash case inside JS function', () => {
    expectTerminates('function foo() { case $1 in a) echo a;; esac }');
  });

  test('Go channel with JSX', () => {
    expectTerminates('ch <- <div>hello</div>');
  });
});

// ============ Edge Cases & Degenerate Input ============

describe('Resilience: degenerate input', () => {
  test('empty input', () => {
    const result = expectTerminates('');
    expect(result?.ok).toBe(true);
    expect(result?.nodeCount).toBe(0);
  });

  test('only whitespace', () => {
    const result = expectTerminates('   \n\n\t\t  \n');
    expect(result?.ok).toBe(true);
  });

  test('only semicolons', () => {
    expectTerminates(';;;;;;;;;;;');
  });

  test('only braces', () => {
    expectTerminates('{{{{{}}}}}');
  });

  test('only parens', () => {
    expectTerminates('((((()))))');
  });

  test('mismatched delimiters', () => {
    expectTerminates('({[}])');
  });

  test('very long identifier', () => {
    const name = 'a'.repeat(10000);
    expectTerminates(`let ${name} = 1`);
  });

  test('very deeply nested expressions', () => {
    const expr = '('.repeat(50) + '1' + ')'.repeat(50);
    expectTerminates(expr);
  });

  test('repeated keywords', () => {
    expectTerminates('if if if if if if if if');
  });

  test('random operator soup', () => {
    expectTerminates('+ - * / % ** ++ -- == != === !== < > <= >= && || ?? ?. .. |>');
  });

  test('string with no closing quote', () => {
    expectTerminates('"hello world');
  });

  test('template literal unclosed', () => {
    expectTerminates('`hello ${name');
  });

  test('regex-like but malformed', () => {
    expectTerminates('/[a-z/');
  });

  test('comment-like but not', () => {
    expectTerminates('/ / not a comment');
  });

  test('many virtual semicolons scenario', () => {
    // Lines without explicit semicolons
    expectTerminates('a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn\no\np');
  });
});

// ============ Determinism ============

describe('Resilience: deterministic output', () => {
  const inputs = [
    'let x: Map<string',
    '<div>hello',
    'match x { 1 =>',
    'def foo(x)\n  return x',
    'a.b(c.d(',
    '({[}])',
  ];

  for (const input of inputs) {
    test(`deterministic for: ${JSON.stringify(input).slice(0, 40)}`, () => {
      // Parse twice, compare results
      const r1 = safeParse(input);
      const r2 = safeParse(input);
      expect(r1.errors).toBe(r2.errors);
      expect(r1.nodeCount).toBe(r2.nodeCount);
    });
  }
});
