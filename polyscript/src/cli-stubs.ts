#!/usr/bin/env node

/**
 * polyc-stubs — cross-language type-stub generator.
 *
 * Given a PolyScript source file (.poly) or an already-compiled dispatch
 * manifest (.json), emits type stubs that let a *consuming* language's type
 * checker verify calls across the polyglot boundary:
 *
 *   - <out>.pyi : Python signatures (for mypy / pyright)
 *   - <out>.d.ts: TypeScript declarations (for tsc)
 *
 * It compiles .poly using the same public API as the manifest CLI; it does not
 * modify the manifest pipeline, it only *consumes* the compiled manifest.
 */

import * as fs from "fs";
import * as path from "path";
import { Lexer } from "./lexer";
import { Parser } from "./parser";
import { RuntimeResolver } from "./runtime-resolver";
import { ManifestCodeGenerator } from "./codegen-omnivm/manifest-generator";
import type { DispatchManifest } from "./codegen-omnivm/manifest-types";
import { emitPyiStubs, emitDtsStubs } from "./codegen-omnivm/stub-emitter";

interface CLIOptions {
  input: string;
  out?: string;
  pyi: boolean;
  dts: boolean;
}

function printHelp(): void {
  console.log(`
PolyScript Cross-Language Stub Generator

Usage: polyc-stubs <input.poly|manifest.json> [options]

Options:
  --out <base>   Output base path. Writes <base>.pyi and/or <base>.d.ts.
                 Defaults to the input path without its extension.
  --pyi          Emit only the Python .pyi stub.
  --dts          Emit only the TypeScript .d.ts stub.
                 (If neither --pyi nor --dts is given, both are emitted.)
  -h, --help     Show this help message.

Examples:
  polyc-stubs app.poly                       # writes app.pyi and app.d.ts
  polyc-stubs app.poly --pyi --out stubs/app # writes stubs/app.pyi
  polyc-stubs manifest.json --dts            # writes manifest.d.ts
`);
}

function parseArgs(args: string[]): CLIOptions {
  const opts: CLIOptions = { input: "", pyi: false, dts: false };
  for (let i = 2; i < args.length; i++) {
    const arg = args[i];
    if (arg === "--out" || arg === "-o") {
      opts.out = args[++i];
    } else if (arg === "--pyi") {
      opts.pyi = true;
    } else if (arg === "--dts") {
      opts.dts = true;
    } else if (arg === "-h" || arg === "--help") {
      printHelp();
      process.exit(0);
    } else if (!arg.startsWith("-")) {
      opts.input = arg;
    } else {
      console.error(`Unknown option: ${arg}`);
      process.exit(1);
    }
  }
  if (!opts.input) {
    console.error("Error: Input file is required");
    printHelp();
    process.exit(1);
  }
  // Default: emit both.
  if (!opts.pyi && !opts.dts) {
    opts.pyi = true;
    opts.dts = true;
  }
  return opts;
}

/** Compile a .poly source string to a dispatch manifest. */
export function compileToManifest(
  source: string,
  sourceFile?: string
): DispatchManifest {
  const lexer = new Lexer(source);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens, source);
  const ast = parser.parse();

  const errors = parser.getErrors();
  if (errors.length > 0) {
    console.error("Parse errors:");
    for (const error of errors) console.error(`  ${error.message}`);
    process.exit(1);
  }

  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, source);
  const gen = new ManifestCodeGenerator();
  return gen.generate(annotated, { sourceFile });
}

/** Load a manifest from a .poly (compile) or .json (parse) input path. */
export function manifestFromInput(inputPath: string): DispatchManifest {
  const text = fs.readFileSync(inputPath, "utf-8");
  if (inputPath.endsWith(".json")) {
    return JSON.parse(text) as DispatchManifest;
  }
  return compileToManifest(text, inputPath);
}

function defaultOutBase(inputPath: string): string {
  const ext = path.extname(inputPath);
  return ext ? inputPath.slice(0, -ext.length) : inputPath;
}

function main(): void {
  const opts = parseArgs(process.argv);

  if (!fs.existsSync(opts.input)) {
    console.error(`Error: Input file '${opts.input}' not found`);
    process.exit(1);
  }

  const manifest = manifestFromInput(opts.input);
  const outBase = opts.out ?? defaultOutBase(opts.input);

  if (opts.pyi) {
    const pyiPath = `${outBase}.pyi`;
    fs.writeFileSync(pyiPath, emitPyiStubs(manifest));
    console.error(`Wrote ${pyiPath}`);
  }
  if (opts.dts) {
    const dtsPath = `${outBase}.d.ts`;
    fs.writeFileSync(dtsPath, emitDtsStubs(manifest));
    console.error(`Wrote ${dtsPath}`);
  }
}

if (require.main === module) {
  main();
}
