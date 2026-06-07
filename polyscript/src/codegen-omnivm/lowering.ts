import * as AST from '../ast';
import { AnnotatedProgram, OmniRuntime } from '../runtime-resolver/types';
import { exprToCode, jsFuncDeclToCode } from './source-reconstruct';
import {
  LoweredManifestIR,
  LoweredManifestNode,
  NativePayload,
  NativeDependency,
  LoweredGoFuncArtifact,
} from './lowering-ir';

export function lowerAnnotatedProgram(annotated: AnnotatedProgram): LoweredManifestIR {
  const lowerer = new ManifestLowerer(annotated);
  return lowerer.lower();
}

class ManifestLowerer {
  private nextId = 1;
  private readonly goFuncDecls: Map<string, AST.FuncDecl>;

  constructor(private readonly annotated: AnnotatedProgram) {
    this.goFuncDecls = this.indexGoFuncDecls(annotated.program.body);
  }

  lower(): LoweredManifestIR {
    const nodes: LoweredManifestNode[] = [];
    for (const node of this.annotated.program.body) {
      nodes.push(...this.lowerTopLevel(node));
    }
    for (const bridge of this.annotated.bridges) {
      nodes.push({
        id: this.allocId(),
        kind: "BridgeValue",
        runtime: bridge.to,
        sourceNode: this.annotated.program,
        from: bridge.from,
        to: bridge.to,
        marshalKind: bridge.marshalKind,
      });
    }

    return {
      version: 1,
      defaultRuntime: this.annotated.defaultRuntime,
      nodes,
    };
  }

  private lowerTopLevel(node: AST.Decl | AST.Stmt): LoweredManifestNode[] {
    const runtime = this.runtimeOf(node);
    const native = this.nativePayload(node);

    if (node.kind === "FuncDecl") {
      const go = runtime === OmniRuntime.Go ? this.lowerGoFunc(node) : undefined;
      return [{
        id: this.allocId(),
        kind: "DefineFunc",
        runtime,
        sourceNode: node,
        native,
        name: node.name.name,
        params: node.params.flatMap(p => p.name.kind === "Identifier" ? [p.name.name] : []),
        bodyRuntime: runtime,
        ...(!go ? { sourceArtifact: this.funcSourceArtifact(node) } : {}),
        dependencies: this.nativeDependencies(node),
        ...(go ? { go } : {}),
      }];
    }

    if (node.kind === "Import" || node.kind === "ImportDecl") {
      return [{
        id: this.allocId(),
        kind: "Import",
        runtime,
        sourceNode: node,
        native,
        artifact: this.importArtifact(node, runtime),
      }];
    }

    if (node.kind === "ConstDecl" || node.kind === "VarDecl") {
      const out: LoweredManifestNode[] = [];
      const values = node.values || [];
      for (let i = 0; i < values.length; i++) {
        const expr = values[i];
        const bind = node.names[i]?.name;
        out.push(this.lowerBoundExpr(expr, runtime, bind, node));
      }
      return out.length > 0 ? out : [this.execNode(node, runtime, native)];
    }

    if (node.kind === "ExprStmt") {
      return [this.lowerBoundExpr(node.expr, runtime, undefined, node)];
    }

    return [this.execNode(node, runtime, native)];
  }

  private lowerBoundExpr(
    expr: AST.Expr,
    runtime: OmniRuntime,
    bind: string | undefined,
    sourceNode: AST.Decl | AST.Stmt | AST.Expr,
  ): LoweredManifestNode {
    const channel = this.channelOp(expr, runtime, bind, sourceNode);
    if (channel) return channel;

    if (expr.kind === "Go") {
      return {
        id: this.allocId(),
        kind: "Spawn",
        runtime: OmniRuntime.Go,
        sourceNode,
        native: this.nativePayload(expr),
        bind,
        expr,
      };
    }

    if (expr.kind === "Call" && expr.callee.kind === "Identifier") {
      return {
        id: this.allocId(),
        kind: "CallRuntimeFunc",
        runtime,
        sourceNode,
        native: this.nativePayload(expr),
        callee: expr.callee.name,
        args: expr.args,
        expr,
        bind,
      };
    }

    return {
      id: this.allocId(),
      kind: "EvalExpr",
      runtime,
      sourceNode,
      native: this.nativePayload(expr),
      bind,
      expr,
    };
  }

  private channelOp(
    expr: AST.Expr,
    runtime: OmniRuntime,
    bind: string | undefined,
    sourceNode: AST.Decl | AST.Stmt | AST.Expr,
  ): LoweredManifestNode | undefined {
    if (expr.kind === "Call" && expr.callee.kind === "Identifier") {
      if (expr.callee.name === "make") {
        const size = expr.args.length > 0 && expr.args[0].kind === "NumericLiteral"
          ? Number(expr.args[0].raw)
          : undefined;
        return {
          id: this.allocId(),
          kind: "ChannelMake",
          action: "make",
          runtime,
          sourceNode,
          native: this.nativePayload(sourceNode),
          bind,
          ...(size !== undefined ? { size } : {}),
        };
      }
      if (expr.callee.name === "close" && expr.args[0]) {
        return {
          id: this.allocId(),
          kind: "ChannelClose",
          action: "close",
          runtime,
          sourceNode,
          native: this.nativePayload(sourceNode),
          channel: exprToCode(expr.args[0], this.annotated.source),
        };
      }
      if (expr.callee.name === "recv" && expr.args[0]) {
        return {
          id: this.allocId(),
          kind: "ChannelRecv",
          action: "recv",
          runtime,
          sourceNode,
          native: this.nativePayload(sourceNode),
          bind,
          channel: exprToCode(expr.args[0], this.annotated.source),
        };
      }
    }

    if (expr.kind === "Binary" && expr.op === "<-") {
      return {
        id: this.allocId(),
        kind: "ChannelSend",
        action: "send",
        runtime,
        sourceNode,
        native: this.nativePayload(sourceNode),
        channel: exprToCode(expr.left, this.annotated.source),
        value: expr.right,
      };
    }

    return undefined;
  }

  private execNode(
    node: AST.Decl | AST.Stmt | AST.Expr,
    runtime: OmniRuntime,
    native?: NativePayload,
  ): LoweredManifestNode {
    return {
      id: this.allocId(),
      kind: "ExecStmt",
      runtime,
      sourceNode: node,
      native,
      node,
    };
  }

  private nativePayload(node: AST.Decl | AST.Stmt | AST.Expr): NativePayload | undefined {
    if (!this.annotated.source || !node.span || node.span.end <= node.span.start) return undefined;
    return {
      source: this.annotated.source.slice(node.span.start, node.span.end),
      span: node.span,
    };
  }

  private funcSourceArtifact(node: AST.FuncDecl) {
    const source = this.annotated.source || "";
    const slice = (span: AST.Span | undefined) =>
      source && span && span.end > span.start ? source.slice(span.start, span.end) : "";
    const functionSource = node.declKeyword === "function"
      ? jsFuncDeclToCode(node, source)
      : slice(node.span);
    const executableSource = this.executableFunctionSource(node, functionSource);
    return {
      paramsSource: node.params.map(param => slice(param.span)),
      bodySource: slice(node.body.span),
      functionSource: executableSource,
    };
  }

  private executableFunctionSource(node: AST.FuncDecl, source: string): string {
    let executable = node.generator ? this.executableGeneratorFunctionSource(source) : source;
    if (node.async && node.declKeyword === "def") {
      executable = this.executablePythonAsyncFunctionSource(executable);
    } else if (node.async && node.declKeyword === "function") {
      executable = this.executableJavaScriptAsyncFunctionSource(executable);
    }
    return executable;
  }

  private executableGeneratorFunctionSource(source: string): string {
    return source.replace(/^(\s*)(async\s+)?\*/, (_match, leading: string, asyncPrefix: string | undefined) =>
      `${leading}${asyncPrefix ?? ""}function*`,
    );
  }

  private executablePythonAsyncFunctionSource(source: string): string {
    if (/^\s*async\s+def\b/.test(source)) return source;
    return source.replace(/^(\s*)def\b/, "$1async def");
  }

  private executableJavaScriptAsyncFunctionSource(source: string): string {
    if (/^\s*async\s+function\b/.test(source)) return source;
    return source.replace(/^(\s*)function\b/, "$1async function");
  }

  private importArtifact(node: AST.Import | AST.ImportDecl, runtime: OmniRuntime) {
    const source = this.nativePayload(node)?.source || "";
    if (node.kind === "Import") {
      return {
        path: node.path,
        bind: node.alias?.name || (runtime === OmniRuntime.Go ? this.goImportBindingName(node.path) : node.path),
        source,
      };
    }
    return {
      path: node.path,
      ...(node.defaultImport ? { defaultImport: node.defaultImport.name } : {}),
      ...(node.namespaceImport ? { namespaceImport: node.namespaceImport.name } : {}),
      ...(node.specifiers && node.specifiers.length > 0 ? { specifiers: node.specifiers.map(s => ({ imported: s.imported, local: s.local })) } : {}),
      source,
    };
  }

  private runtimeOf(node: AST.Decl | AST.Stmt | AST.Expr): OmniRuntime {
    return this.annotated.affinityMap.get(node)?.runtime || this.annotated.defaultRuntime;
  }

  private lowerGoFunc(node: AST.FuncDecl): LoweredGoFuncArtifact {
    const name = node.name.name;
    const exportName = this.toPascalCase(name);
    const params = node.params.map(p => ({
      name: p.name.kind === "Identifier" ? p.name.name : "_",
      type: p.type ? this.typeNodeToGo(p.type) : "interface{}",
    }));
    const returnType = node.returnType
      ? this.typeNodeToGo(node.returnType)
      : (name === "main" ? "" : "interface{}");
    const paramNames = new Set(params.map(p => p.name).filter(name => name !== "_"));
    const calledFuncs = this.goDependencyCalls(node);
    const definedLocals = new Set<string>();
    const bodyLines = node.body.statements.map(
      s => this.goStmtToCode(s, paramNames, definedLocals, calledFuncs),
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

    const goBuiltins = new Set([
      'make', 'len', 'cap', 'append', 'copy', 'delete', 'new',
      'panic', 'recover', 'close', 'print', 'println',
      'complex', 'real', 'imag',
      'bool', 'byte', 'rune', 'string', 'int', 'int8', 'int16', 'int32', 'int64',
      'uint', 'uint8', 'uint16', 'uint32', 'uint64', 'float32', 'float64',
      '[]byte', '[]rune',
    ]);
    const dependencies: NativeDependency[] = [];
    const varDecls: string[] = [];
    for (const [fname, argc] of calledFuncs) {
      if (paramNames.has(fname) || definedLocals.has(fname) || goBuiltins.has(fname)) continue;
      if (sameFileHelpers.has(fname)) continue;
      dependencies.push({ name: fname, argc });
      const fParamTypes = Array.from({ length: argc }, () => "interface{}").join(", ");
      varDecls.push(`var ${fname} func(${fParamTypes}) interface{}`);
    }

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
      lines.push(`func Init(deps map[string]interface{}) {`);
      for (const dep of dependencies) {
        const fParamTypes = Array.from({ length: dep.argc }, () => "interface{}").join(", ");
        lines.push(`\tif fn, ok := deps["${dep.name}"].(func(${fParamTypes}) interface{}); ok {`);
        lines.push(`\t\t${dep.name} = fn`);
        lines.push(`\t}`);
      }
      lines.push("}", "");
    }

    const signature = `func ${exportName}(${params.map(p => `${p.name} ${p.type}`).join(", ")})${returnType ? ` ${returnType}` : ""}`;
    lines.push(`${signature} {`);
    for (const line of bodyLines) {
      lines.push(`\t${line}`);
    }
    lines.push("}");

    return {
      exportName,
      params,
      returnType,
      signature,
      bodyLines,
      imports,
      helperSources,
      dependencies,
      varDecls,
      source: lines.join("\n"),
    };
  }

  private indexGoFuncDecls(nodes: Array<AST.Decl | AST.Stmt>): Map<string, AST.FuncDecl> {
    const funcs = new Map<string, AST.FuncDecl>();
    for (const node of nodes) {
      if (node.kind === "FuncDecl") funcs.set(node.name.name, node);
    }
    return funcs;
  }

  private goDependencyCalls(node: AST.FuncDecl): Map<string, number> {
    const calls = new Map<string, number>();
    for (const stmt of node.body.statements) {
      this.collectCallDependencies(stmt, calls);
    }
    return calls;
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
      s => this.goStmtToCode(s, paramNames, definedLocals, calledFuncs),
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

  private goImportBindingName(path: string): string {
    const last = path.split("/").filter(Boolean).pop() || path;
    const cleaned = last.replace(/[^A-Za-z0-9_]/g, "_");
    return /^[A-Za-z_]/.test(cleaned) ? cleaned : `_${cleaned}`;
  }

  private toPascalCase(name: string): string {
    return name.includes('_')
      ? name.split('_').map(s => s.charAt(0).toUpperCase() + s.slice(1)).join('')
      : name.charAt(0).toUpperCase() + name.slice(1);
  }

  private goBlockToCode(
    statements: Array<AST.Stmt | AST.Decl>,
    params: Set<string>,
    locals: Set<string>,
    calledFuncs: Map<string, number>,
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
    if (this.annotated.source && node.span && node.span.end > node.span.start) {
      return this.annotated.source.slice(node.span.start, node.span.end);
    }
    return "/* unsupported */";
  }

  private typeNodeToGo(t: AST.TypeNode): string {
    switch (t.kind) {
      case "SimpleType":
        return t.id.name;
      case "GenericType": {
        const base = t.base.name;
        if (base === "map" && t.args.length === 2) {
          return `map[${this.typeNodeToGo(t.args[0])}]${this.typeNodeToGo(t.args[1])}`;
        }
        if (base === "chan" && t.args.length === 1) {
          return `chan ${this.typeNodeToGo(t.args[0])}`;
        }
        return `${base}[${t.args.map(a => this.typeNodeToGo(a)).join(", ")}]`;
      }
      case "NullableType":
        return `*${this.typeNodeToGo(t.inner)}`;
      case "ChanType":
        return this.annotated.source && t.span ? this.annotated.source.slice(t.span.start, t.span.end) : "chan interface{}";
      default:
        return this.annotated.source && t.span ? this.annotated.source.slice(t.span.start, t.span.end) : "interface{}";
    }
  }

  private nativeDependencies(node: AST.FuncDecl): NativeDependency[] | undefined {
    const calls = new Map<string, number>();
    this.collectCallDependencies(node.body.statements, calls);
    if (calls.size === 0) return undefined;
    return Array.from(calls.entries())
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([name, argc]) => ({ name, argc }));
  }

  private collectCallDependencies(node: unknown, calls: Map<string, number>): void {
    if (!node || typeof node !== "object") return;
    if (Array.isArray(node)) {
      for (const item of node) this.collectCallDependencies(item, calls);
      return;
    }

    const candidate = node as { kind?: string; callee?: unknown; args?: unknown[] };
    if (candidate.kind === "Call") {
      const callee = candidate.callee as { kind?: string; name?: string } | undefined;
      if (callee?.kind === "Identifier" && callee.name) {
        calls.set(callee.name, Math.max(calls.get(callee.name) ?? 0, candidate.args?.length ?? 0));
      }
    }

    for (const [key, value] of Object.entries(node)) {
      if (key === "span") continue;
      this.collectCallDependencies(value, calls);
    }
  }

  private allocId(): number {
    return this.nextId++;
  }
}
