#!/usr/bin/env node

import * as fs from 'fs';
import { Lexer } from './lexer';
import { Parser } from './parser';
import { RuntimeResolver } from './runtime-resolver';
import { ManifestCodeGenerator } from './codegen-omnivm/manifest-generator';

interface CLIOptions {
  input: string;
  output?: string;
  compact?: boolean;
}

function parseArgs(args: string[]): CLIOptions {
  const options: CLIOptions = {
    input: '',
  };

  for (let i = 2; i < args.length; i++) {
    const arg = args[i];

    if (arg === '-o' || arg === '--output') {
      options.output = args[++i];
    } else if (arg === '--compact') {
      options.compact = true;
    } else if (arg === '-h' || arg === '--help') {
      printHelp();
      process.exit(0);
    } else if (!arg.startsWith('-')) {
      options.input = arg;
    } else {
      console.error(`Unknown option: ${arg}`);
      process.exit(1);
    }
  }

  if (!options.input) {
    console.error('Error: Input file is required');
    printHelp();
    process.exit(1);
  }

  return options;
}

function printHelp() {
  console.log(`
PolyScript to OmniVM Manifest Compiler

Usage: polyc <input-file> [options]

Options:
  -o, --output <file>    Output JSON file (default: stdout)
  --compact              Minified JSON (no pretty-printing)
  -h, --help             Show this help message

Examples:
  polyc input.poly                        # print manifest JSON to stdout
  polyc input.poly -o manifest.json       # write to file
  polyc input.poly --compact              # minified output
  polyc input.poly --compact -o out.json  # minified to file
`);
}

function compileFile(inputPath: string, options: CLIOptions): string {
  const source = fs.readFileSync(inputPath, 'utf-8');

  const lexer = new Lexer(source);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, source);
  const ast = parser.parse();

  const errors = parser.getErrors();
  if (errors.length > 0) {
    console.error('Parse errors:');
    for (const error of errors) {
      console.error(`  ${error.message}`);
    }
    process.exit(1);
  }

  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, source);
  const gen = new ManifestCodeGenerator();
  const manifest = gen.generate(annotated);
  if (manifest.diagnostics?.length) {
    console.error('Manifest diagnostics:');
    for (const diagnostic of manifest.diagnostics) {
      const loc = diagnostic.span?.line
        ? `${diagnostic.span.line}:${diagnostic.span.column}: `
        : '';
      console.error(`  ${diagnostic.severity}: ${loc}${diagnostic.message}`);
    }
  }

  return options.compact
    ? JSON.stringify(manifest)
    : JSON.stringify(manifest, null, 2);
}

function main() {
  const options = parseArgs(process.argv);

  if (!fs.existsSync(options.input)) {
    console.error(`Error: Input file '${options.input}' not found`);
    process.exit(1);
  }

  const json = compileFile(options.input, options);

  if (options.output) {
    fs.writeFileSync(options.output, json + '\n');
    console.error(`Compiled ${options.input} -> ${options.output}`);
  } else {
    console.log(json);
  }
}

if (require.main === module) {
  main();
}
