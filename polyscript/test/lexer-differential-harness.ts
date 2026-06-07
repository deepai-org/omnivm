/**
 * Lexer Differential Harness
 *
 * Captures lexer token streams as JSON snapshots for regression detection.
 *
 * Usage:
 *   npx ts-node test/lexer-differential-harness.ts baseline   # save snapshots
 *   npx ts-node test/lexer-differential-harness.ts check      # compare against snapshots
 */

import * as fs from 'fs';
import * as path from 'path';
import { Lexer, Token } from '../src/lexer';

interface TokenSnapshot {
  type: string;
  value: string;
  start: number;
  end: number;
  line: number;
  column: number;
  indentCol?: number;
  newline?: boolean;
  virtualSemi?: boolean;
}

interface LexerSnapshot {
  name: string;
  input: string;
  tokens: TokenSnapshot[];
}

const SNAPSHOT_DIR = path.join(__dirname, 'lexer-snapshots');

function tokenToSnapshot(t: Token): TokenSnapshot {
  const snap: TokenSnapshot = {
    type: t.type,
    value: t.value,
    start: t.start,
    end: t.end,
    line: t.line,
    column: t.column,
  };
  if (t.indentCol !== undefined) snap.indentCol = t.indentCol;
  if (t.newline) snap.newline = t.newline;
  if (t.virtualSemi) snap.virtualSemi = t.virtualSemi;
  return snap;
}

const INLINE_CORPUS: Record<string, string> = {
  // JSX vs generics ambiguity
  'jsx-generic-component': '<Component<T> />',
  'jsx-array-type': 'const x: Array<string> = []',
  'jsx-div': '<div>hello</div>',
  'jsx-after-return': 'return <Foo bar={1} />',

  // Regex vs division
  'regex-div-chain': 'x / y / z',
  'regex-after-if': 'if (x) /pattern/g',
  'regex-after-return': 'return /regex/i',
  'regex-mixed': 'a / b + /regex/',

  // Heredocs
  'heredoc-basic': '<<EOF\nhello\nEOF',
  'heredoc-var': 'x = <<HEREDOC\nline1\nline2\nHEREDOC',

  // Sigil identifiers
  'sigil-variable': '$variable',
  'sigil-private': '$_private',
  'sigil-question': '$?',
  'sigil-bang': '$!',
  'sigil-brace': '${param}',
  'sigil-paren': '$(command)',

  // Comment edge cases
  'comment-line': '// line',
  'comment-block': '/* block */',
  'comment-hash': '# hash',
  'comment-dash': '-- dash',
  'comment-html': '<!-- html -->',
  'comment-inline': 'x = 1 // inline',
  'comment-nested-block': '/* outer /* inner */ end */',

  // MASI newline suppression
  'masi-continuation-op': 'a +\nb',
  'masi-jsx-multiline': '<div\n  className="x"\n/>',
  'masi-indent-continuation': 'obj\n  .method()',
  'masi-return-newline': 'return\nvalue',
};

function snapshotLexer(name: string, source: string): LexerSnapshot {
  const lexer = new Lexer(source);
  const tokens = lexer.tokenize();
  return {
    name,
    input: source,
    tokens: tokens.map(tokenToSnapshot),
  };
}

function ensureDir(dir: string): void {
  if (!fs.existsSync(dir)) {
    fs.mkdirSync(dir, { recursive: true });
  }
}

function snapshotPath(name: string): string {
  return path.join(SNAPSHOT_DIR, `${name}.json`);
}

function saveBaseline(): void {
  ensureDir(SNAPSHOT_DIR);
  let count = 0;
  for (const [name, source] of Object.entries(INLINE_CORPUS)) {
    const snap = snapshotLexer(name, source);
    fs.writeFileSync(snapshotPath(name), JSON.stringify(snap, null, 2));
    count++;
  }
  console.log(`Baseline saved: ${count} snapshots → ${SNAPSHOT_DIR}`);
}

function checkAgainstBaseline(): void {
  if (!fs.existsSync(SNAPSHOT_DIR)) {
    console.error('No snapshots found. Run with "baseline" first.');
    process.exit(1);
  }

  let diffs = 0;
  let checked = 0;

  for (const [name, source] of Object.entries(INLINE_CORPUS)) {
    const file = snapshotPath(name);
    if (!fs.existsSync(file)) {
      console.log(`NEW: ${name} (no baseline snapshot)`);
      diffs++;
      continue;
    }

    const baseline: LexerSnapshot = JSON.parse(fs.readFileSync(file, 'utf-8'));
    const current = snapshotLexer(name, source);
    checked++;

    if (baseline.tokens.length !== current.tokens.length) {
      console.log(`DIFF: ${name} — token count: ${baseline.tokens.length} → ${current.tokens.length}`);
      diffs++;
      continue;
    }

    const problems: string[] = [];
    for (let i = 0; i < baseline.tokens.length; i++) {
      const bt = baseline.tokens[i];
      const ct = current.tokens[i];
      const fields: (keyof TokenSnapshot)[] = ['type', 'value', 'start', 'end', 'line', 'column', 'indentCol', 'newline', 'virtualSemi'];
      for (const f of fields) {
        if (bt[f] !== ct[f]) {
          problems.push(`token[${i}].${f}: ${JSON.stringify(bt[f])} → ${JSON.stringify(ct[f])}`);
        }
      }
    }

    if (problems.length > 0) {
      diffs++;
      console.log(`DIFF: ${name}`);
      for (const p of problems) {
        console.log(`  - ${p}`);
      }
    }
  }

  if (diffs === 0) {
    console.log(`All ${checked} snapshots match baseline.`);
  } else {
    console.log(`\n${diffs} difference(s) found across ${checked} checked snapshots.`);
    process.exit(1);
  }
}

const cmd = process.argv[2];
if (cmd === 'baseline') {
  saveBaseline();
} else if (cmd === 'check') {
  checkAgainstBaseline();
} else {
  console.log('Usage: npx ts-node test/lexer-differential-harness.ts [baseline|check]');
  process.exit(1);
}
