import * as AST from '../ast';
import {
  OmniRuntime,
  RuntimeAffinity,
  AnnotatedProgram,
} from '../runtime-resolver/types';
import { Emitter } from './emitter';
import { BridgeEmitter } from './bridge-emitter';
import { StringInterpolationEmitter } from './string-interpolation';
import { AsyncOrchestrator, AsyncOperation } from './async-orchestrator';
import { consolidateBlocks, isConsolidatable, isCompiledRuntime, RuntimeBlock } from './runtime-blocks';
import {
  exprToCode,
  stringLiteralToCode,
  nodeToSourceCode,
  paramToCode,
  importSpecsToCode,
  escapeTemplate,
  tagToRuntime,
  isExprKind,
} from './source-reconstruct';

export { Emitter } from './emitter';
export { BridgeEmitter } from './bridge-emitter';
export { StringInterpolationEmitter } from './string-interpolation';
export { AsyncOrchestrator } from './async-orchestrator';
export { consolidateBlocks, isConsolidatable, isCompiledRuntime } from './runtime-blocks';
export { ManifestCodeGenerator } from './manifest-generator';
export * from './manifest-types';
export * from './manifest-schema';
export * from './lowering';
export * from './lowering-ir';

/**
 * OmniVMCodeGenerator: generates JavaScript dispatch code that calls
 * omnivm.call() / omnivm.callAsync() for cross-runtime execution.
 *
 * Generated code is always JavaScript — V8 is the orchestrator.
 * Non-JS code is dispatched to its target runtime via OmniVM bridge calls.
 */
export class OmniVMCodeGenerator {
  private emitter: Emitter;
  private bridgeEmitter: BridgeEmitter;
  private asyncOrchestrator: AsyncOrchestrator;
  private affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity> = new Map();
  private defaultRuntime: OmniRuntime = OmniRuntime.JavaScript;
  private source?: string;

  constructor() {
    this.emitter = new Emitter();
    this.bridgeEmitter = new BridgeEmitter(this.emitter);
    this.asyncOrchestrator = new AsyncOrchestrator(this.emitter, this.bridgeEmitter);
  }

  /**
   * Generate dispatch code from an annotated program.
   */
  generate(annotated: AnnotatedProgram): string {
    this.emitter.reset();
    this.bridgeEmitter.resetTempCounter();
    this.affinityMap = annotated.affinityMap;
    this.defaultRuntime = annotated.defaultRuntime;
    this.source = annotated.source;

    // Consolidate top-level blocks
    const blocks = consolidateBlocks(annotated.program.body, this.affinityMap);

    for (const block of blocks) {
      this.emitBlock(block);
    }

    return this.emitter.toString();
  }

  private emitBlock(block: RuntimeBlock): void {
    if (block.runtime === OmniRuntime.JavaScript) {
      // JS nodes — emit directly
      for (const node of block.nodes) {
        this.emitNode(node);
      }
    } else if (isCompiledRuntime(block.runtime)) {
      // Compiled runtimes (Rust, C) — emit as FFI call placeholder
      this.emitCompiledBlock(block);
    } else if (isConsolidatable(block)) {
      // Multiple same-runtime nodes — consolidate into single omnivm.call()
      this.emitConsolidatedBlock(block);
    } else {
      // Single non-JS node — emit as bridge call
      for (const node of block.nodes) {
        this.emitBridgedNode(node, block.runtime);
      }
    }
  }

  private emitConsolidatedBlock(block: RuntimeBlock): void {
    // Reconstruct source code for the entire block
    const codeSegments: string[] = [];
    for (const node of block.nodes) {
      codeSegments.push(this.nodeToSourceCode(node));
    }
    const code = codeSegments.join("\n");
    const call = this.bridgeEmitter.emitCall(block.runtime, code);
    this.emitter.emitLine(`${call};`);
  }

  private emitCompiledBlock(block: RuntimeBlock): void {
    // Compiled runtimes are loaded as shared libraries via FFI
    const lang = block.runtime === OmniRuntime.Rust ? "rust" : "c";
    this.emitter.emitLine(`// Compiled ${lang} block — requires gcc/rustc compilation`);
    for (const node of block.nodes) {
      const code = this.nodeToSourceCode(node);
      this.emitter.emitLine(`omnivm.callCompiled("${lang}", \`${this.escapeTemplate(code)}\`);`);
    }
  }

  private emitBridgedNode(node: AST.Decl | AST.Stmt | AST.Expr, runtime: OmniRuntime): void {
    const code = this.nodeToSourceCode(node);
    const call = this.bridgeEmitter.emitCall(runtime, code);
    this.emitter.emitLine(`${call};`);
  }

  private emitNode(node: AST.Decl | AST.Stmt | AST.Expr): void {
    switch (node.kind) {
      case "FuncDecl":
        this.emitFuncDecl(node);
        break;
      case "ExprStmt":
        this.emitExprStmt(node);
        break;
      case "VarDecl":
        this.emitVarDecl(node);
        break;
      case "ConstDecl":
        this.emitConstDecl(node);
        break;
      case "Return":
        this.emitReturn(node);
        break;
      case "If":
        this.emitIf(node);
        break;
      case "Loop":
        this.emitLoop(node);
        break;
      case "ExportDecl":
        if (node.declaration) this.emitNode(node.declaration);
        break;
      case "Import":
      case "ImportDecl":
        this.emitImport(node);
        break;
      case "GroupedImport":
        for (const imported of node.imports) {
          this.emitImport(imported);
        }
        break;
      default:
        // For other node types, emit as-is or as bridge call
        const aff = this.affinityMap.get(node);
        if (aff && aff.runtime !== OmniRuntime.JavaScript) {
          this.emitBridgedNode(node, aff.runtime);
        } else {
          this.emitter.emitLine(this.nodeToSourceCode(node) + ";");
        }
        break;
    }
  }

  private emitFuncDecl(node: AST.FuncDecl): void {
    const aff = this.affinityMap.get(node);
    const runtime = aff?.runtime || OmniRuntime.JavaScript;

    if (runtime === OmniRuntime.JavaScript) {
      // Emit as native JS function
      const asyncStr = node.async ? "async " : "";
      const genStr = node.generator ? "* " : "";
      const params = node.params.map(p => this.paramToCode(p)).join(", ");

      this.emitter.emitLine(`${asyncStr}function ${genStr}${node.name.name}(${params}) {`);
      this.emitter.push();

      // Emit body with block consolidation
      const bodyBlocks = consolidateBlocks(node.body.statements, this.affinityMap);
      for (const block of bodyBlocks) {
        this.emitBlock(block);
      }

      this.emitter.pop();
      this.emitter.emitLine("}");
    } else {
      // Non-JS function — wrap in a JS function that calls omnivm
      const params = node.params.map(p => this.paramToCode(p)).join(", ");
      const bodyCode = this.nodeToSourceCode(node);
      const call = this.bridgeEmitter.emitCall(runtime, bodyCode);

      this.emitter.emitLine(`function ${node.name.name}(${params}) {`);
      this.emitter.push();
      this.emitter.emitLine(`return ${call};`);
      this.emitter.pop();
      this.emitter.emitLine("}");
    }
  }

  private emitExprStmt(node: AST.ExprStmt): void {
    const expr = node.expr;
    const code = this.emitExpr(expr);
    this.emitter.emitLine(`${code};`);
  }

  private emitExpr(expr: AST.Expr): string {
    const aff = this.affinityMap.get(expr);
    const runtime = aff?.runtime || OmniRuntime.JavaScript;

    // RuntimeTag — explicit runtime override
    if (expr.kind === "RuntimeTag") {
      const innerCode = this.emitExpr(expr.expr);
      const tagRuntime = this.tagToRuntime(expr.runtime);
      if (tagRuntime === OmniRuntime.JavaScript) {
        return innerCode;
      }
      return this.bridgeEmitter.emitCall(tagRuntime, this.exprToCode(expr.expr));
    }

    // String literal with cross-runtime interpolation
    if (expr.kind === "StringLiteral" && expr.parts.some(p => p.kind === "Interpolation")) {
      const stringEmitter = new StringInterpolationEmitter(
        this.bridgeEmitter,
        this.affinityMap,
        runtime,
      );
      return stringEmitter.emit(expr);
    }

    // If this expression is non-JS and we're in a JS context,
    // wrap it in a bridge call
    if (runtime !== OmniRuntime.JavaScript) {
      const code = this.exprToCode(expr);
      if (aff?.async) {
        return `await ${this.bridgeEmitter.emitCallAsync(runtime, code)}`;
      }
      return this.bridgeEmitter.emitCall(runtime, code);
    }

    // JS expression — emit natively
    return this.exprToCode(expr);
  }

  private emitVarDecl(node: AST.VarDecl): void {
    for (let i = 0; i < node.names.length; i++) {
      const name = node.names[i].name;
      if (node.values && node.values[i]) {
        const value = this.emitExpr(node.values[i]);
        this.emitter.emitLine(`let ${name} = ${value};`);
      } else {
        this.emitter.emitLine(`let ${name};`);
      }
    }
  }

  private emitConstDecl(node: AST.ConstDecl): void {
    for (let i = 0; i < node.names.length; i++) {
      const name = node.names[i].name;
      const value = this.emitExpr(node.values[i]);
      this.emitter.emitLine(`const ${name} = ${value};`);
    }
  }

  private emitReturn(node: AST.Return): void {
    if (node.values.length === 0) {
      this.emitter.emitLine("return;");
    } else if (node.values.length === 1) {
      const value = this.emitExpr(node.values[0]);
      this.emitter.emitLine(`return ${value};`);
    } else {
      const values = node.values.map(v => this.emitExpr(v)).join(", ");
      this.emitter.emitLine(`return [${values}];`);
    }
  }

  private emitIf(node: AST.If): void {
    for (let i = 0; i < node.arms.length; i++) {
      const arm = node.arms[i];
      const test = this.emitExpr(arm.test);
      const keyword = i === 0 ? "if" : "} else if";
      this.emitter.emitLine(`${keyword} (${test}) {`);
      this.emitter.push();
      const bodyBlocks = consolidateBlocks(arm.body.statements, this.affinityMap);
      for (const block of bodyBlocks) {
        this.emitBlock(block);
      }
      this.emitter.pop();
    }
    if (node.elseBody) {
      this.emitter.emitLine("} else {");
      this.emitter.push();
      const bodyBlocks = consolidateBlocks(node.elseBody.statements, this.affinityMap);
      for (const block of bodyBlocks) {
        this.emitBlock(block);
      }
      this.emitter.pop();
    }
    this.emitter.emitLine("}");
  }

  private emitLoop(node: AST.Loop): void {
    switch (node.mode) {
      case "while":
        this.emitter.emitLine(`while (${node.test ? this.emitExpr(node.test) : "true"}) {`);
        break;
      case "for":
        this.emitter.emitLine("for (;;) {");
        break;
      case "infinite":
        this.emitter.emitLine("while (true) {");
        break;
      default:
        this.emitter.emitLine("while (true) {");
        break;
    }
    this.emitter.push();
    const bodyBlocks = consolidateBlocks(node.body.statements, this.affinityMap);
    for (const block of bodyBlocks) {
      this.emitBlock(block);
    }
    this.emitter.pop();
    this.emitter.emitLine("}");
  }

  private emitImport(node: AST.Import | AST.ImportDecl): void {
    const aff = this.affinityMap.get(node);
    if (aff && aff.runtime !== OmniRuntime.JavaScript) {
      // Non-JS import — emit as bridge setup
      if (node.kind === "Import") {
        const alias = node.alias?.name || node.path;
        this.emitter.emitLine(
          `const ${alias} = ${this.bridgeEmitter.emitCall(aff.runtime, `import ${node.path}`)};`
        );
      }
    } else {
      // JS import — emit as-is
      if (node.kind === "ImportDecl") {
        this.emitter.emitLine(`import ${this.importSpecsToCode(node)} from "${node.path}";`);
      } else {
        if (node.alias) {
          this.emitter.emitLine(`const ${node.alias.name} = require("${node.path}");`);
        } else {
          this.emitter.emitLine(`require("${node.path}");`);
        }
      }
    }
  }

  // Source code reconstruction delegated to shared module (source-reconstruct.ts)
  // The following private methods are thin wrappers for backward compatibility
  // within this class's existing method calls.

  private exprToCode(expr: AST.Expr): string {
    return exprToCode(expr, this.source);
  }

  private nodeToSourceCode(node: AST.Decl | AST.Stmt | AST.Expr): string {
    return nodeToSourceCode(node, this.source);
  }

  private paramToCode(param: AST.Param): string {
    return paramToCode(param, this.source);
  }

  private importSpecsToCode(node: AST.ImportDecl): string {
    return importSpecsToCode(node);
  }

  private escapeTemplate(code: string): string {
    return escapeTemplate(code);
  }

  private tagToRuntime(tag: string): OmniRuntime {
    return tagToRuntime(tag);
  }
}
