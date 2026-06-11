import * as fs from "fs";
import * as path from "path";
import { Lexer } from "../src/lexer";
import { Parser } from "../src/parser";
import { RuntimeResolver } from "../src/runtime-resolver";
import { ManifestCodeGenerator } from "../src/codegen-omnivm";

function compile(filePath: string) {
  const code = fs.readFileSync(filePath, "utf8");
  const tokens = new Lexer(code).tokenize();
  const ast = new Parser(tokens, code).parse();
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, code);
  const gen = new ManifestCodeGenerator();
  const manifest = gen.generate(annotated);

  const runtimes = annotated.program.body.map((node) => {
    const aff = annotated.affinityMap.get(node);
    return aff?.runtime ?? "unknown";
  });
  const manifestRuntimes = manifest.ops
    .map((op: any) => op.runtime)
    .filter((runtime: unknown): runtime is string => typeof runtime === "string");
  const coveredRuntimes = Array.from(new Set([...runtimes, ...manifestRuntimes]));

  return { annotated, manifest, runtimes, manifestRuntimes, coveredRuntimes, code };
}

const examplesDir = path.join(__dirname, "..", "examples");
const runtimeContractsDir = path.join(__dirname, "fixtures", "runtime-contracts");

describe("Example files: end-to-end pipeline", () => {
  describe("cursed-polyglot.poly", () => {
    const { manifest, runtimes } = compile(
      path.join(examplesDir, "cursed-polyglot.poly")
    );

    it("has no unknown runtimes", () => {
      expect(runtimes).not.toContain("unknown");
    });

    it("produces a valid manifest with ops", () => {
      expect(manifest.version).toBe(1);
      expect(manifest.ops.length).toBeGreaterThan(10);
    });

    it("starts with python imports", () => {
      expect(runtimes[0]).toBe("python");
      expect(runtimes[1]).toBe("python");
    });

    it("ping-pongs between python and javascript", () => {
      // entries=py, stems=js, matched=py, unique=js, ordered=py, records=js, wire=py
      expect(runtimes[2]).toBe("python"); // entries
      expect(runtimes[3]).toBe("javascript"); // stems (arrow)
      expect(runtimes[4]).toBe("python"); // matched (list comp)
      expect(runtimes[5]).toBe("javascript"); // unique (arrow + ===)
      expect(runtimes[6]).toBe("python"); // ordered (sorted)
      expect(runtimes[7]).toBe("javascript"); // records (arrow)
      expect(runtimes[8]).toBe("python"); // wire (list comp over JS records)
      expect(runtimes[9]).toBe("python"); // survived (len)
      expect(runtimes[10]).toBe("javascript"); // status (arrow + template)
    });

    it("detects f-string print as python", () => {
      expect(runtimes[11]).toBe("python"); // print(f"Pipeline complete...")
    });

    it("manifest ops have valid code strings", () => {
      const evalOps = manifest.ops.filter(
        (o: any) => o.op === "eval" && o.code
      );
      for (const op of evalOps) {
        expect((op as any).code).not.toContain("/* ");
        expect((op as any).code.length).toBeGreaterThan(0);
      }
    });

    it("has cross-runtime captures", () => {
      const withCaptures = manifest.ops.filter(
        (o: any) => o.captures && Object.keys(o.captures).length > 0
      );
      expect(withCaptures.length).toBeGreaterThan(0);
    });
  });

  describe("cursed-concurrency.poly", () => {
    const { manifest, runtimes } = compile(
      path.join(examplesDir, "cursed-concurrency.poly")
    );

    it("has no unknown runtimes", () => {
      expect(runtimes).not.toContain("unknown");
    });

    it("assigns python to imports and discovery", () => {
      expect(runtimes[0]).toBe("python"); // import os
      expect(runtimes[3]).toBe("python"); // discovered list comprehension
      expect((manifest.ops[3] as any).bind).toBe("discovered");
    });

    it("assigns go to channels and workers", () => {
      expect(runtimes[1]).toBe("go"); // inbox = make(16)
      expect(runtimes[2]).toBe("go"); // outbox = make(16)
      expect(runtimes[5]).toBe("go"); // func worker
      expect(runtimes[8]).toBe("go"); // w1 = go worker(1)
    });

    it("assigns javascript to process function", () => {
      expect(runtimes[4]).toBe("javascript"); // function process
    });

    it("assigns go to close() and channel sends", () => {
      expect(runtimes[7]).toBe("go"); // close(inbox)
      expect(runtimes[12]).toBe("go"); // joined = wait(w1, w2, w3, w4)
      expect(runtimes[14]).toBe("go"); // close(outbox)
      expect(runtimes[22]).toBe("go"); // const done = make(1)
      expect(runtimes[23]).toBe("go"); // done <- report
      expect(runtimes[24]).toBe("go"); // close(done)
    });

    it("assigns js to arrow-function expressions", () => {
      expect(runtimes[15]).toBe("javascript"); // rows = Array.from(outbox).map(...)
      expect(runtimes[16]).toBe("javascript"); // raw = rows.map(...)
      expect(runtimes[17]).toBe("javascript"); // names = raw.map(r => ...)
      expect(runtimes[18]).toBe("javascript"); // deduped = names.filter(...)
    });

    it("assigns python to sorted/len/final report", () => {
      expect(runtimes[13]).toBe("python"); // worker_count = len(joined)
      expect(runtimes[19]).toBe("python"); // ranked = sorted(deduped)
      expect(runtimes[20]).toBe("python"); // total = len(ranked)
      expect(runtimes[21]).toBe("python"); // report = ranked
      expect(runtimes[25]).toBe("python"); // final_report = list(done)
      expect(runtimes[26]).toBe("python"); // delivered = len(final_report)
    });

    it("emits chan ops for make/send/close", () => {
      const chanOps = manifest.ops.filter((o: any) => o.op === "chan");
      const actions = chanOps.map((o: any) => o.action);
      expect(actions).toContain("make");
      expect(actions).toContain("send");
      expect(actions).toContain("close");
    });

    it("emits spawn ops for goroutines", () => {
      const spawnOps = manifest.ops.filter((o: any) => o.op === "spawn");
      expect(spawnOps.length).toBe(4);
      expect(spawnOps.map((o: any) => o.bind)).toEqual(["w1", "w2", "w3", "w4"]);
    });

    it("emits func_def with correct bodyRuntime", () => {
      const funcs = manifest.ops.filter((o: any) => o.op === "func_def");
      const process = funcs.find((o: any) => o.name === "process");
      const worker = funcs.find((o: any) => o.name === "worker");
      expect(funcs.map((o: any) => o.name)).toEqual(["process", "worker"]);
      expect((process as any)?.bodyRuntime).toBe("javascript");
      expect((worker as any)?.bodyRuntime).toBe("go");
    });

    it("detects f-string print as python", () => {
      expect(runtimes[27]).toBe("python"); // print(f"Processed {total}...")
    });

    it("has type crossing summary", () => {
      expect(manifest.typeSummary).toBeDefined();
    });
  });

  describe("syntactic-dominance.poly", () => {
    const { manifest, runtimes } = compile(
      path.join(examplesDir, "syntactic-dominance.poly")
    );

    it("has no unknown runtimes", () => {
      expect(runtimes).not.toContain("unknown");
    });

    it("arrows override python provenance", () => {
      expect(runtimes[1]).toBe("python"); // files = os.listdir
      expect(runtimes[2]).toBe("javascript"); // loud = files.map(f => ...)
      expect(runtimes[3]).toBe("javascript"); // valid = loud.filter(f => f !== ...)
    });

    it("python builtins stay python", () => {
      expect(runtimes[4]).toBe("python"); // count = len(valid)
      expect(runtimes[6]).toBe("python"); // ordered = sorted(logs)
      expect(runtimes[7]).toBe("python"); // payload = list(ordered)
    });

    it("regex literal signals javascript", () => {
      expect(runtimes[5]).toBe("javascript"); // logs with /\.log$/i
    });

    it("produces valid manifest", () => {
      expect(manifest.version).toBe(1);
      expect(manifest.ops.length).toBeGreaterThan(8);
      const evalOps = manifest.ops.filter(
        (o: any) => o.op === "eval" && o.code
      );
      for (const op of evalOps) {
        expect((op as any).code).not.toContain("/* ");
      }
    });
  });

  describe("django-go-typescript-views.poly", () => {
    const { manifest, runtimes } = compile(
      path.join(examplesDir, "django-go-typescript-views.poly")
    );

    it("compiles under generic import inference and emits Go plus JavaScript handlers", () => {
      expect(runtimes).not.toContain("unknown");
      expect(runtimes).toContain("go");
      expect(runtimes).toContain("javascript");
    });

    it("compiles framework view handlers into manifest-callable functions", () => {
      const funcs = manifest.ops.filter((o: any) => o.op === "func_def");
      const goView = funcs.find((o: any) => o.name === "go_view");
      const tsView = funcs.find((o: any) => o.name === "ts_view");

      expect((goView as any)?.bodyRuntime).toBe("go");
      expect((goView as any)?.source).toContain("func GoView(path interface{}) interface{}");
      expect((tsView as any)?.bodyRuntime).toBe("javascript");
    });

    it("keeps the Django callable native while delegating through OmniVM stubs", () => {
      const djangoView = manifest.ops.find(
        (o: any) => o.op === "eval" && o.bind === "django_view"
      ) as any;

      expect(djangoView?.code).not.toContain("@py");
      expect(djangoView?.code).toContain("JsonResponse");
      expect(djangoView?.code).toContain("go_view(request.path)");
      expect(djangoView?.code).toContain("ts_view(request.path)");
      expect(djangoView?.code).not.toContain("json.loads");
      expect(djangoView?.code).toContain("lambda request");
    });
  });

  describe("Java ecosystem examples", () => {
    const javaExamples = [
      "java-gson-pandas-zod-express.poly",
      "java-commons-csv-pydantic-go-batching.poly",
      "java-jsoup-bs4-cheerio.poly",
      "java-okhttp-httpx-go-retry.poly",
      "java-kwargs-map-adapter.poly",
      "java-kwargs-record-constructor.poly",
      "java-kwargs-builder-setters.poly",
    ];

    for (const example of javaExamples) {
      it(`${example} compiles without framework-specific import heuristics`, () => {
        const { manifest, runtimes, code } = compile(path.join(examplesDir, example));

        expect(code).not.toMatch(/@(java|js)\(/);
        expect(runtimes).not.toContain("unknown");
        expect(manifest.ops.length).toBeGreaterThan(1);

        const evalOps = manifest.ops.filter((o: any) => o.op === "eval" && o.code);
        for (const op of evalOps) {
          expect((op as any).code).not.toMatch(/@(java|js)\(/);
        }
      });
    }
  });

  describe("edge library examples", () => {
    const edgeExamples: Array<{ file: string; runtimes: string[] }> = [
      {
        file: "async-httpx-rxjs-errgroup.poly",
        runtimes: ["python", "javascript", "go"],
      },
      {
        file: "pandas-pydantic-zod-dry-validation.poly",
        runtimes: ["python", "javascript", "ruby"],
      },
      {
        file: "orm-session-boundaries.poly",
        runtimes: ["python", "javascript", "ruby"],
      },
      {
        file: "framework-middleware-render.poly",
        runtimes: ["python", "javascript", "ruby"],
      },
      {
        file: "java-futures-jdbc-streaming.poly",
        runtimes: ["java", "python"],
      },
      {
        file: "go-http-cobra-observability.poly",
        runtimes: ["go"],
      },
      {
        file: "request-analytics-ecosystem.poly",
        runtimes: ["python", "javascript", "java"],
      },
      {
        file: "orm-model-client-flow.poly",
        runtimes: ["python", "javascript", "java"],
      },
      {
        file: "python-docs-popular-packages.poly",
        runtimes: ["python"],
      },
      {
        file: "javascript-docs-popular-packages.poly",
        runtimes: ["javascript"],
      },
      {
        file: "javascript-map-set-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-destructuring-spread-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-rest-destructuring-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-array-destructuring-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-optional-call-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-error-cause-details.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "python-error-cause-js-catch.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "pydantic-zod-error-fidelity.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "python-map-collision-docs.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "python-object-enumeration-docs.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "javascript-python-dict-enumeration-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-python-mapping-methods-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-map-mapping-methods-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-generator-python-islice-docs.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-generator-python-error.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-async-generator-python-consume.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-async-generator-python-error.poly",
        runtimes: ["javascript", "python"],
      },
      {
        file: "javascript-ruby-mapping-methods-docs.poly",
        runtimes: ["javascript", "ruby"],
      },
      {
        file: "javascript-java-mapping-methods-docs.poly",
        runtimes: ["javascript", "java"],
      },
      {
        file: "python-dataframe-js-table-docs.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "python-async-generator-js-consume.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "python-async-generator-js-error.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "python-async-context-docs.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "java-docs-popular-packages.poly",
        runtimes: ["java", "python"],
      },
      {
        file: "ruby-docs-popular-packages.poly",
        runtimes: ["ruby", "python"],
      },
      {
        file: "go-docs-popular-packages.poly",
        runtimes: ["go", "python"],
      },
      {
        file: "python-fastapi-sqlalchemy-polars-docs.poly",
        runtimes: ["python"],
      },
      {
        file: "javascript-react-jsx-docs.poly",
        runtimes: ["javascript"],
      },
      {
        file: "javascript-jsx-factory-docs.poly",
        runtimes: ["javascript"],
      },
      {
        file: "java-jackson-reactor-docs.poly",
        runtimes: ["java", "python"],
      },
      {
        file: "ruby-activerecord-docs.poly",
        runtimes: ["ruby", "python"],
      },
      {
        file: "go-http-handler-docs.poly",
        runtimes: ["go", "python"],
      },
      {
        file: "vertical-order-review-app.poly",
        runtimes: ["python", "javascript", "java", "ruby", "go"],
      },
    ];

    for (const { file, runtimes: expectedRuntimes } of edgeExamples) {
      it(`${file} compiles without language tags under generic inference`, () => {
        const { manifest, runtimes, code } = compile(path.join(examplesDir, file));

        expect(code).not.toMatch(/@(py|js|go|rb|java)\(/);
        expect(runtimes).not.toContain("unknown");
        expect(expectedRuntimes.length).toBeGreaterThan(0);
        expect(manifest.ops.length).toBeGreaterThan(1);
      });
    }

    it("keeps Python raise-from cause chains inside the native Python statement", () => {
      const { manifest } = compile(path.join(examplesDir, "python-error-cause-js-catch.poly"));
      const func = manifest.ops.find(
        (op: any) => op.op === "func_def" && op.name === "fail_checkout"
      ) as any;
      const strayPythonFragments = manifest.ops.filter(
        (op: any) => op.op === "exec" && op.runtime === "python" && ["from", "cause"].includes(op.code)
      );

      expect(func?.sourceArtifact?.functionSource).toContain(
        'raise RuntimeError("checkout failed") from cause'
      );
      expect(strayPythonFragments).toEqual([]);
    });

    it("models async streams as explicit materialization and worker joins", () => {
      const { manifest } = compile(path.join(examplesDir, "async-httpx-rxjs-errgroup.poly"));
      const chanOps = manifest.ops.filter((o: any) => o.op === "chan");
      const spawnOps = manifest.ops.filter((o: any) => o.op === "spawn");
      const snapshot = manifest.ops.find(
        (o: any) => o.op === "eval" && String(o.code).includes("Array.from(chunks)")
      ) as any;

      expect(chanOps.map((o: any) => o.action)).toEqual(
        expect.arrayContaining(["make", "send", "close"])
      );
      expect(spawnOps.length).toBe(2);
      expect(snapshot?.runtime).toBe("javascript");
    });

    it("keeps Python async generator consumption lazy in JavaScript", () => {
      const { manifest } = compile(path.join(examplesDir, "python-async-generator-js-consume.poly"));
      const ops: any[] = [];
      const collectOps = (value: any) => {
        if (!value || typeof value !== "object") return;
        if (Array.isArray(value)) {
          for (const item of value) collectOps(item);
          return;
        }
        if (typeof value.op === "string") ops.push(value);
        for (const nested of Object.values(value)) collectOps(nested);
      };
      collectOps(manifest.ops);
      const asyncExec = ops.find(
        (op: any) =>
          op.op === "exec" &&
          op.runtime === "javascript" &&
          String(op.code ?? "").includes('for await (const row of row_stream("break"))')
      ) as any;
      const collector = ops.find((op: any) => op.op === "func_def" && op.name === "collect_rows") as any;

      expect(collector?.bodyRuntime).toBe("javascript");
      expect(collector?.async).toBe(true);
      expect(asyncExec).toBeDefined();
      expect(asyncExec?.captures).toMatchObject({ row_stream: "row_stream" });
    });

    it("keeps JavaScript Map and Set docs snippets live across Python", () => {
      const { manifest } = compile(path.join(examplesDir, "javascript-map-set-docs.poly"));
      const pySummary = manifest.ops.find(
        (op: any) => op.op === "eval" && op.bind === "py_label"
      ) as any;
      const mapMutation = manifest.ops.find(
        (op: any) => op.runtime === "javascript" && String(op.code ?? "").includes('registry.set("gamma"')
      ) as any;
      const setMutation = manifest.ops.find(
        (op: any) => op.runtime === "javascript" && String(op.code ?? "").includes('tags.add("closed")')
      ) as any;

      expect(pySummary?.runtime).toBe("python");
      expect(pySummary?.captures).toMatchObject({ registry: "registry", tags: "tags" });
      expect(mapMutation?.captures).toMatchObject({ registry: "registry" });
      expect(setMutation?.captures).toMatchObject({ tags: "tags" });
    });

    it("keeps framework handlers and server rendering in native runtimes", () => {
      const { manifest } = compile(path.join(examplesDir, "framework-middleware-render.poly"));
      const dashboard = manifest.ops.find(
        (o: any) => o.op === "func_def" && o.name === "dashboard"
      ) as any;
      const renderPage = manifest.ops.find(
        (o: any) => o.op === "func_def" && o.name === "render_page"
      ) as any;
      const rack = manifest.ops.find(
        (o: any) => o.op === "eval" && o.bind === "rack_response"
      ) as any;

      expect(dashboard?.bodyRuntime).toBe("python");
      expect(renderPage?.bodyRuntime).toBe("javascript");
      expect(rack?.code).toContain("Rack");
      expect(rack?.code).toContain("render_page");
    });

    it("compiles the vertical order app as one multi-runtime workflow", () => {
      const { manifest } = compile(path.join(examplesDir, "vertical-order-review-app.poly"));
      const readme = fs.readFileSync(
        path.join(examplesDir, "vertical-order-review-app.README.md"),
        "utf8"
      );
      const expectedOutput = fs.readFileSync(
        path.join(__dirname, "fixtures", "vertical-order-review-app.output.txt"),
        "utf8"
      ).trim();
      const runtimes = new Set(
        manifest.ops.map((o: any) => o.runtime || o.bodyRuntime).filter(Boolean)
      );
      const output = manifest.ops.find(
        (o: any) => o.runtime === "python" && String(o.code).includes("Vertical order app")
      ) as any;
      const goSpawns = manifest.ops.filter((o: any) => o.op === "spawn" && o.runtime === "go");
      const javaFuture = manifest.ops.find(
        (o: any) => o.runtime === "java" && String(o.code).includes("CompletableFuture")
      ) as any;
      const javaService = manifest.ops.find(
        (o: any) => o.runtime === "java" && String(o.code).includes("ObjectMapper")
      ) as any;
      const rubyStage = manifest.ops.find(
        (o: any) => o.runtime === "ruby" && String(o.code).includes("ActiveRecord::Type::String")
      ) as any;
      const activeRecordImport = manifest.ops.find(
        (o: any) => o.op === "import" && o.path === "active_record"
      ) as any;
      const reactRender = manifest.ops.find(
        (o: any) => o.runtime === "javascript" && String(o.code).includes("renderToStaticMarkup")
      ) as any;
      const djangoResponse = manifest.ops.find(
        (o: any) => o.runtime === "python" && String(o.code).includes("JsonResponse")
      ) as any;

      for (const runtime of ["python", "javascript", "java", "go"]) {
        expect(runtimes).toContain(runtime);
      }
      expect(goSpawns.length).toBe(2);
      expect(javaService?.code).toContain("ObjectMapper");
      expect(javaFuture?.code).toContain("CompletableFuture.completedFuture");
      expect(activeRecordImport?.runtime).toBe("ruby");
      expect(rubyStage?.code).toContain('"review-active"');
      expect(reactRender?.code).toContain("React.createElement");
      if (djangoResponse) {
        expect(djangoResponse.code).toContain("JsonResponse");
      }
      expect(output?.code).toContain("Vertical order app");
      expect(readme).toContain("canonical public example");
      expect(readme).toContain(expectedOutput);
    });

    it("keeps ORM client handles local while crossing materialized rows", () => {
      const { manifest } = compile(path.join(examplesDir, "orm-session-boundaries.poly"));
      const prisma = manifest.ops.find(
        (o: any) => o.op === "eval" && o.bind === "prisma"
      ) as any;
      const materialized = manifest.ops.find(
        (o: any) => o.op === "eval" && o.bind === "materialized"
      ) as any;

      expect(prisma?.runtime).toBe("javascript");
      expect(prisma?.code).toContain("new PrismaClient()");
      expect(materialized?.runtime).toBe("javascript");
    });
  });

  describe("hard boundary example suite", () => {
    const hardExamples: Array<{ file: string; runtimes: string[] }> = [
      {
        file: "true-async-stream-boundary.poly",
        runtimes: ["python", "javascript", "go"],
      },
      {
        file: "live-middleware-opaque-handles.poly",
        runtimes: ["python", "javascript", "ruby", "go"],
      },
      {
        file: "database-transaction-resource-boundary.poly",
        runtimes: ["python", "javascript", "ruby", "java"],
      },
      {
        file: "cross-runtime-job-queue.poly",
        runtimes: ["python", "javascript", "ruby", "go"],
      },
      {
        file: "dataframe-arrow-zero-copy-boundary.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "python-arrow-table-js-inspect-docs.poly",
        runtimes: ["python", "javascript"],
      },
      {
        file: "reactive-future-streams.poly",
        runtimes: ["python", "javascript", "java"],
      },
      {
        file: "template-component-rendering-boundary.poly",
        runtimes: ["python", "javascript", "ruby", "java"],
      },
      {
        file: "typed-validation-error-fidelity.poly",
        runtimes: ["python", "javascript", "ruby", "java"],
      },
    ];

    for (const { file, runtimes: expectedRuntimes } of hardExamples) {
      it(`${file} compiles without language tags under generic inference`, () => {
        const { manifest, runtimes, coveredRuntimes, code } = compile(path.join(examplesDir, file));

        expect(code).not.toMatch(/@(py|js|go|rb|java)\(/);
        expect(runtimes).not.toContain("unknown");
        expect(expectedRuntimes.length).toBeGreaterThan(0);
        expect(coveredRuntimes.length).toBeGreaterThan(0);
        expect(manifest.ops.length).toBeGreaterThan(1);
      });
    }

    it("preserves async stream materialization as a manifest channel capture", () => {
      const { manifest } = compile(path.join(examplesDir, "true-async-stream-boundary.poly"));
      const chanOps = manifest.ops.filter((o: any) => o.op === "chan");
      const streamSnapshot = manifest.ops.find(
        (o: any) => o.op === "eval" && String(o.code).includes("Array.from(stream_chunks)")
      ) as any;
      const readFunc = manifest.ops.find(
        (o: any) => o.op === "func_def" && o.name === "read_http_lines"
      ) as any;

      expect(chanOps.map((o: any) => o.action)).toEqual(
        expect.arrayContaining(["make", "send", "close"])
      );
      expect(streamSnapshot?.runtime).toBe("javascript");
      expect(readFunc?.async).toBe(true);
      expect(readFunc?.body.length).toBeGreaterThan(0);
      const nestedOps: any[] = [];
      const collectOps = (value: any) => {
        if (Array.isArray(value)) {
          value.forEach(collectOps);
          return;
        }
        if (!value || typeof value !== "object") return;
        if (typeof value.op === "string") nestedOps.push(value);
        Object.values(value).forEach(collectOps);
      };
      collectOps(readFunc.body);
      const contextManagerEvals = nestedOps.filter(
        (o: any) => o.op === "eval" && ["__using_context_1", "client", "__using_context_2", "response"].includes(o.bind)
      );
      expect(contextManagerEvals).toEqual([
        expect.objectContaining({ runtime: "python", code: "httpx.AsyncClient()", bind: "__using_context_1" }),
        expect.objectContaining({ runtime: "python", code: "await __using_context_1.__aenter__()", bind: "client", async: true }),
        expect.objectContaining({ runtime: "python", code: "client.stream(\"GET\", url)", bind: "__using_context_2" }),
        expect.objectContaining({ runtime: "python", code: "await __using_context_2.__aenter__()", bind: "response", async: true }),
      ]);
      expect(JSON.stringify(contextManagerEvals)).not.toContain(" as ");
      const contextCloses = nestedOps.filter((o: any) => o.op === "resource" && o.action === "close");
      expect(contextCloses).toEqual(expect.arrayContaining([
        expect.objectContaining({
          target: "__using_context_1",
          runtime: "python",
          code: "await __using_context_1.__aexit__(None, None, None)",
          async: true,
        }),
        expect.objectContaining({
          target: "__using_context_2",
          runtime: "python",
          code: "await __using_context_2.__aexit__(None, None, None)",
          async: true,
        }),
      ]));
      expect(manifest.ops).not.toContainEqual(
        expect.objectContaining({ op: "exec", code: "async" })
      );
    });

    it("lowers runnable resource/job example to first-class manifest ops", () => {
      const { manifest } = compile(path.join(runtimeContractsDir, "runnable-resource-job-boundary.poly"));
      const resources = manifest.ops.filter((o: any) => o.op === "resource") as any[];
      const jobs = manifest.ops.filter((o: any) => o.op === "job") as any[];

      expect(resources.map((o: any) => o.action)).toEqual(["open", "close"]);
      expect(jobs.map((o: any) => o.action)).toEqual(["enqueue", "complete", "wait"]);
      expect(jobs[0].runtime).toBe("ruby");
      expect(resources[1].code).toContain("cleanup_log.append");
    });

    it("lowers runnable zero-copy table example to table handle ops", () => {
      const { manifest } = compile(path.join(runtimeContractsDir, "runnable-zero-copy-table-boundary.poly"));
      const tables = manifest.ops.filter((o: any) => o.op === "table") as any[];

      expect(tables.map((o: any) => o.action)).toEqual(["export"]);
      expect(tables[0]).toMatchObject({
        runtime: "python",
        bind: "orders",
        format: "arrow_c_data",
        ownership: "borrowed",
      });
      expect(tables[0].value).toEqual({ kind: "literal", value: "np.array([1, 2, 3])" });
      expect(manifest.bridges).toEqual(expect.arrayContaining([
        expect.objectContaining({ binding: "orders", op: "share_memory" }),
      ]));
    });

    it("keeps generated runnable manifests in sync with checked-in golden JSON", () => {
      for (const name of [
        "runnable-resource-job-boundary",
        "runnable-zero-copy-table-boundary",
      ]) {
        const { manifest } = compile(path.join(runtimeContractsDir, `${name}.poly`));
        const fixturePath = path.join(__dirname, "fixtures", `${name}.manifest.json`);
        const expected = JSON.parse(fs.readFileSync(fixturePath, "utf8"));
        expect(manifest).toEqual(expected);
      }
    });

    it("captures framework request handlers without annotation pragmas", () => {
      const { manifest } = compile(path.join(examplesDir, "live-middleware-opaque-handles.poly"));
      const fastapiHandler = manifest.ops.find(
        (o: any) => o.op === "func_def" && o.name === "fastapi_handler"
      ) as any;
      const middleware = manifest.ops.find(
        (o: any) => o.op === "func_def" && o.name === "express_middleware"
      ) as any;

      expect(fastapiHandler?.bodyRuntime).toBe("python");
      expect(middleware?.bodyRuntime).toBe("javascript");
    });
  });

  describe("express-python-view.poly", () => {
    const { manifest, runtimes } = compile(
      path.join(examplesDir, "express-python-view.poly")
    );

    it("imports Express in JavaScript and defines the view in Python", () => {
      expect(runtimes).not.toContain("unknown");
      expect(runtimes[0]).toBe("javascript");
      expect(runtimes[1]).toBe("python");
    });

    it("compiles the Python view as a manifest-callable handler", () => {
      const pyView = manifest.ops.find(
        (o: any) => o.op === "func_def" && o.name === "py_view"
      ) as any;

      expect(pyView).toBeDefined();
      expect(pyView.bodyRuntime).toBe("python");
    });

    it("keeps the Express route native while delegating into Python", () => {
      const route = manifest.ops.find(
        (o: any) => o.op === "eval" && o.bind === "express_view"
      ) as any;
      const registration = manifest.ops.find(
        (o: any) => o.op === "exec" && String((o as any).code).includes("app.get")
      ) as any;

      expect(route?.runtime).toBe("javascript");
      expect(route?.code).toContain("py_view(req.path)");
      expect(route?.code).not.toContain("JSON.parse");
      expect(registration?.runtime).toBe("javascript");
    });
  });

  // The Rust support acceptance target (docs/rust-runtime-design.md): a
  // four-language review service with no runtime annotations anywhere. It
  // runs unchanged under both the binary and libomnivm.so hosts; this e2e
  // entry pins the compiled shape.
  describe("rust-review-service.poly", () => {
    const { manifest, runtimes } = compile(
      path.join(examplesDir, "rust-review-service.poly")
    );

    it("has no unknown runtimes", () => {
      expect(runtimes).not.toContain("unknown");
    });

    it("emits one shared Rust unit across all three func_defs", () => {
      const rustDefs = manifest.ops.filter(
        (o: any) => o.op === "func_def" && o.bodyRuntime === "rust"
      ) as any[];
      expect(rustDefs.map((d) => d.name).sort()).toEqual([
        "classify",
        "enrich",
        "heavy_stats",
      ]);
      const sources = new Set(rustDefs.map((d) => d.source));
      expect(sources.size).toBe(1);
      for (const def of rustDefs) {
        expect(def.exports).toEqual(["enrich", "classify", "heavy_stats"]);
      }
      const enrich = rustDefs.find((d) => d.name === "enrich");
      expect(enrich.async).toBe(true);
      expect(enrich.source).toContain("omnivm::export_async_fn!(OmniVMCall_enrich, enrich, 1);");
      expect(enrich.source).toContain("omnivm::export_fn!(OmniVMCall_classify, classify, (df));");
      expect(enrich.source).toContain('#[serde(tag = "type")]');
    });

    it("routes data loading through python and the HTTP surface through javascript", () => {
      const pythonEvals = manifest.ops.filter(
        (o: any) => o.op === "eval" && o.runtime === "python"
      ) as any[];
      expect(pythonEvals.some((o) => String(o.code).includes("read_parquet"))).toBe(true);

      const rustEvals = manifest.ops.filter(
        (o: any) => (o.op === "eval" || o.op === "exec") && o.runtime === "rust"
      ) as any[];
      expect(rustEvals.some((o) => String(o.code).startsWith("heavy_stats("))).toBe(true);
      expect(rustEvals.some((o) => String(o.code).startsWith("classify("))).toBe(true);

      const handler = manifest.ops.find(
        (o: any) => o.op === "exec" && String(o.code).includes("app.get")
      ) as any;
      expect(handler?.runtime).toBe("javascript");
      expect(String(handler?.code)).toContain("await enrich(");
    });
  });
});
