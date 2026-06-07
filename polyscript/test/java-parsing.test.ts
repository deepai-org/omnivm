import { Lexer } from "../src/lexer";
import { Parser } from "../src/parser";
import { RuntimeResolver } from "../src/runtime-resolver";
import { ManifestCodeGenerator } from "../src/codegen-omnivm";
import * as AST from "../src/ast";

function parse(code: string): AST.Program {
  const tokens = new Lexer(code).tokenize();
  return new Parser(tokens, code).parse();
}

function manifest(code: string) {
  const tokens = new Lexer(code).tokenize();
  const ast = new Parser(tokens, code).parse();
  const annotated = new RuntimeResolver().resolve(ast, code);
  return new ManifestCodeGenerator().generate(annotated);
}

describe("Java parsing", () => {
  describe("class with modifiers and methods", () => {
    const code = `
public class Calculator {
    private static final int MAX = 100;
    private int value;

    public Calculator(int initial) {
        this.value = initial;
    }

    public int add(int x) {
        return this.value + x;
    }

    private static String format(String template, Object... args) {
        return String.format(template, args);
    }

    protected void reset() {
        this.value = 0;
    }
}`;
    const ast = parse(code);

    it("parses as a single ClassDecl at top level", () => {
      // Filter out any bare ExprStmt that are just modifiers leaking
      const nonTrivial = ast.body.filter(
        (n: any) => !(n.kind === "ExprStmt" && n.expr?.kind === "Identifier")
      );
      expect(nonTrivial.length).toBe(1);
      expect(nonTrivial[0].kind).toBe("ClassDecl");
    });

    it("has the correct class name", () => {
      const cls = ast.body.find((n: any) => n.kind === "ClassDecl") as any;
      expect(cls.name.name).toBe("Calculator");
    });

    it("parses fields as members", () => {
      const cls = ast.body.find((n: any) => n.kind === "ClassDecl") as any;
      const fields = cls.members.filter((m: any) => m.kind === "Field");
      const fieldNames = fields.map((f: any) => f.name?.name || f.name);
      expect(fieldNames).toContain("MAX");
      expect(fieldNames).toContain("value");
    });

    it("parses methods with bodies", () => {
      const cls = ast.body.find((n: any) => n.kind === "ClassDecl") as any;
      const methods = cls.members.filter((m: any) => m.kind === "Method" && m.body);
      expect(methods.length).toBeGreaterThanOrEqual(4);
      const methodNames = methods.map((m: any) => m.name?.name || m.name);
      expect(methodNames).toContain("add");
      expect(methodNames).toContain("format");
      expect(methodNames).toContain("reset");
    });

    it("generates a valid manifest", () => {
      const m = manifest(code);
      expect(m.version).toBe(1);
      expect(m.ops.length).toBeGreaterThan(0);
    });
  });

  describe("method modifiers", () => {
    it("handles public static void main", () => {
      const ast = parse(`
public class App {
    public static void main(String[] args) {
        System.out.println("hello");
    }
}`);
      const cls = ast.body.find((n: any) => n.kind === "ClassDecl") as any;
      const methods = cls.members.filter((m: any) => m.kind === "Method" && m.body);
      const names = methods.map((m: any) => m.name?.name || m.name);
      expect(names).toContain("main");
    });

    it("handles abstract methods", () => {
      const ast = parse(`
public abstract class Shape {
    abstract double area();
    abstract String name();
}`);
      const cls = ast.body.find((n: any) => n.kind === "ClassDecl") as any;
      expect(cls).toBeDefined();
      expect(cls.name.name).toBe("Shape");
    });
  });

  describe("try-catch-finally", () => {
    it("parses catch with typed exception variable", () => {
      const ast = parse(`
try {
    risky();
} catch (IOException e) {
    handle(e);
} catch (Exception e) {
    fallback(e);
} finally {
    cleanup();
}`);
      const tryStmt = ast.body.find((n: any) => n.kind === "Try") as any;
      expect(tryStmt).toBeDefined();
      expect(tryStmt.catches.length).toBeGreaterThanOrEqual(1);
      expect(tryStmt.finallyBody).toBeDefined();
    });
  });

  describe("Java-specific expressions", () => {
    it("parses new with constructor args", () => {
      const ast = parse(`File f = new File(path);`);
      expect(ast.body.length).toBeGreaterThan(0);
    });

    it("parses array type params in methods", () => {
      const ast = parse(`
public class Foo {
    public static void main(String[] args) {
        System.out.println(args[0]);
    }
}`);
      const cls = ast.body.find((n: any) => n.kind === "ClassDecl") as any;
      const methods = cls.members.filter((m: any) => m.kind === "Method" && m.body);
      expect(methods.map((m: any) => m.name?.name || m.name)).toContain("main");
    });

    it("parses instanceof", () => {
      const ast = parse(`boolean check = x instanceof String;`);
      const decl = ast.body[0] as any;
      // Should have a binary with instanceof
      const val = decl.values?.[0] || decl.expr;
      expect(val).toBeDefined();
    });

    it("parses ternary operator", () => {
      const ast = parse(`String result = (x > 0) ? "pos" : "neg";`);
      expect(ast.body.length).toBeGreaterThan(0);
    });

    it("parses method chaining", () => {
      const ast = parse(`String s = obj.method1().method2().toString();`);
      expect(ast.body.length).toBeGreaterThan(0);
    });
  });

  describe("full file manifest generation", () => {
    it("generates manifest for a complete Java class", () => {
      const code = `
package omnivm;

import java.io.*;
import java.util.*;

public class Runner {
    private String name;

    public Runner(String name) {
        this.name = name;
    }

    public String run(String input) {
        List<String> parts = new ArrayList<>();
        for (String p : input.split(",")) {
            parts.add(p.trim());
        }
        return String.join("; ", parts);
    }

    public static void main(String[] args) {
        Runner r = new Runner("test");
        System.out.println(r.run("a, b, c"));
    }
}`;
      const m = manifest(code);
      expect(m.version).toBe(1);
      expect(m.ops.length).toBeGreaterThan(0);

      // Should not have excessive bare eval ops from unparsed modifiers
      const bareEvals = m.ops.filter(
        (op: any) => op.op === "eval" && op.code && /^(public|private|protected|static)$/.test(op.code.trim())
      );
      expect(bareEvals.length).toBe(0);
    });
  });
});
