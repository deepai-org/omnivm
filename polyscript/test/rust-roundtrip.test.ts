/**
 * Round-trip oracle for the Rust raw item scanner (Regime 1 of the
 * two-regime Rust parsing architecture).
 *
 * Each fixture in test/fixtures/rust-roundtrip/ is a REAL crate source file
 * (itertools, base64, thiserror, anyhow, regex) using lifetimes, generics,
 * where clauses, `impl<'a> Trait for X<'a>`, doc comments, cfg attributes,
 * raw strings, `mod tests`, and more. We build a .poly source by appending a
 * Python tail (forcing polyglot context), compile through the FULL pipeline,
 * and assert that every top-level item's exact byte string appears verbatim
 * in the generated Rust func_def unit source.
 *
 * Items are extracted from the fixture with the SAME production scanner the
 * parser uses (scanTopLevelRustItems); the verbatim-bytes assertion against
 * the independently-assembled unit source keeps the scanner honest — if it
 * mis-slices, bytes won't match.
 */

import * as fs from "fs";
import * as path from "path";
import { Lexer } from "../src/lexer";
import { Parser } from "../src/parser";
import { RuntimeResolver } from "../src/runtime-resolver";
import { ManifestCodeGenerator } from "../src/codegen-omnivm";
import { scanTopLevelRustItems, ScannedRustItem } from "../src/rust-item-scanner";

function compile(code: string) {
  const tokens = new Lexer(code).tokenize();
  const parser = new Parser(tokens, code);
  const ast = parser.parse();
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, code);
  const gen = new ManifestCodeGenerator();
  const manifest = gen.generate(annotated);
  return { manifest, gen, parseErrors: parser.getErrors() };
}

const fixturesDir = path.join(__dirname, "fixtures", "rust-roundtrip");

const FIXTURES = [
  "itertools-zip_eq_impl.rs",
  "base64-chunked_encoder.rs",
  "thiserror-aserror.rs",
  "anyhow-chain.rs",
  "regex-string.rs",
] as const;

/**
 * Per-fixture expectations. Every fixture is expected fully green; if a
 * fixture ever needs a documented exception, record it here (status +
 * reason) instead of weakening the assertions below.
 */
const EXPECTATIONS: Record<string, { status: "green"; skipItems?: string[] }> = {
  "itertools-zip_eq_impl.rs": { status: "green" },
  "base64-chunked_encoder.rs": { status: "green" },
  "thiserror-aserror.rs": { status: "green" },
  "anyhow-chain.rs": { status: "green" },
  "regex-string.rs": { status: "green" },
};

interface Compiled {
  unitSource: string;
  funcDefs: any[];
  exports: string[];
  items: ScannedRustItem[];
  parseErrors: Error[];
  manifest: any;
}

const compiledByFixture = new Map<string, Compiled>();

function compileFixture(name: string): Compiled {
  const cached = compiledByFixture.get(name);
  if (cached) return cached;

  const fixture = fs.readFileSync(path.join(fixturesDir, name), "utf8");
  const polySource = `${fixture}\n\nprint("done")\n`;

  const { manifest, gen, parseErrors } = compile(polySource);
  const funcDefs = manifest.ops.filter(
    (o: any) => o.op === "func_def" && o.bodyRuntime === "rust"
  ) as any[];

  // All rust func_defs share ONE unit source. A fixture with no top-level
  // fn items (only impls/traits/structs) emits no func_def ops, so read the
  // assembled unit straight off the generator; when func_defs exist they
  // MUST carry that same unit (asserted below).
  const rustUnit = (gen as any).rustUnit as { source: string; exports: string[] } | undefined;
  const unitSource: string = funcDefs.length > 0 ? funcDefs[0].source : rustUnit?.source ?? "";
  const exports: string[] = funcDefs.length > 0 ? funcDefs[0].exports : rustUnit?.exports ?? [];

  const items = scanTopLevelRustItems(fixture);

  const result = { unitSource, funcDefs, exports, items, parseErrors, manifest };
  compiledByFixture.set(name, result);
  return result;
}

describe("Rust item anchors never fire inside other languages' strings/comments", () => {
  it("a Python string containing 'fn main() {}' stays a string", () => {
    const { manifest, parseErrors } = compile('s = "fn main() {}"\nprint(s)\n');
    expect(parseErrors).toEqual([]);
    const json = JSON.stringify(manifest.ops);
    expect(json).not.toContain("func_def");
    expect(json).toContain("fn main() {}"); // survived as a literal value
  });

  it("a JS template literal containing 'impl X {}' stays a template literal", () => {
    const { manifest, parseErrors } = compile("const snippet = `impl X {}`\nconsole.log(snippet)\n");
    expect(parseErrors).toEqual([]);
    const json = JSON.stringify(manifest.ops);
    expect(json).not.toContain("func_def");
    expect(json).not.toContain('"runtime":"rust"');
    expect(json).toContain("impl X {}");
  });

  it("a Python comment containing a struct anchor is inert", () => {
    const { manifest, parseErrors } = compile('# struct Hidden { x: i64 }\nvalue = 41\nprint(value + 1)\n');
    expect(parseErrors).toEqual([]);
    const json = JSON.stringify(manifest.ops);
    expect(json).not.toContain("Hidden");
    expect(json).not.toContain('"runtime":"rust"');
  });
});

describe("Turbofish is definite-Rust evidence in expression position (Regime 2)", () => {
  it("routes a turbofish call to the rust runtime with captures", () => {
    const { manifest, parseErrors } = compile(
      "items = [1, 2, 3]\nconst pairs = items.tuple_windows::<(_, _)>()\nprint(pairs)\n"
    );
    expect(parseErrors).toEqual([]);
    const evalOp = manifest.ops.find(
      (o: any) => o.op === "eval" && String(o.code).includes("tuple_windows")
    ) as any;
    expect(evalOp).toBeDefined();
    expect(evalOp.runtime).toBe("rust");
    expect(evalOp.code).toBe("items.tuple_windows::<(_, _)>()");
    expect(evalOp.captures).toEqual({ items: "items" });
  });
});

describe("Rust round-trip oracle: real crate sources survive verbatim", () => {
  for (const name of FIXTURES) {
    describe(name, () => {
      const expectation = EXPECTATIONS[name];

      it("compiles through the full pipeline without parse errors", () => {
        const { parseErrors } = compileFixture(name);
        expect(parseErrors.map((e) => e.message)).toEqual([]);
      });

      it("reference scanner finds the fixture's top-level items", () => {
        const { items } = compileFixture(name);
        expect(items.length).toBeGreaterThan(0);
      });

      it("every top-level item appears byte-for-byte in the Rust unit", () => {
        const { unitSource, items } = compileFixture(name);
        expect(expectation.status).toBe("green");
        const missing: string[] = [];
        for (const item of items) {
          if (expectation.skipItems?.includes(item.name ?? "")) continue;
          if (unitSource.indexOf(item.text) === -1) {
            missing.push(
              `${item.itemKind} ${item.name ?? "<anon>"}: ${JSON.stringify(item.text.slice(0, 120))}`
            );
          }
        }
        expect(missing).toEqual([]);
      });

      it("export set is empty: the fixture's fns are all internal-only", () => {
        // The export-set analysis exports only fns referenced from OUTSIDE
        // the Rust unit. The fixtures' polyglot tail (`print("done")`) never
        // calls into Rust, so every top-level fn is internal-only: it stays
        // in the unit verbatim (asserted byte-for-byte above) but gets no
        // shim, no entry in `exports`, and no func_def op.
        const { exports, funcDefs } = compileFixture(name);
        expect(exports).toEqual([]);
        expect(funcDefs).toEqual([]);
      });

      it("internal fns get no shims and leak into no other-runtime ops", () => {
        const { items, unitSource, manifest } = compileFixture(name);
        const otherOps = JSON.stringify(
          manifest.ops.filter((o: any) => o.op !== "func_def")
        );
        for (const item of items.filter((i) => i.itemKind === "fn")) {
          for (const sig of item.fns) {
            // No export shim in the unit for an internal-only fn...
            expect(unitSource).not.toContain(`OmniVMCall_${sig.name}`);
            // ...and the fn was absorbed, not leaked into exec/eval ops.
            expect(otherOps).not.toContain(`fn ${sig.name}`);
          }
        }
      });

      it("the polyglot tail survives as its own op (no item scan runs away)", () => {
        const { manifest, unitSource } = (() => {
          const c = compileFixture(name);
          return { manifest: compile(`${fs.readFileSync(path.join(fixturesDir, name), "utf8")}\n\nprint("done")\n`).manifest, unitSource: c.unitSource };
        })();
        // The print tail must come out as a real op AFTER the items — if the
        // raw scanner over-consumed (e.g. a runaway `;` hunt), it would have
        // been swallowed into the unit instead.
        const last = manifest.ops[manifest.ops.length - 1] as any;
        expect(JSON.stringify(last)).toContain("done");
        expect(unitSource).not.toContain('"done"');
      });
    });
  }
});
