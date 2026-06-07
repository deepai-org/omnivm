import * as AST from '../ast';
import {
  OmniRuntime,
  RuntimeAffinity,
  AffinityEvidence,
  AnnotatedNode,
  SymbolEntry,
} from './types';
import { SymbolTable } from './symbol-table';
import { analyzeImportPath, analyzeBareImport } from './import-analyzer';
import { lookupBuiltinAffinity, lookupGlobalAffinity, lookupQualifiedGlobalAffinity } from './method-tables';
import { affinityFromEvidence, chooseRuntime, EVIDENCE_WEIGHTS } from './evidence';
import { inferCatchClauseAffinity } from './catch-affinity';

/**
 * Pass 1: Top-down structural analysis.
 *
 * Walks the AST and assigns definite or inferred runtime affinities
 * to nodes based on their structure, keywords, and imports.
 * Also populates the symbol table with variable affinities.
 */
export class Pass1Structural {
  private affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity> = new Map();
  private symbolTable: SymbolTable;
  private fileDirective?: OmniRuntime;
  private scopeStack: OmniRuntime[] = [];
  private source?: string;
  private importBindingNodes = new Map<string, AST.Import | AST.ImportDecl>();
  private importedJavaQualifiedClasses = new Set<string>();

  constructor(
    symbolTable: SymbolTable,
    fileDirective?: string,
    source?: string,
  ) {
    this.symbolTable = symbolTable;
    this.source = source;
    if (fileDirective) {
      this.fileDirective = this.parseRuntimeName(fileDirective);
    }
  }

  /**
   * Run Pass 1 on the entire program.
   */
  run(program: AST.Program): Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity> {
    for (const node of program.body) {
      this.visitNode(node);
    }
    return this.affinityMap;
  }

  /**
   * Get the symbol table (for use by Pass 2).
   */
  getSymbolTable(): SymbolTable {
    return this.symbolTable;
  }

  private visitNode(node: AST.Decl | AST.Stmt | AST.Expr): void {
    switch (node.kind) {
      // --- Definite Python ---
      case "ListComprehension":
        this.assign(node, OmniRuntime.Python, "definite", { type: "syntax", detail: "ListComprehension" });
        this.visitExpr(node.expression);
        this.visitExpr(node.iterable);
        if (node.filter) this.visitExpr(node.filter);
        break;

      case "Pass":
        this.assign(node, OmniRuntime.Python, "definite", { type: "node_type", detail: "Pass" });
        break;

      // --- Definite JavaScript ---
      case "JSXElement":
        this.assign(node, OmniRuntime.JavaScript, "definite", { type: "node_type", detail: "JSXElement" });
        this.visitJSXChildren(node.children);
        break;

      case "JSXFragment":
        this.assign(node, OmniRuntime.JavaScript, "definite", { type: "node_type", detail: "JSXFragment" });
        this.visitJSXChildren(node.children);
        break;

      // --- Definite Go ---
      case "Go":
        this.assign(node, OmniRuntime.Go, "definite", { type: "node_type", detail: "Go" });
        this.visitExpr(node.expr);
        break;

      case "Defer":
        this.assign(node, OmniRuntime.Go, "definite", { type: "node_type", detail: "Defer" });
        if (node.body && 'kind' in node.body && node.body.kind === "Block") {
          this.visitBlock(node.body as AST.Block);
        } else if (node.body) {
          this.visitExpr(node.body as AST.Expr);
        }
        break;

      case "Select":
        this.assign(node, OmniRuntime.Go, "definite", { type: "node_type", detail: "Select" });
        for (const c of node.cases) {
          this.visitBlock(c.body);
        }
        if (node.defaultCase) this.visitBlock(node.defaultCase);
        break;

      case "ShortDecl":
        this.assign(node, OmniRuntime.Go, "definite", { type: "node_type", detail: "ShortDecl (:=)" });
        if (node.value) this.visitExpr(node.value);
        // Register variables in symbol table
        if (node.targets) {
          for (const target of node.targets) {
            if (target.kind === "Identifier") {
              this.symbolTable.define(target.name, {
                name: target.name,
                affinity: this.getAffinity(node)!,
              });
            }
          }
        }
        if (node.pairs) {
          for (const pair of node.pairs) {
            this.symbolTable.define(pair.name.name, {
              name: pair.name.name,
              affinity: this.getAffinity(node)!,
            });
            this.visitExpr(pair.expr);
          }
        }
        break;

      // --- Definite Rust ---
      case "ImplDecl":
        this.assign(node, OmniRuntime.Rust, "definite", { type: "node_type", detail: "ImplDecl" });
        this.scopeStack.push(OmniRuntime.Rust);
        this.symbolTable.pushScope();
        for (const member of node.members) {
          if (member.body) this.visitBlock(member.body);
        }
        this.symbolTable.popScope();
        this.scopeStack.pop();
        break;

      // --- Runtime-tagged expressions ---
      case "RuntimeTag":
        const taggedRuntime = this.parseRuntimeName(node.runtime);
        if (taggedRuntime) {
          const aff = affinityFromEvidence(chooseRuntime([{
            runtime: taggedRuntime,
            source: "runtime_tag",
            weight: EVIDENCE_WEIGHTS.explicit,
            detail: `@${node.runtime}()`,
          }], this.fileDirective || OmniRuntime.JavaScript));
          this.assign(node, aff.runtime, aff.confidence, ...aff.evidence);
        }
        this.visitExpr(node.expr);
        break;

      // --- Function declarations (inferred from keyword) ---
      case "FuncDecl":
        this.visitFuncDecl(node);
        break;

      // --- Match (style hint) ---
      case "Match":
        this.visitMatch(node);
        break;

      // --- Imports ---
      case "Import":
        this.visitImport(node);
        break;

      case "GroupedImport":
        for (const imported of node.imports) {
          if (imported.kind === "Import") this.visitImport(imported);
          else this.visitImportDecl(imported);
        }
        break;

      case "ImportDecl":
        this.visitImportDecl(node);
        break;

      // --- Calls (builtin detection) ---
      case "Call":
        this.visitCall(node);
        break;

      case "NewExpr":
        this.visitNewExpr(node);
        break;

      case "ExprStmt":
        {
          const rawExpr = this.nodeSource(node.expr)?.trim();
          if (rawExpr && this.isRubyDoBlockSource(rawExpr)) {
            this.assign(node.expr, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby do/end block expression" });
            this.assign(node, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby do/end block expression" });
          } else if (rawExpr && this.isRubyStabbyLambdaSource(rawExpr)) {
            this.assign(node.expr, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby stabby lambda ->" });
            this.assign(node, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby stabby lambda ->" });
          } else if (rawExpr && (this.isRubyLabelSource(rawExpr) || this.isRubySymbolCommandSource(rawExpr))) {
            this.assign(node.expr, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby command argument syntax" });
            this.assign(node, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby command argument syntax" });
          }
        }
        this.visitExpr(node.expr);
        break;

      case "Echo":
        for (const v of node.values) this.visitExpr(v);
        // f-strings are Python syntax
        if (node.values.some((v: any) => v.kind === "StringLiteral" && v.flags?.format)) {
          this.assign(node, OmniRuntime.Python, "definite", { type: "syntax", detail: "f-string in print()" });
        }
        break;

      // --- Variable declarations ---
      case "VarDecl":
        this.visitVarDecl(node);
        break;

      case "ConstDecl":
        this.visitConstDecl(node);
        break;

      // --- Control flow (recurse into bodies) ---
      case "If":
        for (const arm of node.arms) {
          this.visitExpr(arm.test);
          this.visitBlock(arm.body);
        }
        if (node.elseBody) this.visitBlock(node.elseBody);
        break;

      case "Loop":
        {
          const rawLoop = this.nodeSource(node)?.trim();
          if (rawLoop && this.isPythonForLoopSource(rawLoop)) {
            this.assign(node, OmniRuntime.Python, "definite", { type: "syntax", detail: "Python for-in loop syntax" });
          } else if (rawLoop && this.isJavaScriptForEachLoopSource(rawLoop)) {
            this.assign(node, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "JavaScript for-of/in loop syntax" });
          }
        }
        if (node.test) this.visitExpr(node.test);
        if (node.iterable) this.visitExpr(node.iterable);
        this.visitBlock(node.body);
        break;

      case "Switch":
        this.visitExpr(node.discriminant);
        for (const c of node.cases) {
          for (const p of c.patterns) this.visitExpr(p);
          if (c.guard) this.visitExpr(c.guard);
          this.visitBlock(c.body);
        }
        if (node.defaultCase) this.visitBlock(node.defaultCase);
        break;

      case "Try":
        this.visitBlock(node.body);
        for (const c of node.catches) this.visitCatchClause(c);
        if (node.finallyBody) this.visitBlock(node.finallyBody);
        break;

      case "Using":
        this.visitNode(node.resource);
        this.visitBlock(node.body);
        break;

      case "Return":
        for (const v of node.values) this.visitExpr(v);
        break;

      case "Throw":
        this.visitExpr(node.value);
        if (node.cause) this.visitExpr(node.cause);
        break;

      case "Block":
        this.visitBlock(node);
        break;

      case "ClassDecl":
        this.visitClassDecl(node);
        break;

      case "ExportDecl":
        if (node.declaration) this.visitNode(node.declaration);
        break;

      // --- Expression types (recurse) ---
      default:
        this.visitExprGeneric(node as AST.Expr);
        break;
    }
  }

  private visitFuncDecl(node: AST.FuncDecl): void {
    let runtime: OmniRuntime | undefined;
    let confidence: "definite" | "inferred" = "inferred";
    const evidence: AffinityEvidence = { type: "keyword", detail: `declKeyword: ${node.declKeyword}` };

    switch (node.declKeyword) {
      case "func":
        runtime = OmniRuntime.Go;
        break;
      case "def":
        // Could be Python or Ruby — default to Python, Pass 2 may refine
        runtime = OmniRuntime.Python;
        break;
      case "function":
        runtime = OmniRuntime.JavaScript;
        break;
      case "fn":
        runtime = OmniRuntime.Rust;
        break;
      case "fun":
        // Kotlin-style, map to Java for now
        runtime = OmniRuntime.Java;
        break;
    }

    if (runtime) {
      this.assign(node, runtime, confidence, evidence);
      this.symbolTable.define(node.name.name, {
        name: node.name.name,
        affinity: this.getAffinity(node)!,
        declNode: node,
      });
    }

    for (const decorator of node.decorators || []) {
      if (decorator.expression) {
        this.visitExprGeneric(decorator.expression);
      } else {
        this.visitExprGeneric(decorator.name);
        for (const arg of decorator.args || []) this.visitExprGeneric(arg);
      }
    }

    // Visit body with scope
    const scopeRuntime = runtime || this.currentScopeRuntime();
    if (scopeRuntime) {
      this.scopeStack.push(scopeRuntime);
    }
    this.symbolTable.pushScope();

    // Register params
    for (const param of node.params) {
      if (param.name.kind === "Identifier") {
        this.symbolTable.define(param.name.name, {
          name: param.name.name,
          affinity: runtime
            ? { runtime, confidence: "inferred", evidence: [{ type: "scope", detail: "param in function" }] }
            : this.defaultAffinity(),
        });
      }
    }

    this.visitBlock(node.body);
    this.symbolTable.popScope();
    if (scopeRuntime) {
      this.scopeStack.pop();
    }
  }

  private visitMatch(node: AST.Match): void {
    if (node.style === "rust") {
      this.assign(node, OmniRuntime.Rust, "inferred", { type: "keyword", detail: "match style: rust" });
    } else if (node.style === "python") {
      this.assign(node, OmniRuntime.Python, "inferred", { type: "keyword", detail: "match style: python" });
    }

    this.visitExpr(node.expr);
    for (const arm of node.arms) {
      for (const pattern of arm.patterns) this.visitExpr(pattern);
      if (arm.guard) this.visitExpr(arm.guard);
      if ('kind' in arm.body && arm.body.kind === "Block") {
        this.visitBlock(arm.body as AST.Block);
      } else {
        this.visitExpr(arm.body as AST.Expr);
      }
    }
  }

  private visitImport(node: AST.Import): void {
    const raw = this.nodeSource(node);
    const path = node.path.replace(/['"]/g, "");
    const quotedImport = raw ? /^\s*import\s*["']/.test(raw) : node.path.startsWith('"') || node.path.startsWith("'");
    const analyzedAffinity = quotedImport
      ? analyzeImportPath(path, { preferredRuntime: OmniRuntime.Go }) || analyzeBareImport(path)
      : analyzeBareImport(node.path) || analyzeImportPath(node.path);
    const rubyRequireImport = raw ? /^\s*require\s+["']/.test(raw) : false;
    const javaStaticImport = !quotedImport && this.isJavaStaticImportPath(path);
    const javaClassImport = !quotedImport && this.isJavaClassImportPath(path);
    const javaWildcardImport = !quotedImport && this.isJavaWildcardImport(raw, path);
    const quotedSyntaxAffinity = quotedImport && !analyzedAffinity
      ? this.quotedImportSyntaxAffinity(path)
      : undefined;
    const pythonSyntaxImport = raw
      ? /^\s*import\s+[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*(?:\s+as\s+[A-Za-z_][\w]*)?\s*;?\s*$/.test(raw)
      : !quotedImport && /^[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*$/.test(node.path);
    const affinity = analyzedAffinity || (rubyRequireImport
      ? {
          runtime: OmniRuntime.Ruby,
          confidence: "definite" as const,
          evidence: [{ type: "syntax" as const, detail: "Ruby require syntax" }],
        }
      : undefined) || (javaStaticImport || javaClassImport || javaWildcardImport
      ? {
          runtime: OmniRuntime.Java,
          confidence: "definite" as const,
          evidence: [{
            type: "syntax" as const,
            detail: javaStaticImport
              ? "Java static import syntax"
              : javaWildcardImport
                ? "Java wildcard import syntax"
                : "Java class import syntax",
          }],
        }
      : undefined) || (pythonSyntaxImport
      ? {
          runtime: OmniRuntime.Python,
          confidence: "definite" as const,
          evidence: [{ type: "syntax" as const, detail: "Python import syntax" }],
        }
      : undefined) || quotedSyntaxAffinity;

    const bindingNames = affinity
      ? this.importBindingNames(node.path, affinity.runtime, node.alias?.name)
      : node.alias?.name ? [node.alias.name] : [];
    this.registerImportBindingNodes(bindingNames, node);

    if (affinity) {
      const aff = affinityFromEvidence(chooseRuntime([{
        runtime: affinity.runtime,
        source: analyzedAffinity ? "import" : "syntax",
        weight: analyzedAffinity ? EVIDENCE_WEIGHTS.import : EVIDENCE_WEIGHTS.syntax,
        detail: affinity.evidence[0]?.detail || `import: ${node.path}`,
      }], this.fileDirective || OmniRuntime.JavaScript));
      this.assign(node, aff.runtime, aff.confidence, ...aff.evidence);
      if (affinity.runtime === OmniRuntime.Java && this.isJavaClassImportPath(node.path)) {
        this.importedJavaQualifiedClasses.add(node.path.replace(/['"]/g, ""));
      }
      // Register imported names. Java dotted imports bind the simple class name
      // (`import java.util.concurrent.CompletableFuture` -> `CompletableFuture`);
      // Python dotted imports bind the package root
      // (`import package.module` -> `package`).
      for (const name of bindingNames) {
        this.symbolTable.define(name, {
          name,
          affinity,
        });
      }
    }
  }

  private isJavaClassImportPath(path: string): boolean {
    const cleaned = path.replace(/['"]/g, "");
    if (!/^[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)+$/.test(cleaned)) return false;
    const last = cleaned.split(".").pop();
    return !!last && /^[A-Z_$]/.test(last);
  }

  private isJavaStaticImportPath(path: string): boolean {
    const cleaned = path.replace(/['"]/g, "");
    return /^static\s+[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)+(?:\.\*)?$/.test(cleaned);
  }

  private isJavaWildcardImport(raw: string | undefined, path: string): boolean {
    const rawText = raw?.trim();
    if (rawText) {
      return /^import\s+[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)+\.\*\s*;?$/.test(rawText);
    }
    const cleaned = path.replace(/['"]/g, "");
    return /^[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)+\.\*$/.test(cleaned);
  }

  private quotedImportSyntaxAffinity(path: string): RuntimeAffinity | undefined {
    const runtime = this.fileDirective === OmniRuntime.Go || this.isDomainLikeGoImportPath(path)
      ? OmniRuntime.Go
      : OmniRuntime.JavaScript;
    return {
      runtime,
      confidence: "inferred",
      evidence: [{
        type: "syntax",
        detail: runtime === OmniRuntime.Go ? "Go quoted import syntax" : "JavaScript side-effect import syntax",
      }],
    };
  }

  private isDomainLikeGoImportPath(path: string): boolean {
    return /^[A-Za-z0-9_.-]+\.[A-Za-z0-9_.-]+\//.test(path);
  }

  private visitImportDecl(node: AST.ImportDecl): void {
    this.registerImportDeclBindingNodes(node);
    const preferredRuntime = this.importDeclPreferredRuntime(node);
    const analyzedAffinity = preferredRuntime
      ? analyzeImportPath(node.path, { preferredRuntime }) || analyzeImportPath(node.path)
      : analyzeImportPath(node.path);
    const affinity = analyzedAffinity || (preferredRuntime === OmniRuntime.Python
      ? {
          runtime: OmniRuntime.Python,
          confidence: "definite" as const,
          evidence: [{ type: "syntax" as const, detail: "Python from-import syntax" }],
        }
      : preferredRuntime === OmniRuntime.JavaScript
        ? {
            runtime: OmniRuntime.JavaScript,
            confidence: "inferred" as const,
            evidence: [{ type: "syntax" as const, detail: "JavaScript import-from syntax" }],
          }
      : undefined);

    if (affinity) {
      const aff = affinityFromEvidence(chooseRuntime([{
        runtime: affinity.runtime,
        source: analyzedAffinity ? "import" : "syntax",
        weight: analyzedAffinity ? EVIDENCE_WEIGHTS.import : EVIDENCE_WEIGHTS.syntax,
        detail: affinity.evidence[0]?.detail || `import: ${node.path}`,
      }], this.fileDirective || OmniRuntime.JavaScript));
      this.assign(node, aff.runtime, aff.confidence, ...aff.evidence);
      // Register imported names
      if (node.defaultImport) {
        this.symbolTable.define(node.defaultImport.name, {
          name: node.defaultImport.name,
          affinity,
        });
      }
      if (node.namespaceImport) {
        this.symbolTable.define(node.namespaceImport.name, {
          name: node.namespaceImport.name,
          affinity,
        });
      }
      if (node.specifiers) {
        for (const spec of node.specifiers) {
          this.symbolTable.define(spec.local, {
            name: spec.local,
            affinity,
          });
        }
      }
    }
  }

  private registerImportDeclBindingNodes(node: AST.ImportDecl): void {
    if (node.defaultImport) this.importBindingNodes.set(node.defaultImport.name, node);
    if (node.namespaceImport) this.importBindingNodes.set(node.namespaceImport.name, node);
    if (node.specifiers) {
      for (const spec of node.specifiers) {
        this.importBindingNodes.set(spec.local, node);
      }
    }
  }

  private registerImportBindingNodes(names: string[], node: AST.Import | AST.ImportDecl): void {
    for (const name of names) {
      this.importBindingNodes.set(name, node);
    }
  }

  private importDeclPreferredRuntime(node: AST.ImportDecl): OmniRuntime | undefined {
    const raw = this.nodeSource(node)?.trim();
    if (!raw) return undefined;
    if (/\bfrom\s*["']/.test(raw) &&
        node.path.includes("/") &&
        analyzeImportPath(node.path, { preferredRuntime: OmniRuntime.Go })?.runtime === OmniRuntime.Go) {
      return OmniRuntime.Go;
    }
    if (/\bfrom\s*["']/.test(raw)) return OmniRuntime.JavaScript;
    if (raw.startsWith("from ")) return OmniRuntime.Python;
    return undefined;
  }

  private nodeSource(node: AST.Decl | AST.Stmt | AST.Expr): string | undefined {
    if (!this.source || !node.span || node.span.end <= node.span.start) return undefined;
    return this.source.slice(node.span.start, node.span.end);
  }

  private isRubyClassSource(raw: string): boolean {
    const firstLine = raw.split(/\r?\n/, 1)[0]?.trim() || "";
    return /^class\s+[A-Za-z_]\w*(?:::[A-Za-z_]\w*)?(?:\s*<\s*[A-Za-z_]\w*(?:::[A-Za-z_]\w*)*)?\s*$/.test(firstLine) &&
      /\bend\s*$/.test(raw);
  }

  private isRubyDoBlockSource(raw: string): boolean {
    return /\bdo(?:\s*\|[^|]*\|)?\s*(?:\r?\n|;)/.test(raw) && /\bend\s*$/.test(raw);
  }

  private isPythonForLoopSource(raw: string): boolean {
    const firstLine = raw.split(/\r?\n/, 1)[0]?.trim() || "";
    return /^(?:async\s+)?for\s+[^():]+\s+in\s+.+:\s*$/.test(firstLine);
  }

  private isJavaScriptForEachLoopSource(raw: string): boolean {
    const firstLine = raw.split(/\r?\n/, 1)[0]?.trim() || "";
    return /^for\s+(?:await\s+)?\([^)]*\b(?:of|in)\b/.test(firstLine);
  }

  private isPythonClassSource(raw: string): boolean {
    const firstLine = raw.split(/\r?\n/, 1)[0]?.trim() || "";
    return /^class\s+[A-Za-z_]\w*(?:\([^{}]*\))?:\s*$/.test(firstLine) &&
      /(?:^|\n)\s+def\s+[A-Za-z_]\w*\s*\(/.test(raw);
  }

  private isRubyStabbyLambdaSource(raw: string): boolean {
    return /(?:^|[=\s(,])->\s*(?:\([^)]*\))?\s*\{/.test(raw);
  }

  private isRubyLabelSource(raw: string): boolean {
    return /^[A-Za-z_]\w*(?:[!?=]?\s+|(?:\.[A-Za-z_]\w*)+\s+)[A-Za-z_]\w*:\s/.test(raw);
  }

  private isRubySymbolIndexSource(raw: string): boolean {
    return /\[:[A-Za-z_]\w*[!?=]?\]/.test(raw);
  }

  private hasRubyIndexContext(expr: AST.Index): boolean {
    const objectAff = this.getAffinity(expr.object);
    if (objectAff?.runtime === OmniRuntime.Ruby && objectAff.confidence !== "fallback") return true;
    const scopeAff = this.symbolTable.getScopeAffinity();
    return scopeAff?.runtime === OmniRuntime.Ruby && scopeAff.confidence !== "fallback";
  }

  private isRubySymbolCommandSource(raw: string): boolean {
    return /^[A-Za-z_]\w*[!?=]?\s+:[A-Za-z_]\w*[!?=]?(?:\s*,|\s*$)/.test(raw);
  }

  private isRubyHashRocketSource(raw: string): boolean {
    return /=>/.test(raw);
  }

  private importBindingNames(path: string, runtime: OmniRuntime, alias?: string): string[] {
    if (alias) return [alias];

    const cleaned = path.replace(/['"]/g, "");
    const names = new Set<string>();
    const slashName = cleaned.split("/").pop();

    if (runtime === OmniRuntime.Java && cleaned.includes(".")) {
      const last = cleaned.split(".").pop();
      if (last && last !== "*" && /^[A-Z_$]/.test(last)) {
        names.add(last);
      }
      if (cleaned.startsWith("static ")) {
        const staticLast = cleaned.split(".").pop();
        if (staticLast && staticLast !== "*") {
          names.add(staticLast);
        }
      }
    }
    if (runtime === OmniRuntime.Python && cleaned.includes(".")) {
      const root = cleaned.split(".")[0];
      if (root) names.add(root);
    }

    if (slashName) names.add(slashName);
    if (names.size === 0) names.add(path);
    return [...names];
  }

  private visitCall(node: AST.Call): void {
    // Check for builtin calls
    if (node.callee.kind === "Identifier") {
      const builtinRuntime = lookupBuiltinAffinity(node.callee.name);
      if (builtinRuntime) {
        const aff = affinityFromEvidence(chooseRuntime([{
          runtime: builtinRuntime,
          source: "builtin",
          weight: EVIDENCE_WEIGHTS.builtin,
          detail: `builtin: ${node.callee.name}`,
        }], this.fileDirective || OmniRuntime.JavaScript));
        this.assign(node, aff.runtime, aff.confidence, ...aff.evidence);
      }
    }

    this.visitExpr(node.callee);

    for (const arg of node.args) {
      this.visitExpr(arg);
    }

    const rubyLabelArg = node.args
      .map(arg => this.getAffinity(arg))
      .find(aff => aff?.runtime === OmniRuntime.Ruby && aff.confidence !== "fallback");
    if (rubyLabelArg) {
      this.assign(node, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby keyword label argument" });
    }

    if (node.optional) {
      this.assign(node, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "JavaScript optional call ?." });
    }
  }

  private visitVarDecl(node: AST.VarDecl): void {
    if (node.destructurePattern) {
      for (const v of node.values ?? []) {
        this.visitExpr(v);
      }
      const aff: RuntimeAffinity = {
        runtime: OmniRuntime.JavaScript,
        confidence: "definite",
        evidence: [{ type: "syntax", detail: "JavaScript destructuring declaration" }],
      };
      this.affinityMap.set(node, aff);
      for (const name of node.names) {
        this.symbolTable.define(name.name, {
          name: name.name,
          affinity: aff,
        });
      }
      return;
    }

    let valueAff: RuntimeAffinity | undefined;
    if (node.values) {
      for (const v of node.values) {
        this.visitExpr(v);
        valueAff = this.getAffinity(v);
      }
    }
    const symAff = (valueAff && valueAff.confidence !== "fallback")
      ? valueAff : this.currentScopeAffinity();
    for (const name of node.names) {
      this.symbolTable.define(name.name, {
        name: name.name,
        affinity: symAff,
      });
    }
  }

  private visitConstDecl(node: AST.ConstDecl): void {
    if (node.destructurePattern) {
      for (const v of node.values) {
        this.visitExpr(v);
      }
      const aff: RuntimeAffinity = {
        runtime: OmniRuntime.JavaScript,
        confidence: "definite",
        evidence: [{ type: "syntax", detail: "JavaScript destructuring declaration" }],
      };
      this.affinityMap.set(node, aff);
      for (const name of node.names) {
        this.symbolTable.define(name.name, {
          name: name.name,
          affinity: aff,
        });
      }
      return;
    }

    let valueAff: RuntimeAffinity | undefined;
    for (const v of node.values) {
      this.visitExpr(v);
      valueAff = this.getAffinity(v);
    }
    const symAff = (valueAff && valueAff.confidence !== "fallback")
      ? valueAff : this.currentScopeAffinity();
    for (const name of node.names) {
      this.symbolTable.define(name.name, {
        name: name.name,
        affinity: symAff,
      });
    }
  }

  private visitClassDecl(node: AST.ClassDecl): void {
    const rawClass = this.nodeSource(node)?.trim();
    if (rawClass && this.isRubyClassSource(rawClass)) {
      this.assign(node, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby class/end declaration" });
    } else if (rawClass && this.isPythonClassSource(rawClass)) {
      this.assign(node, OmniRuntime.Python, "definite", { type: "syntax", detail: "Python class declaration" });
    }

    const decoratorAffinities: RuntimeAffinity[] = [];
    const visitDecorator = (decorator: AST.Decorator) => {
      if (decorator.expression) {
        this.visitExprGeneric(decorator.expression);
        const aff = this.getAffinity(decorator.expression);
        if (aff && aff.confidence !== "fallback") decoratorAffinities.push(aff);
      } else {
        this.visitExprGeneric(decorator.name);
        const aff = this.getAffinity(decorator.name);
        if (aff && aff.confidence !== "fallback") decoratorAffinities.push(aff);
        for (const arg of decorator.args || []) this.visitExprGeneric(arg);
      }
    };

    for (const decorator of node.decorators || []) {
      visitDecorator(decorator);
    }
    for (const member of node.members) {
      for (const decorator of member.decorators || []) {
        visitDecorator(decorator);
      }
    }

    const decoratorAffinity = decoratorAffinities[0];
    if (decoratorAffinity) {
      this.assign(node, decoratorAffinity.runtime, decoratorAffinity.confidence, ...decoratorAffinity.evidence);
    }

    this.symbolTable.define(node.name.name, {
      name: node.name.name,
      affinity: this.getAffinity(node) || this.currentScopeAffinity(),
    });
    const classAff = this.getAffinity(node);
    if (classAff && classAff.confidence !== "fallback") {
      this.scopeStack.push(classAff.runtime);
    }
    this.symbolTable.pushScope();
    for (const member of node.members) {
      if (member.body) this.visitBlock(member.body);
    }
    this.symbolTable.popScope();
    if (classAff && classAff.confidence !== "fallback") {
      this.scopeStack.pop();
    }
  }

  private visitBlock(block: AST.Block): void {
    for (const stmt of block.statements) {
      this.visitNode(stmt);
    }
  }

  private visitCatchClause(clause: AST.CatchClause): void {
    const catchAff = inferCatchClauseAffinity(clause, this.source);
    const scopeRuntime = catchAff?.runtime || this.currentScopeRuntime();

    if (catchAff) {
      this.assign(clause.body, catchAff.runtime, catchAff.confidence, ...catchAff.evidence);
    }

    if (scopeRuntime) {
      this.scopeStack.push(scopeRuntime);
    }
    this.symbolTable.pushScope();

    if (clause.param) {
      const paramAff = catchAff || this.currentScopeAffinity();
      this.assign(clause.param, paramAff.runtime, paramAff.confidence, ...paramAff.evidence);
      this.symbolTable.define(clause.param.name, {
        name: clause.param.name,
        affinity: paramAff,
      });
    }

    this.visitBlock(clause.body);
    this.symbolTable.popScope();
    if (scopeRuntime) {
      this.scopeStack.pop();
    }
  }

  private visitExpr(expr: AST.Expr): void {
    this.visitNode(expr as any);
  }

  private visitExprGeneric(expr: AST.Expr): void {
    switch (expr.kind) {
      case "Binary":
        if (expr.op === ":") {
          this.assign(expr, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby keyword label name: value" });
        }
        if (expr.op === "===" || expr.op === "!==") {
          this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: `strict equality ${expr.op}` });
        }
        // Channel send: `ch <- value` is Go
        if (expr.op === "<-") {
          this.assign(expr, OmniRuntime.Go, "definite", { type: "syntax", detail: "channel send <-" });
        }
        this.visitExpr(expr.left);
        this.visitExpr(expr.right);
        break;
      case "Unary":
        this.visitExpr(expr.argument);
        // Channel receive <-ch is Go
        if (expr.op === "<-") {
          this.assign(expr, OmniRuntime.Go, "definite", { type: "node_type", detail: "channel receive <-" });
        }
        // Try operator ? is Rust
        if (expr.op === "?" && !expr.prefix) {
          this.assign(expr, OmniRuntime.Rust, "inferred", { type: "node_type", detail: "try operator ?" });
        }
        break;
      case "Assign":
        this.visitExpr(expr.right);
        this.visitExpr(expr.left);
        // Register the assignment target with the right-hand side's affinity
        if (expr.left.kind === "Identifier") {
          const rhsAff = this.getAffinity(expr.right);
          if (rhsAff) {
            this.symbolTable.define(expr.left.name, {
              name: expr.left.name,
              affinity: rhsAff,
            });
          }
        }
        break;
      case "Member":
        this.visitExpr(expr.object);
        this.visitMemberProperty(expr);

        const memberAff = this.inferMemberAffinity(expr);
        if (memberAff) {
          this.assign(expr, memberAff.runtime, memberAff.confidence, ...memberAff.evidence);
          this.applyMemberRootSyntaxAffinity(expr, memberAff);
        }
        if (expr.optional) {
          this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "JavaScript optional member access ?." });
        }
        break;
      case "Index":
        this.visitExpr(expr.object);
        this.visitExpr(expr.index);
        {
          const rawIndex = this.nodeSource(expr)?.trim();
          if (rawIndex && this.isRubySymbolIndexSource(rawIndex) && this.hasRubyIndexContext(expr)) {
            this.assign(expr, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby symbol index [:name]" });
          }
        }
        if (expr.optional) {
          this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "JavaScript optional index access ?." });
        }
        break;
      case "Ternary":
        this.visitExpr(expr.test);
        this.visitExpr(expr.consequent);
        this.visitExpr(expr.alternate);
        break;
      case "ArrayLiteral":
        for (const el of expr.elements) this.visitExpr(el);
        if (this.hasSpreadElement(expr.elements)) {
          this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "JavaScript array spread" });
        }
        break;
      case "ObjectLiteral":
        for (const prop of expr.properties) {
          this.visitExpr(prop.value);
        }
        {
          const rawObject = this.nodeSource(expr)?.trim();
          if (rawObject && this.isRubyHashRocketSource(rawObject)) {
            this.assign(expr, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby hash rocket =>" });
          } else if (this.hasSpreadProperty(expr.properties)) {
            this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "JavaScript object spread" });
          }
        }
        break;
      case "Lambda":
        this.assignLambdaSyntax(expr);
        this.symbolTable.pushScope();
        for (const param of expr.params) {
          if (param.name.kind === "Identifier") {
            this.symbolTable.define(param.name.name, {
              name: param.name.name,
              affinity: this.currentScopeAffinity(),
            });
          }
        }
        if ('kind' in expr.body && expr.body.kind === "Block") {
          this.visitBlock(expr.body as AST.Block);
        } else {
          this.visitExpr(expr.body as AST.Expr);
        }
        this.symbolTable.popScope();
        break;
      case "Spread":
        this.visitExpr(expr.argument);
        break;
      case "Yield":
        if (expr.value) this.visitExpr(expr.value);
        break;
      case "TypeAssertion":
        this.visitExpr(expr.expr);
        break;
      case "StringLiteral":
        {
          const rawString = this.nodeSource(expr)?.trim();
          if (expr.delimiter === "`") {
            this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "JavaScript template literal" });
          }
          if (rawString && this.isRubyStabbyLambdaSource(rawString)) {
            this.assign(expr, OmniRuntime.Ruby, "definite", { type: "syntax", detail: "Ruby stabby lambda ->" });
          }
        }
        break;
      case "Identifier":
        // Check if identifier is a known symbol
        const entry = this.symbolTable.lookup(expr.name);
        if (entry) {
          this.assign(expr, entry.affinity.runtime, entry.affinity.confidence, ...entry.affinity.evidence);
        } else {
          const globalRuntime = lookupGlobalAffinity(expr.name);
          if (globalRuntime) {
            const aff = affinityFromEvidence(chooseRuntime([{
              runtime: globalRuntime,
              source: "global",
              weight: EVIDENCE_WEIGHTS.global,
              detail: `global: ${expr.name}`,
            }], this.fileDirective || OmniRuntime.JavaScript));
            this.assign(expr, aff.runtime, aff.confidence, ...aff.evidence);
          }
        }
        break;
      case "SetLiteral":
        for (const el of expr.elements) this.visitExpr(el);
        // Sets with {a, b, c} notation — could be Python
        this.assign(expr, OmniRuntime.Python, "inferred", { type: "node_type", detail: "SetLiteral" });
        break;
      case "RegexLiteral":
        this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "regex literal /.../" });
        break;
      default:
        // Literals and other leaf nodes — no further traversal needed
        break;
    }
  }

  private visitNewExpr(node: AST.NewExpr): void {
    this.visitExpr(node.callee);
    for (const arg of node.args) {
      this.visitExpr(arg);
    }

    const calleeAff = this.getAffinity(node.callee);
    if (calleeAff && calleeAff.confidence !== "fallback") {
      this.assign(node, calleeAff.runtime, calleeAff.confidence, {
        type: "syntax",
        detail: "constructor expression",
      }, ...calleeAff.evidence);
    }
  }

  private visitJSXChildren(children: AST.JSXChild[]): void {
    for (const child of children) {
      if (child.kind === "JSXExpressionContainer" && child.expression.kind !== "JSXEmptyExpression") {
        this.visitExpr(child.expression as AST.Expr);
      } else if (child.kind === "JSXElement") {
        this.visitNode(child);
      } else if (child.kind === "JSXFragment") {
        this.visitNode(child);
      } else if (child.kind === "JSXSpreadChild") {
        this.visitExpr(child.expression);
      }
    }
  }

  private visitMemberProperty(expr: AST.Member): void {
    const property = expr.property as unknown;
    if (property && typeof property === "object" && "kind" in property && property.kind !== "Identifier") {
      this.visitExpr(property as AST.Expr);
    }
  }

  private inferMemberAffinity(expr: AST.Member): RuntimeAffinity | undefined {
    const objectAff = this.getAffinity(expr.object);
    const property = expr.property as unknown;
    const propertyAff = property && typeof property === "object" && "kind" in property && property.kind !== "Identifier"
      ? this.getAffinity(property as AST.Expr)
      : undefined;
    const chainParts = this.memberChainParts(expr);
    const qualifiedRuntime = lookupQualifiedGlobalAffinity(chainParts);
    const importedJavaClass = this.importedJavaClassForChain(chainParts);
    const rawMember = this.nodeSource(expr);

    if (rawMember?.includes("::")) {
      return {
        runtime: OmniRuntime.Ruby,
        confidence: "definite",
        evidence: [{ type: "syntax", detail: "Ruby constant path ::" }],
      };
    }

    const objectIsKnown = objectAff && objectAff.confidence !== "fallback" &&
      !(objectAff.confidence === "inferred" && objectAff.evidence[0]?.type === "scope" &&
        objectAff.evidence[0]?.detail.startsWith("scope majority"));

    if (qualifiedRuntime) {
      return {
        runtime: qualifiedRuntime,
        confidence: "inferred",
        evidence: [{ type: "builtin", detail: `qualified global: ${chainParts.join(".")}` }],
      };
    }

    if (importedJavaClass) {
      return {
        runtime: OmniRuntime.Java,
        confidence: "inferred",
        evidence: [{ type: "import", detail: `Java imported qualified class: ${importedJavaClass}` }],
      };
    }

    if (objectIsKnown) {
      return {
        runtime: objectAff.runtime,
        confidence: objectAff.confidence,
        evidence: [
          { type: "scope", detail: `member root: ${objectAff.runtime}` },
          ...objectAff.evidence,
        ],
      };
    }

    return propertyAff && propertyAff.confidence !== "fallback" ? propertyAff : undefined;
  }

  private importedJavaClassForChain(parts: string[]): string | undefined {
    if (parts.length === 0) return undefined;
    const chain = parts.join(".");
    for (const importedClass of this.importedJavaQualifiedClasses) {
      if (chain === importedClass || chain.startsWith(`${importedClass}.`)) {
        return importedClass;
      }
    }
    return undefined;
  }

  private hasSpreadElement(elements: AST.Expr[]): boolean {
    return elements.some(element => element.kind === "Spread");
  }

  private hasSpreadProperty(properties: AST.ObjectProperty[]): boolean {
    return properties.some(property => property.value.kind === "Spread");
  }

  private memberChainParts(expr: AST.Expr): string[] {
    if (expr.kind === "Identifier") {
      return [expr.name];
    }
    if (expr.kind === "Member") {
      const property = expr.property as unknown;
      if (property && typeof property === "object" && "kind" in property && property.kind === "Identifier") {
        return [...this.memberChainParts(expr.object), (property as AST.Identifier).name];
      }
    }
    return [];
  }

  private applyMemberRootSyntaxAffinity(expr: AST.Member, affinity: RuntimeAffinity): void {
    if (affinity.runtime !== OmniRuntime.Ruby) return;
    if (!affinity.evidence.some(e => e.type === "syntax" && e.detail.includes("Ruby constant path"))) return;

    const root = this.memberRootIdentifier(expr);
    if (!root) return;

    this.assign(root, affinity.runtime, affinity.confidence, ...affinity.evidence);
    this.symbolTable.define(root.name, {
      name: root.name,
      affinity,
    });

    const importNode = this.importBindingNodes.get(root.name);
    if (importNode) {
      this.assign(importNode, affinity.runtime, affinity.confidence, ...affinity.evidence);
    }
  }

  private memberRootIdentifier(expr: AST.Expr): AST.Identifier | undefined {
    if (expr.kind === "Identifier") return expr;
    if (expr.kind === "Member") return this.memberRootIdentifier(expr.object);
    return undefined;
  }

  // --- Helpers ---

  private assign(
    node: AST.Decl | AST.Stmt | AST.Expr,
    runtime: OmniRuntime,
    confidence: "definite" | "inferred" | "fallback",
    ...evidence: AffinityEvidence[]
  ): void {
    // Don't overwrite definite with inferred
    const existing = this.affinityMap.get(node);
    if (existing && existing.confidence === "definite" && confidence !== "definite") {
      return;
    }
    this.affinityMap.set(node, { runtime, confidence, evidence });
  }

  private assignLambdaSyntax(expr: AST.Lambda): void {
    const raw = this.source && expr.span && expr.span.end > expr.span.start
      ? this.source.slice(expr.span.start, expr.span.end).trim()
      : undefined;

    if (raw?.startsWith("lambda ")) {
      this.assign(expr, OmniRuntime.Python, "definite", { type: "syntax", detail: "lambda expression" });
      return;
    }

    if (raw?.includes("=>")) {
      this.assign(expr, OmniRuntime.JavaScript, "definite", { type: "syntax", detail: "arrow function =>" });
      return;
    }

    this.assign(expr, this.currentScopeRuntime() || OmniRuntime.JavaScript, "inferred", {
      type: "node_type",
      detail: "lambda expression",
    });
  }

  private getAffinity(node: AST.Decl | AST.Stmt | AST.Expr): RuntimeAffinity | undefined {
    return this.affinityMap.get(node);
  }

  private currentScopeRuntime(): OmniRuntime | undefined {
    return this.scopeStack.length > 0
      ? this.scopeStack[this.scopeStack.length - 1]
      : this.fileDirective;
  }

  private currentScopeAffinity(): RuntimeAffinity {
    const rt = this.currentScopeRuntime();
    if (rt) {
      return {
        runtime: rt,
        confidence: "inferred",
        evidence: [{ type: "scope", detail: `scope: ${rt}` }],
      };
    }
    return this.defaultAffinity();
  }

  private defaultAffinity(): RuntimeAffinity {
    return {
      runtime: this.fileDirective || OmniRuntime.JavaScript,
      confidence: "fallback",
      evidence: [{ type: "fallback", detail: "default runtime" }],
    };
  }

  private parseRuntimeName(name: string): OmniRuntime | undefined {
    const normalized = name.toLowerCase().trim();
    switch (normalized) {
      case "python":
      case "py":
        return OmniRuntime.Python;
      case "javascript":
      case "js":
        return OmniRuntime.JavaScript;
      case "go":
      case "golang":
        return OmniRuntime.Go;
      case "ruby":
      case "rb":
        return OmniRuntime.Ruby;
      case "java":
        return OmniRuntime.Java;
      case "rust":
      case "rs":
        return OmniRuntime.Rust;
      case "c":
        return OmniRuntime.C;
      default:
        return undefined;
    }
  }
}
