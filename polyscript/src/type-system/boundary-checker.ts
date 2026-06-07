/**
 * Boundary Checker — Validates types at cross-runtime bridge points.
 *
 * Integrates with the Runtime Resolver output to find every point where
 * data flows from one runtime to another, then checks type compatibility
 * and emits the necessary bridge operations.
 *
 * Integration point: runs AFTER RuntimeResolver, enriches the manifest
 * with type-aware bridging instructions.
 */

import * as C from './canonical';
import { checkCompatibility, CoercionResult, BridgeOp, RuntimeGuard } from './coercion';
import { lowerType } from './lowering';
import * as AST from '../ast';

type Runtime = "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";

/**
 * A binding in the type environment: a name with a resolved canonical type
 * and the runtime it lives in.
 */
export interface TypedBinding {
  name: string;
  type: C.CanonicalType;
  runtime: Runtime;
  mutable?: boolean;
}

/**
 * A boundary crossing: data flows from one runtime to another.
 */
export interface BoundaryCrossing {
  binding: string;              // Name of the value being passed
  from: { runtime: Runtime; type: C.CanonicalType };
  to: { runtime: Runtime; type: C.CanonicalType };
  result: CoercionResult;
  location?: AST.Span;
}

/**
 * Diagnostic emitted when a boundary crossing is problematic.
 */
export interface TypeDiagnostic {
  severity: "error" | "warning" | "info";
  message: string;
  location?: AST.Span;
  crossing: BoundaryCrossing;
}

/**
 * The Boundary Checker maintains a type environment and validates crossings.
 */
export class BoundaryChecker {
  private bindings: Map<string, TypedBinding> = new Map();
  private crossings: BoundaryCrossing[] = [];
  private diagnostics: TypeDiagnostic[] = [];
  /** Stack of narrowing scopes — each scope maps binding names to narrowed types. */
  private narrowScopes: Map<string, C.CanonicalType>[] = [];

  /**
   * Register a binding in the type environment.
   */
  declare(name: string, type: C.CanonicalType, runtime: Runtime, mutable?: boolean): void {
    this.bindings.set(name, { name, type, runtime, mutable });
  }

  /**
   * Register a binding from an AST TypeNode (convenience).
   */
  declareFromAST(name: string, typeNode: AST.TypeNode | undefined, runtime: Runtime): void {
    const type = typeNode ? lowerType(typeNode, runtime) : C.ANY;
    this.declare(name, type, runtime);
  }

  /**
   * Look up a binding by name (for external callers that need type info).
   */
  getBinding(name: string): TypedBinding | undefined {
    return this.bindings.get(name);
  }

  /**
   * Get the effective type of a binding, considering narrowing scopes.
   */
  getEffectiveType(name: string): C.CanonicalType | undefined {
    // Check narrowing scopes (innermost first)
    for (let i = this.narrowScopes.length - 1; i >= 0; i--) {
      const narrowed = this.narrowScopes[i].get(name);
      if (narrowed) return narrowed;
    }
    return this.bindings.get(name)?.type;
  }

  /**
   * Push a narrowing scope — narrows Option<T> to T, union types, etc.
   * Used when entering an if-branch that null-checks a binding.
   */
  pushNarrow(narrowings: Map<string, C.CanonicalType>): void {
    this.narrowScopes.push(narrowings);
  }

  /**
   * Pop the current narrowing scope (when leaving the if-branch).
   */
  popNarrow(): void {
    this.narrowScopes.pop();
  }

  /**
   * Narrow a binding's type for the current scope.
   * Returns the narrowed type, or undefined if narrowing doesn't apply.
   */
  static narrowType(type: C.CanonicalType, guard: "not-null" | "not-undefined"): C.CanonicalType | undefined {
    if (type.kind === "option") return (type as C.OptionType).inner;
    if (type.kind === "union") {
      const members = (type as C.UnionType).members.filter(m => m.kind !== "null");
      if (members.length === 1) return members[0];
      if (members.length > 1 && members.length < (type as C.UnionType).members.length) {
        return { kind: "union", members };
      }
    }
    return undefined;
  }

  /**
   * Check a boundary crossing: binding `name` is used in `targetRuntime`
   * where it's expected to have type `expectedType`.
   *
   * @param fromTypeOverride If provided, use this as the source type instead of
   *   the binding's declared type. Useful for checking a function's return type
   *   rather than the function type itself.
   */
  checkCrossing(
    name: string,
    targetRuntime: Runtime,
    expectedType?: C.CanonicalType,
    location?: AST.Span,
    fromTypeOverride?: C.CanonicalType,
  ): CoercionResult {
    const binding = this.bindings.get(name);
    if (!binding && !fromTypeOverride) {
      // Unknown binding — can't check, assume ok
      return { compat: "safe" };
    }

    const fromRuntime = binding?.runtime;
    // Same runtime — no boundary crossing
    if (fromRuntime && fromRuntime === targetRuntime) {
      return { compat: "safe" };
    }

    const from = fromTypeOverride || this.getEffectiveType(name) || binding!.type;
    const to = expectedType || C.ANY;
    const result = checkCompatibility(from, to);

    const crossing: BoundaryCrossing = {
      binding: name,
      from: { runtime: fromRuntime || targetRuntime, type: from },
      to: { runtime: targetRuntime, type: to },
      result,
      location,
    };

    this.crossings.push(crossing);

    // Emit diagnostics
    if (result.compat === "incompatible") {
      this.diagnostics.push({
        severity: "error",
        message: `Type error at boundary: '${name}' (${typeToString(from)} in ${fromRuntime || '?'}) ` +
                 `cannot flow to ${targetRuntime} as ${typeToString(to)}: ${result.reason}`,
        location,
        crossing,
      });
    } else if (result.compat === "check") {
      this.diagnostics.push({
        severity: "warning",
        message: `Runtime check needed: '${name}' (${typeToString(from)} in ${fromRuntime || '?'}) ` +
                 `flowing to ${targetRuntime} may fail: ${result.reason}`,
        location,
        crossing,
      });
    }

    return result;
  }

  /**
   * Check a function call crossing: a function defined in one runtime
   * is called from another with given argument types.
   */
  checkCallCrossing(
    funcName: string,
    callerRuntime: Runtime,
    argTypes: C.CanonicalType[],
    location?: AST.Span
  ): CoercionResult[] {
    const binding = this.bindings.get(funcName);
    if (!binding || binding.type.kind !== "func") return [];
    if (binding.runtime === callerRuntime) return [];

    const funcType = binding.type as C.FuncType;
    const results: CoercionResult[] = [];

    for (let i = 0; i < argTypes.length && i < funcType.params.length; i++) {
      const result = checkCompatibility(argTypes[i], funcType.params[i].type);
      results.push(result);

      if (result.compat === "incompatible") {
        this.diagnostics.push({
          severity: "error",
          message: `Arg ${i} of ${funcName}(): ${typeToString(argTypes[i])} (${callerRuntime}) ` +
                   `incompatible with param ${typeToString(funcType.params[i].type)} (${binding.runtime})`,
          location,
          crossing: {
            binding: funcName,
            from: { runtime: callerRuntime, type: argTypes[i] },
            to: { runtime: binding.runtime, type: funcType.params[i].type },
            result,
            location,
          },
        });
      }
    }

    return results;
  }

  /**
   * Get all boundary crossings found so far.
   */
  getCrossings(): BoundaryCrossing[] {
    return this.crossings;
  }

  /**
   * Get all diagnostics (errors, warnings).
   */
  getDiagnostics(): TypeDiagnostic[] {
    return this.diagnostics;
  }

  /**
   * Get only errors.
   */
  getErrors(): TypeDiagnostic[] {
    return this.diagnostics.filter(d => d.severity === "error");
  }

  /**
   * Get bridge operations needed for all crossings.
   */
  getBridgeOps(): { binding: string; op: BridgeOp }[] {
    return this.crossings
      .filter(c => c.result.bridgeOp)
      .map(c => ({ binding: c.binding, op: c.result.bridgeOp! }));
  }

  /**
   * Summary of the type checking pass.
   */
  getSummary(): { crossings: number; safe: number; coerce: number; check: number; errors: number } {
    return {
      crossings: this.crossings.length,
      safe: this.crossings.filter(c => c.result.compat === "safe").length,
      coerce: this.crossings.filter(c => c.result.compat === "coerce").length,
      check: this.crossings.filter(c => c.result.compat === "check").length,
      errors: this.crossings.filter(c => c.result.compat === "incompatible").length,
    };
  }
}

// ---- Display Helpers ----

export function typeToString(type: C.CanonicalType): string {
  switch (type.kind) {
    case "int": return `${type.signed ? "i" : "u"}${type.size}`;
    case "float": return `f${type.size}`;
    case "bool": return "bool";
    case "string": return "string";
    case "bytes": return "bytes";
    case "void": return "void";
    case "never": return "never";
    case "any": return "any";
    case "null": return "null";
    case "option": return `Option<${typeToString(type.inner)}>`;
    case "result": return `Result<${typeToString(type.ok)}, ${typeToString(type.err)}>`;
    case "array": return `Array<${typeToString(type.element)}>`;
    case "map": return `Map<${typeToString(type.key)}, ${typeToString(type.value)}>`;
    case "set": return `Set<${typeToString(type.element)}>`;
    case "channel": return `Chan<${typeToString(type.element)}>`;
    case "async": return `Async<${typeToString(type.inner)}>`;
    case "func": {
      const params = type.params.map(p => typeToString(p.type)).join(", ");
      return `(${params}) => ${typeToString(type.returns)}`;
    }
    case "struct": return type.name || "{...}";
    case "tuple": return `(${type.elements.map(typeToString).join(", ")})`;
    case "union": return type.members.map(typeToString).join(" | ");
    case "enum": return type.name;
    case "ref": return `&${type.mutable ? "mut " : ""}${typeToString(type.inner)}`;
    case "typevar": return type.name;
    case "generic": return `${typeToString(type.base)}<${type.args.map(typeToString).join(", ")}>`;
    case "interface": return type.name || `interface{${type.methods.map(m => m.name).join(", ")}}`;
    case "stream": return `Stream<${typeToString(type.element)}>`;
    case "buffer_view": return `BufferView<${typeToString(type.element)}>`;
    case "disposable": return `Disposable<${typeToString(type.inner)}>`;
    case "literal": return JSON.stringify(type.value);
  }
}
