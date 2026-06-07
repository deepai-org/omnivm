#!/usr/bin/env node

import * as fs from 'fs';
import * as path from 'path';
import { Lexer } from './lexer';
import { Parser } from './parser';
import { Transpiler } from './transpiler';

interface CLIOptions {
  input: string;
  output?: string;
  watch?: boolean;
  indent?: string;
  preserveComments?: boolean;
}

function parseArgs(args: string[]): CLIOptions {
  const options: CLIOptions = {
    input: '',
  };

  for (let i = 2; i < args.length; i++) {
    const arg = args[i];
    
    if (arg === '-o' || arg === '--output') {
      options.output = args[++i];
    } else if (arg === '-w' || arg === '--watch') {
      options.watch = true;
    } else if (arg === '--indent') {
      options.indent = args[++i];
    } else if (arg === '--no-comments') {
      options.preserveComments = false;
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
PolyScript to TypeScript Transpiler

Usage: polyscript-transpile <input-file> [options]

Options:
  -o, --output <file>    Output file (default: stdout)
  -w, --watch           Watch input file for changes
  --indent <string>     Indentation string (default: "  ")
  --no-comments         Don't preserve comments
  -h, --help           Show this help message

Examples:
  polyscript-transpile input.poly -o output.ts
  polyscript-transpile script.poly --indent "    "
  polyscript-transpile app.poly -w -o app.ts
`);
}

function transpileFile(inputPath: string, options: CLIOptions): string {
  // Read input file
  const input = fs.readFileSync(inputPath, 'utf-8');
  
  // Lex and parse
  const lexer = new Lexer(input);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  const ast = parser.parse();
  
  // Check for parse errors
  const errors = parser.getErrors();
  if (errors.length > 0) {
    console.error('Parse errors:');
    for (const error of errors) {
      console.error(`  ${error.message}`);
    }
    throw new Error('Failed to parse input file');
  }
  
  // Transpile to TypeScript
  const transpiler = new Transpiler({
    indent: options.indent,
    preserveComments: options.preserveComments,
  });
  
  return transpiler.transpile(ast);
}

function main() {
  const options = parseArgs(process.argv);
  
  try {
    // Check if input file exists
    if (!fs.existsSync(options.input)) {
      console.error(`Error: Input file '${options.input}' not found`);
      process.exit(1);
    }
    
    // Transpile
    const output = transpileFile(options.input, options);
    
    // Write output
    if (options.output) {
      fs.writeFileSync(options.output, output);
      console.log(`✓ Transpiled ${options.input} → ${options.output}`);
    } else {
      console.log(output);
    }
    
    // Watch mode
    if (options.watch && options.output) {
      console.log(`Watching ${options.input} for changes...`);
      
      fs.watchFile(options.input, { interval: 500 }, () => {
        try {
          const output = transpileFile(options.input, options);
          fs.writeFileSync(options.output!, output);
          console.log(`✓ Re-transpiled ${options.input} → ${options.output}`);
        } catch (error) {
          console.error(`Error during re-transpilation:`, error);
        }
      });
    }
  } catch (error) {
    console.error('Transpilation failed:', error);
    process.exit(1);
  }
}

// Run if called directly
if (require.main === module) {
  main();
}