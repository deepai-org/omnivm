#!/usr/bin/env node
/**
 * Registry-wide Rust round-trip oracle sweep.
 *
 * Generalizes test/rust-roundtrip.test.ts from 5 fixtures to every .rs file
 * of a curated set of pinned registry crates (full sources live in the
 * builder image at /opt/cargo/registry/src/<index>/<crate>-<ver>/src/).
 *
 * For each file the oracle is: wrap the file as
 *     <content>\n\nprint("done")\n
 * compile through the FULL pipeline (Lexer -> Parser -> RuntimeResolver ->
 * ManifestCodeGenerator), then require that
 *   - the pipeline produces no parse errors and does not throw,
 *   - every top-level item found by the production scanner
 *     (scanTopLevelRustItems) appears BYTE-IDENTICAL in the assembled Rust
 *     unit source (indexOf),
 *   - no Rust content leaks into non-rust ops, no shim is emitted for an
 *     internal-only fn, every export corresponds to a scanned top-level fn,
 *   - the polyglot tail survives as its own op (no runaway item scan).
 *
 * Classification per file:
 *   clean           all of the above hold
 *   parse-fail      pipeline throws / reports parse errors / times out
 *   slice-mismatch  compiles but >=1 top-level item is not byte-identical
 *                   in the unit (or the scanner found 0 items in a file
 *                   that clearly has item anchors)
 *   leak            Rust content landed in non-rust ops / bogus exports /
 *                   shim for an internal fn / tail swallowed
 *   skip            intentionally out of scope: empty file, file larger
 *                   than --max-kb (default 200)
 *
 * Usage:
 *   node scripts/rust-registry-sweep.js                      # default crates
 *   node scripts/rust-registry-sweep.js --crates=all         # every pinned crate
 *   node scripts/rust-registry-sweep.js --crates=serde,bytes # explicit list
 *   node scripts/rust-registry-sweep.js --out=fails.txt      # failure list to file
 *   node scripts/rust-registry-sweep.js --ratchet <expectations-file>
 *
 * Ratchet mode reads scripts/rust-registry-sweep-expectations.txt:
 *   first non-comment line:  min-pass-rate <percent>
 *   then one line per known-failing file:  <class> <relative-path>
 * Exit nonzero if the pass-rate drops below the floor OR any file NOT in
 * the known-fail list fails. Fixed files print a ratchet-update reminder.
 *
 * The sweep is deterministic: crates and files are walked in sorted order
 * and no timestamps are emitted. Run `npm run build` first (uses dist/).
 */
'use strict';

const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');
const readline = require('readline');

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

/**
 * Curated default crate set: moderate-size, idiomatically diverse crates
 * (~800 files). polars/sqlx/axum and the long tail are reachable via
 * --crates=all.
 */
const DEFAULT_CRATES = [
  'anyhow', 'base64', 'bytes', 'chrono', 'csv', 'csv-core',
  'futures-core', 'futures-util', 'itertools', 'itoa', 'log', 'memchr',
  'once_cell', 'rayon', 'rayon-core', 'regex', 'regex-automata',
  'regex-syntax', 'ryu', 'serde', 'serde_core', 'serde_json', 'sha2',
  'thiserror', 'url', 'uuid',
];

const DEFAULT_MAX_KB = 200;
const DEFAULT_TIMEOUT_MS = 5000;
const FAILURE_CLASSES = ['parse-fail', 'slice-mismatch', 'leak'];

function parseArgs(argv) {
  const args = {
    crates: null,        // null => default set; 'all' => every crate dir
    registry: process.env.OMNIVM_RUST_REGISTRY || null,
    maxKb: DEFAULT_MAX_KB,
    timeoutMs: DEFAULT_TIMEOUT_MS,
    jobs: Math.max(1, Math.min(4, require('os').cpus().length - 1)),
    out: null,
    ratchet: null,
    worker: false,
  };
  for (let i = 2; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--worker') args.worker = true;
    else if (a.startsWith('--crates=')) args.crates = a.slice(9);
    else if (a.startsWith('--registry=')) args.registry = a.slice(11);
    else if (a.startsWith('--max-kb=')) args.maxKb = Number(a.slice(9));
    else if (a.startsWith('--timeout-ms=')) args.timeoutMs = Number(a.slice(13));
    else if (a.startsWith('--jobs=')) args.jobs = Math.max(1, Number(a.slice(7)));
    else if (a.startsWith('--out=')) args.out = a.slice(6);
    else if (a === '--out') args.out = argv[++i];
    else if (a.startsWith('--ratchet=')) args.ratchet = a.slice(10);
    else if (a === '--ratchet') args.ratchet = argv[++i];
    else {
      console.error(`unknown argument: ${a}`);
      process.exit(2);
    }
  }
  return args;
}

// ---------------------------------------------------------------------------
// Worker mode: classify one file per stdin line, one JSON result per line.
// Kept in-process for speed; the parent enforces the per-file timeout and
// respawns this worker if a file hangs or crashes it.
// ---------------------------------------------------------------------------

function runWorker() {
  const { Lexer } = require('../dist/lexer');
  const { Parser } = require('../dist/parser');
  const { RuntimeResolver } = require('../dist/runtime-resolver');
  const { ManifestCodeGenerator } = require('../dist/codegen-omnivm');
  const { scanTopLevelRustItems } = require('../dist/rust-item-scanner');

  // Same pipeline as the compile() helper in test/rust-roundtrip.test.ts.
  function compile(code) {
    const tokens = new Lexer(code).tokenize();
    const parser = new Parser(tokens, code);
    const ast = parser.parse();
    const resolver = new RuntimeResolver();
    const annotated = resolver.resolve(ast, code);
    const gen = new ManifestCodeGenerator();
    const manifest = gen.generate(annotated);
    return { manifest, gen, parseErrors: parser.getErrors() };
  }

  const firstLine = (e) =>
    String((e && e.message) || e).split('\n')[0].slice(0, 200);

  // Loose textual anchor test: does the file plausibly contain module-level
  // Rust items at all? Used only to decide "0 items found" between a real
  // scanner miss (slice-mismatch) and an item-free file (skip).
  const ANCHOR_RE = new RegExp(
    '^\\s*(?:pub(?:\\s*\\([^)]*\\))?\\s+)?' +
    '(?:(?:async|unsafe|const)\\s+)*' +
    '(?:fn|struct|enum|union|trait|impl|mod|use|static|const|type|macro_rules|extern)\\b',
    'm'
  );

  function classifyFile(content) {
    if (content.trim().length === 0) return { class: 'skip', reason: 'empty' };

    let items;
    try {
      items = scanTopLevelRustItems(content);
    } catch (e) {
      return { class: 'parse-fail', reason: `scanner threw: ${firstLine(e)}` };
    }

    let manifest, gen, parseErrors;
    try {
      ({ manifest, gen, parseErrors } = compile(`${content}\n\nprint("done")\n`));
    } catch (e) {
      return { class: 'parse-fail', reason: `pipeline threw: ${firstLine(e)}` };
    }
    if (parseErrors.length > 0) {
      return {
        class: 'parse-fail',
        reason: `${parseErrors.length} parse error(s); first: ${firstLine(parseErrors[0])}`,
      };
    }

    const ops = manifest.ops || [];
    const funcDefs = ops.filter((o) => o.op === 'func_def' && o.bodyRuntime === 'rust');
    const rustUnit = gen.rustUnit;
    const unitSource = funcDefs.length > 0 ? funcDefs[0].source : (rustUnit && rustUnit.source) || '';
    const exports = funcDefs.length > 0 ? funcDefs[0].exports : (rustUnit && rustUnit.exports) || [];

    if (items.length === 0) {
      // Comment-only files (e.g. rayon's compile_fail/*.rs, which are one
      // big `/*! ```compile_fail ...``` */` doctest) genuinely have no
      // items: strip comments before probing for anchors so code samples
      // inside doc comments don't count.
      let code = content.replace(/\/\/[^\n]*/g, '');
      for (let prev = null; prev !== code; ) {
        prev = code;
        code = code.replace(/\/\*[\s\S]*?\*\//g, ' ');
      }
      if (ANCHOR_RE.test(code)) {
        return {
          class: 'slice-mismatch',
          reason: 'scanner found 0 top-level items but the file has item anchors',
        };
      }
      return { class: 'skip', reason: 'no-items' };
    }

    // --- leak checks (most specific diagnosis first) -----------------------
    const otherOps = JSON.stringify(ops.filter((o) => o.op !== 'func_def'));
    const topFnNames = new Set();
    for (const item of items) {
      if (item.itemKind === 'fn') for (const sig of item.fns) topFnNames.add(sig.name);
    }
    for (const ex of exports) {
      if (!topFnNames.has(ex)) {
        return { class: 'leak', reason: `export is not a scanned top-level fn: ${ex}` };
      }
    }
    for (const name of topFnNames) {
      if (exports.includes(name)) continue; // exported fns legitimately get shims
      if (new RegExp(`OmniVMCall_${name}\\b`).test(unitSource)) {
        return { class: 'leak', reason: `unexpected export shim for internal fn ${name}` };
      }
      if (otherOps.includes(`fn ${name}`)) {
        return { class: 'leak', reason: `fn ${name} leaked into non-rust ops` };
      }
    }
    for (const item of items) {
      if (!item.name) continue;
      if (['struct', 'enum', 'union', 'trait', 'mod'].includes(item.itemKind) &&
          otherOps.includes(`${item.itemKind} ${item.name}`)) {
        return {
          class: 'leak',
          reason: `${item.itemKind} ${item.name} leaked into non-rust ops`,
        };
      }
    }
    const lastOp = ops[ops.length - 1];
    if (!lastOp || !JSON.stringify(lastOp).includes('done')) {
      return { class: 'leak', reason: 'polyglot tail swallowed (no trailing print op)' };
    }

    // --- byte-identity ------------------------------------------------------
    const missing = items.filter((it) => unitSource.indexOf(it.text) === -1);
    if (missing.length > 0) {
      const first = missing[0];
      return {
        class: 'slice-mismatch',
        reason: `${missing.length}/${items.length} items not byte-identical; first: ` +
          `${first.itemKind} ${first.name || '<anon>'}`,
      };
    }
    for (const fd of funcDefs) {
      if (fd.source !== unitSource) {
        return { class: 'leak', reason: 'rust func_defs carry diverging unit sources' };
      }
    }
    return { class: 'clean', reason: '' };
  }

  const rl = readline.createInterface({ input: process.stdin, terminal: false });
  rl.on('line', (file) => {
    if (!file) return;
    let result;
    try {
      result = classifyFile(fs.readFileSync(file, 'utf8'));
    } catch (e) {
      result = { class: 'parse-fail', reason: `worker error: ${firstLine(e)}` };
    }
    process.stdout.write(JSON.stringify({ file, ...result }) + '\n');
  });
  rl.on('close', () => process.exit(0));
}

// ---------------------------------------------------------------------------
// Parent mode: file discovery
// ---------------------------------------------------------------------------

function findRegistryRoot(explicit) {
  const base = '/opt/cargo/registry/src';
  if (explicit) return explicit;
  if (!fs.existsSync(base)) {
    console.error(`registry not found at ${base}; pass --registry=DIR (run inside omnivm-builder)`);
    process.exit(2);
  }
  const dirs = fs.readdirSync(base).filter((d) =>
    fs.statSync(path.join(base, d)).isDirectory()).sort();
  if (dirs.length === 0) {
    console.error(`no registry index under ${base}`);
    process.exit(2);
  }
  return path.join(base, dirs[0]);
}

function resolveCrateDirs(registryRoot, cratesArg) {
  const allDirs = fs.readdirSync(registryRoot)
    .filter((d) => fs.statSync(path.join(registryRoot, d)).isDirectory())
    .sort();
  if (cratesArg === 'all') return allDirs;
  const wanted = cratesArg ? cratesArg.split(',').map((s) => s.trim()).filter(Boolean)
                           : DEFAULT_CRATES;
  const dirs = [];
  for (const name of [...wanted].sort()) {
    const re = new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}-[0-9]`);
    const matches = allDirs.filter((d) => re.test(d));
    if (matches.length === 0) {
      console.error(`note: crate not pinned in registry, skipping: ${name}`);
    }
    dirs.push(...matches);
  }
  return [...new Set(dirs)].sort();
}

function collectRsFiles(dir) {
  const out = [];
  const walk = (d) => {
    let entries;
    try { entries = fs.readdirSync(d, { withFileTypes: true }); } catch { return; }
    for (const e of entries.sort((a, b) => a.name.localeCompare(b.name))) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile() && e.name.endsWith('.rs')) out.push(p);
    }
  };
  walk(path.join(dir, 'src'));
  return out.sort();
}

// ---------------------------------------------------------------------------
// Parent mode: worker pool with per-file timeout
// ---------------------------------------------------------------------------

function sweep(files, registryRoot, args) {
  return new Promise((resolve) => {
    const queue = files.filter((f) => {
      const size = fs.statSync(f.abs).size;
      if (size === 0) { f.result = { class: 'skip', reason: 'empty' }; return false; }
      if (size > args.maxKb * 1024) {
        f.result = { class: 'skip', reason: `too-large (${Math.round(size / 1024)}KB > ${args.maxKb}KB)` };
        return false;
      }
      return true;
    });
    let next = 0;
    let inFlight = 0;
    let done = false;

    const finishIfDone = () => {
      if (!done && next >= queue.length && inFlight === 0) {
        done = true;
        resolve();
      }
    };

    const startWorker = () => {
      const w = spawn(process.execPath, [__filename, '--worker'], {
        cwd: path.dirname(__dirname), // polyscript/
        stdio: ['pipe', 'pipe', 'inherit'],
      });
      let current = null;
      let timer = null;
      const rl = readline.createInterface({ input: w.stdout, terminal: false });

      const feed = () => {
        if (next >= queue.length) { w.stdin.end(); finishIfDone(); return; }
        current = queue[next++];
        inFlight++;
        timer = setTimeout(() => {
          current.result = {
            class: 'parse-fail',
            reason: `timeout after ${args.timeoutMs}ms`,
          };
          inFlight--;
          current = null;
          w.removeAllListeners('exit');
          rl.close();
          w.kill('SIGKILL');
          startWorker(); // resume with a fresh worker
        }, args.timeoutMs);
        w.stdin.write(current.abs + '\n');
      };

      rl.on('line', (line) => {
        if (!current) return;
        clearTimeout(timer);
        let parsed;
        try { parsed = JSON.parse(line); }
        catch { parsed = { class: 'parse-fail', reason: 'unparseable worker reply' }; }
        current.result = { class: parsed.class, reason: parsed.reason || '' };
        inFlight--;
        current = null;
        feed();
      });

      w.on('exit', (code, signal) => {
        if (current) {
          clearTimeout(timer);
          current.result = {
            class: 'parse-fail',
            reason: `worker died (${signal || `exit ${code}`}) while compiling`,
          };
          inFlight--;
          current = null;
          startWorker();
        } else {
          finishIfDone();
        }
      });

      feed();
    };

    if (queue.length === 0) { resolve(); return; }
    for (let i = 0; i < Math.min(args.jobs, queue.length); i++) startWorker();
  });
}

// ---------------------------------------------------------------------------
// Reporting + ratchet
// ---------------------------------------------------------------------------

function pct(n, d) {
  return d === 0 ? '100.0' : ((100 * n) / d).toFixed(1);
}

function printReport(files) {
  const byCrate = new Map();
  for (const f of files) {
    const crate = f.rel.split('/')[0];
    if (!byCrate.has(crate)) {
      byCrate.set(crate, { files: 0, clean: 0, 'parse-fail': 0, 'slice-mismatch': 0, leak: 0, skip: 0 });
    }
    const s = byCrate.get(crate);
    s.files++;
    s[f.result.class]++;
  }

  const cols = ['CRATE', 'FILES', 'CLEAN', 'PARSE-FAIL', 'SLICE-MISMATCH', 'LEAK', 'SKIP', 'PASS-RATE'];
  const rows = [];
  const total = { files: 0, clean: 0, 'parse-fail': 0, 'slice-mismatch': 0, leak: 0, skip: 0 };
  for (const crate of [...byCrate.keys()].sort()) {
    const s = byCrate.get(crate);
    for (const k of Object.keys(total)) total[k] += s[k];
    const judged = s.files - s.skip;
    rows.push([crate, s.files, s.clean, s['parse-fail'], s['slice-mismatch'], s.leak, s.skip,
               `${pct(s.clean, judged)}%`]);
  }
  const judgedTotal = total.files - total.skip;
  rows.push(['TOTAL', total.files, total.clean, total['parse-fail'], total['slice-mismatch'],
             total.leak, total.skip, `${pct(total.clean, judgedTotal)}%`]);

  const widths = cols.map((c, i) => Math.max(c.length, ...rows.map((r) => String(r[i]).length)));
  const fmt = (r) => r.map((v, i) => String(v)[i === 0 ? 'padEnd' : 'padStart'](widths[i])).join('  ');
  console.log(fmt(cols));
  for (const r of rows) console.log(fmt(r));

  const failures = files
    .filter((f) => FAILURE_CLASSES.includes(f.result.class))
    .sort((a, b) => a.rel.localeCompare(b.rel));

  console.log('');
  console.log(`overall pass-rate: ${pct(total.clean, judgedTotal)}% ` +
              `(${total.clean}/${judgedTotal} judged, ${total.skip} skipped of ${total.files} files)`);
  const skipReasons = new Map();
  for (const f of files) {
    if (f.result.class !== 'skip') continue;
    const key = f.result.reason.split(' ')[0];
    skipReasons.set(key, (skipReasons.get(key) || 0) + 1);
  }
  if (skipReasons.size > 0) {
    console.log('skips: ' + [...skipReasons.keys()].sort()
      .map((k) => `${k}=${skipReasons.get(k)}`).join(' '));
  }

  if (failures.length > 0) {
    console.log('\nfailures:');
    for (const f of failures) {
      console.log(`${f.result.class.padEnd(14)} ${f.rel} — ${f.result.reason}`);
    }
  }
  return { failures, passRate: judgedTotal === 0 ? 100 : (100 * total.clean) / judgedTotal };
}

function parseExpectations(file) {
  const lines = fs.readFileSync(file, 'utf8').split('\n');
  let minPassRate = null;
  const known = new Map(); // rel path -> class
  for (const raw of lines) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const parts = line.split(/\s+/);
    if (minPassRate === null) {
      if (parts[0] !== 'min-pass-rate' || isNaN(Number(parts[1]))) {
        console.error(`expectations file ${file}: first line must be "min-pass-rate <percent>"`);
        process.exit(2);
      }
      minPassRate = Number(parts[1]);
      continue;
    }
    if (parts.length >= 2) known.set(parts[1], parts[0]);
  }
  if (minPassRate === null) {
    console.error(`expectations file ${file}: missing "min-pass-rate" line`);
    process.exit(2);
  }
  return { minPassRate, known };
}

function applyRatchet(expectationsFile, failures, passRate) {
  const { minPassRate, known } = parseExpectations(expectationsFile);
  const failingByPath = new Map(failures.map((f) => [f.rel, f.result.class]));

  const newFailures = failures.filter((f) => !known.has(f.rel));
  const fixed = [...known.keys()].filter((p) => !failingByPath.has(p)).sort();
  const classChanged = [...known.entries()]
    .filter(([p, c]) => failingByPath.has(p) && failingByPath.get(p) !== c)
    .map(([p, c]) => `${p} (${c} -> ${failingByPath.get(p)})`);

  console.log(`\nratchet: floor ${minPassRate}% | measured ${passRate.toFixed(1)}% | ` +
              `known-fail ${known.size} | new ${newFailures.length} | fixed ${fixed.length}`);

  let bad = false;
  if (passRate < minPassRate) {
    console.error(`RATCHET FAIL: pass-rate ${passRate.toFixed(1)}% dropped below floor ${minPassRate}%`);
    bad = true;
  }
  if (newFailures.length > 0) {
    console.error('RATCHET FAIL: files failing that are NOT in the known-fail list:');
    for (const f of newFailures) {
      console.error(`  ${f.result.class} ${f.rel} — ${f.result.reason}`);
    }
    bad = true;
  }
  if (classChanged.length > 0) {
    console.log('note: known-fail files changed class (still failing, not a regression):');
    for (const line of classChanged) console.log(`  ${line}`);
  }
  if (!bad && fixed.length > 0) {
    console.log('ratchet-update reminder: these known-fail files now PASS; ' +
                `remove them from ${path.basename(expectationsFile)} (and consider raising the floor):`);
    for (const p of fixed) console.log(`  ${p}`);
  }
  return bad ? 1 : 0;
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  const args = parseArgs(process.argv);
  if (args.worker) { runWorker(); return; }

  if (!fs.existsSync(path.join(path.dirname(__dirname), 'dist', 'rust-item-scanner.js'))) {
    console.error('polyscript dist/ not found — run `npm run build` first');
    process.exit(2);
  }

  const registryRoot = findRegistryRoot(args.registry);
  const crateDirs = resolveCrateDirs(registryRoot, args.crates);
  if (crateDirs.length === 0) {
    console.error('no crates matched');
    process.exit(2);
  }

  const files = [];
  for (const dir of crateDirs) {
    for (const abs of collectRsFiles(path.join(registryRoot, dir))) {
      files.push({ abs, rel: path.relative(registryRoot, abs), result: null });
    }
  }
  console.log(`sweeping ${files.length} files across ${crateDirs.length} crates ` +
              `(registry: ${registryRoot}, jobs: ${args.jobs}, timeout: ${args.timeoutMs}ms, ` +
              `max ${args.maxKb}KB)\n`);

  await sweep(files, registryRoot, args);

  for (const f of files) {
    if (!f.result) f.result = { class: 'parse-fail', reason: 'internal: no result recorded' };
  }

  const { failures, passRate } = printReport(files);

  if (args.out) {
    const body = failures.map((f) => `${f.result.class} ${f.rel} ${f.result.reason}`).join('\n');
    fs.writeFileSync(args.out, body + (body ? '\n' : ''));
    console.log(`\nfailure list written to ${args.out}`);
  }

  if (args.ratchet) {
    process.exit(applyRatchet(args.ratchet, failures, passRate));
  }
  process.exit(0);
}

main().catch((e) => {
  console.error(e && e.stack || String(e));
  process.exit(2);
});
