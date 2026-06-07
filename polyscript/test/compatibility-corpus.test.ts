import * as fs from "fs";
import * as path from "path";
import { Lexer } from "../src/lexer";
import { Parser } from "../src/parser";
import { RuntimeResolver } from "../src/runtime-resolver";
import { ManifestCodeGenerator } from "../src/codegen-omnivm";
import { lowerAnnotatedProgram } from "../src/codegen-omnivm/lowering";

function compile(example: string) {
  const filePath = path.join(__dirname, "..", "examples", example);
  const code = fs.readFileSync(filePath, "utf8");
  const tokens = new Lexer(code).tokenize();
  const ast = new Parser(tokens, code).parse();
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, code);
  const ir = lowerAnnotatedProgram(annotated);
  const manifest = new ManifestCodeGenerator().generate(annotated);
  return { ast, manifest, ir };
}

describe("unchanged source compatibility corpus", () => {
  test("runs Python service helpers without dropping decorators or class fields", () => {
    const { ast, manifest } = compile("compat-python-service.py");

    expect(ast.body.some((node: any) => node.kind === "ClassDecl" && node.name.name === "UserScore")).toBe(true);
    const classOp = manifest.ops.find((op: any) => op.op === "native" && op.code.includes("UserScore")) as any;
    expect(classOp).toBeDefined();
    expect(classOp.runtime).toBe("python");
    expect(classOp.code).toContain("@dataclass");
    expect(classOp.code).toContain("user_id: str");
    expect(classOp.code).toContain("score: int");

    const rankUser = manifest.ops.find((op: any) => op.op === "func_def" && op.name === "rank_user") as any;
    expect(rankUser?.bodyRuntime).toBe("python");

    const sample = manifest.ops.find((op: any) => op.op === "eval" && op.bind === "sample") as any;
    expect(sample?.runtime).toBe("python");
    expect(sample?.code).toContain('"id": "u-42"');

    const result = manifest.ops.find((op: any) => op.op === "eval" && op.bind === "result") as any;
    expect(result).toMatchObject({ runtime: "python", code: "rank_user(sample)" });
    expect(result?.captures ?? {}).not.toHaveProperty("sample");
  });

  test("runs TypeScript modules without executing type-only declarations", () => {
    const { manifest } = compile("compat-order-schema.ts");

    expect(manifest.ops.some((op: any) => op.op === "native" && op.code.includes("type Order"))).toBe(false);
    const summarize = manifest.ops.find((op: any) => op.op === "func_def" && op.name === "summarize") as any;
    expect(summarize?.bodyRuntime).toBe("javascript");
    const log = manifest.ops.find((op: any) => op.op === "exec" && op.runtime === "javascript") as any;
    expect(log?.code).toContain("summarize(sample)");
  });

  test("runs Go helper files without executing package declarations", () => {
    const { ast, manifest, ir } = compile("compat-go-status.go");

    const grouped = ast.body.find((node: any) => node.kind === "GroupedImport") as any;
    expect(grouped?.imports.map((imp: any) => imp.path)).toEqual(["fmt", "net/http"]);
    expect(manifest.ops.some((op: any) => op.op === "native" && op.code.includes("package main"))).toBe(false);
    expect(manifest.ops.filter((op: any) => op.op === "import" && op.runtime === "go").map((op: any) => [op.path, op.bind])).toEqual([
      ["fmt", "fmt"],
      ["net/http", "http"],
    ]);

    const statusLabel = manifest.ops.find((op: any) => op.op === "func_def" && op.name === "statusLabel") as any;
    expect(statusLabel?.bodyRuntime).toBe("go");
    expect(statusLabel?.source).toContain('"fmt"');
    expect(statusLabel?.source).toContain('"net/http"');
    expect(statusLabel?.source).toContain("fmt.Sprintf");
    expect(statusLabel?.source).toContain("http.StatusBadRequest");

    const main = manifest.ops.find((op: any) => op.op === "func_def" && op.name === "main") as any;
    expect(main?.bodyRuntime).toBe("go");
    expect(main?.source).toContain("func statusLabel(code int) string");
    expect(main?.source).toContain("func Main() {");
    expect(main?.source).not.toContain("func Main() interface{}");
    expect(main?.source).not.toContain("var statusLabel");
    expect(main?.requires ?? []).not.toContain("statusLabel");
    const loweredMain = ir.nodes.find((node: any) => node.kind === "DefineFunc" && node.name === "main") as any;
    expect(loweredMain?.dependencies).toEqual(expect.arrayContaining([{ name: "statusLabel", argc: 1 }]));
    const mainCall = manifest.ops.find((op: any) => op.op === "eval" && op.func === "main") as any;
    expect(mainCall).toMatchObject({ op: "eval", runtime: "go", func: "main", args: [] });

    const topLevelInit = manifest.ops.find((op: any) => op.op === "eval" && op.bind === "compatibilityStatus") as any;
    expect(topLevelInit?.runtime).toBe("go");
    expect(topLevelInit?.func).toBe("statusLabel");
    expect(topLevelInit?.args).toEqual(["http.StatusAccepted"]);
  });
});
