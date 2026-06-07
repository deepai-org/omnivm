#!/usr/bin/env node
const fs = require('fs');
const path = require('path');
const {Lexer} = require('../dist/lexer');
const {Parser} = require('../dist/parser');

const testDir = path.join(__dirname, '..', 'test');
const files = fs.readdirSync(testDir).filter(f => f.endsWith('.test.ts'));
const regex = /const code = `([\s\S]*?)`;/g;
const issues = [];

for (const file of files) {
  if (file === 'parser-error-detection.test.ts') continue;
  const src = fs.readFileSync(path.join(testDir, file), 'utf8');
  let match;
  let blockNum = 0;
  while ((match = regex.exec(src)) != null) {
    blockNum++;
    const code = match[1].replace(/\\n/g, '\n').replace(/\\t/g, '\t');
    if (!code.trim()) continue;
    const tokens = new Lexer(code).tokenize();
    const parser = new Parser(tokens, code);
    parser.parse();
    const errors = parser.getErrors();
    if (errors.length > 0) {
      const lines = code.trim().split('\n');
      const errLine = errors[0].token ? errors[0].token.line : 0;
      const errSrc = (lines[errLine - 1] || '').trim().substring(0, 70);
      issues.push({
        file, blockNum, n: errors.length,
        msg: errors[0].message,
        errLine,
        errSrc,
        firstLine: lines[0].substring(0, 70),
      });
    }
  }
}

// Group by root cause
const groups = {};
for (const i of issues) {
  const key = i.msg;
  if (!groups[key]) groups[key] = [];
  groups[key].push(i);
}

for (const [msg, items] of Object.entries(groups)) {
  console.log('=== ' + msg + ' (' + items.length + ') ===');
  for (const i of items) {
    console.log('  ' + i.file + ' block ' + i.blockNum + ' (line ' + i.errLine + '): ' + i.errSrc);
  }
  console.log();
}
console.log('Total: ' + issues.length + ' blocks with parse errors (excluding error-detection tests)');
