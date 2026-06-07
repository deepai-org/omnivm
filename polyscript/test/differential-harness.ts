/**
 * Differential Parse Harness
 *
 * Captures parser output (AST JSON, error list, token consumption) for a corpus
 * of .poly files. Run before/after each refactoring chunk to detect regressions
 * in speculative parsing, rollback behavior, and error ordering.
 *
 * Usage:
 *   npx ts-node test/differential-harness.ts baseline   # save baseline
 *   npx ts-node test/differential-harness.ts check      # compare against baseline
 */

import * as fs from 'fs';
import * as path from 'path';
import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';

interface Snapshot {
  file: string;
  success: boolean;
  ast: string;            // JSON stringified AST
  errors: ErrorEntry[];
  tokenCount: number;     // total tokens fed to parser
}

interface ErrorEntry {
  message: string;
  line: number | undefined;
  column: number | undefined;
}

const CORPUS_DIR = path.join(__dirname, '..', 'examples');
const BASELINE_PATH = path.join(__dirname, '..', '.differential-baseline.json');

// Also include inline test corpus for invalid/ambiguous cases
const INLINE_CORPUS: Record<string, string> = {
  // Invalid: unclosed brace
  'invalid-unclosed-brace': 'function foo() {\n  let x = 1;\n',
  // Invalid: unexpected token
  'invalid-unexpected': 'let x = ;',
  // Ambiguous: JSX vs type assertion
  'ambiguous-jsx-type': 'const x = <Foo>bar</Foo>;\nconst y = <string>value;',
  // Ambiguous: generic vs comparison
  'ambiguous-generic-cmp': 'const a = f<number>(1);\nconst b = x < y;',
  // Ruby block ambiguity
  'ambiguous-ruby-block': 'items.each do |x|\n  puts x\nend',
  // Match as identifier vs keyword
  'ambiguous-match': 'const match = 5;\nmatch x {\n  1 => "one",\n  _ => "other"\n}',
  // Python indent blocks
  'indent-blocks': 'def greet(name):\n  print(f"Hello {name}")\n  return True',
  // Bash case-esac
  'bash-case-esac': 'case $x in\n  foo) echo "foo";;\n  *) echo "default";;\nesac',
  // Go for-range
  'go-for-range': 'for task := range tasks {\n  process(task)\n}',
  // Deep generics
  'deep-generics': 'const x: Map<string, Promise<Result<Option<Vec<number>>, Error>>> = new Map();',
  // Invalid: missing closing paren
  'invalid-missing-paren': 'function foo(a, b {\n  return a + b;\n}',
  // Multiple errors
  'invalid-multi-error': 'let = 5;\nconst = ;\nfunction {}',
};

function collectCorpus(): { name: string; source: string }[] {
  const corpus: { name: string; source: string }[] = [];

  // Collect .poly files from examples/
  if (fs.existsSync(CORPUS_DIR)) {
    for (const file of fs.readdirSync(CORPUS_DIR)) {
      if (file.endsWith('.poly')) {
        corpus.push({
          name: `file:${file}`,
          source: fs.readFileSync(path.join(CORPUS_DIR, file), 'utf-8')
        });
      }
    }
  }

  // Collect test.poly from root
  const rootPoly = path.join(__dirname, '..', 'test.poly');
  if (fs.existsSync(rootPoly)) {
    corpus.push({
      name: 'file:test.poly',
      source: fs.readFileSync(rootPoly, 'utf-8')
    });
  }

  // Add inline corpus
  for (const [name, source] of Object.entries(INLINE_CORPUS)) {
    corpus.push({ name: `inline:${name}`, source });
  }

  return corpus;
}

function snapshotParse(name: string, source: string): Snapshot {
  const lexer = new Lexer(source);
  const tokens = lexer.tokenize();
  const tokenCount = tokens.length;
  const parser = new Parser(tokens, source);

  let ast: any;
  let success = true;
  try {
    ast = parser.parse();
  } catch (e) {
    ast = { error: String(e) };
    success = false;
  }

  const errors = parser.getErrors().map(e => ({
    message: e.message,
    line: e.token?.line,
    column: e.token?.column
  }));

  return {
    file: name,
    success,
    ast: JSON.stringify(ast, null, 0),
    errors,
    tokenCount
  };
}

function saveBaseline(): void {
  const corpus = collectCorpus();
  const snapshots = corpus.map(c => snapshotParse(c.name, c.source));
  fs.writeFileSync(BASELINE_PATH, JSON.stringify(snapshots, null, 2));
  console.log(`Baseline saved: ${snapshots.length} files → ${BASELINE_PATH}`);
}

function checkAgainstBaseline(): void {
  if (!fs.existsSync(BASELINE_PATH)) {
    console.error('No baseline found. Run with "baseline" first.');
    process.exit(1);
  }

  const baseline: Snapshot[] = JSON.parse(fs.readFileSync(BASELINE_PATH, 'utf-8'));
  const corpus = collectCorpus();
  const current = corpus.map(c => snapshotParse(c.name, c.source));

  const baseMap = new Map(baseline.map(s => [s.file, s]));
  let diffs = 0;

  for (const snap of current) {
    const base = baseMap.get(snap.file);
    if (!base) {
      console.log(`NEW: ${snap.file} (no baseline entry)`);
      continue;
    }

    const problems: string[] = [];

    if (base.success !== snap.success) {
      problems.push(`success: ${base.success} → ${snap.success}`);
    }
    if (base.ast !== snap.ast) {
      problems.push('AST JSON differs');
    }
    if (base.tokenCount !== snap.tokenCount) {
      problems.push(`tokenCount: ${base.tokenCount} → ${snap.tokenCount}`);
    }
    if (base.errors.length !== snap.errors.length) {
      problems.push(`error count: ${base.errors.length} → ${snap.errors.length}`);
    } else {
      for (let i = 0; i < base.errors.length; i++) {
        const be = base.errors[i];
        const se = snap.errors[i];
        if (be.message !== se.message || be.line !== se.line || be.column !== se.column) {
          problems.push(`error[${i}] differs: "${be.message}" @${be.line}:${be.column} → "${se.message}" @${se.line}:${se.column}`);
        }
      }
    }

    if (problems.length > 0) {
      diffs++;
      console.log(`DIFF: ${snap.file}`);
      for (const p of problems) {
        console.log(`  - ${p}`);
      }
    }
  }

  // Check for removed files
  const currentNames = new Set(current.map(s => s.file));
  for (const base of baseline) {
    if (!currentNames.has(base.file)) {
      console.log(`REMOVED: ${base.file} (in baseline but not in current corpus)`);
      diffs++;
    }
  }

  if (diffs === 0) {
    console.log(`✓ All ${current.length} snapshots match baseline.`);
  } else {
    console.log(`\n✗ ${diffs} difference(s) found across ${current.length} snapshots.`);
    process.exit(1);
  }
}

const cmd = process.argv[2];
if (cmd === 'baseline') {
  saveBaseline();
} else if (cmd === 'check') {
  checkAgainstBaseline();
} else {
  console.log('Usage: npx ts-node test/differential-harness.ts [baseline|check]');
  process.exit(1);
}
