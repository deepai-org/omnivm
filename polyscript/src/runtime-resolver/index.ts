import * as AST from '../ast';
import {
  OmniRuntime,
  RuntimeAffinity,
  BridgeDescriptor,
  AnnotatedNode,
  AnnotatedProgram,
} from './types';
import { SymbolTable } from './symbol-table';
import { Pass1Structural } from './pass1-structural';
import { Pass2Propagation } from './pass2-propagation';
import { optimizeBridges } from './cost-model';

export { OmniRuntime, RuntimeAffinity, BridgeDescriptor, AnnotatedNode, AnnotatedProgram, MarshalKind } from './types';
export { SymbolTable } from './symbol-table';
export { lookupMethodAffinity, lookupBuiltinAffinity } from './method-tables';
export { analyzeImportPath, analyzeBareImport } from './import-analyzer';
export { computeBridgeCost, totalBridgeCost, majorityRuntime } from './cost-model';
export * from './evidence';

/**
 * RuntimeResolver: determines which OmniVM runtime should execute each AST node.
 *
 * Two-pass algorithm:
 * - Pass 1 (top-down): Assigns definite runtimes to unambiguous nodes
 * - Pass 2 (bottom-up): Propagates affinity through expressions, inserts bridges
 *
 * Fallback chain for ambiguous nodes:
 * 1. Scope affinity (enclosing function/block)
 * 2. File-level `// @runtime` directive
 * 3. Import evidence
 * 4. JavaScript (global default)
 */
export class RuntimeResolver {
  private defaultRuntime: OmniRuntime;

  constructor(defaultRuntime: OmniRuntime = OmniRuntime.JavaScript) {
    this.defaultRuntime = defaultRuntime;
  }

  /**
   * Resolve runtime affinities for an entire program.
   */
  resolve(program: AST.Program, source?: string): AnnotatedProgram {
    const symbolTable = new SymbolTable();
    const fileDirective = program.runtimeDirective;

    // Pass 1: structural analysis
    const pass1 = new Pass1Structural(symbolTable, fileDirective, source);
    const affinityMap = pass1.run(program);

    // Pass 2: propagation
    const pass2 = new Pass2Propagation(
      affinityMap,
      pass1.getSymbolTable(),
      fileDirective ? this.parseRuntime(fileDirective) || this.defaultRuntime : this.defaultRuntime,
    );
    const bridges = pass2.run(program);

    // Bridge optimization: reroute fallback nodes to majority runtime
    optimizeBridges(pass2.getAffinityMap(), program.body);

    // Build annotated tree
    const root = this.buildAnnotatedTree(program, pass2.getAffinityMap());

    return {
      program,
      root,
      affinityMap: pass2.getAffinityMap(),
      bridges,
      defaultRuntime: fileDirective ? this.parseRuntime(fileDirective) || this.defaultRuntime : this.defaultRuntime,
      ...(source !== undefined ? { source } : {}),
    };
  }

  /**
   * Get the resolved runtime for a specific node.
   */
  getRuntimeForNode(
    annotatedProgram: AnnotatedProgram,
    node: AST.Decl | AST.Stmt | AST.Expr,
  ): OmniRuntime {
    const affinity = annotatedProgram.affinityMap.get(node);
    return affinity?.runtime || annotatedProgram.defaultRuntime;
  }

  /**
   * Explain why a node resolved to its runtime. This is intended for compiler
   * diagnostics and developer tooling, not for driving resolution decisions.
   */
  explainRuntimeForNode(
    annotatedProgram: AnnotatedProgram,
    node: AST.Decl | AST.Stmt | AST.Expr,
  ): string {
    const affinity = annotatedProgram.affinityMap.get(node);
    if (!affinity) {
      return `selected ${annotatedProgram.defaultRuntime} (fallback; no resolver evidence recorded)`;
    }
    const evidence = affinity.evidence.length > 0
      ? affinity.evidence.map(e => `${e.type}: ${e.detail}`).join("; ")
      : "no evidence";
    return `selected ${affinity.runtime} (${affinity.confidence}; ${evidence})`;
  }

  private buildAnnotatedTree(
    program: AST.Program,
    affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>,
  ): AnnotatedNode<AST.Program> {
    const children = program.body.map(node => this.buildAnnotatedNode(node, affinityMap));

    return {
      node: program,
      affinity: {
        runtime: this.defaultRuntime,
        confidence: "fallback",
        evidence: [{ type: "fallback", detail: "program root" }],
      },
      children,
    };
  }

  private buildAnnotatedNode(
    node: AST.Decl | AST.Stmt | AST.Expr,
    affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>,
  ): AnnotatedNode {
    const affinity = affinityMap.get(node) || {
      runtime: this.defaultRuntime,
      confidence: "fallback" as const,
      evidence: [{ type: "fallback" as const, detail: "unresolved" }],
    };

    const children: AnnotatedNode[] = [];

    // Recursively build children based on node type
    this.collectChildren(node).forEach(child => {
      children.push(this.buildAnnotatedNode(child, affinityMap));
    });

    return { node, affinity, children };
  }

  private collectChildren(node: AST.Decl | AST.Stmt | AST.Expr): (AST.Decl | AST.Stmt | AST.Expr)[] {
    const children: (AST.Decl | AST.Stmt | AST.Expr)[] = [];

    switch (node.kind) {
      case "FuncDecl":
        children.push(...node.body.statements);
        break;
      case "ExprStmt":
        children.push(node.expr);
        break;
      case "If":
        for (const arm of node.arms) {
          children.push(arm.test);
          children.push(...arm.body.statements);
        }
        if (node.elseBody) children.push(...node.elseBody.statements);
        break;
      case "Loop":
        if (node.test) children.push(node.test);
        if (node.iterable) children.push(node.iterable);
        children.push(...node.body.statements);
        break;
      case "Switch":
        children.push(node.discriminant);
        for (const c of node.cases) {
          children.push(...c.patterns);
          if (c.guard) children.push(c.guard);
          children.push(...c.body.statements);
        }
        if (node.defaultCase) children.push(...node.defaultCase.statements);
        break;
      case "Match":
        children.push(node.expr);
        for (const arm of node.arms) {
          children.push(...arm.patterns);
          if (arm.guard) children.push(arm.guard);
          if ('kind' in arm.body && arm.body.kind === "Block") {
            children.push(...(arm.body as AST.Block).statements);
          } else {
            children.push(arm.body as AST.Expr);
          }
        }
        break;
      case "Call":
        children.push(node.callee);
        children.push(...node.args);
        break;
      case "NewExpr":
        children.push(node.callee);
        children.push(...node.args);
        break;
      case "Binary":
        children.push(node.left);
        children.push(node.right);
        break;
      case "Unary":
        children.push(node.argument);
        break;
      case "Member":
        children.push(node.object);
        break;
      case "Index":
        children.push(node.object);
        children.push(node.index);
        break;
      case "Assign":
        children.push(node.left);
        children.push(node.right);
        break;
      case "Ternary":
        children.push(node.test);
        children.push(node.consequent);
        children.push(node.alternate);
        break;
      case "ArrayLiteral":
        children.push(...node.elements);
        break;
      case "ObjectLiteral":
        for (const prop of node.properties) children.push(prop.value);
        break;
      case "Return":
        children.push(...node.values);
        break;
      case "Throw":
        children.push(node.value);
        if (node.cause) children.push(node.cause);
        break;
      case "Try":
        children.push(...node.body.statements);
        for (const c of node.catches) children.push(...c.body.statements);
        if (node.finallyBody) children.push(...node.finallyBody.statements);
        break;
      case "Block":
        children.push(...node.statements);
        break;
      case "Go":
        children.push(node.expr);
        break;
      case "Defer":
        if (node.body && 'kind' in node.body && node.body.kind === "Block") {
          children.push(...(node.body as AST.Block).statements);
        } else if (node.body) {
          children.push(node.body as AST.Expr);
        }
        break;
      case "RuntimeTag":
        children.push(node.expr);
        break;
      case "Spread":
        children.push(node.argument);
        break;
      case "Yield":
        if (node.value) children.push(node.value);
        break;
      case "Lambda":
        if ('kind' in node.body && (node.body as any).kind === "Block") {
          children.push(...(node.body as AST.Block).statements);
        } else {
          children.push(node.body as AST.Expr);
        }
        break;
      case "ListComprehension":
        children.push(node.expression);
        children.push(node.iterable);
        if (node.filter) children.push(node.filter);
        break;
      case "TypeAssertion":
        children.push(node.expr);
        break;
      case "ExportDecl":
        if (node.declaration) children.push(node.declaration);
        break;
      case "ClassDecl":
        for (const member of node.members) {
          if (member.body) children.push(...member.body.statements);
        }
        break;
      case "ImplDecl":
        for (const member of node.members) {
          if (member.body) children.push(...member.body.statements);
        }
        break;
      case "VarDecl":
        if (node.values) children.push(...node.values);
        break;
      case "ConstDecl":
        children.push(...node.values);
        break;
      case "ShortDecl":
        if (node.value) children.push(node.value);
        if (node.pairs) {
          for (const pair of node.pairs) children.push(pair.expr);
        }
        break;
      case "Select":
        for (const c of node.cases) children.push(...c.body.statements);
        if (node.defaultCase) children.push(...node.defaultCase.statements);
        break;
      // Leaf nodes: no children
      default:
        break;
    }

    return children;
  }

  private parseRuntime(name: string): OmniRuntime | undefined {
    const normalized = name.toLowerCase().trim();
    switch (normalized) {
      case "python": case "py": return OmniRuntime.Python;
      case "javascript": case "js": return OmniRuntime.JavaScript;
      case "go": case "golang": return OmniRuntime.Go;
      case "ruby": case "rb": return OmniRuntime.Ruby;
      case "java": return OmniRuntime.Java;
      case "rust": case "rs": return OmniRuntime.Rust;
      case "c": return OmniRuntime.C;
      default: return undefined;
    }
  }
}
