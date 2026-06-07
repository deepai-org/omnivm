#!/usr/bin/env node
/**
 * Audit all test files: extract code blocks, run through full pipeline,
 * report parse errors, manifest placeholders, and crashes.
 *
 * NOTE: This script extracts code from template literals in test files.
 * Some escape sequences may not be processed identically to how JS
 * evaluates them at runtime. The authoritative error count comes from
 * the diagnostics file written during `npm test` (/tmp/polyscript-diag.jsonl).
 * This script provides an approximate audit for manifest quality.
 */
const fs = require('fs');
const path = require('path');
const {Lexer} = require('../dist/lexer');
const {Parser} = require('../dist/parser');
const {RuntimeResolver} = require('../dist/runtime-resolver');
const {ManifestCodeGenerator} = require('../dist/codegen-omnivm');

const testDir = path.join(__dirname, '..', 'test');
const files = fs.readdirSync(testDir).filter(f => f.endsWith('.test.ts'));

const results = { ok: 0, parseErrors: 0, placeholders: 0, crashes: 0 };
const issues = [];

for (const file of files) {
  const src = fs.readFileSync(path.join(testDir, file), 'utf8');

  // Extract code blocks from const code = `...` with proper backtick tracking
  const marker = 'const code = `';
  let searchPos = 0;
  let blockNum = 0;

  while (true) {
    const idx = src.indexOf(marker, searchPos);
    if (idx === -1) break;
    const contentStart = idx + marker.length;
    // Find the unescaped closing backtick
    let i = contentStart;
    while (i < src.length) {
      if (src[i] === '\\') { i += 2; continue; }
      if (src[i] === '`') break;
      i++;
    }
    if (i >= src.length) break;
    searchPos = i + 1;
    blockNum++;
    // Unescape template literal content character by character
    const raw = src.slice(contentStart, i);
    let code = '';
    for (let j = 0; j < raw.length; j++) {
      if (raw[j] === '\\' && j + 1 < raw.length) {
        const next = raw[j + 1];
        if (next === '`') { code += '`'; j++; }
        else if (next === '$') { code += '$'; j++; }
        else if (next === '\\') { code += '\\'; j++; }
        else if (next === 'n') { code += '\n'; j++; }
        else if (next === 't') { code += '\t'; j++; }
        else if (next === 'r') { code += '\r'; j++; }
        else { code += raw[j]; }
      } else {
        code += raw[j];
      }
    }
    if (!code.trim()) continue;

    try {
      const tokens = new Lexer(code).tokenize();
      const parser = new Parser(tokens, code);
      const ast = parser.parse();
      const errors = parser.getErrors();

      if (errors.length > 0) {
        results.parseErrors++;
        issues.push({
          file,
          block: blockNum,
          type: 'parse',
          details: errors.map(e => e.message + (e.token ? ' @L' + e.token.line : '')),
          source: code.trim().split('\n')[0].substring(0, 80)
        });
        continue;
      }

      const resolver = new RuntimeResolver();
      const annotated = resolver.resolve(ast, code);
      const gen = new ManifestCodeGenerator();
      const manifest = gen.generate(annotated);
      JSON.stringify(manifest);

      // Check for placeholder comments (real issues, not glob patterns)
      const badOps = manifest.ops.filter(o => {
        if (!o.code) return false;
        return /\/\*\s*(ERROR|[A-Z][a-z]+[A-Z])/.test(o.code);
      });

      if (badOps.length > 0) {
        results.placeholders++;
        issues.push({
          file,
          block: blockNum,
          type: 'placeholder',
          details: badOps.map(o => o.code.substring(0, 120)),
          source: code.trim().split('\n')[0].substring(0, 80)
        });
      } else {
        results.ok++;
      }
    } catch (e) {
      results.crashes++;
      issues.push({
        file,
        block: blockNum,
        type: 'crash',
        details: [e.message.substring(0, 120)],
        source: code.trim().split('\n')[0].substring(0, 80)
      });
    }
  }
}

console.log('\n=== MANIFEST AUDIT RESULTS ===\n');
console.log('OK:           ' + results.ok);
console.log('Parse errors: ' + results.parseErrors + ' (skipped)');
console.log('Placeholders: ' + results.placeholders);
console.log('Crashes:      ' + results.crashes);
console.log('Total blocks: ' + (results.ok + results.parseErrors + results.placeholders + results.crashes));

if (issues.length > 0) {
  console.log('\n=== ISSUES ===\n');
  for (const issue of issues) {
    console.log('[' + issue.type.toUpperCase() + '] ' + issue.file + ' block ' + issue.block);
    console.log('  source: ' + issue.source);
    for (const d of issue.details) {
      console.log('  detail: ' + d);
    }
    console.log();
  }
}

console.log('\nNote: For authoritative parse error count, check /tmp/polyscript-diag.jsonl after npm test');
