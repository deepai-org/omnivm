import * as AST from '../ast';
import { OmniRuntime, RuntimeAffinity } from '../runtime-resolver/types';
import { BridgeEmitter } from './bridge-emitter';

/**
 * Generates code for polyglot string interpolation.
 *
 * Template literals and f-strings with cross-runtime interpolation
 * decompose into string concatenation of individual bridge calls.
 * Each ${} segment that crosses a runtime boundary becomes a
 * separate `omnivm.call()`.
 *
 * Example:
 *   `User: ${@py(db.get_user().name)}, Items: ${@rb(inventory.count)}`
 * becomes:
 *   "User: " + omnivm.call("python", `db.get_user().name`)
 *   + ", Items: " + omnivm.call("ruby", `inventory.count`)
 */
export class StringInterpolationEmitter {
  private bridgeEmitter: BridgeEmitter;
  private affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>;
  private scopeRuntime: OmniRuntime;

  constructor(
    bridgeEmitter: BridgeEmitter,
    affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>,
    scopeRuntime: OmniRuntime = OmniRuntime.JavaScript,
  ) {
    this.bridgeEmitter = bridgeEmitter;
    this.affinityMap = affinityMap;
    this.scopeRuntime = scopeRuntime;
  }

  /**
   * Generate code for a string literal with potential cross-runtime interpolations.
   *
   * @returns A JS expression string that evaluates to the interpolated string.
   */
  emit(node: AST.StringLiteral): string {
    const hasInterpolations = node.parts.some(p => p.kind === "Interpolation");

    if (!hasInterpolations) {
      // Simple string — no cross-runtime concerns
      const text = node.parts
        .filter(p => p.kind === "Text")
        .map(p => p.value as string)
        .join("");
      return JSON.stringify(text);
    }

    // Build concatenation of segments
    const segments: string[] = [];

    for (const part of node.parts) {
      if (part.kind === "Text") {
        const text = part.value as string;
        if (text) {
          segments.push(JSON.stringify(text));
        }
      } else {
        // Interpolation
        segments.push(this.emitInterpolation(part));
      }
    }

    if (segments.length === 0) return '""';
    if (segments.length === 1) return segments[0];
    return segments.join(" + ");
  }

  private emitInterpolation(part: AST.StringPart): string {
    if (typeof part.value === "string") {
      // String-based interpolation expression (not parsed as AST)
      return this.emitStringInterpolation(part.value);
    }

    // AST-based interpolation expression
    const expr = part.value as AST.Expr;
    return this.emitExprInterpolation(expr);
  }

  private emitStringInterpolation(exprStr: string): string {
    // Check for @runtime() markers
    const runtimeTagMatch = exprStr.match(/^@(py|js|go|rb|java)\((.+)\)$/s);

    if (runtimeTagMatch) {
      const [, runtimeTag, innerExpr] = runtimeTagMatch;
      const runtime = this.tagToRuntime(runtimeTag);

      if (runtime === OmniRuntime.JavaScript || runtime === this.scopeRuntime) {
        // Same runtime — just inline it
        return `\${${innerExpr}}`;
      }

      // Cross-runtime — emit bridge call
      return this.bridgeEmitter.emitCall(runtime, innerExpr);
    }

    // No runtime tag — use scope runtime
    if (this.scopeRuntime === OmniRuntime.JavaScript) {
      return `\${${exprStr}}`;
    }

    // Non-JS scope — bridge to scope runtime
    return this.bridgeEmitter.emitCall(this.scopeRuntime, exprStr);
  }

  private emitExprInterpolation(expr: AST.Expr): string {
    // If it's a RuntimeTag node, use its explicit runtime
    if (expr.kind === "RuntimeTag") {
      const runtime = this.tagToRuntime(expr.runtime);
      const innerCode = this.exprToCode(expr.expr);

      if (runtime === OmniRuntime.JavaScript) {
        return innerCode;
      }

      return this.bridgeEmitter.emitCall(runtime, innerCode);
    }

    // Check the affinity map for this expression
    const affinity = this.affinityMap.get(expr);
    const exprRuntime = affinity?.runtime || this.scopeRuntime;

    const code = this.exprToCode(expr);

    if (exprRuntime === OmniRuntime.JavaScript) {
      return code;
    }

    // Need a bridge call
    return this.bridgeEmitter.emitCall(exprRuntime, code);
  }

  /**
   * Convert an AST expression back to source code.
   * This is a simplified reconstruction — real implementation
   * would use the transpiler.
   */
  private exprToCode(expr: AST.Expr): string {
    switch (expr.kind) {
      case "Identifier":
        return expr.name;
      case "NumericLiteral":
        return expr.raw;
      case "StringLiteral":
        return JSON.stringify(
          expr.parts
            .filter(p => p.kind === "Text")
            .map(p => p.value)
            .join("")
        );
      case "BooleanLiteral":
        return String(expr.value);
      case "NullLiteral":
        return "null";
      case "Member": {
        const dot = expr.optional ? "?." : ".";
        return `${this.exprToCode(expr.object)}${dot}${expr.property.name}`;
      }
      case "Call": {
        const args = expr.args.map(a => this.exprToCode(a)).join(", ");
        const optional = expr.optional ? "?." : "";
        return `${this.exprToCode(expr.callee)}${optional}(${args})`;
      }
      case "NewExpr": {
        const args = expr.args.map(a => this.exprToCode(a)).join(", ");
        return `new ${this.exprToCode(expr.callee)}(${args})`;
      }
      case "Binary":
        return `${this.exprToCode(expr.left)} ${expr.op} ${this.exprToCode(expr.right)}`;
      case "Index": {
        const bracket = expr.optional ? "?.[" : "[";
        return `${this.exprToCode(expr.object)}${bracket}${this.exprToCode(expr.index)}]`;
      }
      default:
        return "/* unsupported expr */";
    }
  }

  private tagToRuntime(tag: string): OmniRuntime {
    switch (tag) {
      case "py": return OmniRuntime.Python;
      case "js": return OmniRuntime.JavaScript;
      case "go": return OmniRuntime.Go;
      case "rb": return OmniRuntime.Ruby;
      case "java": return OmniRuntime.Java;
      default: return OmniRuntime.JavaScript;
    }
  }
}
