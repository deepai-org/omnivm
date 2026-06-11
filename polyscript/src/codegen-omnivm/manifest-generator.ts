/**
 * ManifestCodeGenerator: generates a dispatch manifest (JSON IR)
 * that OmniVM interprets directly.
 *
 * No language is "on top" — the manifest is a structured sequence of
 * operations that tell OmniVM which runtime to dispatch each code
 * fragment to, how data flows between runtimes via named bindings,
 * and how control flow and async coordination work.
 */
import * as AST from '../ast';
import {
  OmniRuntime,
  RuntimeAffinity,
  AnnotatedProgram,
} from '../runtime-resolver/types';
import { consolidateBlocks, isConsolidatable, isCompiledRuntime, RuntimeBlock } from './runtime-blocks';
import {
  exprToCode,
  exprToCodeForRuntime,
  nodeToSourceCode,
  paramToCode,
  importSpecsToCode,
  tagToRuntime,
  isExprKind,
  collectFreeIdentifiers,
} from './source-reconstruct';
import {
  DispatchManifest,
  ManifestOp,
  ManifestBridgeOp,
  ExecOp,
  EvalOp,
  ExecCompiledOp,
  EvalCompiledOp,
  DeclareOp,
  AssignOp,
  FuncDefOp,
  ReturnOp,
  IfOp,
  LoopOp,
  TryOp,
  ThrowOp,
  ManifestCatch,
  ParallelOp,
  ConcatOp,
  ImportOp,
  NativeOp,
  ChanOp,
  ResourceOp,
  TableOp,
  JobOp,
  SelectOp,
  SelectCase,
  SpawnOp,
  YieldOp,
  AwaitOp,
  ParamDef,
  CallableShape,
  ManifestValue,
  ConditionExpr,
  CaptureMap,
  ConcatSegment,
  ManifestDiagnostic,
  RustSourceMapEntry,
} from './manifest-types';
import { BoundaryChecker, typeToString } from '../type-system/boundary-checker';
import { lowerType } from '../type-system/lowering';
import * as C from '../type-system/canonical';
import { lowerAnnotatedProgram } from './lowering';
import { LoweredDefineFunc, LoweredManifestIR, LoweredManifestNode } from './lowering-ir';
import { extractRustFnSignatures, attributePrefixStart } from '../rust-item-scanner';

/** Fn signature shape shared by raw-scanned RustItems and Rust FuncDecls. */
type RustExportSig = AST.RustItem["fns"][number];

type BindingKind = "value" | "channel" | "stream" | "resource" | "table" | "job_handle" | "spawn_handle" | "function";

/**
 * One section piece of the assembled Rust compilation unit: the verbatim
 * text plus, for slices lifted from the .poly source, the byte offset where
 * the slice begins (undefined for generated glue — lowered statics, shims).
 */
type RustUnitPiece = { text: string; polyOffset?: number };

export class ManifestCodeGenerator {
  private affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity> = new Map();
  private defaultRuntime: OmniRuntime = OmniRuntime.JavaScript;
  private source?: string;
  private destructureCounter = 0;
  /** Tracks variable bindings and their defining runtime for captures analysis. */
  private bindingTable: Map<string, OmniRuntime> = new Map();
  /** Tracks manifest-visible binding roles for semantic diagnostics. */
  private bindingKinds: Map<string, BindingKind> = new Map();
  /** Tracks owner-side cleanup semantics for manifest resource handles. */
  private resourceDisposers: Map<string, string> = new Map();
  /** Records why a binding was assigned to its owning runtime. */
  private bindingAffinities: Map<string, RuntimeAffinity> = new Map();
  /** Non-fatal diagnostics that help users understand runtime boundary mistakes. */
  private diagnostics: ManifestDiagnostic[] = [];
  private boundaryDiagnosticKeys: Set<string> = new Set();
  /** Nesting depth inside func_def bodies. When > 0, captures include all external refs. */
  private funcDepth: number = 0;
  /** Type system boundary checker — validates types at cross-runtime crossings. */
  private typeChecker: BoundaryChecker = new BoundaryChecker();
  /** Registry of named types (class/interface/enum) for resolving type annotations. */
  private typeRegistry: Map<string, C.CanonicalType> = new Map();
  private typedBindingKinds: Map<string, BindingKind> = new Map();
  private typedBindingRuntimeHints: Map<string, OmniRuntime> = new Map();
  /** Bridge operations inferred directly from manifest captures and type annotations. */
  private explicitBridgeOps: ManifestBridgeOp[] = [];
  private usingCounter = 0;
  /** Lowered manifest IR for diagnostics and future manifest emission. */
  private loweredIR?: LoweredManifestIR;
  /** Lowered nodes indexed by their source AST node for incremental IR-backed emission. */
  private loweredNodesBySource: Map<AST.Program | AST.Decl | AST.Stmt | AST.Expr, LoweredManifestNode[]> = new Map();
  /** Go functions declared in the current source file, used to keep main() self-contained. */
  private goFuncDecls: Map<string, AST.FuncDecl> = new Map();
  /**
   * The single shared Rust compilation unit for this program: every Rust
   * top-level item (use statements, structs, enums, impls, lowered statics)
   * plus all Rust fns and one export shim macro line per EXPORTED fn.
   * Every Rust func_def op carries this same source so the host compiles
   * and caches exactly one cdylib.
   *
   * A fn is exported only when it is referenced from OUTSIDE the unit
   * (other-runtime expressions, top-level poly statements, spawn ops,
   * stubs). Internal helpers — called only from other Rust items, or not
   * at all — stay in the unit source verbatim but get no shim, no entry in
   * `exports`, and no func_def op.
   */
  private rustUnit?: {
    source: string;
    exports: string[];
    /** Module-level Rust nodes absorbed into the unit (emit no standalone ops). */
    absorbed: Set<AST.Decl | AST.Stmt>;
    /** Fns proven internal-only (or generic, unexportable): no func_def ops. */
    internalFns: Set<string>;
    /**
     * Line mapping from unit source back to the .poly file: one entry per
     * verbatim item slice (generated glue gets none). The host uses it to
     * rewrite rustc diagnostics to .poly coordinates.
     */
    sourceMap: RustSourceMapEntry[];
  };
  /** The .poly file name source_map coordinates refer to, when known. */
  private sourceFile?: string;

  /**
   * Generate a dispatch manifest from an annotated program.
   * Returns a DispatchManifest object (JSON-serializable).
   */
  generate(annotated: AnnotatedProgram, options?: { sourceFile?: string }): DispatchManifest {
    this.affinityMap = annotated.affinityMap;
    this.defaultRuntime = annotated.defaultRuntime;
    this.source = annotated.source;
    this.sourceFile = options?.sourceFile;
    this.bindingTable = new Map();
    this.bindingKinds = new Map();
    this.resourceDisposers = new Map();
    this.bindingAffinities = new Map();
    this.diagnostics = [];
    this.boundaryDiagnosticKeys = new Set();
    this.typeChecker = new BoundaryChecker();
    this.typeRegistry = new Map();
    this.typedBindingKinds = new Map();
    this.typedBindingRuntimeHints = new Map();
    this.explicitBridgeOps = [];
    this.usingCounter = 0;
    this.goFuncDecls = this.indexGoFuncDecls(annotated.program.body);
    this.rustUnit = undefined;
    this.collectRustUnit(annotated.program.body);
    this.loweredIR = lowerAnnotatedProgram(annotated);
    this.loweredNodesBySource = this.indexLoweredNodes(this.loweredIR);

    // Pass 1: Declare all typed bindings in the type checker
    this.declareTypedBindings(annotated.program.body);

    const blocks = consolidateBlocks(annotated.program.body, this.affinityMap);
    const ops: ManifestOp[] = [];

    for (const block of blocks) {
      ops.push(...this.emitBlock(block));
    }
    this.appendGoMainEntrypoint(annotated.program.body, ops);

    // Collect bridge ops and type summary from the checker
    const bridgeOps = this.typeChecker.getBridgeOps().map(b => this.toBridgeManifestOp(b));
    const bridges = this.dedupeBridgeOps([...bridgeOps, ...this.explicitBridgeOps]);
    const summary = this.typeChecker.getSummary();

    const manifest: DispatchManifest = {
      version: 1,
      defaultRuntime: this.defaultRuntime,
      ops,
    };

    // Only attach type info if there are actual crossings
    if (bridges.length > 0) {
      manifest.bridges = bridges;
    }
    if (summary.crossings > 0) {
      manifest.typeSummary = summary;
    }

    if (this.diagnostics.length > 0) {
      manifest.diagnostics = this.diagnostics;
    }

    return manifest;
  }

  /**
   * Generate and return pretty-printed JSON.
   */
  generateJSON(annotated: AnnotatedProgram, options?: { sourceFile?: string }): string {
    return JSON.stringify(this.generate(annotated, options), null, 2);
  }

  private indexLoweredNodes(ir: LoweredManifestIR): Map<AST.Program | AST.Decl | AST.Stmt | AST.Expr, LoweredManifestNode[]> {
    const bySource = new Map<AST.Program | AST.Decl | AST.Stmt | AST.Expr, LoweredManifestNode[]>();
    for (const node of ir.nodes) {
      const existing = bySource.get(node.sourceNode) || [];
      existing.push(node);
      bySource.set(node.sourceNode, existing);
    }
    return bySource;
  }

  private loweredNodesFor(node: AST.Program | AST.Decl | AST.Stmt | AST.Expr): LoweredManifestNode[] {
    return this.loweredNodesBySource.get(node) || [];
  }

  private loweredNodeForBinding(
    source: AST.Decl | AST.Stmt | AST.Expr,
    bind: string | undefined,
  ): LoweredManifestNode | undefined {
    return this.loweredNodesFor(source).find(node => "bind" in node && node.bind === bind);
  }

  private loweredDefineFuncFor(source: AST.FuncDecl): LoweredDefineFunc | undefined {
    return this.loweredNodesFor(source).find(
      (node): node is LoweredDefineFunc => node.kind === "DefineFunc" && node.name === source.name.name,
    );
  }

  // ─── Captures Analysis ──────────────────────────────────────────

  /**
   * Compute captures for an expression being emitted in a target runtime.
   * Returns a CaptureMap if any referenced identifiers were bound in
   * a different runtime, or undefined if no cross-runtime refs exist.
   */
  private computeCaptures(
    node: AST.Expr | AST.Stmt | AST.Decl,
    targetRuntime: OmniRuntime,
  ): CaptureMap | undefined {
    const ids = collectFreeIdentifiers(node);
    const captures: CaptureMap = {};
    for (const name of ids) {
      const boundIn = this.bindingTable.get(name);
      if (!boundIn) continue;
      // Inside func_def body: capture ALL external bindings (params + top-level scope).
      // OmniVM executes function bodies in an isolated scope — all external values
      // must be explicitly injected via captures.
      if (this.funcDepth > 0) {
        captures[name] = name;
        // Type-check the crossing if it's actually cross-runtime
        if (boundIn !== targetRuntime) {
          this.checkTypeCrossing(name, targetRuntime);
          this.addBridgeHintForCapture(name, boundIn, targetRuntime);
          if (this.bindingKind(name) !== "channel") {
            this.addBoundaryDiagnostic(name, boundIn, targetRuntime, node);
          }
        }
      } else if (boundIn !== targetRuntime) {
        // Top-level: only capture cross-runtime references.
        this.checkTypeCrossing(name, targetRuntime);
        this.addBridgeHintForCapture(name, boundIn, targetRuntime);
        if (this.bindingKind(name) !== "channel") {
          this.addBoundaryDiagnostic(name, boundIn, targetRuntime, node);
        }
        captures[name] = name;
      }
    }
    return Object.keys(captures).length > 0 ? captures : undefined;
  }

  /**
   * Record a variable binding with its runtime.
   */
  private recordBinding(
    name: string,
    runtime: OmniRuntime,
    kind: BindingKind = "value",
    sourceNode?: AST.Program | AST.Decl | AST.Stmt | AST.Expr,
  ): void {
    this.bindingTable.set(name, runtime);
    this.bindingKinds.set(name, kind);
    const affinity = sourceNode && sourceNode.kind !== "Program" ? this.affinityMap.get(sourceNode) : undefined;
    this.bindingAffinities.set(name, affinity && affinity.runtime === runtime
      ? affinity
      : {
          runtime,
          confidence: "inferred",
          evidence: [{ type: "scope", detail: "manifest binding recorded during lowering" }],
        });
  }

  private recordResourceBinding(
    name: string,
    runtime: OmniRuntime,
    sourceNode: AST.Program | AST.Decl | AST.Stmt | AST.Expr,
    op: ResourceOp,
  ): void {
    this.recordBinding(name, runtime, "resource", sourceNode);
    if (op.disposer) {
      this.resourceDisposers.set(name, op.disposer);
    }
  }

  private addDiagnostic(
    severity: ManifestDiagnostic["severity"],
    code: string,
    message: string,
    node?: { span?: AST.Span },
  ): void {
    const span = node?.span;
    this.diagnostics.push({
      severity,
      code,
      message,
      ...(span ? { span: this.diagnosticSpan(span) } : {}),
    });
  }

  private diagnosticSpan(span: AST.Span): ManifestDiagnostic["span"] {
    if (!this.source) return { start: span.start, end: span.end };
    const prefix = this.source.slice(0, span.start);
    const lines = prefix.split(/\r?\n/);
    return {
      start: span.start,
      end: span.end,
      line: lines.length,
      column: lines[lines.length - 1].length + 1,
    };
  }

  private bindingKind(name: string): BindingKind | undefined {
    return this.bindingKinds.get(name);
  }

  private addBridgeHintForCapture(binding: string, from: OmniRuntime, to: OmniRuntime): void {
    const hint = this.bridgeHintForBinding(binding);
    if (!hint) return;
    this.addExplicitBridge({
      binding,
      from,
      to,
      op: hint.op,
      ...(hint.meta ? { meta: hint.meta } : {}),
    });
  }

  private bridgeHintForBinding(binding: string): { op: string; meta?: Record<string, unknown> } | undefined {
    const kind = this.bindingKind(binding);
    if (kind === "channel" || kind === "stream") {
      return { op: "stream_proxy", meta: { backpressure: true } };
    }
    if (kind === "resource") {
      return { op: "proxy_with_finalizer", meta: { disposer: this.resourceDisposers.get(binding) || "close" } };
    }
    if (kind === "table") {
      return { op: "share_memory", meta: { format: "arrow_c_data", ownership: "borrowed" } };
    }

    const typed = this.typeChecker.getBinding(binding);
    if (!typed) return undefined;

    switch (typed.type.kind) {
      case "stream":
        return { op: "stream_proxy", meta: { backpressure: (typed.type as C.StreamType).backpressure ?? true } };
      case "disposable":
        return { op: "attach_disposer", meta: { disposer: (typed.type as C.DisposableType).disposer || "dispose" } };
      case "array":
        return { op: "copy_array" };
      case "func":
        return { op: "proxy_callable" };
      case "struct":
        if ((typed.type as C.StructType).nominal) {
          return { op: "proxy_with_finalizer", meta: { type: (typed.type as C.StructType).name || binding } };
        }
        return undefined;
      default:
        return undefined;
    }
  }

  private addExplicitBridge(bridge: ManifestBridgeOp): void {
    const key = this.bridgeKey(bridge);
    if (this.explicitBridgeOps.some(existing => this.bridgeKey(existing) === key)) return;
    this.explicitBridgeOps.push(bridge);
  }

  private dedupeBridgeOps(bridges: ManifestBridgeOp[]): ManifestBridgeOp[] {
    const seen = new Set<string>();
    const out: ManifestBridgeOp[] = [];
    for (const bridge of bridges) {
      const key = this.bridgeKey(bridge);
      if (seen.has(key)) continue;
      seen.add(key);
      out.push(bridge);
    }
    return out;
  }

  private bridgeKey(bridge: ManifestBridgeOp): string {
    return [
      bridge.binding,
      bridge.op,
      bridge.from || "",
      bridge.to || "",
      JSON.stringify(bridge.meta || {}),
    ].join("|");
  }

  private addBoundaryDiagnostic(
    binding: string,
    sourceRuntime: OmniRuntime,
    targetRuntime: OmniRuntime,
    node: AST.Expr | AST.Stmt | AST.Decl,
  ): void {
    const span = node.span;
    const spanKey = span ? `${span.start}-${span.end}` : "unknown";
    const key = `${binding}|${sourceRuntime}|${targetRuntime}|${spanKey}`;
    if (this.boundaryDiagnosticKeys.has(key)) return;
    this.boundaryDiagnosticKeys.add(key);

    const sourceAffinity = this.bindingAffinities.get(binding);
    const targetAffinity = this.affinityMap.get(node);
    const sourceWhy = sourceAffinity
      ? this.describeAffinity(sourceAffinity)
      : `${sourceRuntime} (binding table; no resolver evidence recorded)`;
    const targetWhy = targetAffinity
      ? this.describeAffinity(targetAffinity)
      : `${targetRuntime} (manifest lowering fallback)`;

    this.addDiagnostic(
      "info",
      "runtime-boundary-capture",
      `Inserted capture boundary for '${binding}' from ${sourceRuntime} to ${targetRuntime}. Source binding: ${sourceWhy}. Target expression: ${targetWhy}.`,
      node,
    );
  }

  private describeAffinity(affinity: RuntimeAffinity): string {
    const evidence = affinity.evidence.length > 0
      ? affinity.evidence.map(e => `${e.type}: ${e.detail}`).join("; ")
      : "no evidence";
    return `${affinity.runtime} (${affinity.confidence}; ${evidence})`;
  }

  private diagnoseChannelRef(channel: string, action: "send" | "recv" | "close", node?: { span?: AST.Span }): void {
    if (!/^[A-Za-z_$][\w$]*$/.test(channel)) return;
    const kind = this.bindingKind(channel);
    if (!kind) {
      this.addDiagnostic(
        "warning",
        "unknown-channel-binding",
        `Channel ${action} references '${channel}', but no manifest channel with that name has been created yet. Create it with 'const ${channel} = make(size)' before using it.`,
        node,
      );
      return;
    }
    if (kind !== "channel") {
      this.addDiagnostic(
        "warning",
        "non-channel-binding",
        `Channel ${action} references '${channel}', but '${channel}' is currently a ${kind.replace("_", " ")} binding, not a channel.`,
        node,
      );
    }
  }

  private diagnoseExpr(expr: AST.Expr, runtime: OmniRuntime): void {
    this.diagnoseBoundaryExpr(expr);
    if (expr.kind !== "Call" || expr.callee.kind !== "Identifier") return;
    if (expr.callee.name !== "wait") return;
    if (runtime !== OmniRuntime.Go) {
      this.addDiagnostic(
        "warning",
        "wait-runtime",
        `wait(...) is a manifest/Go synchronization helper, but this expression resolved to ${runtime}. Use it in Go-owned concurrency flow or add a Go runtime tag if inference is ambiguous.`,
        expr,
      );
    }
    for (const arg of expr.args) {
      if (arg.kind !== "Identifier") {
        this.addDiagnostic(
          "warning",
          "wait-non-handle-expression",
          `wait(...) can only selectively join spawn handles returned by 'go worker(...)'. Argument '${exprToCode(arg, this.source)}' is not a named handle.`,
          arg,
        );
        continue;
      }
      const kind = this.bindingKind(arg.name);
      if (kind !== "spawn_handle") {
        const detail = kind ? `it is a ${kind.replace("_", " ")} binding` : "it has not been bound yet";
        this.addDiagnostic(
          "warning",
          "wait-non-handle-binding",
          `wait(${arg.name}) expects '${arg.name}' to be a spawn handle returned by 'const ${arg.name} = go worker(...)'; ${detail}.`,
          arg,
        );
      }
    }
  }

  private diagnoseBoundaryExpr(expr: AST.Expr): void {
    const materialized = this.detectArrayFrom(expr);
    if (materialized && materialized.kind === "Identifier") {
      const kind = this.bindingKind(materialized.name);
      if (!kind) {
        this.addDiagnostic(
          "warning",
          "unknown-stream-materialization",
          `Array.from(${materialized.name}) materializes a stream-like value, but '${materialized.name}' has not been bound yet. Channels must be created with make(...) before materialization.`,
          materialized,
        );
      } else if (kind !== "channel" && kind !== "stream") {
        this.addDiagnostic(
          "warning",
          "non-stream-materialization",
          `Array.from(${materialized.name}) is an explicit stream materialization, but '${materialized.name}' is a ${kind.replace("_", " ")} binding, not a channel or stream.`,
          materialized,
        );
      }
    }
  }

  private detectArrayFrom(expr: AST.Expr): AST.Expr | undefined {
    if (expr.kind !== "Call") return undefined;
    if (expr.callee.kind !== "Member") return undefined;
    const callee = expr.callee;
    if (callee.property.name !== "from") return undefined;
    if (callee.object.kind !== "Identifier" || callee.object.name !== "Array") return undefined;
    return expr.args[0];
  }

  // ─── Type System Integration ─────────────────────────────────────

  /**
   * Walk top-level declarations and register typed bindings with the BoundaryChecker.
   */
  private declareTypedBindings(body: (AST.Decl | AST.Stmt | AST.Expr)[]): void {
    // Pass 1: Register all typed declarations
    for (const node of body) {
      const aff = this.affinityMap.get(node);
      const runtime = (aff?.runtime || this.defaultRuntime) as string as
        "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";

      switch (node.kind) {
        case "FuncDecl": {
          const funcType = this.funcDeclToCanonical(node);
          this.typeChecker.declare(node.name.name, funcType, runtime);
          break;
        }
        case "VarDecl": {
          const varType = this.resolveFromRegistry(node.type ? lowerType(node.type, runtime) : C.ANY);
          for (const name of node.names) {
            this.typeChecker.declare(name.name, varType, runtime);
            this.recordTypedBindingHint(name.name, node.type);
          }
          break;
        }
        case "ConstDecl": {
          const constType = this.resolveFromRegistry(node.type ? lowerType(node.type, runtime) : C.ANY);
          for (const name of node.names) {
            this.typeChecker.declare(name.name, constType, runtime);
            this.recordTypedBindingHint(name.name, node.type);
          }
          break;
        }
        case "ShortDecl":
          if (node.pairs) {
            for (const pair of node.pairs) {
              this.typeChecker.declare(pair.name.name, C.ANY, runtime);
            }
          }
          break;
        case "ClassDecl": {
          const structType = this.classDeclToCanonical(node, runtime);
          this.typeChecker.declare(node.name.name, structType, runtime);
          // Also register type so variables typed as this class resolve to the struct
          this.typeRegistry.set(node.name.name, structType);
          break;
        }
        case "InterfaceDecl": {
          const ifaceType = this.interfaceDeclToCanonical(node, runtime);
          this.typeChecker.declare(node.name.name, ifaceType, runtime);
          this.typeRegistry.set(node.name.name, ifaceType);
          break;
        }
        case "TypeDecl": {
          const aliasType = this.resolveFromRegistry(lowerType(node.definition, runtime));
          this.typeChecker.declare(node.name.name, aliasType, runtime);
          this.typeRegistry.set(node.name.name, aliasType);
          break;
        }
        case "EnumDecl": {
          const enumType = this.enumDeclToCanonical(node);
          this.typeChecker.declare(node.name.name, enumType, runtime);
          this.typeRegistry.set(node.name.name, enumType);
          break;
        }
      }
    }

    // Pass 2: Check cross-runtime function calls and variable assignments
    this.checkCrossRuntimeCalls(body);
  }

  /**
   * Walk the AST looking for cross-runtime interactions:
   * 1. Function calls where callee is declared in a different runtime
   * 2. Variable declarations that receive the return value of a cross-runtime call
   *
   * Recurses into function bodies to find nested cross-runtime calls.
   */
  private checkCrossRuntimeCalls(body: (AST.Decl | AST.Stmt | AST.Expr)[], callerRuntime?: string): void {
    for (const node of body) {
      const nodeAff = this.affinityMap.get(node);
      const nodeRuntime = (nodeAff?.runtime || callerRuntime || this.defaultRuntime) as string as
        "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";
      // The "caller" runtime is the enclosing function's runtime.
      // For cross-runtime call checking, we care about where the CALL happens,
      // not what runtime the callee expression was assigned to.
      const effectiveCallerRuntime = callerRuntime || nodeRuntime;

      // Recurse into function bodies with the function's own runtime as caller context
      if (node.kind === "FuncDecl" && node.body?.statements) {
        const funcRuntime = (nodeAff?.runtime || callerRuntime || this.defaultRuntime) as string;
        // Register variables declared inside the function body
        this.declareInnerBindings(node.body.statements, funcRuntime);
        this.checkCrossRuntimeCalls(node.body.statements, funcRuntime);
      }

      // Check const/var declarations: const x: TargetType = crossRuntimeCall()
      if ((node.kind === "ConstDecl" || node.kind === "VarDecl") && node.type) {
        const targetType = lowerType(node.type, effectiveCallerRuntime as any);
        const values = node.kind === "ConstDecl" ? node.values : node.values;
        if (values) {
          for (const val of values) {
            this.checkCallReturnType(val, effectiveCallerRuntime, targetType);
          }
        }
      }

      // Check expression statements: standalone calls like calculate(v)
      if (node.kind === "ExprStmt") {
        this.checkCallArgTypes(node.expr, effectiveCallerRuntime);
      }

      // Control-flow narrowing: if (x !== null) { ... }
      if (node.kind === "If") {
        const ifNode = node as AST.If;
        for (const arm of ifNode.arms) {
          const narrowings = this.extractNarrowings(arm.test);
          if (narrowings.size > 0) {
            this.typeChecker.pushNarrow(narrowings);
            if (arm.body?.statements) {
              this.checkCrossRuntimeCalls(arm.body.statements, effectiveCallerRuntime);
            }
            this.typeChecker.popNarrow();
          } else if (arm.body?.statements) {
            this.checkCrossRuntimeCalls(arm.body.statements, effectiveCallerRuntime);
          }
        }
        // Check else branch without narrowing
        if (ifNode.elseBody?.statements) {
          this.checkCrossRuntimeCalls(ifNode.elseBody.statements, effectiveCallerRuntime);
        }
      }
    }
  }

  /**
   * Extract narrowing information from an if-condition.
   * Detects patterns like `x !== null`, `x != null`, `x !== undefined`, `x != nil`.
   */
  private extractNarrowings(test: AST.Expr): Map<string, C.CanonicalType> {
    const narrowings = new Map<string, C.CanonicalType>();
    if (test.kind !== "Binary") return narrowings;
    const bin = test as AST.Binary;
    const op = bin.op;

    // x !== null / x != null / x !== undefined / x != nil
    if (op === "!==" || op === "!=") {
      const [ident, nullish] = this.extractNullCheck(bin);
      if (ident && nullish) {
        const binding = this.typeChecker.getBinding(ident);
        if (binding) {
          const narrowed = BoundaryChecker.narrowType(binding.type, "not-null");
          if (narrowed) narrowings.set(ident, narrowed);
        }
      }
    }

    // && chains: x !== null && y !== undefined
    if (op === "&&") {
      const left = this.extractNarrowings(bin.left);
      const right = this.extractNarrowings(bin.right);
      for (const [k, v] of left) narrowings.set(k, v);
      for (const [k, v] of right) narrowings.set(k, v);
    }

    return narrowings;
  }

  /**
   * From a binary != or !== expression, extract (identName, true) if it's a null check.
   */
  private extractNullCheck(bin: AST.Binary): [string | null, boolean] {
    const isNullLiteral = (e: AST.Expr) =>
      e.kind === "NullLiteral" ||
      (e.kind === "Identifier" && ((e as AST.Identifier).name === "null" || (e as AST.Identifier).name === "nil" || (e as AST.Identifier).name === "undefined" || (e as AST.Identifier).name === "None"));

    if (bin.left.kind === "Identifier" && isNullLiteral(bin.right)) {
      return [(bin.left as AST.Identifier).name, true];
    }
    if (bin.right.kind === "Identifier" && isNullLiteral(bin.left)) {
      return [(bin.right as AST.Identifier).name, true];
    }
    return [null, false];
  }

  /**
   * If expr is a call to a cross-runtime function, check that the return type
   * is compatible with expectedType.
   */
  private checkCallReturnType(expr: AST.Expr, callerRuntime: string, expectedType: C.CanonicalType): void {
    if (expr.kind !== "Call" || expr.callee.kind !== "Identifier") return;
    const funcName = expr.callee.name;
    const binding = this.typeChecker.getBinding(funcName);
    if (!binding || binding.runtime === callerRuntime) return;
    if (binding.type.kind !== "func") return;

    const funcType = binding.type as C.FuncType;

    // Register the return value as a temporary binding in the callee's runtime,
    // then check crossing to the caller's runtime with the expected type.
    const returnBindingName = `${funcName}()`;
    this.typeChecker.declare(returnBindingName, funcType.returns, binding.runtime);
    this.typeChecker.checkCrossing(
      returnBindingName,
      callerRuntime as any,
      expectedType,
    );

    // Also check argument types
    this.checkCallArgTypesForFunc(expr, callerRuntime, funcType, binding.runtime);
  }

  /**
   * If expr is a call to a cross-runtime function, check argument types.
   */
  private checkCallArgTypes(expr: AST.Expr, callerRuntime: string): void {
    if (expr.kind !== "Call" || expr.callee.kind !== "Identifier") return;
    const funcName = expr.callee.name;
    const binding = this.typeChecker.getBinding(funcName);
    if (!binding || binding.runtime === callerRuntime) return;
    if (binding.type.kind !== "func") return;

    const funcType = binding.type as C.FuncType;
    this.checkCallArgTypesForFunc(expr, callerRuntime, funcType, binding.runtime);
  }

  /**
   * Check each argument's type against the function's parameter types.
   */
  private checkCallArgTypesForFunc(
    call: AST.Call,
    callerRuntime: string,
    funcType: C.FuncType,
    targetRuntime: string,
  ): void {
    for (let i = 0; i < call.args.length && i < funcType.params.length; i++) {
      const arg = call.args[i];
      const paramType = funcType.params[i].type;

      // Try to resolve the argument's type
      let argType: C.CanonicalType = C.ANY;
      if (arg.kind === "Identifier") {
        const argBinding = this.typeChecker.getBinding(arg.name);
        if (argBinding) argType = argBinding.type;
      } else if (arg.kind === "StringLiteral") {
        argType = C.STRING;
      } else if (arg.kind === "NumericLiteral") {
        argType = C.FLOAT64; // JS numbers are f64
      } else if (arg.kind === "BooleanLiteral") {
        argType = C.BOOL;
      } else if (arg.kind === "Lambda") {
        // Infer function type from lambda's typed params
        const lambda = arg as AST.Lambda;
        const params = (lambda.params || []).map((p: AST.Param) => ({
          name: p.name?.kind === "Identifier" ? p.name.name : undefined,
          type: p.type ? lowerType(p.type, callerRuntime as any) : C.ANY,
        }));
        const returns = lambda.returnType ? lowerType(lambda.returnType, callerRuntime as any) : C.ANY;
        argType = { kind: "func", params, returns };
      }

      if (argType.kind !== "any") {
        const argBindingName = `arg${i}:${call.callee.kind === "Identifier" ? call.callee.name : "?"}`;
        this.typeChecker.declare(argBindingName, argType, callerRuntime as any);
        this.typeChecker.checkCrossing(
          argBindingName,
          targetRuntime as any,
          paramType,
          call.span,
        );
      }
    }
  }

  /**
   * Register typed bindings from inside a function body.
   * If no type annotation, infer from the initializer (e.g., call to a known function).
   */
  private declareInnerBindings(body: (AST.Decl | AST.Stmt | AST.Expr)[], runtime: string): void {
    type RT = "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";
    for (const node of body) {
      if (node.kind === "VarDecl" || node.kind === "ConstDecl") {
        let t: C.CanonicalType | undefined;
        if (node.type) {
          t = lowerType(node.type, runtime as RT as any);
          // Enrich opaque named structs with field info from the type registry
          t = this.resolveFromRegistry(t);
        } else {
          // Infer type from initializer: if it's a call to a known function, use its return type
          const values = node.kind === "ConstDecl" ? node.values : node.values;
          if (values && values.length > 0) {
            t = this.inferExprType(values[0]);
          }
        }
        if (t) {
          const names = node.names;
          // Determine the binding's runtime from the expression's affinity
          const nodeAff = this.affinityMap.get(node);
          const bindRuntime = (nodeAff?.runtime || runtime) as RT;
          for (const name of names) {
            this.typeChecker.declare(name.name, t, bindRuntime);
          }
        }
      }
    }
  }

  /**
   * Try to infer the canonical type of an expression.
   * Handles:
   * - Calls to known functions (return type)
   * - Member access on known structs (field type)
   * - Array/object literals
   * - Identifiers (bound type)
   * - Generic calls with type arguments (instantiation)
   */
  private inferExprType(expr: AST.Expr): C.CanonicalType | undefined {
    switch (expr.kind) {
      case "Call": {
        const call = expr as AST.Call;
        if (call.callee.kind === "Identifier") {
          const binding = this.typeChecker.getBinding(call.callee.name);
          if (binding && binding.type.kind === "func") {
            const funcType = binding.type as C.FuncType;
            // Generic instantiation: substitute typeArgs into return type
            if (call.typeArgs && call.typeArgs.length > 0) {
              return this.instantiateReturn(funcType, call.typeArgs, binding.runtime);
            }
            return funcType.returns;
          }
        }
        // Method call: x.method() — infer from x's type if known
        if (expr.callee.kind === "Member" && (expr.callee as AST.Member).object.kind === "Identifier") {
          const member = expr.callee as AST.Member;
          const objBinding = this.typeChecker.getBinding((member.object as AST.Identifier).name);
          if (objBinding && objBinding.type.kind === "struct") {
            const field = (objBinding.type as C.StructType).fields.find(
              f => f.name === member.property.name
            );
            if (field && field.type.kind === "func") {
              return (field.type as C.FuncType).returns;
            }
          }
        }
        return undefined;
      }
      case "NewExpr":
        return undefined;
      case "Member": {
        const member = expr as AST.Member;
        if (member.object.kind === "Identifier") {
          const binding = this.typeChecker.getBinding((member.object as AST.Identifier).name);
          if (binding && binding.type.kind === "struct") {
            const field = (binding.type as C.StructType).fields.find(f => f.name === member.property.name);
            if (field) return field.type;
          }
          // Map access: infer value type
          if (binding && binding.type.kind === "map") {
            return (binding.type as C.MapType).value;
          }
        }
        return undefined;
      }
      case "Index": {
        const idx = expr as AST.Index;
        if (idx.object.kind === "Identifier") {
          const binding = this.typeChecker.getBinding((idx.object as AST.Identifier).name);
          if (binding) {
            if (binding.type.kind === "array") return (binding.type as C.ArrayType).element;
            if (binding.type.kind === "map") return (binding.type as C.MapType).value;
          }
        }
        return undefined;
      }
      case "Identifier": {
        const binding = this.typeChecker.getBinding((expr as AST.Identifier).name);
        if (binding) return binding.type;
        return undefined;
      }
      case "StringLiteral":
        return C.STRING;
      case "NumericLiteral":
        return C.FLOAT64;
      case "BooleanLiteral":
        return C.BOOL;
      case "ArrayLiteral": {
        const arr = expr as AST.ArrayLiteral;
        if (arr.elements.length > 0) {
          const elemType = this.inferExprType(arr.elements[0]);
          if (elemType) return C.array(elemType);
        }
        return C.array(C.ANY);
      }
      case "ObjectLiteral":
        return { kind: "struct", fields: [], nominal: false };
      default:
        return undefined;
    }
  }


  /**
   * Convert a FuncDecl to a canonical FuncType for the type checker.
   */
  private funcDeclToCanonical(node: AST.FuncDecl): C.FuncType {
    const aff = this.affinityMap.get(node);
    const runtime = (aff?.runtime || this.defaultRuntime) as string as
      "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";

    const params = node.params.map(p => ({
      name: p.name?.kind === "Identifier" ? p.name.name : undefined,
      type: p.type ? lowerType(p.type, runtime) : C.ANY,
      optional: !!p.defaultValue,
    }));
    const returns = node.returnType ? lowerType(node.returnType, runtime) : C.ANY;
    return { kind: "func", params, returns, async: node.async };
  }

  /**
   * Convert a ClassDecl to a canonical StructType with fields.
   */
  private classDeclToCanonical(node: AST.ClassDecl, runtime: string): C.StructType {
    type RT = "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";
    const fields: C.Field[] = [];
    for (const member of node.members) {
      if (!member.name) continue;
      if (member.kind === "Field" || member.kind === "Property") {
        fields.push({
          name: member.name.name,
          type: member.type ? lowerType(member.type, runtime as RT) : C.ANY,
          optional: false,
          mutable: !member.readonly,
          visibility: member.visibility,
        });
      } else if (member.kind === "Method") {
        const params = (member.params || []).map(p => ({
          name: p.name?.kind === "Identifier" ? p.name.name : undefined,
          type: p.type ? lowerType(p.type, runtime as RT) : C.ANY,
          optional: !!p.defaultValue,
        }));
        const returns = member.type ? lowerType(member.type, runtime as RT) : C.ANY;
        fields.push({
          name: member.name.name,
          type: { kind: "func", params, returns },
        });
      }
    }
    return { kind: "struct", name: node.name.name, fields, nominal: true, origin: runtime };
  }

  /**
   * Convert an InterfaceDecl to a canonical StructType (or InterfaceType) with fields.
   */
  private interfaceDeclToCanonical(node: AST.InterfaceDecl, runtime: string): C.StructType {
    type RT = "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";
    const fields: C.Field[] = [];
    for (const member of node.members) {
      if (member.kind === "Method" || member.params) {
        const params = (member.params || []).map(p => ({
          name: p.name?.kind === "Identifier" ? p.name.name : undefined,
          type: p.type ? lowerType(p.type, runtime as RT) : C.ANY,
          optional: !!p.defaultValue,
        }));
        const returns = member.returnType ? lowerType(member.returnType, runtime as RT) : C.ANY;
        fields.push({
          name: member.name.name,
          type: { kind: "func", params, returns },
          optional: member.optional,
        });
      } else {
        fields.push({
          name: member.name.name,
          type: member.type ? lowerType(member.type, runtime as RT) : C.ANY,
          optional: member.optional,
        });
      }
    }
    // Interfaces are structural (duck typing at boundaries)
    return { kind: "struct", name: node.name.name, fields, nominal: false, origin: runtime };
  }

  /**
   * Convert an EnumDecl to a canonical EnumType.
   */
  private enumDeclToCanonical(node: AST.EnumDecl): C.EnumType {
    return {
      kind: "enum",
      name: node.name.name,
      variants: node.members.map(m => ({
        name: m.name.name,
        payload: m.value ? this.inferExprType(m.value) : undefined,
      })),
    };
  }

  /**
   * If a type is an opaque named struct with no fields, check the type registry
   * for a full definition with field info.
   */
  private resolveFromRegistry(type: C.CanonicalType): C.CanonicalType {
    if (type.kind === "struct" && type.name && type.fields.length === 0) {
      const registered = this.typeRegistry.get(type.name);
      if (registered && registered.kind === "struct" && (registered as C.StructType).fields.length > 0) {
        return registered;
      }
    }
    return type;
  }

  /**
   * Instantiate a generic function's return type with concrete type arguments.
   * Maps typevar positions to concrete types from the call-site type args.
   */
  private instantiateReturn(
    funcType: C.FuncType,
    typeArgs: AST.TypeNode[],
    runtime: string,
  ): C.CanonicalType {
    type RT = "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";
    const concreteArgs = typeArgs.map(t => lowerType(t, runtime as RT));
    return this.substituteTypevars(funcType.returns, concreteArgs);
  }

  /**
   * Substitute typevars in a type with concrete types.
   * Uses positional mapping: first typevar → first concrete arg, etc.
   */
  private substituteTypevars(type: C.CanonicalType, args: C.CanonicalType[], depth = 0): C.CanonicalType {
    if (depth > 10) return type; // guard against infinite recursion
    switch (type.kind) {
      case "typevar":
        // Simple positional: first typevar gets first arg
        return args[0] || type;
      case "generic": {
        const gen = type as C.GenericType;
        return {
          ...gen,
          base: this.substituteTypevars(gen.base, args, depth + 1),
          args: gen.args.map(a => this.substituteTypevars(a, args, depth + 1)),
        };
      }
      case "async":
        return { ...type, inner: this.substituteTypevars((type as C.AsyncType).inner, args, depth + 1) };
      case "option":
        return { ...type, inner: this.substituteTypevars((type as C.OptionType).inner, args, depth + 1) };
      case "result": {
        const r = type as C.ResultType;
        return { ...r, ok: this.substituteTypevars(r.ok, args, depth + 1), err: this.substituteTypevars(r.err, args, depth + 1) };
      }
      case "array":
        return { ...type, element: this.substituteTypevars((type as C.ArrayType).element, args, depth + 1) };
      default:
        return type;
    }
  }

  /**
   * Convert a BridgeOp from the type checker into a ManifestBridgeOp.
   */
  private toBridgeManifestOp(b: { binding: string; op: { op: string; [k: string]: unknown } }): ManifestBridgeOp {
    const crossing = this.typeChecker.getCrossings().find(c => c.binding === b.binding && c.result.bridgeOp);
    const result: ManifestBridgeOp = {
      binding: b.binding,
      op: b.op.op,
    };
    if (crossing) {
      result.from = crossing.from.runtime;
      result.to = crossing.to.runtime;
    }
    // Extract any extra metadata from the bridge op
    const { op: _, ...meta } = b.op;
    if (Object.keys(meta).length > 0) {
      result.meta = meta as Record<string, unknown>;
    }
    return result;
  }

  /**
   * Check a cross-runtime reference and register the crossing with the type checker.
   * Called when a capture crosses a runtime boundary.
   */
  checkTypeCrossing(name: string, targetRuntime: OmniRuntime, expectedType?: C.CanonicalType): void {
    this.typeChecker.checkCrossing(
      name,
      targetRuntime as string as "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash",
      expectedType,
    );
  }

  // ─── Parallel Pattern Detection ─────────────────────────────────

  /**
   * Detect if an expression is a parallelizable pattern:
   * - Promise.all([expr1, expr2, ...])
   * - asyncio.gather(expr1, expr2, ...)
   * - CompletableFuture.allOf(expr1, expr2, ...)
   * Optionally wrapped in Unary("await", ...).
   * Returns the list of branch expressions, or null.
   */
  private isParallelPattern(expr: AST.Expr): { exprs: AST.Expr[] } | null {
    let inner = expr;
    // Unwrap await
    if (inner.kind === "Unary" && inner.op === "await" && inner.prefix) {
      inner = inner.argument;
    }
    if (inner.kind !== "Call") return null;
    const callee = inner.callee;
    if (callee.kind !== "Member") return null;
    const obj = callee.object;
    const prop = callee.property;
    if (obj.kind !== "Identifier") return null;

    // Promise.all([expr1, expr2, ...]) — single ArrayLiteral arg
    if (obj.name === "Promise" && prop.name === "all") {
      if (inner.args.length === 1 && inner.args[0].kind === "ArrayLiteral") {
        return { exprs: (inner.args[0] as AST.ArrayLiteral).elements };
      }
      return null;
    }

    // asyncio.gather(expr1, expr2, ...)
    if (obj.name === "asyncio" && prop.name === "gather") {
      if (inner.args.length > 0) {
        return { exprs: inner.args };
      }
      return null;
    }

    // CompletableFuture.allOf(expr1, expr2, ...)
    if (obj.name === "CompletableFuture" && prop.name === "allOf") {
      if (inner.args.length > 0) {
        return { exprs: inner.args };
      }
      return null;
    }

    return null;
  }

  /**
   * Emit a ParallelOp from a list of branch expressions.
   * Each branch gets its own runtime affinity from the resolver.
   */
  private emitParallel(exprs: AST.Expr[], bindPrefix: string): ParallelOp {
    const branches = exprs.map((expr, i) => {
      const aff = this.affinityMap.get(expr);
      const runtime = aff?.runtime || this.defaultRuntime;
      const captures = this.computeCaptures(expr, runtime);
      return {
        runtime,
        code: this.exprCode(expr, runtime),
        bind: `${bindPrefix}_${i}`,
        ...(captures ? { captures } : {}),
      };
    });
    return { op: "parallel" as const, branches };
  }

  // ─── Await Detection ────────────────────────────────────────────

  /**
   * Detect if an expression is `await expr` (not a parallel pattern).
   * Returns the inner expression, or null.
   */
  private isAwaitExpr(expr: AST.Expr): { inner: AST.Expr } | null {
    if (expr.kind === "Unary" && expr.op === "await" && expr.prefix) {
      return { inner: expr.argument };
    }
    return null;
  }

  /**
   * Emit an AwaitOp for a non-parallel await expression.
   */
  private emitAwait(inner: AST.Expr, runtime: OmniRuntime, bind?: string): AwaitOp {
    const captures = this.computeCaptures(inner, runtime);
    return {
      op: "await",
      runtime,
      from: {
        op: "eval",
        runtime,
        code: this.exprCode(inner, runtime),
        bind: bind || "__awaited",
        ...(captures ? { captures } : {}),
      },
      ...(bind ? { bind } : {}),
    };
  }

  // ─── make() Detection ─────────────────────────────────────────

  /**
   * Detect if an expression is a `make(N)` call (channel creation).
   * Returns the buffer size, or null.
   */
  private isMakeCall(expr: AST.Expr): { size?: number } | null {
    if (expr.kind === "Call" && expr.callee.kind === "Identifier" && expr.callee.name === "make") {
      const size = expr.args.length > 0 && expr.args[0].kind === "NumericLiteral"
        ? Number((expr.args[0] as AST.NumericLiteral).raw)
        : undefined;
      return { size };
    }
    return null;
  }

  private isManifestHelperCall(expr: AST.Expr, objectName: "resource" | "table" | "job"): AST.Call | undefined {
    if (expr.kind !== "Call" || expr.callee.kind !== "Member") return undefined;
    const member = expr.callee;
    if (member.object.kind !== "Identifier" || member.object.name !== objectName) return undefined;
    return expr;
  }

  private resourceOpFromCall(expr: AST.Expr, bind?: string, fallbackRuntime?: OmniRuntime): ResourceOp | undefined {
    const call = this.isManifestHelperCall(expr, "resource");
    if (!call || call.callee.kind !== "Member") return undefined;
    const action = call.callee.property.name;
    const runtimeFallback = fallbackRuntime || this.defaultRuntime;

    if (action === "open") {
      let argOffset = 0;
      let runtime = runtimeFallback;
      if (this.isRuntimeStringValue(call.args[0]) && call.args.length > 1) {
        runtime = this.manifestRuntimeFromValue(call.args[0], runtimeFallback);
        argOffset = 1;
      }
      const kind = this.manifestStringFromValue(call.args[argOffset]);
      const disposer = this.manifestStringFromValue(call.args[argOffset + 1]);
      const valueArg = call.args[argOffset + 2];
      return {
        op: "resource",
        action: "open",
        runtime,
        ...(bind ? { bind } : {}),
        ...(kind ? { kind } : {}),
        ...(disposer ? { disposer } : {}),
        ...(valueArg ? { value: this.manifestValue(valueArg) } : {}),
      };
    }

    if (action === "close") {
      const targetArg = call.args[0];
      const target = targetArg?.kind === "Identifier"
        ? targetArg.name
        : this.manifestStringFromValue(targetArg);
      if (!target) return undefined;
      const runtimeArg = call.args.find(arg => this.isRuntimeStringValue(arg));
      const runtime = runtimeArg ? this.manifestRuntimeFromValue(runtimeArg, runtimeFallback) : undefined;
      const op: ResourceOp = {
        op: "resource",
        action: "close",
        target,
        ...(runtime ? { runtime } : {}),
      };
      const code = call.args
        .slice(1)
        .map(arg => this.manifestStringFromValue(arg))
        .find(value => value && !this.isRuntimeString(value));
      if (code) (op as ResourceOp & { code: string }).code = code;
      return op;
    }

    return undefined;
  }

  private tableExportOpFromDeclaration(name: string, value: AST.Expr, fallbackRuntime: OmniRuntime): TableOp | undefined {
    if (this.declaredBindingKind(name) !== "table") return undefined;
    return {
      op: "table",
      action: "export",
      runtime: this.typedBindingRuntimeHints.get(name) || fallbackRuntime,
      bind: name,
      format: "arrow_c_data",
      ownership: "borrowed",
      release: "producer",
      value: this.manifestValue(value),
    };
  }

  private jobOpFromCall(expr: AST.Expr, bind?: string, fallbackRuntime?: OmniRuntime): JobOp | undefined {
    const call = this.isManifestHelperCall(expr, "job");
    if (!call || call.callee.kind !== "Member") return undefined;
    const action = call.callee.property.name;
    const runtimeFallback = fallbackRuntime || this.defaultRuntime;

    if (action === "enqueue") {
      let argOffset = 0;
      let runtime = runtimeFallback;
      if (this.isRuntimeStringValue(call.args[0]) && call.args.length > 1) {
        runtime = this.manifestRuntimeFromValue(call.args[0], runtimeFallback);
        argOffset = 1;
      }
      const kind = this.manifestStringFromValue(call.args[argOffset]);
      const payload = call.args[argOffset + 1];
      return {
        op: "job",
        action: "enqueue",
        runtime,
        ...(bind ? { bind } : {}),
        ...(kind ? { kind } : {}),
        ...(payload ? { payload: this.manifestValue(payload) } : {}),
      };
    }

    if (action === "complete") {
      const target = this.manifestTargetName(call.args[0]);
      if (!target) return undefined;
      return {
        op: "job",
        action: "complete",
        target,
        ...(call.args[1] ? { value: this.manifestValue(call.args[1]) } : {}),
      };
    }

    if (action === "wait") {
      const target = this.manifestTargetName(call.args[0]);
      if (!target) return undefined;
      return {
        op: "job",
        action: "wait",
        target,
        ...(bind ? { bind } : {}),
      };
    }

    if (action === "cancel") {
      const target = this.manifestTargetName(call.args[0]);
      if (!target) return undefined;
      const tailArgs = call.args.slice(2);
      const runtimeArg = tailArgs.find(arg => this.isRuntimeStringValue(arg));
      const runtime = runtimeArg ? this.manifestRuntimeFromValue(runtimeArg, runtimeFallback) : undefined;
      const reason = call.args[1];
      const code = tailArgs
        .map(arg => this.manifestStringFromValue(arg))
        .find(value => value && !this.isRuntimeString(value));
      return {
        op: "job",
        action: "cancel",
        target,
        ...(runtime ? { runtime } : {}),
        ...(reason ? { value: this.manifestValue(reason) } : {}),
        ...(code ? { code } : {}),
      };
    }

    return undefined;
  }

  private manifestTargetName(expr: AST.Expr | undefined): string | undefined {
    if (!expr) return undefined;
    if (expr.kind === "Identifier") return expr.name;
    return this.manifestStringFromValue(expr);
  }

  private isRuntimeStringValue(expr: AST.Expr | undefined): boolean {
    const value = this.manifestStringFromValue(expr);
    return !!value && this.isRuntimeString(value);
  }

  private isRuntimeString(value: string): boolean {
    return ["py", "python", "js", "javascript", "typescript", "go", "rb", "ruby", "java", "rust", "c", "cpp", "c++"].includes(value.toLowerCase());
  }

  // ─── Literal Detection ──────────────────────────────────────────

  /**
   * Check if an expression is a simple literal that can be represented
   * as a manifest-scope value (no runtime eval needed).
   */
  private isSimpleLiteral(expr: AST.Expr): boolean {
    switch (expr.kind) {
      case "NumericLiteral":
      case "BooleanLiteral":
      case "NullLiteral":
        return true;
      case "StringLiteral":
        // Only plain strings (no interpolation)
        return expr.parts.length === 1 &&
          expr.parts[0].kind === "Text" &&
          this.isSourceStringLiteral(expr);
      default:
        return false;
    }
  }

  private isSourceStringLiteral(expr: AST.StringLiteral): boolean {
    if (!this.source || !expr.span || expr.span.end <= expr.span.start) return true;
    const raw = this.source.slice(expr.span.start, expr.span.end).trim();
    return /^(?:[rubfcRUBFC]+)?["'`]/.test(raw);
  }

  /**
   * Extract the runtime value from a simple literal expression.
   */
  private literalValue(expr: AST.Expr): unknown {
    switch (expr.kind) {
      case "NumericLiteral":
        return Number(expr.raw);
      case "BooleanLiteral":
        return expr.value;
      case "NullLiteral":
        return null;
      case "StringLiteral":
        return expr.parts[0].value as string;
      default:
        return null;
    }
  }

  private manifestValue(expr: AST.Expr): ManifestValue {
    const literal = this.tryLiteralValue(expr);
    if (literal.ok) {
      return { kind: "literal", value: literal.value };
    }
    if (expr.kind === "Identifier") {
      return { kind: "ref", name: expr.name };
    }
    return { kind: "literal", value: exprToCode(expr, this.source) };
  }

  private tryLiteralValue(expr: AST.Expr): { ok: true; value: unknown } | { ok: false } {
    if (this.isSimpleLiteral(expr)) return { ok: true, value: this.literalValue(expr) };
    if (expr.kind === "ArrayLiteral") {
      const values: unknown[] = [];
      for (const element of expr.elements) {
        const value = this.tryLiteralValue(element);
        if (!value.ok) return { ok: false };
        values.push(value.value);
      }
      return { ok: true, value: values };
    }
    if (expr.kind === "ObjectLiteral") {
      const object: Record<string, unknown> = {};
      for (const prop of expr.properties) {
        if (prop.computed) return { ok: false };
        const key = this.literalObjectKey(prop.key);
        if (key === undefined) return { ok: false };
        const value = this.tryLiteralValue(prop.value);
        if (!value.ok) return { ok: false };
        object[key] = value.value;
      }
      return { ok: true, value: object };
    }
    return { ok: false };
  }

  private literalObjectKey(key: AST.ObjectProperty["key"]): string | undefined {
    switch (key.kind) {
      case "Identifier":
        return key.name;
      case "NumericLiteral":
        return String(Number(key.raw));
      case "StringLiteral":
        return key.parts.length === 1 && key.parts[0].kind === "Text"
          ? String(key.parts[0].value)
          : undefined;
      default:
        return undefined;
    }
  }

  private declaredBindingKind(name: string): BindingKind {
    const hinted = this.typedBindingKinds.get(name);
    if (hinted) return hinted;
    const typed = this.typeChecker.getBinding(name);
    if (!typed) return "value";
    switch (typed.type.kind) {
      case "stream":
        return "stream";
      case "disposable":
        return "resource";
      default:
        return "value";
    }
  }

  private recordTypedBindingHint(name: string, type?: AST.TypeNode): void {
    if (!type) return;
    if (this.isTableType(type)) {
      this.typedBindingKinds.set(name, "table");
      const runtimeHint = this.tableRuntimeHint(type);
      if (runtimeHint) {
        this.typedBindingRuntimeHints.set(name, runtimeHint);
      }
    } else if (this.isStreamLikeType(type)) {
      this.typedBindingKinds.set(name, "stream");
      const runtimeHint = this.streamRuntimeHint(type);
      if (runtimeHint) {
        this.typedBindingRuntimeHints.set(name, runtimeHint);
      }
    } else if (this.isResourceLikeType(type)) {
      this.typedBindingKinds.set(name, "resource");
      const runtimeHint = this.resourceRuntimeHint(type);
      if (runtimeHint) {
        this.typedBindingRuntimeHints.set(name, runtimeHint);
      }
      const disposer = this.resourceDisposerHint(type);
      if (disposer) {
        this.resourceDisposers.set(name, disposer);
      }
    }
  }

  private tableRuntimeHint(type: AST.TypeNode): OmniRuntime | undefined {
    const name = this.typeName(type).toLowerCase();
    if (/(bytebuffer|directbytebuffer|intbuffer|floatbuffer|doublebuffer|longbuffer|shortbuffer|charbuffer)/.test(name)) return OmniRuntime.Java;
    if (/(arraybuffer|dataview|typedarray|uint8array|uint8clampedarray|uint16array|uint32array|int8array|int16array|int32array|float32array|float64array|bigint64array|biguint64array)/.test(name)) return OmniRuntime.JavaScript;
    return undefined;
  }

  private isTableType(type: AST.TypeNode): boolean {
    const name = this.typeName(type).toLowerCase();
    return /(dataframe|arrowtable|datatable|recordbatch|ndarray|tensor|dlpack|bytebuffer|directbytebuffer|intbuffer|floatbuffer|doublebuffer|longbuffer|shortbuffer|charbuffer|arraybuffer|dataview|typedarray|uint8array|uint8clampedarray|uint16array|uint32array|int8array|int16array|int32array|float32array|float64array|bigint64array|biguint64array)/.test(name)
      || this.matchesTypeName(name, ["table"]);
  }

  private streamRuntimeHint(type: AST.TypeNode): OmniRuntime | undefined {
    const name = this.typeName(type).toLowerCase();
    return this.streamRuntimeForTypeName(name);
  }

  private isStreamLikeType(type: AST.TypeNode): boolean {
    const name = this.typeName(type).toLowerCase();
    return this.streamRuntimeForTypeName(name) !== undefined;
  }

  private streamRuntimeForTypeName(name: string): OmniRuntime | undefined {
    if (this.matchesStreamType(name, [
      "publisher", "basestream",
      "inputstream", "java.io.inputstream", "java.io.reader",
      "readablebytechannel", "java.nio.channels.readablebytechannel",
      "resultset", "java.sql.resultset",
    ])) return OmniRuntime.Java;
    if (this.matchesStreamType(name, [
      "readablestream", "nodereadable", "readable", "webstream",
      "stream.readable", "node.stream.readable",
    ])) return OmniRuntime.JavaScript;
    return undefined;
  }

  private resourceRuntimeHint(type: AST.TypeNode): OmniRuntime | undefined {
    const name = this.typeName(type).toLowerCase();
    return this.resourceRuntimeForTypeName(name);
  }

  private isResourceLikeType(type: AST.TypeNode): boolean {
    const name = this.typeName(type).toLowerCase();
    return this.resourceRuntimeForTypeName(name) !== undefined;
  }

  private resourceRuntimeForTypeName(name: string): OmniRuntime | undefined {
    if (this.matchesTypeName(name, [
      "completablefuture", "java.util.concurrent.completablefuture",
      "futuretask", "java.util.concurrent.futuretask",
      "scheduledfuture", "java.util.concurrent.scheduledfuture",
      "future", "java.util.concurrent.future",
      "executorservice", "java.util.concurrent.executorservice",
      "autocloseable", "java.lang.autocloseable",
      "closeable", "java.io.closeable",
    ])) return OmniRuntime.Java;
    return undefined;
  }

  private resourceDisposerHint(type: AST.TypeNode): string | undefined {
    const name = this.typeName(type).toLowerCase();
    if (this.matchesTypeName(name, [
      "executorservice", "java.util.concurrent.executorservice",
    ])) return "shutdown";
    if (this.matchesTypeName(name, [
      "autocloseable", "java.lang.autocloseable",
      "closeable", "java.io.closeable",
    ])) return "close";
    if (this.matchesTypeName(name, [
      "completablefuture", "java.util.concurrent.completablefuture",
      "futuretask", "java.util.concurrent.futuretask",
      "scheduledfuture", "java.util.concurrent.scheduledfuture",
      "future", "java.util.concurrent.future",
    ])) return "cancel";
    return undefined;
  }

  private matchesStreamType(name: string, candidates: string[]): boolean {
    return this.matchesTypeName(name, candidates);
  }

  private matchesTypeName(name: string, candidates: string[]): boolean {
    const normalized = name.replace(/\s+/g, "");
    return candidates.some(candidate => {
      const suffix = candidate.replace(/\s+/g, "").toLowerCase();
      return normalized === suffix || normalized.endsWith(`.${suffix}`);
    });
  }

  private typedRuntimeHint(name: string, fallback: OmniRuntime): OmniRuntime {
    return this.typedBindingRuntimeHints.get(name) || fallback;
  }

  private typeName(type: AST.TypeNode): string {
    switch (type.kind) {
      case "SimpleType":
        return type.id.name;
      case "GenericType":
        return type.base.name;
      default:
        return "";
    }
  }

  private manifestRuntimeFromValue(expr: AST.Expr | undefined, fallback: OmniRuntime): OmniRuntime {
    if (!expr) return fallback;
    if (expr.kind !== "StringLiteral" || expr.parts.length !== 1 || expr.parts[0].kind !== "Text") return fallback;
    const value = String(expr.parts[0].value).toLowerCase();
    switch (value) {
      case "python": return OmniRuntime.Python;
      case "javascript":
      case "typescript": return OmniRuntime.JavaScript;
      case "go": return OmniRuntime.Go;
      case "ruby": return OmniRuntime.Ruby;
      case "java": return OmniRuntime.Java;
      case "rust": return OmniRuntime.Rust;
      case "c":
      case "cpp":
      case "c++": return OmniRuntime.C;
    }
    const runtime = tagToRuntime(value);
    return runtime || fallback;
  }

  private manifestStringFromValue(expr: AST.Expr | undefined): string | undefined {
    if (!expr || expr.kind !== "StringLiteral" || expr.parts.length !== 1 || expr.parts[0].kind !== "Text") {
      return undefined;
    }
    return String(expr.parts[0].value);
  }

  // ─── Block Emission ───────────────────────────────────────────

  private emitBlock(block: RuntimeBlock): ManifestOp[] {
    if (isCompiledRuntime(block.runtime)) {
      return this.emitCompiledBlock(block);
    }

    // Always process node-by-node for explicit bind/captures on every op.
    const ops: ManifestOp[] = [];
    for (const node of block.nodes) {
      ops.push(...this.emitNode(node, block.runtime));
    }
    return ops;
  }

  private appendGoMainEntrypoint(nodes: Array<AST.Decl | AST.Stmt>, ops: ManifestOp[]): void {
    const hasMainPackage = nodes.some(node =>
      node.kind === "PackageDecl" && node.name.name === "main"
    );
    if (!hasMainPackage) return;

    const hasGoMain = ops.some(op =>
      op.op === "func_def" &&
      (op as FuncDefOp).name === "main" &&
      (op as FuncDefOp).bodyRuntime === OmniRuntime.Go
    );
    if (!hasGoMain) return;

    const alreadyCallsMain = ops.some(op =>
      op.op === "eval" &&
      (op as EvalOp).runtime === OmniRuntime.Go &&
      (op as EvalOp).func === "main"
    );
    if (alreadyCallsMain) return;

    ops.push({
      op: "eval",
      runtime: OmniRuntime.Go,
      func: "main",
      args: [],
      bind: "",
    } as EvalOp);
  }

  private indexGoFuncDecls(nodes: Array<AST.Decl | AST.Stmt>): Map<string, AST.FuncDecl> {
    const funcs = new Map<string, AST.FuncDecl>();
    for (const node of nodes) {
      if (node.kind !== "FuncDecl") continue;
      const aff = this.affinityMap.get(node);
      const runtime = aff?.runtime || this.defaultRuntime;
      if (runtime === OmniRuntime.Go || node.declKeyword === "func") {
        funcs.set(node.name.name, node);
      }
    }
    return funcs;
  }

  private emitConsolidatedBlock(block: RuntimeBlock): ExecOp {
    const codeSegments: string[] = [];
    const allCaptures: CaptureMap = {};
    for (const node of block.nodes) {
      codeSegments.push(nodeToSourceCode(node, this.source));
      const caps = this.computeCaptures(node, block.runtime);
      if (caps) Object.assign(allCaptures, caps);
    }
    return {
      op: "exec",
      runtime: block.runtime,
      code: codeSegments.join("\n"),
      ...(Object.keys(allCaptures).length > 0 ? { captures: allCaptures } : {}),
    };
  }

  private emitCompiledBlock(block: RuntimeBlock): ManifestOp[] {
    if (block.runtime === OmniRuntime.Rust) {
      const ops: ManifestOp[] = [];
      for (const node of block.nodes) {
        // Module-level Rust items (use/struct/enum/impl/static) live in the
        // shared unit source, not in exec ops.
        if (this.rustUnit?.absorbed.has(node as AST.Decl | AST.Stmt)) {
          continue;
        }
        if (node.kind === "FuncDecl") {
          ops.push(...this.emitRustFuncDef(node));
          continue;
        }
        if (node.kind === "RustItem") {
          // fn items become func_def ops; any other unabsorbed item is
          // unit-only and emits nothing.
          if (node.fns.length > 0) ops.push(...this.emitRustItemFuncDefs(node));
          continue;
        }
        // Orchestration-level Rust statements (e.g. calls into exported Rust
        // fns with cross-runtime arguments) go through the generic emitters.
        ops.push(...this.emitNode(node as AST.Decl | AST.Stmt, block.runtime));
      }
      return ops;
    }

    return block.nodes.map(node => ({
      op: "exec_compiled" as const,
      lang: "c",
      code: nodeToSourceCode(node, this.source),
    }));
  }

  // ─── Node Emission ────────────────────────────────────────────

  private emitNode(node: AST.Decl | AST.Stmt | AST.Expr, blockRuntime: OmniRuntime): ManifestOp[] {
    switch (node.kind) {
      case "FuncDecl":
        return this.emitFuncDecl(node);

      case "ExprStmt":
        return [this.emitExprStmt(node, blockRuntime)];

      case "VarDecl":
        return this.emitVarDecl(node);

      case "ConstDecl":
        return this.emitConstDecl(node);

      case "Return":
        return [this.emitReturn(node)];

      case "If":
        return [this.emitIf(node)];

      case "Loop":
        return [this.emitLoop(node)];

      case "Try":
        return [this.emitTry(node)];

      case "Throw":
        return [this.emitThrow(node)];

      case "ExportDecl":
        if (node.declaration) return this.emitNode(node.declaration, blockRuntime);
        return [];

      case "PackageDecl":
      case "TypeDecl":
      case "InterfaceDecl":
        return [];

      case "Import":
      case "ImportDecl":
        return [this.emitImport(node)];

      case "GroupedImport":
        return node.imports.map(imported => this.emitImport(imported));

      case "ShortDecl":
        return this.emitShortDecl(node);

      case "Go":
        return [this.emitSpawn(node)];

      case "Echo":
        return [this.emitEcho(node, blockRuntime)];

      case "Defer": {
        const deferAff = this.affinityMap.get(node);
        const deferRuntime = deferAff?.runtime || OmniRuntime.Go;
        // Reconstruct from source span or expression
        let deferCode: string;
        if (this.source && node.span) {
          deferCode = this.source.slice(node.span.start, node.span.end);
        } else if (node.body.kind !== "Block") {
          deferCode = `defer ${exprToCode(node.body as AST.Expr, this.source)}`;
        } else {
          deferCode = "defer { ... }";
        }
        return [{ op: "exec", runtime: deferRuntime, code: deferCode }];
      }

      case "Using":
        return [this.emitUsing(node, blockRuntime)];

      case "Select":
        return [this.emitSelect(node)];

      case "Yield":
        return [this.emitYield(node)];

      default: {
        const aff = this.affinityMap.get(node);
        const runtime = aff?.runtime || this.defaultRuntime;
        if (isExprKind(node.kind)) {
          return [this.emitExprAsOp(node as AST.Expr, runtime)];
        }
        return [{
          op: "native",
          runtime,
          code: nodeToSourceCode(node, this.source),
        }];
      }
    }
  }

  // ─── Expression Emission ──────────────────────────────────────

  private emitExprStmt(node: AST.ExprStmt, blockRuntime: OmniRuntime): ManifestOp {
    const aff = this.affinityMap.get(node.expr);
    const runtime = aff?.runtime || blockRuntime;
    const resourceOp = this.resourceOpFromCall(node.expr, undefined, runtime);
    if (resourceOp) return resourceOp;
    const jobOp = this.jobOpFromCall(node.expr, undefined, runtime);
    if (jobOp) return jobOp;

    const lowered = this.loweredNodesFor(node)[0];
    const loweredOp = lowered ? this.emitLoweredNode(lowered, false) : undefined;
    if (loweredOp) return loweredOp;
    return this.emitExprAsOp(node.expr, blockRuntime);
  }

  private emitUsing(node: AST.Using, blockRuntime: OmniRuntime): TryOp {
    const resourceOps: ManifestOp[] = [];
    let resourceName: string | undefined;
    let cleanupRuntime: OmniRuntime | undefined;
    let cleanupCode: string | undefined;

    if (node.resource.kind === "VarDecl" || node.resource.kind === "ConstDecl") {
      const names = node.resource.names;
      resourceName = names[0]?.name;
      const value = node.resource.values?.[0];
      const valueAff = value ? this.affinityMap.get(value) : undefined;
      const declAff = this.affinityMap.get(node.resource);
      const runtime = valueAff?.runtime || declAff?.runtime || blockRuntime;
      if (resourceName && value && runtime === OmniRuntime.Python) {
        const contextName = `__using_context_${++this.usingCounter}`;
        const asyncContext = node.async === true;
        const captures = this.computeCaptures(value, runtime);
        resourceOps.push({
          op: "eval",
          runtime,
          bind: contextName,
          code: this.exprCode(value, runtime),
          ...(captures ? { captures } : {}),
        });
        resourceOps.push({
          op: "eval",
          runtime,
          bind: resourceName,
          code: asyncContext ? `await ${contextName}.__aenter__()` : `${contextName}.__enter__()`,
          ...(asyncContext ? { async: true } : {}),
        });
        this.recordBinding(contextName, runtime, "value", value);
        this.recordBinding(resourceName, runtime, "value", node.resource);
        cleanupRuntime = runtime;
        cleanupCode = asyncContext
          ? `await ${contextName}.__aexit__(None, None, None)`
          : `${contextName}.__exit__(None, None, None)`;
        resourceName = contextName;
      } else {
        resourceOps.push(...(node.resource.kind === "VarDecl"
          ? this.emitVarDecl(node.resource)
          : this.emitConstDecl(node.resource)));
      }
    } else {
      resourceName = `__using_resource_${++this.usingCounter}`;
      const resourceExpr = node.resource as AST.Expr;
      const aff = this.affinityMap.get(resourceExpr);
      const runtime = aff?.runtime || blockRuntime;
      const open = this.resourceOpFromCall(resourceExpr, resourceName, runtime);
      if (open) {
        resourceOps.push(open);
        this.recordResourceBinding(resourceName, (open.runtime as OmniRuntime) || runtime, resourceExpr, open);
      } else {
        const captures = this.computeCaptures(resourceExpr, runtime);
        resourceOps.push({
          op: "eval",
          runtime,
          bind: resourceName,
          code: this.exprCode(resourceExpr, runtime),
          ...(captures ? { captures } : {}),
        });
        this.recordBinding(resourceName, runtime, "resource", resourceExpr);
      }
    }

    const bodyOps: ManifestOp[] = [];
    for (const stmt of node.body.statements) {
      const aff = this.affinityMap.get(stmt);
      bodyOps.push(...this.emitNode(stmt, aff?.runtime || blockRuntime));
    }

    const closeOp: ResourceOp = resourceName
      ? {
          op: "resource",
          action: "close",
          target: resourceName,
          ...(cleanupRuntime ? { runtime: cleanupRuntime } : {}),
          ...(cleanupCode ? { code: cleanupCode } : {}),
          ...(node.async === true ? { async: true } : {}),
        }
      : { op: "resource", action: "close", target: "__unknown_resource" };

    return {
      op: "try",
      body: [...resourceOps, ...bodyOps],
      catches: [],
      finallyBody: [closeOp],
    };
  }

  private exprCode(expr: AST.Expr, runtime: OmniRuntime): string {
    return exprToCodeForRuntime(expr, runtime, this.source);
  }

  private loweredExprCode(expr: AST.Expr, runtime: OmniRuntime, nativeSource?: string): string {
    if (runtime === OmniRuntime.JavaScript) {
      return this.exprCode(expr, runtime);
    }
    return nativeSource || this.exprCode(expr, runtime);
  }

  private statementCode(expr: AST.Expr, runtime: OmniRuntime): string {
    const code = this.exprCode(expr, runtime);
    if (runtime !== OmniRuntime.Java) {
      return code;
    }
    const trimmed = code.trim();
    if (trimmed === "" || trimmed.endsWith(";") || trimmed.endsWith("}")) {
      return code;
    }
    return `${code};`;
  }

  private emitEcho(node: AST.Echo, blockRuntime: OmniRuntime): NativeOp {
    const aff = this.affinityMap.get(node);
    const runtime = aff?.runtime || blockRuntime;
    const args = node.values.map(v => exprToCode(v, this.source)).join(", ");

    let code: string;
    switch (runtime) {
      case OmniRuntime.Python:
        if (this.source && node.span) {
          const original = this.source.slice(node.span.start, node.span.end);
          code = original.trim() || `print(${args})`;
        } else {
          code = `print(${args})`;
        }
        break;
      case OmniRuntime.Ruby:
        code = `puts ${args}`;
        break;
      default:
        code = `console.log(${args})`;
        break;
    }

    return { op: "native", runtime, code };
  }

  private emitExprAsOp(expr: AST.Expr, contextRuntime: OmniRuntime): ManifestOp {
    const aff = this.affinityMap.get(expr);
    const runtime = aff?.runtime || contextRuntime;

    // Parallel pattern check (standalone expression)
    const parallel = this.isParallelPattern(expr);
    if (parallel) {
      return this.emitParallel(parallel.exprs, "__parallel");
    }

    // Await (non-parallel) → AwaitOp
    const awaitExpr = this.isAwaitExpr(expr);
    if (awaitExpr) {
      return this.emitAwait(awaitExpr.inner, runtime);
    }

    // make() → ChanOp make
    const makeCall = this.isMakeCall(expr);
    if (makeCall) {
      return {
        op: "chan",
        action: "make",
        runtime,
        size: makeCall.size,
      } as ChanOp;
    }

    const resourceOp = this.resourceOpFromCall(expr, undefined, runtime);
    if (resourceOp) return resourceOp;

    const jobOp = this.jobOpFromCall(expr, undefined, runtime);
    if (jobOp) return jobOp;

    // Channel operations
    const chanOp = this.detectChanOp(expr, runtime);
    if (chanOp) return chanOp;

    // Yield expression → YieldOp
    if (expr.kind === "Yield") {
      return this.emitYield(expr as AST.Yield);
    }

    // Compound assignment (+=, -=, *=, /=) with Identifier LHS → assign op
    if (expr.kind === "Assign" && expr.op !== "=" && expr.left.kind === "Identifier") {
      const target = (expr.left as AST.Identifier).name;
      if (this.isSimpleLiteral(expr.right)) {
        return {
          op: "assign",
          target,
          operator: expr.op,
          value: { kind: "literal", value: this.literalValue(expr.right) },
        };
      }
      const captures = this.computeCaptures(expr.right, runtime);
      return {
        op: "assign",
        target,
        operator: expr.op,
        from: {
          op: "eval",
          runtime,
          code: this.exprCode(expr.right, runtime),
          bind: "__rhs",
          ...(captures ? { captures } : {}),
        },
      };
    }

    // Simple assignment (=) with Identifier LHS → decompose into eval+bind
    // Must come before Go interception so Go assignments are handled properly
    if (expr.kind === "Assign" && expr.op === "=" && expr.left.kind === "Identifier") {
      const bindName = (expr.left as AST.Identifier).name;

      if (this.isSimpleLiteral(expr.right)) {
        // Manifest-scope literal → assign with value
        this.recordBinding(bindName, runtime);
        return {
          op: "assign",
          target: bindName,
          operator: "=",
          value: { kind: "literal", value: this.literalValue(expr.right) },
        };
      }

      // Go + Call RHS → func/args
      if (runtime === OmniRuntime.Go && expr.right.kind === "Call") {
        this.diagnoseExpr(expr.right, runtime);
        const funcName = exprToCode((expr.right as AST.Call).callee, this.source);
        const args = (expr.right as AST.Call).args.map(a => exprToCode(a, this.source));
        this.recordBinding(bindName, runtime);
        return {
          op: "eval",
          runtime: OmniRuntime.Go,
          func: funcName,
          args,
          bind: bindName,
        };
      }

      // Runtime eval → bare eval with bind
      this.diagnoseExpr(expr.right, runtime);
      const captures = this.computeCaptures(expr.right, runtime);
      this.recordBinding(bindName, runtime);
      return {
        op: "eval",
        runtime,
        code: this.exprCode(expr.right, runtime),
        bind: bindName,
        ...(captures ? { captures } : {}),
      };
    }

    // Go runtime — restrict to pre-registered function calls
    if (runtime === OmniRuntime.Go) {
      return this.emitGoOp(expr);
    }

    // RuntimeTag — explicit runtime override
    if (expr.kind === "RuntimeTag") {
      const tagRt = tagToRuntime(expr.runtime);
      if (tagRt === OmniRuntime.Go) {
        return this.emitGoOp(expr.expr);
      }
      const captures = this.computeCaptures(expr.expr, tagRt);
      return {
        op: "exec",
        runtime: tagRt,
        code: this.statementCode(expr.expr, tagRt),
        ...(captures ? { captures } : {}),
      };
    }

    // String literal with interpolation → ConcatOp
    if (expr.kind === "StringLiteral" && expr.parts.some(p => p.kind === "Interpolation")) {
      return this.emitStringInterpolation(expr, runtime);
    }

    const captures = this.computeCaptures(expr, runtime);
    return {
      op: "exec",
      runtime,
      code: this.statementCode(expr, runtime),
      ...(captures ? { captures } : {}),
    };
  }

  // ─── Go Runtime (pre-registered functions only) ──────────────

  private emitGoOp(expr: AST.Expr): ManifestOp {
    if (expr.kind === "Call") {
      const funcName = exprToCode(expr.callee, this.source);
      const args = expr.args.map(a => exprToCode(a, this.source));
      return {
        op: "exec",
        runtime: OmniRuntime.Go,
        func: funcName,
        args,
      };
    }
    // Fallback: emit as exec with reconstructed code
    return {
      op: "exec",
      runtime: OmniRuntime.Go,
      code: exprToCode(expr, this.source),
    };
  }

  private emitLoweredNode(node: LoweredManifestNode, mutable: boolean): ManifestOp | undefined {
    switch (node.kind) {
      case "ChannelMake":
        if (node.bind) this.recordBinding(node.bind, node.runtime, "channel", node.sourceNode);
        return {
          op: "chan",
          action: "make",
          runtime: node.runtime,
          ...(node.bind ? { bind: node.bind } : {}),
          ...(node.size !== undefined ? { size: node.size } : {}),
        } as ChanOp;

      case "ChannelRecv":
        if (node.channel) this.diagnoseChannelRef(node.channel, "recv", node.sourceNode);
        if (node.bind) this.recordBinding(node.bind, node.runtime, "value", node.sourceNode);
        return {
          op: "chan",
          action: "recv",
          runtime: node.runtime,
          ...(node.channel ? { channel: node.channel } : {}),
          ...(node.bind ? { bind: node.bind } : {}),
        } as ChanOp;

      case "ChannelSend": {
        const valueExpr = node.value;
        const value: ManifestValue | undefined = valueExpr
          ? this.isSimpleLiteral(valueExpr)
            ? { kind: "literal", value: this.literalValue(valueExpr) }
            : { kind: "ref", name: exprToCode(valueExpr, this.source) }
          : undefined;
        if (node.channel) this.diagnoseChannelRef(node.channel, "send", node.sourceNode);
        const captures = this.computeCaptures(node.sourceNode as AST.Expr | AST.Stmt | AST.Decl, node.runtime);
        return {
          op: "chan",
          action: "send",
          runtime: node.runtime,
          ...(node.channel ? { channel: node.channel } : {}),
          ...(value ? { value } : {}),
          ...(captures ? { captures } : {}),
        } as ChanOp;
      }

      case "ChannelClose":
        if (node.channel) this.diagnoseChannelRef(node.channel, "close", node.sourceNode);
        return {
          op: "chan",
          action: "close",
          runtime: node.runtime,
          ...(node.channel ? { channel: node.channel } : {}),
        } as ChanOp;

      case "Spawn":
        if (node.bind) this.recordBinding(node.bind, OmniRuntime.Go, "spawn_handle", node.sourceNode);
        return this.emitSpawn(node.expr, node.bind);

      case "EvalExpr": {
        if (!node.bind) return undefined;
        if (this.isParallelPattern(node.expr) || this.isAwaitExpr(node.expr)) return undefined;
        if (this.isSimpleLiteral(node.expr)) {
          this.recordBinding(node.bind, node.runtime, "value", node.sourceNode);
          return {
            op: "declare",
            bind: node.bind,
            mutable,
            value: { kind: "literal", value: this.literalValue(node.expr) },
          };
        }
        this.diagnoseExpr(node.expr, node.runtime);
        const captures = this.computeCaptures(node.expr, node.runtime);
        this.recordBinding(node.bind, node.runtime, "value", node.sourceNode);
        return {
          op: "eval",
          runtime: node.runtime,
          code: this.loweredExprCode(node.expr, node.runtime, node.native?.source),
          bind: node.bind,
          ...(captures ? { captures } : {}),
        };
      }

      case "CallRuntimeFunc": {
        if (!node.bind) return undefined;
        if (node.callee === "wait") return undefined;
        const captures = this.computeCaptures(node.sourceNode as AST.Expr | AST.Stmt | AST.Decl, node.runtime);
        this.recordBinding(node.bind, node.runtime, "value", node.sourceNode);
        if (node.runtime === OmniRuntime.Go) {
          return {
            op: "eval",
            runtime: OmniRuntime.Go,
            func: node.callee,
            args: node.args.map(a => exprToCode(a, this.source)),
            bind: node.bind,
          };
        }
        return {
          op: "eval",
          runtime: node.runtime,
          code: this.loweredExprCode(node.expr, node.runtime, node.native?.source),
          bind: node.bind,
          ...(captures ? { captures } : {}),
        };
      }

      default:
        return undefined;
    }
  }

  // ─── Channel Detection Helpers ──────────────────────────────

  /** Detect channel send: Binary with op "<-" → { channel, value } */
  private isChanSend(expr: AST.Expr): { channel: string; value: AST.Expr } | null {
    if (expr.kind === "Binary" && expr.op === "<-") {
      const channel = exprToCode(expr.left, this.source);
      return { channel, value: expr.right };
    }
    return null;
  }

  /** Detect channel recv: Unary with op "<-", prefix: true → { channel } */
  private isChanRecv(expr: AST.Expr): { channel: string } | null {
    if (expr.kind === "Unary" && expr.op === "<-" && expr.prefix) {
      const channel = exprToCode(expr.argument, this.source);
      return { channel };
    }
    return null;
  }

  /** Detect channel close: Call to close(ch) → { channel } */
  private isChanClose(expr: AST.Expr): { channel: string } | null {
    if (expr.kind === "Call" && expr.callee.kind === "Identifier" && expr.callee.name === "close") {
      if (expr.args.length === 1) {
        const channel = exprToCode(expr.args[0], this.source);
        return { channel };
      }
    }
    return null;
  }

  /** Detect any channel operation and emit a ChanOp, or null. */
  private detectChanOp(expr: AST.Expr, runtime: OmniRuntime): ChanOp | null {
    // Channel send: ch <- value
    const send = this.isChanSend(expr);
    if (send) {
      this.diagnoseChannelRef(send.channel, "send", expr);
      const captures = this.computeCaptures(expr, runtime);
      const value: ManifestValue = this.isSimpleLiteral(send.value)
        ? { kind: "literal", value: this.literalValue(send.value) }
        : { kind: "ref", name: exprToCode(send.value, this.source) };
      return {
        op: "chan",
        action: "send",
        runtime,
        channel: send.channel,
        value,
        ...(captures ? { captures } : {}),
      };
    }

    // Channel recv: <-ch (standalone, no bind)
    const recv = this.isChanRecv(expr);
    if (recv) {
      this.diagnoseChannelRef(recv.channel, "recv", expr);
      const captures = this.computeCaptures(expr, runtime);
      return {
        op: "chan",
        action: "recv",
        runtime,
        channel: recv.channel,
        ...(captures ? { captures } : {}),
      };
    }

    // Channel close: close(ch)
    const close = this.isChanClose(expr);
    if (close) {
      this.diagnoseChannelRef(close.channel, "close", expr);
      return {
        op: "chan",
        action: "close",
        runtime,
        channel: close.channel,
      };
    }

    return null;
  }

  // ─── Spawn (goroutine) ─────────────────────────────────────────

  private emitSpawn(node: AST.Go, bind?: string): SpawnOp {
    const aff = this.affinityMap.get(node);
    const runtime = aff?.runtime || OmniRuntime.Go;
    if (runtime !== OmniRuntime.Go) {
      this.addDiagnostic(
        "warning",
        "spawn-runtime",
        `go spawn expressions should resolve to the Go runtime, but this expression resolved to ${runtime}.`,
        node,
      );
    }
    if (node.expr.kind !== "Call") {
      this.addDiagnostic(
        "warning",
        "unsupported-spawn-expression",
        `OmniVM can only join named worker calls like 'go worker(...)'; got '${exprToCode(node.expr, this.source)}'.`,
        node,
      );
    } else if (node.expr.callee.kind !== "Identifier") {
      this.addDiagnostic(
        "warning",
        "unsupported-spawn-callee",
        `OmniVM spawn handles currently require a named worker function; got '${exprToCode(node.expr.callee, this.source)}'.`,
        node.expr.callee,
      );
    }
    const captures = this.computeCaptures(node.expr, runtime);
    return {
      op: "spawn",
      runtime,
      code: exprToCode(node.expr, this.source),
      ...(bind ? { bind } : {}),
      ...(captures ? { captures } : {}),
    };
  }

  // ─── Select ────────────────────────────────────────────────────

  private emitSelect(node: AST.Select): SelectOp {
    const cases: SelectCase[] = [];
    let defaultBody: ManifestOp[] | undefined;

    for (const c of node.cases) {
      // Each case pattern should be a channel operation expression
      for (const pattern of c.patterns) {
        const bodyBlocks = consolidateBlocks(c.body.statements, this.affinityMap);
        const bodyOps: ManifestOp[] = [];
        for (const block of bodyBlocks) {
          bodyOps.push(...this.emitBlock(block));
        }

        // Detect recv pattern: <-ch or val := <-ch
        const recv = this.isChanRecv(pattern);
        if (recv) {
          cases.push({
            action: "recv",
            channel: recv.channel,
            body: bodyOps,
          });
          continue;
        }

        // Detect send pattern: ch <- value
        const send = this.isChanSend(pattern);
        if (send) {
          const value: ManifestValue = this.isSimpleLiteral(send.value)
            ? { kind: "literal", value: this.literalValue(send.value) }
            : { kind: "ref", name: exprToCode(send.value, this.source) };
          cases.push({
            action: "send",
            channel: send.channel,
            value,
            body: bodyOps,
          });
          continue;
        }

        // Fallback: treat as recv on the expression
        cases.push({
          action: "recv",
          channel: exprToCode(pattern, this.source),
          body: bodyOps,
        });
      }
    }

    // defaultCase is a separate Block on the AST node
    if (node.defaultCase) {
      const defBlocks = consolidateBlocks(node.defaultCase.statements, this.affinityMap);
      const defOps: ManifestOp[] = [];
      for (const block of defBlocks) {
        defOps.push(...this.emitBlock(block));
      }
      defaultBody = defOps;
    }

    return {
      op: "select",
      cases,
      ...(defaultBody ? { defaultBody } : {}),
    };
  }

  // ─── Yield ─────────────────────────────────────────────────────

  private emitYield(node: AST.Yield): YieldOp {
    const yieldOp: YieldOp = { op: "yield" };

    if (node.value) {
      if (this.isSimpleLiteral(node.value)) {
        yieldOp.value = { kind: "literal", value: this.literalValue(node.value) };
      } else if (node.value.kind === "Identifier") {
        yieldOp.value = { kind: "ref", name: node.value.name };
      } else {
        const aff = this.affinityMap.get(node.value);
        const runtime = aff?.runtime || this.defaultRuntime;
        const captures = this.computeCaptures(node.value, runtime);
        yieldOp.from = {
          op: "eval",
          runtime,
          code: exprToCode(node.value, this.source),
          bind: "__yield",
          ...(captures ? { captures } : {}),
        };
      }
    }

    if (node.delegate) {
      yieldOp.delegate = true;
    }

    return yieldOp;
  }

  // ─── Declarations ─────────────────────────────────────────────

  private emitVarDecl(node: AST.VarDecl): ManifestOp[] {
    if (node.destructurePattern && node.values?.[0]) {
      return this.emitDestructuringDecl(node.destructurePattern, node.values[0], true);
    }

    const ops: ManifestOp[] = [];
    for (let i = 0; i < node.names.length; i++) {
      const name = node.names[i].name;

      if (node.values && node.values[i]) {
        const valExpr = node.values[i];
        const aff = this.affinityMap.get(valExpr);
        const runtime = this.typedRuntimeHint(name, aff?.runtime || this.defaultRuntime);
        const resourceOp = this.resourceOpFromCall(valExpr, name, runtime);
        if (resourceOp) {
          ops.push(resourceOp);
          this.recordResourceBinding(name, (resourceOp.runtime as OmniRuntime) || runtime, valExpr, resourceOp);
          continue;
        }

        const tableOp = this.tableExportOpFromDeclaration(name, valExpr, runtime);
        if (tableOp) {
          ops.push(tableOp);
          this.recordBinding(name, (tableOp.runtime as OmniRuntime) || runtime, "table", valExpr);
          continue;
        }

        const jobOp = this.jobOpFromCall(valExpr, name, runtime);
        if (jobOp) {
          ops.push(jobOp);
          this.recordBinding(name, (jobOp.runtime as OmniRuntime) || runtime, jobOp.action === "enqueue" ? "job_handle" : "value", valExpr);
          continue;
        }

        const lowered = this.loweredNodeForBinding(node, name);
        const loweredOp = lowered ? this.emitLoweredNode(lowered, true) : undefined;
        if (loweredOp) {
          if (this.typedBindingRuntimeHints.has(name)) {
            if ("runtime" in loweredOp) {
              (loweredOp as ManifestOp & { runtime?: OmniRuntime }).runtime = runtime;
            }
            this.recordBinding(name, runtime, this.declaredBindingKind(name), valExpr);
          }
          ops.push(loweredOp);
          continue;
        }

        if (valExpr.kind === "Go") {
          ops.push(this.emitSpawn(valExpr, name));
          this.recordBinding(name, OmniRuntime.Go, "spawn_handle", valExpr);
          continue;
        }

        // Parallel pattern check
        const parallel = this.isParallelPattern(valExpr);
        if (parallel) {
          ops.push(this.emitParallel(parallel.exprs, name));
          this.recordBinding(name, runtime, "value", valExpr);
          continue;
        }

        // Await (non-parallel) → AwaitOp with bind
        const awaitExpr = this.isAwaitExpr(valExpr);
        if (awaitExpr && !this.isParallelPattern(awaitExpr.inner)) {
          ops.push(this.emitAwait(awaitExpr.inner, runtime, name));
          this.recordBinding(name, runtime, "value", valExpr);
          continue;
        }

        // make() → ChanOp make with bind
        const makeCall = this.isMakeCall(valExpr);
        if (makeCall) {
          ops.push({
            op: "chan",
            action: "make",
            runtime,
            bind: name,
            size: makeCall.size,
          } as ChanOp);
          this.recordBinding(name, runtime, "channel", valExpr);
          continue;
        }

        // Channel recv: val = <-ch → ChanOp recv with bind
        const recv = this.isChanRecv(valExpr);
        if (recv) {
          this.diagnoseChannelRef(recv.channel, "recv", valExpr);
          const captures = this.computeCaptures(valExpr, runtime);
          ops.push({
            op: "chan",
            action: "recv",
            runtime,
            channel: recv.channel,
            bind: name,
            ...(captures ? { captures } : {}),
          });
          this.recordBinding(name, runtime, "value", valExpr);
          continue;
        }

        if (this.isSimpleLiteral(valExpr)) {
          // Manifest-scope literal → declare
          ops.push({
            op: "declare",
            bind: name,
            mutable: true,
            value: { kind: "literal", value: this.literalValue(valExpr) },
          });
        } else {
          // Runtime eval → bare eval with bind
          this.diagnoseExpr(valExpr, runtime);
          const captures = this.computeCaptures(valExpr, runtime);
          ops.push({
            op: "eval",
            runtime,
            code: this.exprCode(valExpr, runtime),
            bind: name,
            ...(captures ? { captures } : {}),
          });
        }
        this.recordBinding(name, runtime, this.declaredBindingKind(name), valExpr);
      } else {
        // Forward declaration (no value)
        ops.push({
          op: "declare",
          bind: name,
          mutable: true,
        });
        this.recordBinding(name, this.defaultRuntime, "value", node);
      }
    }
    return ops;
  }

  private emitConstDecl(node: AST.ConstDecl): ManifestOp[] {
    if (node.destructurePattern && node.values[0]) {
      return this.emitDestructuringDecl(node.destructurePattern, node.values[0], false);
    }

    const ops: ManifestOp[] = [];
    for (let i = 0; i < node.names.length; i++) {
      const name = node.names[i].name;
      const valExpr = node.values[i];
      if (!valExpr) continue;
      const aff = this.affinityMap.get(valExpr);
      const runtime = this.typedRuntimeHint(name, aff?.runtime || this.defaultRuntime);
      const resourceOp = this.resourceOpFromCall(valExpr, name, runtime);
      if (resourceOp) {
        ops.push(resourceOp);
        this.recordResourceBinding(name, (resourceOp.runtime as OmniRuntime) || runtime, valExpr, resourceOp);
        continue;
      }

      const tableOp = this.tableExportOpFromDeclaration(name, valExpr, runtime);
      if (tableOp) {
        ops.push(tableOp);
        this.recordBinding(name, (tableOp.runtime as OmniRuntime) || runtime, "table", valExpr);
        continue;
      }

      const jobOp = this.jobOpFromCall(valExpr, name, runtime);
      if (jobOp) {
        ops.push(jobOp);
        this.recordBinding(name, (jobOp.runtime as OmniRuntime) || runtime, jobOp.action === "enqueue" ? "job_handle" : "value", valExpr);
        continue;
      }

      const lowered = this.loweredNodeForBinding(node, name);
      const loweredOp = lowered ? this.emitLoweredNode(lowered, false) : undefined;
      if (loweredOp) {
        if (this.typedBindingRuntimeHints.has(name)) {
          if ("runtime" in loweredOp) {
            (loweredOp as ManifestOp & { runtime?: OmniRuntime }).runtime = runtime;
          }
          this.recordBinding(name, runtime, this.declaredBindingKind(name), valExpr);
        }
        ops.push(loweredOp);
        continue;
      }

      if (valExpr.kind === "Go") {
        ops.push(this.emitSpawn(valExpr, name));
        this.recordBinding(name, OmniRuntime.Go, "spawn_handle", valExpr);
        continue;
      }

      // Parallel pattern check
      const parallel = this.isParallelPattern(valExpr);
      if (parallel) {
        ops.push(this.emitParallel(parallel.exprs, name));
        this.recordBinding(name, runtime, "value", valExpr);
        continue;
      }

      // Await (non-parallel) → AwaitOp with bind
      const awaitExpr = this.isAwaitExpr(valExpr);
      if (awaitExpr && !this.isParallelPattern(awaitExpr.inner)) {
        ops.push(this.emitAwait(awaitExpr.inner, runtime, name));
        this.recordBinding(name, runtime, "value", valExpr);
        continue;
      }

      // make() → ChanOp make with bind
      const makeCall = this.isMakeCall(valExpr);
      if (makeCall) {
        ops.push({
          op: "chan",
          action: "make",
          runtime,
          bind: name,
          size: makeCall.size,
        } as ChanOp);
        this.recordBinding(name, runtime, "channel", valExpr);
        continue;
      }

      // Channel recv: val = <-ch → ChanOp recv with bind
      const recv = this.isChanRecv(valExpr);
      if (recv) {
        this.diagnoseChannelRef(recv.channel, "recv", valExpr);
        const captures = this.computeCaptures(valExpr, runtime);
        ops.push({
          op: "chan",
          action: "recv",
          runtime,
          channel: recv.channel,
          bind: name,
          ...(captures ? { captures } : {}),
        });
        this.recordBinding(name, runtime, "value", valExpr);
        continue;
      }

      if (this.isSimpleLiteral(valExpr)) {
        // Manifest-scope literal → declare
        ops.push({
          op: "declare",
          bind: name,
          mutable: false,
          value: { kind: "literal", value: this.literalValue(valExpr) },
        });
      } else {
        // Runtime eval → bare eval with bind
        this.diagnoseExpr(valExpr, runtime);
        const captures = this.computeCaptures(valExpr, runtime);
        ops.push({
          op: "eval",
          runtime,
          code: this.exprCode(valExpr, runtime),
          bind: name,
          ...(captures ? { captures } : {}),
        });
      }
      this.recordBinding(name, runtime, this.declaredBindingKind(name), valExpr);
    }
    return ops;
  }

  private emitDestructuringDecl(pattern: AST.ArrayPattern | AST.ObjectPattern, value: AST.Expr, mutable: boolean): ManifestOp[] {
    const ops: ManifestOp[] = [];
    const targetRuntime = OmniRuntime.JavaScript;
    const valueAff = this.affinityMap.get(value);
    const valueRuntime = valueAff?.runtime || this.defaultRuntime;
    let baseName: string;

    if (value.kind === "Identifier") {
      baseName = value.name;
    } else {
      baseName = `__destructure_${++this.destructureCounter}`;
      const captures = this.computeCaptures(value, valueRuntime);
      ops.push({
        op: "eval",
        runtime: valueRuntime,
        code: this.exprCode(value, valueRuntime),
        bind: baseName,
        ...(captures ? { captures } : {}),
      });
      this.recordBinding(baseName, valueRuntime, "value", value);
    }

    this.emitPatternBindings(pattern, baseName, targetRuntime, mutable, ops);
    return ops;
  }

  private emitPatternBindings(
    pattern: AST.ArrayPattern | AST.ObjectPattern,
    baseExpr: string,
    runtime: OmniRuntime,
    mutable: boolean,
    ops: ManifestOp[],
  ): void {
    if (pattern.kind === "ArrayPattern") {
      pattern.elements.forEach((element, index) => {
        if (!element) return;
        const rest = element.kind === "ArrayPatternElement" && element.rest;
        const target = element.kind === "ArrayPatternElement" ? element.value : element;
        const access = rest ? `Array.from(${baseExpr}).slice(${index})` : `${baseExpr}[${index}]`;
        const valueCode = element.kind === "ArrayPatternElement" && element.defaultValue
          ? `(typeof ${access} === "undefined" ? ${this.exprCode(element.defaultValue, runtime)} : ${access})`
          : access;
        if (target.kind === "Identifier") {
          this.emitDestructuredBinding(target.name, valueCode, runtime, mutable, ops);
        } else {
          this.emitPatternBindings(target, valueCode, runtime, mutable, ops);
        }
      });
      return;
    }

    for (const property of pattern.properties) {
      if (property.rest) {
        // Rest destructuring needs runtime enumeration support; keep it in the JS syntax layer.
        const excluded = pattern.properties
          .filter(prop => !prop.rest)
          .map(prop => prop.key.name);
        const access = `Object.fromEntries(Object.entries(${baseExpr}).filter(([key]) => !${JSON.stringify(excluded)}.includes(key)))`;
        if (property.value.kind === "Identifier") {
          this.emitDestructuredBinding(property.value.name, access, runtime, mutable, ops);
        }
        continue;
      }

      const access = `${baseExpr}.${property.key.name}`;
      const valueCode = property.defaultValue
        ? `(typeof ${access} === "undefined" ? ${this.exprCode(property.defaultValue, runtime)} : ${access})`
        : access;

      if (property.value.kind === "Identifier") {
        this.emitDestructuredBinding(property.value.name, valueCode, runtime, mutable, ops);
      } else {
        this.emitPatternBindings(property.value, access, runtime, mutable, ops);
      }
    }
  }

  private emitDestructuredBinding(
    name: string,
    code: string,
    runtime: OmniRuntime,
    mutable: boolean,
    ops: ManifestOp[],
  ): void {
    const captures: CaptureMap = {};
    for (const identifier of code.matchAll(/\b[A-Za-z_$][\w$]*\b/g)) {
      const boundIn = this.bindingTable.get(identifier[0]);
      if (boundIn && boundIn !== runtime) {
        captures[identifier[0]] = identifier[0];
      }
    }
    ops.push({
      op: "eval",
      runtime,
      code,
      bind: name,
      ...(Object.keys(captures).length > 0 ? { captures } : {}),
    });
    this.recordBinding(name, runtime, mutable ? "value" : "value");
  }

  // ─── Short Declarations (:=) ────────────────────────────────────

  private emitShortDecl(node: AST.ShortDecl): ManifestOp[] {
    const aff = this.affinityMap.get(node);
    const runtime = aff?.runtime || OmniRuntime.Go;
    const ops: ManifestOp[] = [];

    if (node.pairs) {
      for (const pair of node.pairs) {
        const name = pair.name.name;

        if (pair.expr.kind === "Go") {
          ops.push(this.emitSpawn(pair.expr, name));
          this.recordBinding(name, OmniRuntime.Go, "spawn_handle");
          continue;
        }

        // Await (non-parallel) → AwaitOp with bind
        const awaitExpr = this.isAwaitExpr(pair.expr);
        if (awaitExpr && !this.isParallelPattern(awaitExpr.inner)) {
          ops.push(this.emitAwait(awaitExpr.inner, runtime, name));
          this.recordBinding(name, runtime);
          continue;
        }

        // make() → ChanOp make with bind
        const makeCall = this.isMakeCall(pair.expr);
        if (makeCall) {
          ops.push({
            op: "chan",
            action: "make",
            runtime,
            bind: name,
            size: makeCall.size,
          } as ChanOp);
          this.recordBinding(name, runtime, "channel");
          continue;
        }

        // Channel recv: val := <-ch → ChanOp recv with bind
        const recv = this.isChanRecv(pair.expr);
        if (recv) {
          this.diagnoseChannelRef(recv.channel, "recv", pair.expr);
          const captures = this.computeCaptures(pair.expr, runtime);
          ops.push({
            op: "chan",
            action: "recv",
            runtime,
            channel: recv.channel,
            bind: name,
            ...(captures ? { captures } : {}),
          });
          this.recordBinding(name, runtime);
          continue;
        }
        ops.push(this.emitBindingEval(name, pair.expr, runtime));
        this.recordBinding(name, runtime);
      }
    }

    if (node.targets && node.value) {
      // Destructuring: x, y := expr
      const names = node.targets
        .filter(t => t.kind === "Identifier")
        .map(t => (t as AST.Identifier).name);
      const bindName = names[0] || "_";
      ops.push(this.emitBindingEval(bindName, node.value, runtime));
      for (const name of names) {
        this.recordBinding(name, runtime);
      }
    }

    return ops;
  }

  /**
   * Emit an eval op that binds a value. For Go + Call, uses func/args format.
   */
  private emitBindingEval(bindName: string, valExpr: AST.Expr, runtime: OmniRuntime): ManifestOp {
    this.diagnoseExpr(valExpr, runtime);

    // Await (non-parallel) → AwaitOp with bind
    const awaitExpr = this.isAwaitExpr(valExpr);
    if (awaitExpr && !this.isParallelPattern(awaitExpr.inner)) {
      return this.emitAwait(awaitExpr.inner, runtime, bindName);
    }

    // make() → ChanOp make with bind
    const makeCall = this.isMakeCall(valExpr);
    if (makeCall) {
      return {
        op: "chan",
        action: "make",
        runtime,
        bind: bindName,
        size: makeCall.size,
      } as ChanOp;
    }

    if (runtime === OmniRuntime.Go && valExpr.kind === "Call") {
      return {
        op: "eval",
        runtime: OmniRuntime.Go,
        func: exprToCode(valExpr.callee, this.source),
        args: valExpr.args.map(a => exprToCode(a, this.source)),
        bind: bindName,
      };
    }

    const captures = this.computeCaptures(valExpr, runtime);
    return {
      op: "eval",
      runtime,
      code: this.exprCode(valExpr, runtime),
      bind: bindName,
      ...(captures ? { captures } : {}),
    };
  }

  // ─── Functions ────────────────────────────────────────────────

  /**
   * Emit a function declaration. Returns an array: hoisted ops (imports +
   * param-independent initialization) followed by the func_def op.
   *
   * Hoisting rule: walk body ops from the start. An op is hoistable if
   * its captures (if any) don't reference any function parameter. Once we
   * hit an op that depends on a param, everything from there stays in the body.
   */
  private emitFuncDecl(node: AST.FuncDecl): ManifestOp[] {
    const aff = this.affinityMap.get(node);
    const syntaxRuntime = this.funcDeclSyntaxRuntime(node);
    const funcRuntime = syntaxRuntime || aff?.runtime || this.defaultRuntime;
    this.recordBinding(node.name.name, funcRuntime, "function");

    // Go func_def: emit raw source for OmniVM to compile, not decomposed ops.
    // The source field is a complete Go compilation unit.
    if (funcRuntime === OmniRuntime.Go || node.declKeyword === "func") {
      return this.emitGoFuncDef(node);
    }

    // Rust func_def: every Rust fn carries the shared compilation unit.
    if (funcRuntime === OmniRuntime.Rust || node.declKeyword === "fn") {
      return this.emitRustFuncDef(node);
    }

    const paramNames = new Set<string>();
    const params: ParamDef[] = node.params.map(p => {
      const def = this.paramDef(p);
      if (p.spread) def.spread = true;
      if (p.defaultValue) {
        def.defaultValue = { kind: "literal", value: exprToCode(p.defaultValue, this.source) };
      }
      if (p.name.kind === "Identifier") paramNames.add(p.name.name);
      return def;
    });

    // Register function params in the binding table so body ops capture them.
    // Use sentinel runtime — params live in manifest scope, not any runtime's scope.
    for (const name of paramNames) {
      this.recordBinding(name, "__params__" as OmniRuntime);
    }

    const loweredSourceArtifact = this.loweredDefineFuncFor(node)?.sourceArtifact;
    // Increment funcDepth so body ops capture ALL external bindings
    // (not just cross-runtime). Function bodies run in isolated scope.
    this.funcDepth++;
    const bodyBlocks = consolidateBlocks(node.body.statements, this.affinityMap);
    const allBodyOps: ManifestOp[] = [];
    for (const block of bodyBlocks) {
      allBodyOps.push(...this.emitBlock(block));
    }
    this.funcDepth--;

    // Hoist imports and param-independent initialization to top level.
    const hoisted: ManifestOp[] = [];
    const bodyOps: ManifestOp[] = [];
    let doneHoisting = false;

    for (const op of allBodyOps) {
      if (doneHoisting) {
        bodyOps.push(op);
        continue;
      }

      if (this.isHoistable(op, paramNames)) {
        hoisted.push(op);
        // Re-register any bindings from hoisted ops at top level (not func scope)
        // so downstream func body ops still capture them correctly.
      } else {
        doneHoisting = true;
        bodyOps.push(op);
      }
    }

    // Only set bodyRuntime if every block belongs to the same single runtime
    const runtimes = new Set(bodyBlocks.map(b => b.runtime));
    const singleRuntime = syntaxRuntime || (runtimes.size === 1 ? [...runtimes][0] : undefined);

    const funcDef: FuncDefOp = {
      op: "func_def",
      name: node.name.name,
      params,
      body: bodyOps,
      ...(singleRuntime ? { bodyRuntime: singleRuntime } : {}),
      ...(loweredSourceArtifact ? { sourceArtifact: loweredSourceArtifact } : {}),
      ...(node.async ? { async: true } : {}),
      ...(node.generator ? { generator: true } : {}),
    };

    return [...hoisted, funcDef];
  }

  private funcDeclSyntaxRuntime(node: AST.FuncDecl): OmniRuntime | undefined {
    switch (node.declKeyword) {
      case "function":
        return OmniRuntime.JavaScript;
      case "func":
        return OmniRuntime.Go;
      case "fn":
        return OmniRuntime.Rust;
      default:
        return undefined;
    }
  }

  private paramDef(param: AST.Param): ParamDef {
    if (param.name.kind === "Identifier") {
      return { name: param.name.name };
    }
    const name = param.name.kind === "ObjectPattern" ? "__options" : "/* pattern */";
    const callableShape = this.callableShapeForParam(param);
    return {
      name,
      ...(callableShape ? { callableShape } : {}),
    };
  }

  private callableShapeForParam(param: AST.Param): CallableShape | undefined {
    if (param.name.kind !== "ObjectPattern") return undefined;
    const destructuredKeys = this.objectPatternKeys(param.name);
    return {
      acceptsOptionsObject: true,
      ...(destructuredKeys.length > 0 ? { destructuredKeys } : {}),
    };
  }

  private objectPatternKeys(pattern: AST.ObjectPattern): string[] {
    return pattern.properties
      .map(prop => prop.key.name)
      .filter((name, index, names) => name && names.indexOf(name) === index);
  }

  /**
   * An op is hoistable out of a func_def body if it doesn't depend on
   * any function parameter (directly via captures).
   */
  private isHoistable(op: ManifestOp, paramNames: Set<string>): boolean {
    // Imports are always hoistable (module-level by nature)
    if (op.op === "import") return true;

    // Eval ops: check if captures reference any param. Exec ops are
    // side-effecting statements and must remain in the function body.
    if (op.op === "eval") {
      const caps = op.captures;
      if (!caps) return true; // no captures → no param dependency
      for (const key of Object.keys(caps)) {
        if (paramNames.has(key)) return false;
      }
      return true;
    }

    // Declare ops with no runtime eval are hoistable
    if (op.op === "declare") return true;

    // Everything else stays in the body
    return false;
  }

  // ─── Go Function Definitions ────────────────────────────────────

  /**
   * Emit a Go func_def with compilable Go source for OmniVM.
   * Reconstructs valid Go from AST: PascalCase name, typed params/returns,
   * make() fixup, and forward declarations for undefined functions.
   */
  private emitGoFuncDef(node: AST.FuncDecl): ManifestOp[] {
    const name = node.name.name;

    // Go exports use PascalCase (Go visibility rules).
    const goExportName = this.toPascalCase(name);

    const params: ParamDef[] = node.params.map(p => ({
      name: p.name.kind === "Identifier" ? p.name.name : "/* pattern */",
      ...(p.spread ? { spread: true } : {}),
    }));

    const lowered = this.loweredDefineFuncFor(node);
    if (lowered?.go) {
      return [{
        op: "func_def",
        name,
        params,
        body: [],
        bodyRuntime: OmniRuntime.Go,
        source: lowered.go.source,
        exports: [lowered.go.exportName],
        ...(lowered.go.dependencies.length > 0 ? { requires: lowered.go.dependencies.map(dep => dep.name) } : {}),
      }];
    }

    // Reconstruct Go function signature with proper types
    const goParams = node.params.map(p => {
      const pName = p.name.kind === "Identifier" ? p.name.name : "_";
      const pType = p.type ? this.typeNodeToGo(p.type) : "interface{}";
      return `${pName} ${pType}`;
    }).join(", ");

    const returnType = node.returnType
      ? this.typeNodeToGo(node.returnType)
      : (name === "main" ? "" : "interface{}");

    // Reconstruct body from AST with Go-specific fixups
    const paramNames = new Set(node.params
      .map(p => p.name.kind === "Identifier" ? p.name.name : null)
      .filter(Boolean) as string[]);

    const calledFuncs = this.goDependencyCalls(node); // name → arg count
    const definedLocals = new Set<string>();

    const bodyLines = node.body.statements.map(
      s => this.goStmtToCode(s, paramNames, definedLocals, calledFuncs)
    );

    const helperSources: string[] = [];
    const helperImportInputs: string[] = [];
    const sameFileHelpers = new Set<string>();
    if (name === "main") {
      for (const fname of calledFuncs.keys()) {
        const helper = this.goFuncDecls.get(fname);
        if (!helper || helper === node) continue;
        sameFileHelpers.add(fname);
        const helperSource = this.goHelperFuncToCode(helper);
        helperSources.push(helperSource.source);
        helperImportInputs.push(helperSource.body);
      }
    }

    // Detect undefined function calls → external dependencies.
    // These become var function pointers + an Init() that OmniVM calls
    // to inject real implementations (same pattern as SetBridgeCallback).
    const goBuiltins = new Set([
      'make', 'len', 'cap', 'append', 'copy', 'delete', 'new',
      'panic', 'recover', 'close', 'print', 'println',
      'complex', 'real', 'imag',
      'bool', 'byte', 'rune', 'string', 'int', 'int8', 'int16', 'int32', 'int64',
      'uint', 'uint8', 'uint16', 'uint32', 'uint64', 'float32', 'float64',
      '[]byte', '[]rune',
    ]);
    const requires: string[] = [];
    const varDecls: string[] = [];
    for (const [fname, argc] of calledFuncs) {
      if (paramNames.has(fname) || definedLocals.has(fname) || goBuiltins.has(fname)) continue;
      if (sameFileHelpers.has(fname)) continue;
      requires.push(fname);
      const fParamTypes = Array.from({ length: argc }, () => "interface{}").join(", ");
      varDecls.push(`var ${fname} func(${fParamTypes}) interface{}`);
    }

    // Build complete Go compilation unit
    const lines: string[] = ["package polyfunc", ""];
    const imports = this.inferGoImports([...helperImportInputs, bodyLines.join("\n")].join("\n"));
    if (imports.length > 0) {
      lines.push("import (");
      for (const imp of imports) {
        lines.push(`\t"${imp}"`);
      }
      lines.push(")", "");
    }
    if (helperSources.length > 0) {
      lines.push(...helperSources, "");
    }
    if (varDecls.length > 0) {
      lines.push(...varDecls, "");
      // Init function: OmniVM calls this at plugin load time to inject
      // real implementations for external dependencies.
      lines.push(`func Init(deps map[string]interface{}) {`);
      for (const r of requires) {
        const argc = calledFuncs.get(r) || 0;
        const fParamTypes = Array.from({ length: argc }, () => "interface{}").join(", ");
        lines.push(`\tif fn, ok := deps["${r}"].(func(${fParamTypes}) interface{}); ok {`);
        lines.push(`\t\t${r} = fn`);
        lines.push(`\t}`);
      }
      lines.push("}", "");
    }
    const returnSuffix = returnType ? ` ${returnType}` : "";
    lines.push(`func ${goExportName}(${goParams})${returnSuffix} {`);
    for (const line of bodyLines) {
      lines.push(`\t${line}`);
    }
    lines.push("}");

    const funcDef: FuncDefOp = {
      op: "func_def",
      name,
      params,
      body: [],
      bodyRuntime: OmniRuntime.Go,
      source: lines.join("\n"),
      exports: [goExportName],
      ...(requires.length > 0 ? { requires } : {}),
    };

    return [funcDef];
  }

  private goHelperFuncToCode(node: AST.FuncDecl): { source: string; body: string } {
    const name = node.name.name;
    const params = node.params.map(p => {
      const pName = p.name.kind === "Identifier" ? p.name.name : "_";
      const pType = p.type ? this.typeNodeToGo(p.type) : "interface{}";
      return `${pName} ${pType}`;
    }).join(", ");
    const returnType = node.returnType ? this.typeNodeToGo(node.returnType) : "";
    const returnSuffix = returnType ? ` ${returnType}` : "";
    const paramNames = new Set(node.params
      .map(p => p.name.kind === "Identifier" ? p.name.name : null)
      .filter(Boolean) as string[]);
    const calledFuncs = this.goDependencyCalls(node);
    const definedLocals = new Set<string>();
    const bodyLines = node.body.statements.map(
      s => this.goStmtToCode(s, paramNames, definedLocals, calledFuncs)
    );
    const lines = [`func ${name}(${params})${returnSuffix} {`];
    for (const line of bodyLines) {
      lines.push(`\t${line}`);
    }
    lines.push("}");
    return { source: lines.join("\n"), body: bodyLines.join("\n") };
  }

  private inferGoImports(source: string): string[] {
    const imports: Array<[RegExp, string]> = [
      [/\bhmac\./, "crypto/hmac"],
      [/\bsha256\./, "crypto/sha256"],
      [/\bmd5\./, "crypto/md5"],
      [/\brand\./, "crypto/rand"],
      [/\bhex\./, "encoding/hex"],
      [/\bbase64\./, "encoding/base64"],
      [/\bjson\./, "encoding/json"],
      [/\bfmt\./, "fmt"],
      [/\bhttp\./, "net/http"],
      [/\bstrings\./, "strings"],
      [/\btime\./, "time"],
    ];

    const selected = new Set<string>();
    for (const [pattern, path] of imports) {
      if (pattern.test(source)) selected.add(path);
    }
    return Array.from(selected).sort();
  }

  private toPascalCase(name: string): string {
    return name.includes('_')
      ? name.split('_').map(s => s.charAt(0).toUpperCase() + s.slice(1)).join('')
      : name.charAt(0).toUpperCase() + name.slice(1);
  }

  // ─── Rust Function Definitions ──────────────────────────────────

  /**
   * Emit a Rust func_def op. The per-op `name`/`params`/`async` describe the
   * individual fn so stubs register correctly; `source` is the complete
   * shared Rust compilation unit and `exports` lists ALL exported Rust fn
   * names, identical across every Rust func_def in the program, so the host
   * compiles and caches exactly one cdylib.
   */
  private emitRustFuncDef(node: AST.FuncDecl): ManifestOp[] {
    const name = node.name.name;
    // Internal-only fns (and generic unexportables) live in the unit source
    // verbatim but emit no func_def op.
    if (this.rustUnit?.internalFns.has(name)) return [];
    this.recordBinding(name, OmniRuntime.Rust, "function");

    const params: ParamDef[] = node.params.map(p => ({
      name: p.name.kind === "Identifier" ? p.name.name : "/* pattern */",
      ...(p.spread ? { spread: true } : {}),
    }));

    const funcDef: FuncDefOp = {
      op: "func_def",
      name,
      params,
      body: [],
      bodyRuntime: OmniRuntime.Rust,
      ...(node.async ? { async: true } : {}),
      ...(this.rustUnit
        ? this.rustUnitOpFields()
        : { source: this.rustItemSource(node, node.async ? "async " : ""), exports: [name] }),
    };

    return [funcDef];
  }

  /**
   * The unit-carrying fields every Rust func_def op shares: the full
   * compilation unit source, the export list, and — when the unit has
   * verbatim .poly slices — the diagnostic source map (so the host rewrites
   * rustc errors to .poly coordinates).
   */
  private rustUnitOpFields(): Pick<FuncDefOp, "source" | "exports" | "source_map" | "poly_file"> {
    const unit = this.rustUnit!;
    return {
      source: unit.source,
      exports: unit.exports,
      ...(unit.sourceMap.length > 0
        ? {
            source_map: unit.sourceMap,
            ...(this.sourceFile ? { poly_file: this.sourceFile } : {}),
          }
        : {}),
    };
  }

  /**
   * Emit func_def ops for an opaque raw-scanned Rust `fn` item. Same contract
   * as emitRustFuncDef: per-fn name/params/async extracted from the verbatim
   * signature; every op carries the SAME full unit source and export list.
   */
  private emitRustItemFuncDefs(node: AST.RustItem): ManifestOp[] {
    const ops: ManifestOp[] = [];
    for (const sig of node.fns) {
      // Internal-only fns (and generic unexportables) live in the unit
      // source verbatim but emit no func_def op and no manifest binding.
      if (this.rustUnit?.internalFns.has(sig.name)) continue;
      this.recordBinding(sig.name, OmniRuntime.Rust, "function");
      const params: ParamDef[] = sig.params.map(name => ({ name }));
      const funcDef: FuncDefOp = {
        op: "func_def",
        name: sig.name,
        params,
        body: [],
        bodyRuntime: OmniRuntime.Rust,
        ...(sig.async ? { async: true } : {}),
        ...(this.rustUnit
          ? this.rustUnitOpFields()
          : { source: node.text, exports: [sig.name] }),
      };
      ops.push(funcDef);
    }
    return ops;
  }

  /**
   * Collect the shared Rust compilation unit from the program's top level:
   * use statements, structs/enums/traits/impls, module-level `const x = expr`
   * bindings lowered to `std::sync::LazyLock` statics, all Rust fns, and one
   * generated export shim macro line per EXPORTED fn (fns referenced from
   * outside the unit — see the export-set analysis below).
   */
  private collectRustUnit(nodes: Array<AST.Decl | AST.Stmt>): void {
    if (!this.source) return;

    const useItems: RustUnitPiece[] = [];
    const typeItems: RustUnitPiece[] = [];
    const staticItems: RustUnitPiece[] = [];
    const fnItems: RustUnitPiece[] = [];
    const shims: string[] = [];
    const exports: string[] = [];
    const absorbed = new Set<AST.Decl | AST.Stmt>();
    /** Source extents of nodes that live INSIDE the unit; masked out of the
     *  export-set analysis so intra-unit calls don't force exports. */
    const unitSpans: Array<{ start: number; end: number }> = [];
    /** Top-level Rust fn signatures pending export-set analysis, decl order. */
    const pendingFns: Array<{ sig: RustExportSig; node: AST.Decl | AST.Stmt }> = [];

    // Names usable inside the unit without crossing the bridge: use-import
    // bindings, Rust type names, and previously lowered statics.
    const moduleBindings = new Set<string>();
    // Exported Rust fns are manifest stubs — a const whose value calls one is
    // orchestration, not a module-level Rust item.
    const rustFnNames = new Set<string>();

    const isRustNode = (node: AST.Decl | AST.Stmt): boolean =>
      this.affinityMap.get(node)?.runtime === OmniRuntime.Rust;

    const isRustFn = (node: AST.Decl | AST.Stmt): node is AST.FuncDecl =>
      node.kind === "FuncDecl" && (node.declKeyword === "fn" || isRustNode(node));

    for (const node of nodes) {
      if (isRustFn(node)) rustFnNames.add(node.name.name);
      if (node.kind === "RustItem") {
        for (const sig of node.fns) rustFnNames.add(sig.name);
      }
    }

    for (const node of nodes) {
      switch (node.kind) {
        // Opaque items from the raw scanner flow into the unit VERBATIM.
        case "RustItem": {
          unitSpans.push({ start: node.span.start, end: node.span.end });
          switch (node.itemKind) {
            case "use":
              useItems.push({ text: node.text, polyOffset: node.span.start });
              absorbed.add(node);
              for (const bound of node.bindings) moduleBindings.add(bound);
              break;
            case "fn":
              // Not absorbed: emitCompiledBlock emits func_def ops for the
              // exported subset after the export-set analysis below.
              fnItems.push({ text: node.text, polyOffset: node.span.start });
              for (const sig of node.fns) pendingFns.push({ sig, node });
              break;
            case "static":
            case "const":
              staticItems.push({ text: node.text, polyOffset: node.span.start });
              absorbed.add(node);
              if (node.name) {
                moduleBindings.add(node.name);
                this.recordBinding(node.name, OmniRuntime.Rust, "value", node);
              }
              break;
            default: // struct/enum/union/trait/impl/mod/type/macro/extern
              typeItems.push({ text: node.text, polyOffset: node.span.start });
              absorbed.add(node);
              if (node.name) moduleBindings.add(node.name);
              break;
          }
          break;
        }

        case "Import": {
          if (!isRustNode(node)) break;
          const raw = this.rustUseItemSource(node);
          if (!raw) break;
          useItems.push(raw);
          absorbed.add(node);
          unitSpans.push({ start: node.span.start, end: node.span.end });
          const segments = node.path.split("::");
          if (segments[0]) moduleBindings.add(segments[0]);
          const last = segments[segments.length - 1];
          if (last && last !== "*") moduleBindings.add(last);
          break;
        }

        case "ImportDecl": {
          if (!isRustNode(node)) break;
          const raw = this.rustUseItemSource(node);
          if (!raw) break;
          useItems.push(raw);
          absorbed.add(node);
          unitSpans.push({ start: node.span.start, end: node.span.end });
          const root = node.path.split("::")[0];
          if (root) moduleBindings.add(root);
          for (const spec of node.specifiers || []) moduleBindings.add(spec.local);
          break;
        }

        case "TypeDecl":
          if (isRustNode(node) && node.structDecl) {
            typeItems.push(this.rustItemPiece(node));
            absorbed.add(node);
            unitSpans.push({ start: node.span.start, end: node.span.end });
            moduleBindings.add(node.name.name);
          } else if (isRustNode(node)) {
            // Rust type alias `type A = B;` parsed by the union grammar.
            const rawPiece = this.rustItemPiece(node);
            const raw = rawPiece.text;
            if (/^\s*(?:#\[[^\n]*\]\s*\n|\/\/\/[^\n]*\s*\n)*\s*(?:pub(?:\([^)]*\))?\s+)?type\s+[A-Za-z_]/.test(raw)) {
              typeItems.push({
                text: raw.trimEnd().endsWith(";") ? raw : `${raw};`,
                polyOffset: rawPiece.polyOffset,
              });
              absorbed.add(node);
              unitSpans.push({ start: node.span.start, end: node.span.end });
              moduleBindings.add(node.name.name);
            }
          }
          break;

        case "EnumDecl":
          if (isRustNode(node)) {
            typeItems.push(this.rustItemPiece(node));
            absorbed.add(node);
            unitSpans.push({ start: node.span.start, end: node.span.end });
            moduleBindings.add(node.name.name);
          }
          break;

        case "InterfaceDecl":
        case "ImplDecl":
          if (isRustNode(node)) {
            typeItems.push(this.rustItemPiece(node));
            absorbed.add(node);
            unitSpans.push({ start: node.span.start, end: node.span.end });
          }
          break;

        case "FuncDecl": {
          if (!isRustFn(node)) break;
          unitSpans.push({ start: node.span.start, end: node.span.end });
          const itemPiece = this.rustItemPiece(node, node.async ? "async " : "");
          const itemSource = itemPiece.text;
          fnItems.push(itemPiece);
          // Re-scan the reconstructed item so the export-set analysis sees
          // the same raw type texts as raw-scanned fn items; fall back to
          // AST-derived facts when the signature scan finds nothing.
          const scanned = extractRustFnSignatures(itemSource)
            .find(sig => sig.name === node.name.name);
          const sig: RustExportSig = scanned ?? {
            name: node.name.name,
            async: Boolean(node.async),
            paramCount: node.params.length,
            params: node.params.map(p => p.name.kind === "Identifier" ? p.name.name : "arg"),
          };
          pendingFns.push({ sig, node });
          break;
        }

        case "ConstDecl":
        case "VarDecl": {
          if (node.names.length !== 1 || !node.values || node.values.length !== 1) break;
          const value = node.values[0];
          const valueAff = this.affinityMap.get(value) || this.affinityMap.get(node);
          if (valueAff?.runtime !== OmniRuntime.Rust) break;
          // Absorb only when every referenced root identifier resolves inside
          // the unit (imports/types/earlier statics) — calls into exported
          // Rust fns or other-runtime bindings stay orchestration ops.
          const roots = new Set<string>();
          this.collectExprRootIdents(value, roots);
          const allLocal = [...roots].every(r => moduleBindings.has(r) && !rustFnNames.has(r));
          if (roots.size === 0 || !allLocal) break;

          const bindName = node.names[0].name;
          // Lowered statics are generated glue (LazyLock wrapper): no map.
          staticItems.push({ text: this.rustStaticItem(bindName, node, value) });
          absorbed.add(node);
          unitSpans.push({ start: node.span.start, end: node.span.end });
          moduleBindings.add(bindName);
          this.recordBinding(bindName, OmniRuntime.Rust, "value", node);
          break;
        }

        default:
          break;
      }
    }

    // ── Export-set analysis ─────────────────────────────────────────
    // A fn is exported only when its name is referenced OUTSIDE the unit:
    // mask every unit-resident extent out of the source and look for the
    // bare identifier in what remains (other-runtime expressions, top-level
    // poly statements, spawn ops, stubs). This is deliberately conservative:
    // any appearance of the name in non-unit text — even in a comment —
    // keeps the fn exported; only fns whose names appear nowhere outside
    // the unit are proven internal-only.
    const outsideText = this.maskedOutsideText(unitSpans);
    const internalFns = new Set<string>();
    for (const { sig, node } of pendingFns) {
      const referencedOutside = new RegExp(`\\b${sig.name}\\b`).test(outsideText);
      if (!referencedOutside) {
        internalFns.add(sig.name);
        continue;
      }
      if (sig.typeGenerics) {
        // A generic fn cannot be monomorphized behind a serde shim. Skip the
        // shim AND the export so the unit still compiles — the call site
        // fails at runtime with unknown-function, which is the accurate
        // error.
        this.addDiagnostic(
          "warning",
          "rust-generic-export",
          `Rust fn '${sig.name}' is generic and cannot be exported; ` +
            `call it through a concrete wrapper fn instead ` +
            `(e.g. fn ${sig.name}_concrete(...) { ${sig.name}(...) })`,
          node,
        );
        internalFns.add(sig.name);
        continue;
      }
      shims.push(this.rustExportShim(sig, node));
      exports.push(sig.name);
    }

    if (exports.length === 0 && absorbed.size === 0 && fnItems.length === 0) return;

    // Assemble the unit source EXACTLY as before (pieces in a section join
    // with "\n", sections join with "\n\n", empty sections drop out) while
    // walking the same layout to record, for each verbatim .poly slice, its
    // start line inside the unit. Generated glue (lowered statics, shims)
    // contributes lines but no entries. The byte layout must stay identical:
    // unit hashes key the artifact cache and the round-trip oracle.
    const sectionPieces: RustUnitPiece[][] = [
      useItems,
      ...typeItems.map(piece => [piece]),
      ...staticItems.map(piece => [piece]),
      ...fnItems.map(piece => [piece]),
      [{ text: shims.join("\n") }],
    ];
    const sections: string[] = [];
    const sourceMap: RustSourceMapEntry[] = [];
    let unitLine = 1;
    for (const pieces of sectionPieces) {
      const sectionText = pieces.map(piece => piece.text).join("\n");
      if (sectionText.length === 0) continue;
      for (const piece of pieces) {
        const lineCount = piece.text.split("\n").length;
        if (piece.polyOffset !== undefined) {
          sourceMap.push({
            unit_line: unitLine,
            poly_line: this.lineOfOffset(piece.polyOffset),
            lines: lineCount,
          });
        }
        unitLine += lineCount; // pieces within a section join with "\n"
      }
      sections.push(sectionText);
      unitLine += 1; // sections join with "\n\n": one blank separator line
    }

    this.rustUnit = {
      source: sections.join("\n\n"),
      exports,
      absorbed,
      internalFns,
      sourceMap,
    };
  }

  /** 1-based line number of a byte offset in the original source. */
  private lineOfOffset(offset: number): number {
    let line = 1;
    const src = this.source!;
    for (let i = 0; i < offset && i < src.length; i++) {
      if (src.charCodeAt(i) === 10) line++;
    }
    return line;
  }

  /**
   * The program source with every unit-resident extent blanked out — the
   * text the export-set analysis scans for outside references to Rust fns.
   */
  private maskedOutsideText(spans: Array<{ start: number; end: number }>): string {
    const src = this.source!;
    const sorted = [...spans].sort((a, b) => a.start - b.start);
    let out = "";
    let pos = 0;
    for (const span of sorted) {
      if (span.start > pos) out += src.slice(pos, span.start);
      pos = Math.max(pos, span.end);
    }
    if (pos < src.length) out += src.slice(pos);
    return out;
  }

  /**
   * Build the export shim for one exported Rust fn. When the signature has
   * borrowed params (`&str`, `&[T]`) — and, where applicable, a borrowed
   * `&str`-shaped return — generate a concrete owned-data adapter fn in the
   * unit and shim THAT instead. The exported symbol stays
   * `OmniVMCall_<original name>`: the host's call contract is by original
   * name.
   *
   * Adaptation is strictly mechanical:
   *   params:  `&str` -> String (pass `&x`); `&[T]` -> Vec<T> (pass `&x`);
   *            everything else passes through unchanged.
   *   return:  `&str` -> String via .to_string();
   *            `Vec<&str>` -> Vec<String>; `Option<&str>` -> Option<String>.
   * A return type with any OTHER borrow cannot be adapted: emit a warning
   * diagnostic and fall back to the direct shim (rustc reports the rest).
   */
  private rustExportShim(sig: RustExportSig, node: AST.Decl | AST.Stmt): string {
    const macroName = sig.async ? "export_async_fn" : "export_fn";
    // DataFrame params use the typed-kind macro form so the Arrow C-Data
    // pointer handoff stays zero-copy (the json kind goes through serde).
    const kindList = rustShimKinds(sig);
    const arityOrKinds = kindList ?? `${sig.paramCount}`;
    const directShim =
      `omnivm::${macroName}!(OmniVMCall_${sig.name}, ${sig.name}, ${arityOrKinds});` +
      rustTypedShim(sig, sig.name, (sig.paramTypes ?? []).map(t => stripRustLifetimes(t ?? "").trim()),
        stripRustLifetimes(sig.returnType ?? "").trim());

    const paramTypes = sig.paramTypes ?? [];
    const params = sig.params.map((name, i) => adaptRustParamType(name, paramTypes[i] ?? ""));
    const ret = adaptRustReturnType(sig.returnType ?? "");

    if (ret.kind === "unadaptable") {
      this.addDiagnostic(
        "warning",
        "rust-borrowed-return",
        `Exported Rust fn '${sig.name}' returns borrowed data ` +
          `(${sig.returnType}); exported fns must return owned data`,
        node,
      );
      return directShim;
    }

    const needsAdapter = params.some(p => p.adapted) || ret.kind === "mapped";
    if (!needsAdapter) return directShim;
    // An adapter must restate every param type; bail to the direct shim when
    // any passthrough type text is unavailable (pattern params etc.).
    if (params.some(p => p.ownedType.length === 0)) return directShim;

    const adapterName = `__omnivm_export_${sig.name}`;
    const paramList = params.map(p => `${p.name}: ${p.ownedType}`).join(", ");
    const args = params.map(p => (p.adapted ? `&${p.name}` : p.name)).join(", ");
    const retClause = ret.ownedType.length > 0 ? ` -> ${ret.ownedType}` : "";
    const body = `${sig.name}(${args})${sig.async ? ".await" : ""}${ret.mapExpr}`;
    const fnIntro = sig.async ? "async fn" : "fn";
    return [
      `${fnIntro} ${adapterName}(${paramList})${retClause} {`,
      `    ${body}`,
      `}`,
      `omnivm::${macroName}!(OmniVMCall_${sig.name}, ${adapterName}, ${rustShimKinds(sig) ?? sig.paramCount});` +
        rustTypedShim(sig, adapterName, params.map(p => p.ownedType),
          ret.ownedType.length > 0 ? ret.ownedType : stripRustLifetimes(sig.returnType ?? "").trim()),
    ].join("\n");
  }

  /**
   * Lower a module-level Rust `const name = expr` binding to a lazily
   * initialized static (`std::sync::LazyLock`), preserving the author's name
   * so fn bodies referencing it verbatim keep working.
   */
  private rustStaticItem(name: string, node: AST.ConstDecl | AST.VarDecl, value: AST.Expr): string {
    const valueSource = this.sliceSource(value) || exprToCode(value, this.source);
    let typeName: string | undefined;
    if (node.type) {
      typeName = this.sliceSource(node.type as unknown as { span: AST.Span });
    }
    if (!typeName && value.kind === "Call" && value.callee.kind === "Member" &&
        value.callee.property.kind === "Identifier" && value.callee.property.name === "new") {
      typeName = this.sliceSource(value.callee.object)?.replace(/\./g, "::");
    }
    if (!typeName) typeName = "omnivm::Value";
    return [
      "#[allow(non_upper_case_globals)]",
      `static ${name}: std::sync::LazyLock<${typeName}> = std::sync::LazyLock::new(|| ${valueSource});`,
    ].join("\n");
  }

  /**
   * Verbatim source for a union-parsed `use` import absorbed into the Rust
   * unit, including preceding attribute lines (`#[cfg(...)]`) and a trailing
   * semicolon. Returns undefined when the import is not a Rust `use`.
   */
  private rustUseItemSource(node: { span: AST.Span }): RustUnitPiece | undefined {
    const piece = this.rustItemPiece(node);
    const raw = piece.text;
    if (!raw || !/(?:^|\n)\s*(?:pub(?:\([^)]*\))?\s+)?use\s/.test(raw)) return undefined;
    return {
      text: raw.trimEnd().endsWith(";") ? raw : `${raw};`,
      polyOffset: piece.polyOffset,
    };
  }

  /**
   * Slice a Rust item's raw source, expanding backwards to include preceding
   * attribute lines (#[derive(...)], #[serde(tag = "type")]) which the lexer
   * treats as comments and the node span excludes. `prefix` re-attaches
   * modifiers the parser consumed before the span start (e.g. `async `).
   */
  private rustItemSource(node: { span: AST.Span }, prefix = ""): string {
    return this.rustItemPiece(node, prefix).text;
  }

  /**
   * rustItemSource plus the byte offset in the original source where the
   * reconstructed slice begins (attribute walk-back included) — the anchor
   * the Rust unit source map records. Internal newlines of the slice match
   * the original extent, so line arithmetic from the anchor stays valid.
   */
  private rustItemPiece(node: { span: AST.Span }, prefix = ""): RustUnitPiece {
    const src = this.source;
    if (!src) return { text: "" };

    // When the parser consumed leading modifiers (pub/async/unsafe/const)
    // before the node span, the line residue restores them verbatim.
    const residueLineStart = src.lastIndexOf("\n", node.span.start - 1) + 1;
    const residue = src.slice(residueLineStart, node.span.start);
    let text: string;
    let start: number;
    if (residue.trim().length > 0 &&
        /^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+|unsafe\s+|const\s+)*$/.test(residue)) {
      text = src.slice(residueLineStart, node.span.end);
      start = residueLineStart;
    } else {
      const body = src.slice(node.span.start, node.span.end);
      text = prefix && !body.startsWith(prefix.trim()) ? prefix + body : body;
      // A re-attached prefix has no newline: span.start's line is the anchor.
      start = node.span.start;
    }

    // Walk back over immediately preceding attribute/doc lines (same rule —
    // and same code — as the raw item scanner's walk-back, including
    // multi-line attribute groups).
    const attrStart = attributePrefixStart(src, residueLineStart);
    if (attrStart < residueLineStart) {
      return { text: src.slice(attrStart, residueLineStart) + text, polyOffset: attrStart };
    }
    return { text, polyOffset: start };
  }

  /**
   * Collect root identifier names referenced by an expression: bare
   * identifiers and the left-most objects of member chains (property names
   * are skipped — `Client::new()` references only `Client`).
   */
  private collectExprRootIdents(node: unknown, out: Set<string>): void {
    if (!node || typeof node !== "object") return;
    if (Array.isArray(node)) {
      for (const item of node) this.collectExprRootIdents(item, out);
      return;
    }
    const n = node as any;
    if (n.kind === "Identifier") {
      out.add(n.name);
      return;
    }
    if (n.kind === "Member") {
      this.collectExprRootIdents(n.object, out);
      return;
    }
    for (const [key, value] of Object.entries(n)) {
      if (key === "span") continue;
      this.collectExprRootIdents(value, out);
    }
  }

  /** Raw source slice for a node, when source text and a real span exist. */
  private sliceSource(node: { span?: AST.Span } | undefined): string | undefined {
    if (!node || !this.source || !node.span || node.span.end <= node.span.start) return undefined;
    return this.source.slice(node.span.start, node.span.end);
  }

  // ─── Go Source Reconstruction ──────────────────────────────────

  private collectGoCalls(node: unknown, calledFuncs: Map<string, number>): void {
    if (!node || typeof node !== "object") return;
    if (Array.isArray(node)) {
      for (const item of node) this.collectGoCalls(item, calledFuncs);
      return;
    }

    const n = node as any;
    if (n.kind === "Call" && n.callee?.kind === "Identifier") {
      calledFuncs.set(n.callee.name, Math.max(
        calledFuncs.get(n.callee.name) ?? 0,
        n.args?.length ?? 0
      ));
    }

    for (const [key, value] of Object.entries(n)) {
      if (key === "span") continue;
      this.collectGoCalls(value, calledFuncs);
    }
  }

  private goDependencyCalls(node: AST.FuncDecl): Map<string, number> {
    const calls = new Map<string, number>();
    const lowered = this.loweredDefineFuncFor(node);
    if (lowered?.dependencies) {
      for (const dep of lowered.dependencies) {
        calls.set(dep.name, Math.max(calls.get(dep.name) ?? 0, dep.argc));
      }
      return calls;
    }

    for (const stmt of node.body.statements) {
      this.collectGoCalls(stmt, calls);
    }
    return calls;
  }

  private goBlockToCode(
    statements: Array<AST.Stmt | AST.Decl>,
    params: Set<string>,
    locals: Set<string>,
    calledFuncs: Map<string, number>
  ): string {
    return statements
      .flatMap(s => this.goStmtToCode(s, params, locals, calledFuncs).split("\n"))
      .map(line => `\t${line}`)
      .join("\n");
  }

  private goStmtToCode(
    node: AST.Stmt | AST.Decl,
    params: Set<string>,
    locals: Set<string>,
    calledFuncs: Map<string, number>,
  ): string {
    switch (node.kind) {
      case "ShortDecl": {
        if (node.pairs) {
          return node.pairs.map(p => {
            locals.add(p.name.name);
            return `${p.name.name} := ${this.goExprToCode(p.expr, params, locals, calledFuncs)}`;
          }).join("; ");
        }
        if (node.targets && node.value) {
          const names = node.targets.filter(t => t.kind === "Identifier").map(t => (t as AST.Identifier).name);
          for (const n of names) locals.add(n);
          return `${names.join(", ")} := ${this.goExprToCode(node.value, params, locals, calledFuncs)}`;
        }
        return this.goSpanFallback(node);
      }
      case "Return":
        if (node.values.length === 0) return "return";
        return `return ${node.values.map(v => this.goExprToCode(v, params, locals, calledFuncs)).join(", ")}`;
      case "ExprStmt":
        return this.goExprToCode(node.expr, params, locals, calledFuncs);
      case "VarDecl":
        return node.names.map((n, i) => {
          locals.add(n.name);
          const t = node.type ? this.typeNodeToGo(node.type) : "interface{}";
          if (node.values?.[i]) {
            return `var ${n.name} ${t} = ${this.goExprToCode(node.values[i], params, locals, calledFuncs)}`;
          }
          return `var ${n.name} ${t}`;
        }).join("; ");
      case "Loop": {
        const body = this.goBlockToCode(node.body.statements, params, locals, calledFuncs);
        if (node.mode === "while" && node.test) {
          const test = this.goExprToCode(node.test, params, locals, calledFuncs);
          return `for ${test} {\n${body}\n}`;
        }
        return `for {\n${body}\n}`;
      }
      case "If": {
        const arms = node.arms.map((arm, idx) => {
          const test = this.goExprToCode(arm.test, params, locals, calledFuncs);
          const body = this.goBlockToCode(arm.body.statements, params, locals, calledFuncs);
          return `${idx === 0 ? "if" : "else if"} ${test} {\n${body}\n}`;
        });
        if (node.elseBody) {
          const elseBody = this.goBlockToCode(node.elseBody.statements, params, locals, calledFuncs);
          arms.push(`else {\n${elseBody}\n}`);
        }
        return arms.join(" ");
      }
      default:
        return this.goSpanFallback(node);
    }
  }

  private goExprToCode(
    expr: AST.Expr,
    params: Set<string>,
    locals: Set<string>,
    calledFuncs: Map<string, number>,
  ): string {
    switch (expr.kind) {
      case "Identifier":
        return expr.name;
      case "NumericLiteral":
        return expr.raw;
      case "StringLiteral":
        if (expr.parts.length === 1 && expr.parts[0].kind === "Text") {
          return JSON.stringify(expr.parts[0].value as string);
        }
        return this.goSpanFallback(expr);
      case "BooleanLiteral":
        return String(expr.value);
      case "NullLiteral":
        return "nil";
      case "Call": {
        const callee = this.goExprToCode(expr.callee, params, locals, calledFuncs);
        const args = expr.args.map(a => this.goExprToCode(a, params, locals, calledFuncs));

        // make() fixup: make(N) → make(chan interface{}, N)
        if (callee === "make" && args.length === 1 && /^\d+$/.test(args[0])) {
          return `make(chan interface{}, ${args[0]})`;
        }

        return `${callee}(${args.join(", ")})`;
      }
      case "NewExpr": {
        const callee = this.goExprToCode(expr.callee, params, locals, calledFuncs);
        const args = expr.args.map(a => this.goExprToCode(a, params, locals, calledFuncs));
        return `new ${callee}(${args.join(", ")})`;
      }
      case "Member":
        return `${this.goExprToCode(expr.object, params, locals, calledFuncs)}.${expr.property.name}`;
      case "Index":
        return `${this.goExprToCode(expr.object, params, locals, calledFuncs)}[${this.goExprToCode(expr.index, params, locals, calledFuncs)}]`;
      case "Binary": {
        const arithmeticOps = new Set(['*', '+', '-', '/', '%', '^', '>>', '<<', '&', '|', '&^']);
        const left = this.goExprToCode(expr.left, params, locals, calledFuncs);
        const right = this.goExprToCode(expr.right, params, locals, calledFuncs);
        // interface{} params used in arithmetic need type assertion
        if (arithmeticOps.has(expr.op)) {
          const lhs = (expr.left.kind === "Identifier" && params.has(expr.left.name)) ? `${left}.(int)` : left;
          const rhs = (expr.right.kind === "Identifier" && params.has(expr.right.name)) ? `${right}.(int)` : right;
          return `${lhs} ${expr.op} ${rhs}`;
        }
        return `${left} ${expr.op} ${right}`;
      }
      case "Unary":
        if (expr.prefix) return `${expr.op}${this.goExprToCode(expr.argument, params, locals, calledFuncs)}`;
        return `${this.goExprToCode(expr.argument, params, locals, calledFuncs)}${expr.op}`;
      case "Assign":
        return `${this.goExprToCode(expr.left, params, locals, calledFuncs)} ${expr.op} ${this.goExprToCode(expr.right, params, locals, calledFuncs)}`;
      default:
        return this.goSpanFallback(expr);
    }
  }

  private goSpanFallback(node: { span?: AST.Span }): string {
    if (this.source && node.span && node.span.end > node.span.start) {
      return this.source.slice(node.span.start, node.span.end);
    }
    return "/* unsupported */";
  }

  /**
   * Convert a TypeNode to a Go type string.
   * Falls back to interface{} for types that don't map cleanly.
   */
  private typeNodeToGo(t: AST.TypeNode): string {
    switch (t.kind) {
      case "SimpleType":
        return t.id.name;
      case "GenericType": {
        // map[K]V, []T, chan T
        const base = t.base.name;
        if (base === "map" && t.args.length === 2) {
          return `map[${this.typeNodeToGo(t.args[0])}]${this.typeNodeToGo(t.args[1])}`;
        }
        if (base === "chan" && t.args.length === 1) {
          return `chan ${this.typeNodeToGo(t.args[0])}`;
        }
        // Slice: []T — parser may represent as GenericType with base "Array" or similar
        return `${base}[${t.args.map(a => this.typeNodeToGo(a)).join(", ")}]`;
      }
      case "NullableType":
        return `*${this.typeNodeToGo(t.inner)}`;
      case "ChanType":
        return this.source && t.span ? this.source.slice(t.span.start, t.span.end) : "chan interface{}";
      default:
        // Use span extraction for complex types
        return this.source && t.span ? this.source.slice(t.span.start, t.span.end) : "interface{}";
    }
  }

  // ─── Control Flow ─────────────────────────────────────────────

  private emitIf(node: AST.If): IfOp {
    const arms = node.arms.map(arm => {
      const aff = this.affinityMap.get(arm.test);
      const testRuntime = aff?.runtime || this.defaultRuntime;

      const test: ConditionExpr = {
        kind: "expr",
        runtime: testRuntime,
        code: exprToCode(arm.test, this.source),
      };

      const bodyBlocks = consolidateBlocks(arm.body.statements, this.affinityMap);
      const bodyOps: ManifestOp[] = [];
      for (const block of bodyBlocks) {
        bodyOps.push(...this.emitBlock(block));
      }

      return { test, body: bodyOps };
    });

    const ifOp: IfOp = { op: "if", arms };

    if (node.elseBody) {
      const elseBlocks = consolidateBlocks(node.elseBody.statements, this.affinityMap);
      const elseOps: ManifestOp[] = [];
      for (const block of elseBlocks) {
        elseOps.push(...this.emitBlock(block));
      }
      ifOp.elseBody = elseOps;
    }

    return ifOp;
  }

  private emitLoop(node: AST.Loop): ManifestOp {
    const loopAff = this.affinityMap.get(node);
    const loopRuntime = loopAff?.runtime || this.defaultRuntime;
    if (this.shouldEmitNativeJavaScriptLoop(node, loopRuntime)) {
      const captures = this.computeCaptures(node, OmniRuntime.JavaScript);
      return {
        op: "exec",
        runtime: OmniRuntime.JavaScript,
        code: nodeToSourceCode(node, this.source),
        ...(captures ? { captures } : {}),
      };
    }

    const bodyBlocks = consolidateBlocks(node.body.statements, this.affinityMap);
    const bodyOps: ManifestOp[] = [];
    for (const block of bodyBlocks) {
      bodyOps.push(...this.emitBlock(block));
    }

    const loopOp: LoopOp = {
      op: "loop",
      mode: node.mode === "while" ? "while" :
            node.mode === "for" ? "for" :
            node.mode === "foreach" ? "foreach" : "infinite",
      body: bodyOps,
    };
    if (node.await) {
      loopOp.await = true;
    }

    if (node.test) {
      const aff = this.affinityMap.get(node.test);
      const testRuntime = aff?.runtime || this.defaultRuntime;
      loopOp.test = {
        kind: "expr",
        runtime: testRuntime,
        code: exprToCode(node.test, this.source),
      };
    }

    // Foreach: set variable and iterable
    if (node.mode === "foreach") {
      if (node.variable) {
        if (node.variable.kind === "ArrayPattern") {
          loopOp.variable = node.variable.elements
            .filter((e): e is AST.Identifier => e !== null && e.kind === "Identifier")
            .map(e => e.name)
            .join(", ");
        } else {
          loopOp.variable = node.variable.kind === "Identifier" ? node.variable.name : "/* pattern */";
        }
      }
      if (node.iterable) {
        loopOp.iterable = this.foreachIterableValue(node.iterable);
      }
      const iterationMode = this.foreachIterationMode(node);
      if (iterationMode) {
        loopOp.iterationMode = iterationMode;
      }
    }

    return loopOp;
  }

  private foreachIterationMode(node: AST.Loop): "values" | "keys" | "auto" | undefined {
    if (node.iterationKind !== "in") {
      return undefined;
    }
    const raw = this.source ? this.source.slice(node.span.start, node.span.end).trim() : nodeToSourceCode(node, this.source).trim();
    return /^for\s*\(/.test(raw) ? "keys" : "auto";
  }

  private foreachIterableValue(expr: AST.Expr): ManifestValue {
    const literal = this.tryLiteralValue(expr);
    if (literal.ok) {
      return { kind: "literal", value: literal.value };
    }
    if (expr.kind === "Identifier") {
      return { kind: "ref", name: expr.name };
    }
    return { kind: "ref", name: exprToCode(expr, this.source) };
  }

  private shouldEmitNativeJavaScriptLoop(node: AST.Loop, loopRuntime: OmniRuntime): boolean {
    if (loopRuntime !== OmniRuntime.JavaScript || node.mode !== "foreach" || !node.iterable) {
      return false;
    }
    const iterableAff = this.affinityMap.get(node.iterable);
    return !!iterableAff && iterableAff.runtime !== OmniRuntime.JavaScript && iterableAff.confidence !== "fallback";
  }

  private emitReturn(node: AST.Return): ReturnOp {
    if (node.values.length === 0) {
      return { op: "return" };
    }

    if (node.values.length === 1) {
      const val = node.values[0];

      // Simple literal → return with value (no runtime eval needed)
      if (this.isSimpleLiteral(val)) {
        return {
          op: "return",
          value: { kind: "literal", value: this.literalValue(val) },
        };
      }

      // Simple identifier reference → return with ref
      if (val.kind === "Identifier") {
        return {
          op: "return",
          value: { kind: "ref", name: val.name },
        };
      }

      // Complex expression → return from eval
      const aff = this.affinityMap.get(val);
      const runtime = aff?.runtime || this.defaultRuntime;
      const captures = this.computeCaptures(val, runtime);

      return {
        op: "return",
        from: {
          op: "eval",
          runtime,
          code: this.exprCode(val, runtime),
          bind: "__ret",
          ...(captures ? { captures } : {}),
        },
      };
    }

    // Multiple return values
    return {
      op: "return",
      value: {
        kind: "literal",
        value: node.values.map(v => exprToCode(v, this.source)),
      },
    };
  }

  // ─── Try/Catch/Throw ────────────────────────────────────────

  private emitTry(node: AST.Try): TryOp {
    // Decompose try body
    const bodyBlocks = consolidateBlocks(node.body.statements, this.affinityMap);
    const bodyOps: ManifestOp[] = [];
    for (const block of bodyBlocks) {
      bodyOps.push(...this.emitBlock(block));
    }

    // Decompose catch clauses with runtime and optional errorType
    const catches: ManifestCatch[] = node.catches.map(c => {
      const catchBlocks = consolidateBlocks(c.body.statements, this.affinityMap);
      const catchOps: ManifestOp[] = [];
      for (const block of catchBlocks) {
        catchOps.push(...this.emitBlock(block));
      }

      // Determine catch handler runtime from first body op or try body's dominant runtime
      let catchRuntime: string | undefined;
      if (catchBlocks.length > 0) {
        catchRuntime = catchBlocks[0].runtime;
      } else if (bodyBlocks.length > 0) {
        catchRuntime = bodyBlocks[0].runtime;
      }

      // Extract errorType from typed catch param (Python except ValueError, Java catch IOException)
      let errorType: string | undefined;
      if (c.type) {
        if (c.type.kind === "SimpleType") {
          errorType = c.type.id.name;
        } else if (this.source && c.type.span) {
          errorType = this.source.slice(c.type.span.start, c.type.span.end);
        }
      }

      return {
        ...(c.param ? { param: c.param.name } : {}),
        body: catchOps,
        ...(catchRuntime ? { runtime: catchRuntime } : {}),
        ...(errorType ? { errorType } : {}),
      };
    });

    const tryOp: TryOp = {
      op: "try",
      body: bodyOps,
      catches,
    };

    // Decompose finally body if present
    if (node.finallyBody) {
      const finallyBlocks = consolidateBlocks(node.finallyBody.statements, this.affinityMap);
      const finallyOps: ManifestOp[] = [];
      for (const block of finallyBlocks) {
        finallyOps.push(...this.emitBlock(block));
      }
      tryOp.finallyBody = finallyOps;
    }

    return tryOp;
  }

  private emitThrow(node: AST.Throw): ThrowOp | ExecOp {
    if (!node.cause && this.isSimpleLiteral(node.value)) {
      return {
        op: "throw",
        value: { kind: "literal", value: this.literalValue(node.value) },
      };
    }
    if (!node.cause && node.value.kind === "Identifier") {
      return {
        op: "throw",
        value: { kind: "ref", name: node.value.name },
      };
    }

    // Complex throw expressions should stay native so runtime-specific error
    // objects preserve name/message/stack through the manifest catch boundary.
    const aff = this.affinityMap.get(node.value);
    const runtime = aff?.runtime || this.defaultRuntime;
    const captures = this.computeCaptures(node, runtime);
    return {
      op: "exec",
      runtime,
      code: this.throwStatementCode(node.value, runtime, node.cause),
      ...(captures ? { captures } : {}),
    };
  }

  private throwStatementCode(value: AST.Expr, runtime: OmniRuntime, cause?: AST.Expr): string {
    const code = this.exprCode(value, runtime);
    switch (runtime) {
      case OmniRuntime.Python:
        return cause ? `raise ${code} from ${this.exprCode(cause, runtime)}` : `raise ${code}`;
      case OmniRuntime.Ruby:
        return `raise ${code}`;
      case OmniRuntime.Java:
        return `throw ${code};`;
      default:
        return `throw ${code}`;
    }
  }

  // ─── Imports ──────────────────────────────────────────────────

  private emitImport(node: AST.Import | AST.ImportDecl): ImportOp {
    const aff = this.affinityMap.get(node);
    const runtime = aff?.runtime || this.defaultRuntime;
    const lowered = this.loweredNodesFor(node).find(
      (item): item is Extract<LoweredManifestNode, { kind: "Import" }> => item.kind === "Import",
    );
    const artifact = lowered?.artifact;

    if (node.kind === "Import") {
      // Record binding for the imported module — always bind the module name
      // so it enters the manifest's binding table for captures tracking.
      const bindName = artifact?.bind || node.alias?.name || (runtime === OmniRuntime.Go ? this.goImportBindingName(node.path) : node.path);
      this.recordBinding(bindName, runtime);
      return {
        op: "import",
        path: artifact?.path || node.path,
        runtime,
        bind: bindName,
        ...(artifact?.source ? { sourceArtifact: artifact.source } : {}),
      };
    }

    // ImportDecl (ES-style)
    const importOp: ImportOp = {
      op: "import",
      path: artifact?.path || node.path,
      runtime,
      ...(artifact?.source ? { sourceArtifact: artifact.source } : {}),
    };

    const defaultImport = artifact?.defaultImport || node.defaultImport?.name;
    if (defaultImport) {
      importOp.defaultImport = defaultImport;
      this.recordBinding(defaultImport, runtime);
    }
    const namespaceImport = artifact?.namespaceImport || node.namespaceImport?.name;
    if (namespaceImport) {
      importOp.namespaceImport = namespaceImport;
      this.recordBinding(namespaceImport, runtime);
    }
    const specifiers = artifact?.specifiers || node.specifiers;
    if (specifiers && specifiers.length > 0) {
      importOp.specifiers = specifiers.map(s => {
        this.recordBinding(s.local, runtime);
        return { imported: s.imported, local: s.local };
      });
    }

    return importOp;
  }

  private goImportBindingName(path: string): string {
    const last = path.split("/").filter(Boolean).pop() || path;
    const cleaned = last.replace(/[^A-Za-z0-9_]/g, "_");
    return /^[A-Za-z_]/.test(cleaned) ? cleaned : `_${cleaned}`;
  }

  // ─── String Interpolation ─────────────────────────────────────

  private emitStringInterpolation(node: AST.StringLiteral, scopeRuntime: OmniRuntime): ConcatOp {
    const segments: ConcatSegment[] = [];

    for (const part of node.parts) {
      if (part.kind === "Text") {
        const text = part.value as string;
        if (text) {
          segments.push({ kind: "text", value: text });
        }
      } else {
        // Interpolation
        if (typeof part.value === "string") {
          // String-based interpolation — check for @runtime() markers
          const runtimeTagMatch = part.value.match(/^@(py|js|go|rb|java)\((.+)\)$/s);
          if (runtimeTagMatch) {
            const [, tag, innerExpr] = runtimeTagMatch;
            const rt = tagToRuntime(tag);
            segments.push({ kind: "eval", runtime: rt, code: innerExpr });
          } else {
            // No tag — use scope runtime
            segments.push({ kind: "eval", runtime: scopeRuntime, code: part.value });
          }
        } else {
          // AST expression interpolation
          const expr = part.value as AST.Expr;
          if (expr.kind === "RuntimeTag") {
            const rt = tagToRuntime(expr.runtime);
            segments.push({ kind: "eval", runtime: rt, code: exprToCode(expr.expr, this.source) });
          } else {
            const aff = this.affinityMap.get(expr);
            const exprRuntime = aff?.runtime || scopeRuntime;
            segments.push({ kind: "eval", runtime: exprRuntime, code: exprToCode(expr, this.source) });
          }
        }
      }
    }

    return {
      op: "concat",
      bind: "__str",
      segments,
    };
  }
}

// ─── Rust export adapter helpers ──────────────────────────────────────

/** Strip lifetime annotations and collapse whitespace for type matching. */
function stripRustLifetimes(typeText: string): string {
  return typeText.replace(/'\w+\s*/g, "").replace(/\s+/g, " ").trim();
}

interface AdaptedRustParam {
  name: string;
  /** Owned type text for the adapter signature ("" when unknown). */
  ownedType: string;
  /** True when the adapter must re-borrow (`&x`) at the call site. */
  adapted: boolean;
}

/**
 * Owned-data adaptation for one exported-fn parameter:
 * `&str` -> String, `&[T]` -> Vec<T>; anything else passes through with the
 * author's type text unchanged.
 */
/**
 * Per-param extraction kinds for the export shim: `(df, json, ...)` when any
 * param is DataFrame-typed (zero-copy Arrow import), else null (arity form).
 */
/**
 * Scalar-shaped exports also get an omni_value_t typed entry: args/returns
 * cross with no JSON text. Presence of the OmniVMCallTyped_ symbol is the
 * host's capability signal.
 */
function rustTypedShim(sig: RustExportSig, target: string, paramTypes: string[], returnType: string): string {
  if (sig.async || sig.typeGenerics) return "";
  if (sig.paramCount > 3 || paramTypes.length !== sig.paramCount) return "";
  const scalar = new Set(["i64", "f64", "bool", "String"]);
  if (!paramTypes.every(t => scalar.has(t))) return "";
  if (!scalar.has(returnType)) return "";
  return `\nomnivm::export_typed_fn!(OmniVMCallTyped_${sig.name}, ${target}, ${sig.paramCount});`;
}

function rustShimKinds(sig: RustExportSig): string | null {
  const paramTypes = sig.paramTypes ?? [];
  if (sig.paramCount === 0 || sig.paramCount > 4) return null;
  const kinds: string[] = [];
  let sawDf = false;
  for (let i = 0; i < sig.paramCount; i++) {
    const t = stripRustLifetimes(paramTypes[i] ?? "").trim();
    if (t === "DataFrame" || t === "polars::prelude::DataFrame" || t === "prelude::DataFrame") {
      kinds.push("df");
      sawDf = true;
    } else {
      kinds.push("json");
    }
  }
  return sawDf ? `(${kinds.join(", ")})` : null;
}

function adaptRustParamType(name: string, typeText: string): AdaptedRustParam {
  const t = stripRustLifetimes(typeText);
  if (/^&\s*str$/.test(t)) {
    return { name, ownedType: "String", adapted: true };
  }
  const slice = /^&\s*\[\s*(.+?)\s*\]$/.exec(t);
  if (slice && !slice[1].includes("&")) {
    return { name, ownedType: `Vec<${slice[1]}>`, adapted: true };
  }
  return { name, ownedType: typeText.trim(), adapted: false };
}

interface AdaptedRustReturn {
  kind: "none" | "passthrough" | "mapped" | "unadaptable";
  /** Owned return type text for the adapter signature ("" when fn returns ()). */
  ownedType: string;
  /** Postfix expression mapping the borrowed result to owned data. */
  mapExpr: string;
}

/**
 * Owned-data adaptation for an exported fn's return type. Only `&str`-shaped
 * borrows are mapped (`&str`, `Vec<&str>`, `Option<&str>`); any other borrow
 * is unadaptable and keeps the direct shim (plus a warning diagnostic).
 */
function adaptRustReturnType(returnText: string): AdaptedRustReturn {
  const raw = returnText.trim();
  if (raw.length === 0) return { kind: "none", ownedType: "", mapExpr: "" };
  const t = stripRustLifetimes(raw);
  if (!t.includes("&")) return { kind: "passthrough", ownedType: raw, mapExpr: "" };
  if (/^&\s*str$/.test(t)) {
    return { kind: "mapped", ownedType: "String", mapExpr: ".to_string()" };
  }
  if (/^Vec\s*<\s*&\s*str\s*>$/.test(t)) {
    return {
      kind: "mapped",
      ownedType: "Vec<String>",
      mapExpr: ".into_iter().map(|v| v.to_string()).collect()",
    };
  }
  if (/^Option\s*<\s*&\s*str\s*>$/.test(t)) {
    return { kind: "mapped", ownedType: "Option<String>", mapExpr: ".map(|v| v.to_string())" };
  }
  return { kind: "unadaptable", ownedType: raw, mapExpr: "" };
}
