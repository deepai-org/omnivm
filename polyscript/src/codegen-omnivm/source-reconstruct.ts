/**
 * Shared source code reconstruction helpers.
 *
 * Used by both the legacy JS code generator and the new manifest generator
 * to convert AST nodes back to source code strings.
 *
 * When `source` is provided, span-based extraction is used as a fallback
 * for node kinds that don't have explicit reconstruction logic, ensuring
 * all parseable nodes produce valid source code.
 */
import * as AST from '../ast';
import { OmniRuntime } from '../runtime-resolver/types';

/**
 * Extract source text from a node's span. Returns undefined if source
 * or span information is unavailable.
 */
function spanExtract(node: { span?: AST.Span }, source?: string): string | undefined {
  if (source && node.span && node.span.end > node.span.start) {
    const decorators = (node as { decorators?: Array<{ span?: AST.Span }> }).decorators;
    let start = node.span.start;
    if (decorators && decorators.length > 0) {
      start = decorators.reduce(
        (min, decorator) => decorator.span ? Math.min(min, decorator.span.start) : min,
        start,
      );
      if (start > 0 && source[start - 1] === "@") {
        start--;
      }
    }
    return source.slice(start, node.span.end);
  }
  return undefined;
}

export function exprToCode(expr: AST.Expr, source?: string): string {
  switch (expr.kind) {
    case "Identifier":
      return expr.name;
    case "NumericLiteral":
      return expr.raw;
    case "StringLiteral":
      return stringLiteralToCode(expr);
    case "BooleanLiteral":
      return String(expr.value);
    case "NullLiteral":
      return "null";
    case "Member": {
      const obj = exprToCode(expr.object, source);
      if (expr.computed) {
        const dot = expr.optional ? "?." : "";
        return `${obj}${dot}[${expr.property.name}]`;
      }
      const dot = expr.optional ? "?." : ".";
      return `${obj}${dot}${expr.property.name}`;
    }
    case "Call": {
      const args = expr.args.map(a => exprToCode(a, source)).join(", ");
      const optional = expr.optional ? "?." : "";
      return `${exprToCode(expr.callee, source)}${optional}(${args})`;
    }
    case "NewExpr": {
      const args = expr.args.map(a => exprToCode(a, source)).join(", ");
      return `new ${exprToCode(expr.callee, source)}(${args})`;
    }
    case "Binary":
      return `(${exprToCode(expr.left, source)} ${expr.op} ${exprToCode(expr.right, source)})`;
    case "Unary": {
      if (expr.prefix) {
        // Word operators (await, typeof, delete, void, throw) need a space
        const space = /^[a-z]+$/i.test(expr.op) ? " " : "";
        return `${expr.op}${space}${exprToCode(expr.argument, source)}`;
      }
      return `${exprToCode(expr.argument, source)}${expr.op}`;
    }
    case "Index": {
      const idxObj = exprToCode(expr.object, source);
      const idxBracket = expr.optional ? "?.[" : "[";
      return `${idxObj}${idxBracket}${exprToCode(expr.index, source)}]`;
    }
    case "Assign":
      return `${exprToCode(expr.left, source)} ${expr.op} ${exprToCode(expr.right, source)}`;
    case "Ternary":
      return `(${exprToCode(expr.test, source)} ? ${exprToCode(expr.consequent, source)} : ${exprToCode(expr.alternate, source)})`;
    case "ArrayLiteral":
      return `[${expr.elements.map(e => exprToCode(e, source)).join(", ")}]`;
    case "ObjectLiteral": {
      const objProps = expr.properties.map(p => {
        if (p.value.kind === "Spread") {
          return exprToCode(p.value, source);
        }
        if (p.shorthand && p.key.kind === "Identifier") {
          return p.key.name;
        }
        const key = p.computed
          ? `[${exprToCode(p.key as AST.Expr, source)}]`
          : (p.key.kind === "Identifier" ? p.key.name : exprToCode(p.key as AST.Expr, source));
        return `${key}: ${exprToCode(p.value, source)}`;
      }).join(", ");
      return `{${objProps}}`;
    }
    case "Lambda": {
      const lParams = expr.params.map(p => paramToCode(p, source)).join(", ");
      return lambdaToCode(expr, lParams, source);
    }
    case "Spread":
      return `...${exprToCode(expr.argument, source)}`;
    case "ListComprehension":
      const targets = expr.targets.map(t => t.name).join(", ");
      const filter = expr.filter ? ` if ${exprToCode(expr.filter, source)}` : "";
      return `[${exprToCode(expr.expression, source)} for ${targets} in ${exprToCode(expr.iterable, source)}${filter}]`;
    case "RuntimeTag":
      return exprToCode(expr.expr, source);
    case "Go":
      return `go ${exprToCode(expr.expr, source)}`;
    case "JSXElement":
      return jsxToCreateElement(expr, source);
    case "JSXFragment":
      return jsxFragmentToCreateElement(expr, source);
    case "Match":
      return matchToTernary(expr, source);
    case "Yield": {
      const prefix = expr.delegate ? "yield* " : "yield";
      if (expr.value) return `${prefix}${expr.delegate ? "" : " "}${exprToCode(expr.value, source)}`;
      return "yield";
    }
    case "RegexLiteral":
      return `/${expr.pattern}/${expr.flags}`;
    case "SetLiteral":
      return `new Set([${expr.elements.map(e => exprToCode(e, source)).join(", ")}])`;
    case "TypeAssertion":
      return `(${exprToCode(expr.expr, source)} as ${typeToCode(expr.type, source)})`;
    default:
      return spanExtract(expr, source) || "/* expr */";
  }
}

export function exprToCodeForRuntime(expr: AST.Expr, runtime: OmniRuntime, source?: string): string {
  if (expr.kind === "RuntimeTag") {
    return exprToCodeForRuntime(expr.expr, tagToRuntime(expr.runtime), source);
  }
  if (runtime === OmniRuntime.Java) {
    return exprToJavaCode(expr, source);
  }
  if (runtime === OmniRuntime.Ruby) {
    return spanExtract(expr, source) || exprToCode(expr, source);
  }
  if (runtime === OmniRuntime.Python) {
    return exprToPythonCode(expr, source);
  }
  return exprToCode(expr, source);
}

function exprToJavaCode(expr: AST.Expr, source?: string): string {
  switch (expr.kind) {
    case "Identifier":
      return expr.name;
    case "NumericLiteral":
      return expr.raw;
    case "StringLiteral":
      return stringLiteralToCode(expr);
    case "BooleanLiteral":
      return String(expr.value);
    case "NullLiteral":
      return "null";
    case "Member": {
      const obj = exprToJavaCode(expr.object, source);
      if (expr.computed) {
        return `${obj}[${exprToJavaCode(expr.property as AST.Expr, source)}]`;
      }
      return `${obj}.${expr.property.name}`;
    }
    case "Call":
      return `${exprToJavaCode(expr.callee, source)}(${expr.args.map(a => exprToJavaCode(a, source)).join(", ")})`;
    case "NewExpr":
      return `new ${exprToJavaCode(expr.callee, source)}(${expr.args.map(a => exprToJavaCode(a, source)).join(", ")})`;
    case "Binary":
      return `(${exprToJavaCode(expr.left, source)} ${expr.op} ${exprToJavaCode(expr.right, source)})`;
    case "Unary": {
      if (expr.prefix) {
        const space = /^[a-z]+$/i.test(expr.op) ? " " : "";
        return `${expr.op}${space}${exprToJavaCode(expr.argument, source)}`;
      }
      return `${exprToJavaCode(expr.argument, source)}${expr.op}`;
    }
    case "Index":
      return `${exprToJavaCode(expr.object, source)}[${exprToJavaCode(expr.index, source)}]`;
    case "Assign":
      return `${exprToJavaCode(expr.left, source)} ${expr.op} ${exprToJavaCode(expr.right, source)}`;
    case "Ternary":
      return `(${exprToJavaCode(expr.test, source)} ? ${exprToJavaCode(expr.consequent, source)} : ${exprToJavaCode(expr.alternate, source)})`;
    case "ArrayLiteral":
      return `java.util.Arrays.asList(${expr.elements.map(e => exprToJavaCode(e, source)).join(", ")})`;
    case "ObjectLiteral": {
      if (expr.properties.length === 0) {
        return "new java.util.LinkedHashMap()";
      }
      const entries = expr.properties.map(p => {
        if (p.value.kind === "Spread") {
          return exprToJavaCode(p.value, source);
        }
        if (p.shorthand && p.key.kind === "Identifier") {
          return `java.util.Map.entry(${JSON.stringify(p.key.name)}, ${p.key.name})`;
        }
        const key = p.computed
          ? exprToJavaCode(p.key as AST.Expr, source)
          : (p.key.kind === "Identifier" ? JSON.stringify(p.key.name) : exprToJavaCode(p.key as AST.Expr, source));
        return `java.util.Map.entry(${key}, ${exprToJavaCode(p.value, source)})`;
      }).join(", ");
      return `new java.util.LinkedHashMap(java.util.Map.ofEntries(${entries}))`;
    }
    case "RuntimeTag":
      return exprToJavaCode(expr.expr, source);
    default:
      return spanExtract(expr, source) || exprToCode(expr, source);
  }
}

function exprToPythonCode(expr: AST.Expr, source?: string): string {
  switch (expr.kind) {
    case "StringLiteral":
      return stringLiteralToPythonCode(expr, source);
    case "Lambda":
      return lambdaToPythonCode(expr, source);
    case "BooleanLiteral":
      return expr.value ? "True" : "False";
    case "NullLiteral":
      return "None";
    case "ArrayLiteral":
      return `[${expr.elements.map(e => exprToPythonCode(e, source)).join(", ")}]`;
    case "ListComprehension": {
      const targets = expr.targets.map(t => t.name).join(", ");
      const filter = expr.filter ? ` if ${exprToPythonCode(expr.filter, source)}` : "";
      return `[${exprToPythonCode(expr.expression, source)} for ${targets} in ${exprToPythonCode(expr.iterable, source)}${filter}]`;
    }
    case "ObjectLiteral": {
      const objProps = expr.properties.map(p => {
        if (p.value.kind === "Spread") {
          return exprToPythonCode(p.value, source);
        }
        const key = p.computed
          ? `[${exprToPythonCode(p.key as AST.Expr, source)}]`
          : (p.key.kind === "Identifier" ? JSON.stringify(p.key.name) : exprToPythonCode(p.key as AST.Expr, source));
        return `${key}: ${exprToPythonCode(p.value, source)}`;
      }).join(", ");
      return `{${objProps}}`;
    }
    case "Call":
      return `${exprToPythonCode(expr.callee, source)}(${expr.args.map(a => exprToPythonCode(a, source)).join(", ")})`;
    case "NewExpr":
      return exprToCode(expr, source);
    case "Member":
      return `${exprToPythonCode(expr.object, source)}.${expr.property.name}`;
    case "Index":
      return spanExtract(expr, source) || `${exprToPythonCode(expr.object, source)}[${exprToPythonCode(expr.index, source)}]`;
    case "Binary":
      return `(${exprToPythonCode(expr.left, source)} ${expr.op} ${exprToPythonCode(expr.right, source)})`;
    case "Unary": {
      if (expr.prefix) {
        const op = expr.op === "!" ? "not " : expr.op;
        const space = /^[a-z]+$/i.test(op) && !op.endsWith(" ") ? " " : "";
        return `${op}${space}${exprToPythonCode(expr.argument, source)}`;
      }
      return `${exprToPythonCode(expr.argument, source)}${expr.op}`;
    }
    case "Assign":
      return `${exprToPythonCode(expr.left, source)} ${expr.op} ${exprToPythonCode(expr.right, source)}`;
    case "RuntimeTag":
      return exprToPythonCode(expr.expr, source);
    default:
      return exprToCode(expr, source);
  }
}

function stringLiteralToPythonCode(node: AST.StringLiteral, source?: string): string {
  const raw = spanExtract(node, source)?.trim();
  if (raw && /^[rubf]*f/i.test(raw)) {
    return raw;
  }
  if (node.parts.length === 1 && node.parts[0].kind === "Text") {
    return JSON.stringify(node.parts[0].value as string);
  }

  let result = 'f"';
  for (const part of node.parts) {
    if (part.kind === "Text") {
      result += String(part.value)
        .replace(/\\/g, "\\\\")
        .replace(/"/g, '\\"')
        .replace(/{/g, "{{")
        .replace(/}/g, "}}");
    } else {
      const value = typeof part.value === "string" ? part.value : exprToPythonCode(part.value as AST.Expr, source);
      result += "{" + value + "}";
    }
  }
  result += '"';
  return result;
}

export function stringLiteralToCode(node: AST.StringLiteral): string {
  if (node.parts.length === 1 && node.parts[0].kind === "Text") {
    return JSON.stringify(node.parts[0].value as string);
  }
  let result = "`";
  for (const part of node.parts) {
    if (part.kind === "Text") {
      result += part.value as string;
    } else {
      const val = typeof part.value === "string" ? part.value : exprToCode(part.value as AST.Expr);
      result += "${" + val + "}";
    }
  }
  result += "`";
  return result;
}

export function nodeToSourceCode(node: AST.Decl | AST.Stmt | AST.Expr, source?: string): string {
  switch (node.kind) {
    case "ExprStmt":
      return exprToCode(node.expr, source);
    case "FuncDecl":
      if (node.declKeyword === "function") {
        return jsFuncDeclToCode(node, source);
      }
      // For non-JS runtimes, prefer span extraction to get the original syntax.
      return spanExtract(node, source) || (() => {
        const kw = node.declKeyword || "function";
        const params = node.params.map(p => paramToCode(p, source)).join(", ");
        return `${kw} ${node.name.name}(${params}) { /* body */ }`;
      })();
    case "VarDecl":
      return node.names.map((n, i) =>
        node.values?.[i] ? `let ${n.name} = ${exprToCode(node.values[i], source)}` : `let ${n.name}`
      ).join("; ");
    case "ConstDecl":
      return node.names.map((n, i) =>
        node.values?.[i] ? `const ${n.name} = ${exprToCode(node.values[i], source)}` : `const ${n.name}`
      ).join("; ");
    case "ShortDecl": {
      // Rewrite := to const for JS-valid output
      if (node.pairs) {
        return node.pairs.map((p: any) =>
          `const ${p.name.name} = ${exprToCode(p.expr, source)}`
        ).join("; ");
      }
      if (node.targets && node.value) {
        const names = node.targets
          .filter((t: any) => t.kind === "Identifier")
          .map((t: any) => t.name);
        return `const ${names.join(", ")} = ${exprToCode(node.value, source)}`;
      }
      return spanExtract(node, source) || "/* ShortDecl */";
    }
    case "Return":
      return `return ${node.values.map(v => exprToCode(v, source)).join(", ")}`;
    case "Go":
      return `go ${exprToCode(node.expr, source)}`;
    case "Yield": {
      const prefix = node.delegate ? "yield* " : "yield";
      if (node.value) return `${prefix}${node.delegate ? "" : " "}${exprToCode(node.value, source)}`;
      return "yield";
    }
    case "Break":
      return node.label ? `break ${node.label.name}` : "break";
    case "Continue":
      return node.label ? `continue ${node.label.name}` : "continue";
    case "Throw":
      return spanExtract(node, source) || `throw ${exprToCode(node.value, source)}`;
    case "Pass":
      return "/* pass */";
    case "Echo":
      return `console.log(${node.values.map(v => exprToCode(v, source)).join(", ")})`;
    case "Defer": {
      if ("kind" in node.body && (node.body as any).kind === "Block") {
        return `defer { ${blockToCode(node.body as AST.Block, source)} }`;
      }
      return `defer ${exprToCode(node.body as AST.Expr, source)}`;
    }
    case "Reassign":
      return `${node.name.name} = ${exprToCode(node.expr, source)}`;
    case "If": {
      const parts: string[] = [];
      for (let i = 0; i < node.arms.length; i++) {
        const arm = node.arms[i];
        const kw = i === 0 ? "if" : "else if";
        parts.push(`${kw} (${exprToCode(arm.test, source)}) { ${blockToCode(arm.body, source)} }`);
      }
      if (node.elseBody) {
        parts.push(`else { ${blockToCode(node.elseBody, source)} }`);
      }
      return parts.join(" ");
    }
    case "Loop": {
      const loopNode = node as AST.Loop;
      switch (loopNode.mode) {
        case "while":
          return `while (${loopNode.test ? exprToCode(loopNode.test, source) : "true"}) { ${blockToCode(loopNode.body, source)} }`;
        case "do-while":
          return `do { ${blockToCode(loopNode.body, source)} } while (${loopNode.test ? exprToCode(loopNode.test, source) : "true"})`;
        case "for": {
          const init = loopNode.init ? nodeToSourceCode(loopNode.init, source) : "";
          const test = loopNode.test ? exprToCode(loopNode.test, source) : "";
          const step = loopNode.step ? exprToCode(loopNode.step, source) : "";
          return `for (${init}; ${test}; ${step}) { ${blockToCode(loopNode.body, source)} }`;
        }
        case "foreach": {
          const varName = loopNode.variable
            ? (loopNode.variable.kind === "Identifier" ? loopNode.variable.name : spanExtract(loopNode.variable, source) || "item")
            : "item";
          const iter = loopNode.iterable ? exprToCode(loopNode.iterable, source) : "[]";
          const kw = loopNode.await ? "for await" : "for";
          const iterKind = loopNode.iterationKind === "in" && !loopNode.await ? "in" : "of";
          return `${kw} (const ${varName} ${iterKind} ${iter}) { ${blockToCode(loopNode.body, source)} }`;
        }
        case "infinite":
          return `while (true) { ${blockToCode(loopNode.body, source)} }`;
        default:
          return `while (true) { ${blockToCode(loopNode.body, source)} }`;
      }
    }
    case "Try": {
      let result = `try { ${blockToCode(node.body, source)} }`;
      for (const c of node.catches) {
        const param = c.param ? c.param.name : "";
        result += ` catch${param ? ` (${param})` : ""} { ${blockToCode(c.body, source)} }`;
      }
      if (node.finallyBody) {
        result += ` finally { ${blockToCode(node.finallyBody, source)} }`;
      }
      return result;
    }
    case "Switch": {
      const cases = node.cases.map(c => {
        const patterns = c.patterns.map(p => exprToCode(p, source)).join(", ");
        return `case ${patterns}: ${blockToCode(c.body, source)}; break;`;
      }).join(" ");
      const dflt = node.defaultCase ? ` default: ${blockToCode(node.defaultCase, source)};` : "";
      return `switch (${exprToCode(node.discriminant, source)}) { ${cases}${dflt} }`;
    }
    case "Import":
      return `import "${node.path}"`;
    case "GroupedImport":
      return `import (\n${node.imports.map(imp => {
        if (imp.kind === "Import") {
          const alias = imp.alias ? `${imp.alias.name} ` : "";
          return `\t${alias}"${imp.path}"`;
        }
        return `\t${importSpecsToCode(imp)} from "${imp.path}"`;
      }).join("\n")}\n)`;
    case "ImportDecl":
      return `import ${importSpecsToCode(node)} from "${node.path}"`;
    default:
      if (isExprKind(node.kind)) {
        return exprToCode(node as AST.Expr, source);
      }
      return spanExtract(node, source) || `/* ${node.kind} */`;
  }
}

export function jsFuncDeclToCode(node: AST.FuncDecl, source?: string): string {
  const asyncPrefix = node.async ? "async " : "";
  const generatorMark = node.generator ? "*" : "";
  const params = node.params.map(p => paramToCode(p, source)).join(", ");
  return `${asyncPrefix}function${generatorMark} ${node.name.name}(${params}) { ${blockToCode(node.body, source)} }`;
}

export function paramToCode(param: AST.Param, source?: string): string {
  const name = param.name.kind === "Identifier" ? param.name.name : (spanExtract(param.name, source) || "/* pattern */");
  const spread = param.spread ? "..." : "";
  const defaultVal = param.defaultValue ? ` = ${exprToCode(param.defaultValue, source)}` : "";
  return `${spread}${name}${defaultVal}`;
}

export function typeToCode(type: AST.TypeNode, source?: string): string {
  switch (type.kind) {
    case "SimpleType":
      return type.id.name;
    case "GenericType":
      return `${type.base.name}<${type.args.map(a => typeToCode(a, source)).join(", ")}>`;
    case "NullableType":
      return `${typeToCode(type.inner, source)}?`;
    case "UnionType":
      return type.types.map(t => typeToCode(t, source)).join(" | ");
    case "FuncType": {
      const params = type.params.map(p => {
        const name = p.name ? `${p.name.name}: ` : "";
        const opt = p.optional ? "?" : "";
        return `${name}${opt}${typeToCode(p.type, source)}`;
      }).join(", ");
      return `(${params}) => ${typeToCode(type.ret, source)}`;
    }
    case "ChanType":
      return `chan ${type.elementType ? typeToCode(type.elementType, source) : ""}`.trim();
    case "ImplType":
      return `impl ${typeToCode(type.trait, source)}`;
    case "DynType":
      return `dyn ${typeToCode(type.trait, source)}`;
    case "IndexedAccessType":
      return `${typeToCode(type.object, source)}[${type.index}]`;
    case "ObjectType": {
      const props = type.properties.map(p => {
        const ro = p.readonly ? "readonly " : "";
        const opt = p.optional ? "?" : "";
        return `${ro}${p.name}${opt}: ${typeToCode(p.type, source)}`;
      }).join("; ");
      return `{ ${props} }`;
    }
    default:
      return spanExtract(type, source) || "unknown";
  }
}

function blockToCode(block: AST.Block, source?: string): string {
  return block.statements.map(s => nodeToSourceCode(s, source)).join("; ");
}

export function importSpecsToCode(node: AST.ImportDecl): string {
  const parts: string[] = [];
  if (node.defaultImport) parts.push(node.defaultImport.name);
  if (node.namespaceImport) parts.push(`* as ${node.namespaceImport.name}`);
  if (node.specifiers && node.specifiers.length > 0) {
    const specs = node.specifiers.map(s =>
      s.imported === s.local ? s.local : `${s.imported} as ${s.local}`
    ).join(", ");
    parts.push(`{ ${specs} }`);
  }
  return parts.join(", ");
}

export function escapeTemplate(code: string): string {
  return code.replace(/\\/g, "\\\\").replace(/`/g, "\\`").replace(/\$\{/g, "\\${");
}

export function tagToRuntime(tag: string): OmniRuntime {
  switch (tag) {
    case "py": return OmniRuntime.Python;
    case "js": return OmniRuntime.JavaScript;
    case "go": return OmniRuntime.Go;
    case "rb": return OmniRuntime.Ruby;
    case "java": return OmniRuntime.Java;
    default: return OmniRuntime.JavaScript;
  }
}

// ─── JSX Lowering ──────────────────────────────────────────────────

interface JSXLoweringOptions {
  factory: string;
  fragment: string;
}

const DEFAULT_JSX_FACTORY = "React.createElement";
const DEFAULT_JSX_FRAGMENT = "React.Fragment";

function jsxLoweringOptions(source?: string): JSXLoweringOptions {
  if (!source) {
    return { factory: DEFAULT_JSX_FACTORY, fragment: DEFAULT_JSX_FRAGMENT };
  }

  const name = "[A-Za-z_$][\\w$]*(?:\\.[A-Za-z_$][\\w$]*)*";
  const factory = source.match(new RegExp(`@jsx\\s+(${name})`))?.[1] || DEFAULT_JSX_FACTORY;
  const fragment = source.match(new RegExp(`@jsxFrag\\s+(${name})`))?.[1] || DEFAULT_JSX_FRAGMENT;
  return { factory, fragment };
}

function jsxElementNameToCode(name: AST.JSXElementName): string {
  switch (name.kind) {
    case "JSXIdentifier":
      return name.name;
    case "JSXMemberExpression":
      return `${jsxElementNameToCode(name.object)}.${name.property.name}`;
    case "JSXNamespacedName":
      return `"${name.namespace.name}:${name.name.name}"`;
    default:
      return "null";
  }
}

function jsxElementNameToArg(name: AST.JSXElementName): string {
  if (name.kind === "JSXIdentifier") {
    // Lowercase → string literal (HTML element), uppercase → identifier (component)
    const n = name.name;
    return /^[a-z]/.test(n) ? `"${n}"` : n;
  }
  if (name.kind === "JSXMemberExpression") {
    return jsxElementNameToCode(name);
  }
  // Namespaced
  return `"${(name as AST.JSXNamespacedName).namespace.name}:${(name as AST.JSXNamespacedName).name.name}"`;
}

function jsxAttrToProps(attributes: AST.JSXAttribute[], source?: string): string {
  if (attributes.length === 0) return "null";

  const hasSpread = attributes.some(a => a.kind === "JSXSpreadAttribute");
  const parts: string[] = [];

  if (hasSpread) {
    // Use Object.assign for spread attributes
    parts.push("Object.assign({}");
    for (const attr of attributes) {
      if (attr.kind === "JSXSpreadAttribute") {
        parts.push(`, ${exprToCode(attr.argument, source)}`);
      } else {
        const key = attr.name.kind === "JSXIdentifier" ? attr.name.name : `"${attr.name.namespace.name}:${attr.name.name.name}"`;
        const val = jsxAttrValue(attr.value, source);
        parts.push(`, {${key}: ${val}}`);
      }
    }
    parts.push(")");
    return parts.join("");
  }

  // Simple object literal
  const propParts: string[] = [];
  for (const attr of attributes) {
    if (attr.kind === "JSXSpreadAttribute") continue;
    const key = attr.name.kind === "JSXIdentifier" ? attr.name.name : `"${attr.name.namespace.name}:${attr.name.name.name}"`;
    const val = jsxAttrValue(attr.value, source);
    propParts.push(`${key}: ${val}`);
  }
  return `{${propParts.join(", ")}}`;
}

function jsxAttrValue(value: AST.JSXAttributeValue | null, source?: string): string {
  if (value === null) return "true"; // boolean attribute: <div disabled />
  if (value.kind === "StringLiteral") return stringLiteralToCode(value);
  if (value.kind === "JSXExpressionContainer") {
    if (value.expression.kind === "JSXEmptyExpression") return "undefined";
    return exprToCode(value.expression as AST.Expr, source);
  }
  if (value.kind === "JSXElement") return jsxToCreateElement(value, source);
  if (value.kind === "JSXFragment") return jsxFragmentToCreateElement(value, source);
  return "null";
}

function jsxChildToArg(child: AST.JSXChild, source?: string): string | null {
  switch (child.kind) {
    case "JSXText": {
      // Collapse whitespace-only text to null
      const text = child.value.replace(/^\s*\n\s*/g, "").replace(/\s*\n\s*$/g, "");
      if (!text.trim()) return null;
      return JSON.stringify(text);
    }
    case "JSXExpressionContainer":
      if (child.expression.kind === "JSXEmptyExpression") return null;
      return exprToCode(child.expression as AST.Expr, source);
    case "JSXSpreadChild":
      return `...${exprToCode(child.expression, source)}`;
    case "JSXElement":
      return jsxToCreateElement(child, source);
    case "JSXFragment":
      return jsxFragmentToCreateElement(child, source);
    default:
      return null;
  }
}

function jsxToCreateElement(node: AST.JSXElement, source?: string): string {
  const options = jsxLoweringOptions(source);
  const type = jsxElementNameToArg(node.openingElement.name);
  const props = jsxAttrToProps(node.openingElement.attributes, source);
  const children = node.children
    .map(c => jsxChildToArg(c, source))
    .filter((c): c is string => c !== null);

  if (children.length === 0) {
    return `${options.factory}(${type}, ${props})`;
  }
  return `${options.factory}(${type}, ${props}, ${children.join(", ")})`;
}

function jsxFragmentToCreateElement(node: AST.JSXFragment, source?: string): string {
  const options = jsxLoweringOptions(source);
  const children = node.children
    .map(c => jsxChildToArg(c, source))
    .filter((c): c is string => c !== null);

  if (children.length === 0) {
    return `${options.factory}(${options.fragment}, null)`;
  }
  return `${options.factory}(${options.fragment}, null, ${children.join(", ")})`;
}

// ─── Match Expression Lowering ─────────────────────────────────────

function matchToTernary(node: AST.Match, source?: string): string {
  const disc = exprToCode(node.expr, source);
  const arms = node.arms;

  // Build nested ternary chain from the end
  let result = "undefined"; // default if no wildcard

  // Process arms in reverse to build nested ternary
  for (let i = arms.length - 1; i >= 0; i--) {
    const arm = arms[i];
    const body = armBodyToCode(arm.body, source);

    // Check for wildcard/default pattern
    const isWildcard = arm.patterns.length === 1 &&
      arm.patterns[0].kind === "Identifier" &&
      (arm.patterns[0] as AST.Identifier).name === "_";

    if (isWildcard && !arm.guard) {
      result = body;
      continue;
    }

    // Build pattern test: (disc === p1 || disc === p2 || ...)
    const patternTests = arm.patterns.map(p => {
      if (p.kind === "Identifier" && (p as AST.Identifier).name === "_") {
        return "true";
      }
      return `(${disc} === ${exprToCode(p, source)})`;
    });
    let test = patternTests.length === 1
      ? patternTests[0]
      : `(${patternTests.join(" || ")})`;

    // Add guard clause
    if (arm.guard) {
      test = `(${test} && ${exprToCode(arm.guard, source)})`;
    }

    result = `(${test} ? ${body} : ${result})`;
  }

  return result;
}

function armBodyToCode(body: AST.Expr | AST.Block, source?: string): string {
  if ('kind' in body && body.kind === "Block") {
    const block = body as AST.Block;
    if (block.statements.length === 1 && block.statements[0].kind === "ExprStmt") {
      return exprToCode((block.statements[0] as AST.ExprStmt).expr, source);
    }
    // Multi-statement block → IIFE
    return `(() => { ${blockToCode(block, source)} })()`;
  }
  return exprToCode(body as AST.Expr, source);
}

// ─── Lambda Body Reconstruction ────────────────────────────────────

function lambdaToCode(expr: AST.Lambda, paramsCode: string, source?: string): string {
  const asyncPrefix = expr.async ? "async " : "";

  if ('kind' in expr.body && (expr.body as any).kind === "Block") {
    return `${asyncPrefix}(${paramsCode}) => { ${blockToCode(expr.body as AST.Block, source)} }`;
  }

  // Expression body — exprToCode handles JSX, Match, etc. Object literal
  // arrow bodies must be parenthesized or JavaScript parses them as blocks.
  const bodyCode = exprToCode(expr.body as AST.Expr, source);
  if ((expr.body as AST.Expr).kind === "ObjectLiteral") {
    return `${asyncPrefix}(${paramsCode}) => (${bodyCode})`;
  }
  return `${asyncPrefix}(${paramsCode}) => ${bodyCode}`;
}

function lambdaToPythonCode(expr: AST.Lambda, source?: string): string {
  const raw = spanExtract(expr, source)?.trim();
  if (raw?.startsWith("lambda ")) return raw;

  const params = expr.params.map(p => paramToCode(p, source)).join(", ");
  if ('kind' in expr.body && (expr.body as any).kind === "Block") {
    const block = expr.body as AST.Block;
    if (block.statements.length === 1 && block.statements[0].kind === "ExprStmt") {
      return `lambda ${params}: ${exprToPythonCode((block.statements[0] as AST.ExprStmt).expr, source)}`;
    }
    return raw || `lambda ${params}: None`;
  }

  return `lambda ${params}: ${exprToPythonCode(expr.body as AST.Expr, source)}`;
}

// ─── Identifier Collection (for captures analysis) ────────────────

/**
 * Collect free identifier names from an AST node.
 * Used for captures analysis — identifies variables that may need
 * to be injected from another runtime.
 *
 * Excludes: property names in Member expressions, parameter names
 * in Lambda/FuncDecl (they shadow outer scope).
 */
export function collectFreeIdentifiers(node: AST.Expr | AST.Stmt | AST.Decl): Set<string> {
  const ids = new Set<string>();
  collectIds(node, ids, new Set());
  return ids;
}

function collectIds(
  node: AST.Expr | AST.Stmt | AST.Decl,
  ids: Set<string>,
  locals: Set<string>,
): void {
  if (!node || typeof node !== "object" || !("kind" in node)) return;

  switch (node.kind) {
    case "Identifier":
      if (!locals.has(node.name)) ids.add(node.name);
      break;
    case "Member":
      // Only walk the object, not the property (property is not a free var)
      collectIds(node.object, ids, locals);
      break;
    case "Call":
      collectIds(node.callee, ids, locals);
      for (const arg of node.args) collectIds(arg, ids, locals);
      break;
    case "NewExpr":
      collectIds(node.callee, ids, locals);
      for (const arg of node.args) collectIds(arg, ids, locals);
      break;
    case "Binary":
      collectIds(node.left, ids, locals);
      collectIds(node.right, ids, locals);
      break;
    case "Unary":
      collectIds(node.argument, ids, locals);
      break;
    case "Index":
      collectIds(node.object, ids, locals);
      collectIds(node.index, ids, locals);
      break;
    case "Assign":
      collectIds(node.left, ids, locals);
      collectIds(node.right, ids, locals);
      break;
    case "Ternary":
      collectIds(node.test, ids, locals);
      collectIds(node.consequent, ids, locals);
      collectIds(node.alternate, ids, locals);
      break;
    case "ArrayLiteral":
      for (const el of node.elements) collectIds(el, ids, locals);
      break;
    case "ObjectLiteral":
      for (const p of node.properties) collectIds(p.value, ids, locals);
      break;
    case "Spread":
      collectIds(node.argument, ids, locals);
      break;
    case "Lambda": {
      // Lambda params shadow outer scope
      const innerLocals = new Set(locals);
      for (const p of node.params) {
        if (p.name.kind === "Identifier") innerLocals.add(p.name.name);
      }
      if ("kind" in node.body && (node.body as any).kind === "Block") {
        for (const s of (node.body as AST.Block).statements) collectIds(s, ids, innerLocals);
      } else {
        collectIds(node.body as AST.Expr, ids, innerLocals);
      }
      break;
    }
    case "ExprStmt":
      collectIds(node.expr, ids, locals);
      break;
    case "Return":
      for (const v of node.values) collectIds(v, ids, locals);
      break;
    case "ListComprehension":
      collectIds(node.expression, ids, locals);
      collectIds(node.iterable, ids, locals);
      if (node.filter) collectIds(node.filter, ids, locals);
      break;
    case "RuntimeTag":
      collectIds(node.expr, ids, locals);
      break;
    case "StringLiteral":
      for (const part of node.parts) {
        if (part.kind === "Interpolation" && typeof part.value !== "string") {
          collectIds(part.value as AST.Expr, ids, locals);
        }
      }
      break;
    case "JSXElement":
      // Walk JSX attributes and children for identifiers
      for (const attr of node.openingElement.attributes) {
        if (attr.kind === "JSXSpreadAttribute") {
          collectIds(attr.argument, ids, locals);
        } else if (attr.value && attr.value.kind === "JSXExpressionContainer") {
          if (attr.value.expression.kind !== "JSXEmptyExpression") {
            collectIds(attr.value.expression as AST.Expr, ids, locals);
          }
        }
      }
      for (const child of node.children) {
        if (child.kind === "JSXExpressionContainer" && child.expression.kind !== "JSXEmptyExpression") {
          collectIds(child.expression as AST.Expr, ids, locals);
        } else if (child.kind === "JSXElement") {
          collectIds(child, ids, locals);
        } else if (child.kind === "JSXFragment") {
          for (const fc of child.children) {
            if (fc.kind === "JSXExpressionContainer" && fc.expression.kind !== "JSXEmptyExpression") {
              collectIds(fc.expression as AST.Expr, ids, locals);
            }
          }
        } else if (child.kind === "JSXSpreadChild") {
          collectIds(child.expression, ids, locals);
        }
      }
      break;
    case "Match":
      collectIds(node.expr, ids, locals);
      for (const arm of node.arms) {
        if (arm.guard) collectIds(arm.guard, ids, locals);
        if ("kind" in arm.body && (arm.body as any).kind === "Block") {
          for (const s of (arm.body as AST.Block).statements) collectIds(s, ids, locals);
        } else {
          collectIds(arm.body as AST.Expr, ids, locals);
        }
      }
      break;
    case "Yield":
      if ((node as any).value) collectIds((node as any).value, ids, locals);
      break;
    case "Go":
      collectIds((node as any).expr, ids, locals);
      break;
    case "If": {
      const ifNode = node as AST.If;
      for (const arm of ifNode.arms) {
        collectIds(arm.test, ids, locals);
        for (const s of arm.body.statements) collectIds(s, ids, locals);
      }
      if (ifNode.elseBody) {
        for (const s of ifNode.elseBody.statements) collectIds(s, ids, locals);
      }
      break;
    }
    case "Loop": {
      const loopNode = node as AST.Loop;
      const loopLocals = new Set(locals);
      if (loopNode.variable) {
        if (loopNode.variable.kind === "Identifier") loopLocals.add(loopNode.variable.name);
      }
      if (loopNode.init) collectIds(loopNode.init, ids, loopLocals);
      if (loopNode.test) collectIds(loopNode.test, ids, loopLocals);
      if (loopNode.step) collectIds(loopNode.step, ids, loopLocals);
      if (loopNode.iterable) collectIds(loopNode.iterable, ids, locals); // iterable evaluated before variable binding
      for (const s of loopNode.body.statements) collectIds(s, ids, loopLocals);
      break;
    }
    case "Try": {
      const tryNode = node as AST.Try;
      for (const s of tryNode.body.statements) collectIds(s, ids, locals);
      for (const c of tryNode.catches) {
        const catchLocals = new Set(locals);
        if (c.param) catchLocals.add(c.param.name);
        for (const s of c.body.statements) collectIds(s, ids, catchLocals);
      }
      if (tryNode.finallyBody) {
        for (const s of tryNode.finallyBody.statements) collectIds(s, ids, locals);
      }
      break;
    }
    case "Switch": {
      const switchNode = node as AST.Switch;
      collectIds(switchNode.discriminant, ids, locals);
      for (const c of switchNode.cases) {
        for (const p of c.patterns) collectIds(p, ids, locals);
        for (const s of c.body.statements) collectIds(s, ids, locals);
      }
      if (switchNode.defaultCase) {
        for (const s of switchNode.defaultCase.statements) collectIds(s, ids, locals);
      }
      break;
    }
    case "Throw":
      collectIds((node as AST.Throw).value, ids, locals);
      if ((node as AST.Throw).cause) collectIds((node as AST.Throw).cause as AST.Expr, ids, locals);
      break;
    case "Defer": {
      const deferNode = node as AST.Defer;
      if ("kind" in deferNode.body && (deferNode.body as any).kind === "Block") {
        for (const s of (deferNode.body as AST.Block).statements) collectIds(s, ids, locals);
      } else {
        collectIds(deferNode.body as AST.Expr, ids, locals);
      }
      break;
    }
    case "Reassign":
      collectIds((node as AST.Reassign).name, ids, locals);
      collectIds((node as AST.Reassign).expr, ids, locals);
      break;
    case "Echo":
      for (const v of (node as AST.Echo).values) collectIds(v, ids, locals);
      break;
    case "VarDecl": {
      const varNode = node as AST.VarDecl;
      if (varNode.values) {
        for (const v of varNode.values) collectIds(v, ids, locals);
      }
      for (const n of varNode.names) locals.add(n.name);
      break;
    }
    case "ConstDecl": {
      const constNode = node as AST.ConstDecl;
      for (const v of constNode.values) collectIds(v, ids, locals);
      for (const n of constNode.names) locals.add(n.name);
      break;
    }
    case "ShortDecl": {
      const sdNode = node as AST.ShortDecl;
      if (sdNode.pairs) {
        for (const p of sdNode.pairs) {
          collectIds(p.expr, ids, locals);
          locals.add(p.name.name);
        }
      }
      if (sdNode.value) collectIds(sdNode.value, ids, locals);
      if (sdNode.targets) {
        for (const t of sdNode.targets) {
          if (t.kind === "Identifier") locals.add(t.name);
        }
      }
      break;
    }
    case "FuncDecl": {
      const funcNode = node as AST.FuncDecl;
      for (const decorator of funcNode.decorators || []) collectDecoratorIds(decorator, ids, locals);
      locals.add(funcNode.name.name);
      const funcLocals = new Set(locals);
      for (const p of funcNode.params) {
        if (p.name.kind === "Identifier") funcLocals.add(p.name.name);
      }
      for (const s of funcNode.body.statements) collectIds(s, ids, funcLocals);
      break;
    }
    case "ClassDecl": {
      const classNode = node as AST.ClassDecl;
      for (const decorator of classNode.decorators || []) collectDecoratorIds(decorator, ids, locals);
      locals.add(classNode.name.name);
      const classLocals = new Set(locals);
      for (const member of classNode.members) {
        for (const decorator of member.decorators || []) collectDecoratorIds(decorator, ids, classLocals);
        const memberLocals = new Set(classLocals);
        for (const param of member.params || []) {
          if (param.name.kind === "Identifier") memberLocals.add(param.name.name);
        }
        if (member.name) memberLocals.add(member.name.name);
        if (member.body) {
          for (const stmt of member.body.statements) collectIds(stmt, ids, memberLocals);
        }
      }
      break;
    }
    // For other node kinds, don't try to walk — they use span extraction anyway
    default:
      break;
  }
}

function collectDecoratorIds(
  decorator: AST.Decorator,
  ids: Set<string>,
  locals: Set<string>,
): void {
  if (decorator.expression) {
    collectIds(decorator.expression, ids, locals);
    return;
  }
  collectIds(decorator.name, ids, locals);
  for (const arg of decorator.args || []) collectIds(arg, ids, locals);
}

export function isExprKind(kind: string): boolean {
  return [
    "NumericLiteral", "StringLiteral", "RegexLiteral", "BooleanLiteral",
    "NullLiteral", "Identifier", "NewExpr", "Call", "Index", "Member", "Unary",
    "Binary", "Assign", "Lambda", "Ternary", "ArrayLiteral", "SetLiteral",
    "ObjectLiteral", "ListComprehension", "Spread", "Yield", "TypeAssertion",
    "JSXElement", "JSXFragment", "Match", "RuntimeTag",
  ].includes(kind);
}
