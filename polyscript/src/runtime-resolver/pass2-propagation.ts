import * as AST from '../ast';
import {
  OmniRuntime,
  RuntimeAffinity,
  AffinityEvidence,
  BridgeDescriptor,
  MarshalKind,
  AnnotatedNode,
} from './types';
import { SymbolTable } from './symbol-table';
import { lookupGlobalAffinity, lookupMethodAffinity, lookupQualifiedGlobalAffinity } from './method-tables';
import { computeBridgeCost } from './cost-model';
import { affinityFromEvidence, chooseRuntime, EVIDENCE_WEIGHTS } from './evidence';

/**
 * Pass 2: Bottom-up affinity propagation.
 *
 * Propagates runtime affinities through expression chains,
 * handles method name lookups, async infection, and inserts
 * bridge descriptors at runtime boundaries.
 */
export class Pass2Propagation {
  private affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>;
  private symbolTable: SymbolTable;
  private bridges: BridgeDescriptor[] = [];
  private defaultRuntime: OmniRuntime;
  private scopeRuntimeStack: OmniRuntime[] = [];

  constructor(
    affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>,
    symbolTable: SymbolTable,
    defaultRuntime: OmniRuntime = OmniRuntime.JavaScript,
  ) {
    this.affinityMap = affinityMap;
    this.symbolTable = symbolTable;
    this.defaultRuntime = defaultRuntime;
  }

  /**
   * Run Pass 2. Returns the bridge descriptors inserted.
   */
  run(program: AST.Program): BridgeDescriptor[] {
    for (const node of program.body) {
      this.propagateNode(node);
    }
    return this.bridges;
  }

  /**
   * Get the updated affinity map.
   */
  getAffinityMap(): Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity> {
    return this.affinityMap;
  }

  private propagateNode(node: AST.Decl | AST.Stmt | AST.Expr): RuntimeAffinity {
    switch (node.kind) {
      case "ExprStmt": {
        const exprAff = this.propagateExpr(node.expr);
        // Inherit the expression's affinity for the statement wrapper
        if (exprAff && exprAff.confidence !== "fallback") {
          this.affinityMap.set(node, { ...exprAff });
        }
        return this.getOrDefault(node);
      }

      case "Echo": {
        let echoArgAff: RuntimeAffinity | undefined;
        for (const v of node.values) echoArgAff = this.propagateExpr(v);
        // Inherit arg affinity if Echo wasn't already tagged (e.g. by f-string)
        const echoExisting = this.affinityMap.get(node);
        if (echoArgAff && echoArgAff.confidence !== "fallback" &&
            (!echoExisting || echoExisting.confidence === "fallback" ||
            (echoExisting.confidence === "inferred" && echoExisting.evidence[0]?.type === "scope"))) {
          this.affinityMap.set(node, { ...echoArgAff });
        }
        return this.getOrDefault(node);
      }

      case "FuncDecl":
        return this.propagateFuncDecl(node);

      case "If":
        let bestIfAff: RuntimeAffinity | undefined;
        for (const arm of node.arms) {
          const testAff = this.propagateExpr(arm.test);
          const bodyAff = this.propagateBlock(arm.body);
          bestIfAff = this.preferRuntimeAffinity(bestIfAff, bodyAff);
          bestIfAff = this.preferRuntimeAffinity(bestIfAff, testAff);
        }
        if (node.elseBody) bestIfAff = this.preferRuntimeAffinity(bestIfAff, this.propagateBlock(node.elseBody));
        if (bestIfAff) {
          const ifExisting = this.affinityMap.get(node);
          if (!ifExisting || this.isWeakScopeAffinity(ifExisting) || ifExisting.confidence === "fallback") {
            this.affinityMap.set(node, { ...bestIfAff });
          }
        }
        return this.getOrDefault(node);

      case "Loop": {
        if (node.test) this.propagateExpr(node.test);
        let loopIterAff: RuntimeAffinity | undefined;
        if (node.iterable) loopIterAff = this.propagateExpr(node.iterable);
        const loopBodyAff = this.propagateBlock(node.body);
        // The iterable may be foreign. Keep loop execution anchored to body/control
        // syntax when present, and leave iterable affinity attached to the iterable
        // expression for bridge/capture analysis.
        const loopExisting = this.affinityMap.get(node);
        const bestLoopAff = this.preferRuntimeAffinity(loopBodyAff, loopIterAff);
        if (bestLoopAff && (!loopExisting || loopExisting.confidence === "fallback" || this.isWeakScopeAffinity(loopExisting))) {
          this.affinityMap.set(node, { ...bestLoopAff });
        }
        return this.getOrDefault(node);
      }

      case "Switch":
        this.propagateExpr(node.discriminant);
        for (const c of node.cases) {
          for (const p of c.patterns) this.propagateExpr(p);
          if (c.guard) this.propagateExpr(c.guard);
          this.propagateBlock(c.body);
        }
        if (node.defaultCase) this.propagateBlock(node.defaultCase);
        return this.getOrDefault(node);

      case "Match":
        return this.propagateMatch(node);

      case "Return":
        for (const v of node.values) this.propagateExpr(v);
        return this.getOrDefault(node);

      case "Throw":
        this.propagateExpr(node.value);
        if (node.cause) this.propagateExpr(node.cause);
        return this.getOrDefault(node);

      case "Break":
      case "Continue": {
        const controlExisting = this.affinityMap.get(node);
        if (!controlExisting || controlExisting.confidence === "fallback" || this.isWeakScopeAffinity(controlExisting)) {
          const scopeRuntime = this.scopeRuntimeStack.length > 0
            ? this.scopeRuntimeStack[this.scopeRuntimeStack.length - 1]
            : undefined;
          if (scopeRuntime) {
            this.affinityMap.set(node, {
              runtime: scopeRuntime,
              confidence: "inferred",
              evidence: [{ type: "scope", detail: `control flow in ${scopeRuntime} block` }],
            });
          }
        }
        return this.getOrDefault(node);
      }

      case "Try":
        this.propagateBlock(node.body);
        for (const c of node.catches) this.propagateCatchClause(c);
        if (node.finallyBody) this.propagateBlock(node.finallyBody);
        return this.getOrDefault(node);

      case "Using": {
        const resourceAff = this.propagateNode(node.resource);
        const bodyAff = this.propagateBlock(node.body);
        const usingExisting = this.affinityMap.get(node);
        const bestUsingAff = resourceAff.confidence !== "fallback" ? resourceAff :
                             bodyAff.confidence !== "fallback" ? bodyAff : undefined;
        if (bestUsingAff && (!usingExisting || usingExisting.confidence === "fallback" ||
            (usingExisting.confidence === "inferred" && usingExisting.evidence[0]?.type === "scope"))) {
          this.affinityMap.set(node, { ...bestUsingAff });
        }
        return this.getOrDefault(node);
      }

      case "Block":
        return this.propagateBlock(node);

      case "VarDecl": {
        let valAff: RuntimeAffinity | undefined;
        if (node.values) {
          for (const v of node.values) valAff = this.propagateExpr(v);
        }
        // Inherit value's runtime if declaration only has scope fallback
        const varExisting = this.affinityMap.get(node);
        if (this.isMeaningfulAffinity(valAff) &&
            (!varExisting || varExisting.confidence === "fallback" ||
            this.isWeakScopeAffinity(varExisting))) {
          this.affinityMap.set(node, { ...valAff });
        } else if (!varExisting || varExisting.confidence === "fallback" || this.isWeakScopeAffinity(varExisting)) {
          this.affinityMap.set(node, {
            runtime: this.defaultRuntime,
            confidence: "fallback",
            evidence: [{ type: "fallback", detail: "neutral declaration literal" }],
          });
        }
        const varAff = this.getOrDefault(node);
        if (node.destructurePattern) {
          this.defineDeclaredNames(node.names, varAff, node);
        } else if (this.isMeaningfulAffinity(valAff)) {
          this.defineDeclaredNames(node.names, valAff, node);
        } else if (varAff.runtime === OmniRuntime.JavaScript) {
          this.defineDeclaredNames(node.names, varAff, node);
        }
        return this.getOrDefault(node);
      }

      case "ConstDecl": {
        let constValAff: RuntimeAffinity | undefined;
        for (const v of node.values) constValAff = this.propagateExpr(v);
        // Inherit value's runtime if declaration only has scope fallback
        const constExisting = this.affinityMap.get(node);
        if (this.isMeaningfulAffinity(constValAff) &&
            (!constExisting || constExisting.confidence === "fallback" ||
            this.isWeakScopeAffinity(constExisting))) {
          this.affinityMap.set(node, { ...constValAff });
        } else if (!constExisting || constExisting.confidence === "fallback" || this.isWeakScopeAffinity(constExisting)) {
          this.affinityMap.set(node, {
            runtime: this.defaultRuntime,
            confidence: "fallback",
            evidence: [{ type: "fallback", detail: "neutral declaration literal" }],
          });
        }
        const constAff = this.getOrDefault(node);
        if (node.destructurePattern) {
          this.defineDeclaredNames(node.names, constAff, node);
        } else if (this.isMeaningfulAffinity(constValAff)) {
          this.defineDeclaredNames(node.names, constValAff, node);
        } else if (constAff.runtime === OmniRuntime.JavaScript) {
          this.defineDeclaredNames(node.names, constAff, node);
        }
        return this.getOrDefault(node);
      }

      case "Reassign": {
        const exprAff = this.propagateExpr(node.expr);
        if (exprAff && exprAff.confidence !== "fallback") {
          this.affinityMap.set(node, { ...exprAff });
          this.symbolTable.define(node.name.name, {
            name: node.name.name,
            affinity: { ...exprAff },
            declNode: node,
          });
        }
        return this.getOrDefault(node);
      }

      case "ExportDecl":
        if (node.declaration) return this.propagateNode(node.declaration);
        return this.getOrDefault(node);

      case "Import":
      case "ImportDecl":
        return this.getOrDefault(node);

      case "GroupedImport": {
        for (const imported of node.imports) {
          this.propagateNode(imported);
        }
        return this.getOrDefault(node);
      }

      case "ClassDecl":
        for (const decorator of node.decorators || []) {
          this.propagateDecorator(decorator);
        }
        for (const member of node.members) {
          for (const decorator of member.decorators || []) {
            this.propagateDecorator(decorator);
          }
        }
        for (const member of node.members) {
          if (member.body) this.propagateBlock(member.body);
        }
        this.propagateClassFromDecorators(node);
        return this.getOrDefault(node);

      case "ImplDecl":
        for (const member of node.members) {
          if (member.body) this.propagateBlock(member.body);
        }
        return this.getOrDefault(node);

      default:
        // Expression nodes
        if (this.isExpr(node)) {
          return this.propagateExpr(node as AST.Expr);
        }
        return this.getOrDefault(node);
    }
  }

  private propagateExpr(expr: AST.Expr): RuntimeAffinity {
    switch (expr.kind) {
      case "Call":
        return this.propagateCall(expr);

      case "NewExpr":
        return this.propagateNewExpr(expr);

      case "Member":
        return this.propagateMember(expr);

      case "Binary":
        return this.propagateBinary(expr);

      case "Unary":
        return this.propagateUnary(expr);

      case "Assign":
        const rightAff = this.propagateExpr(expr.right);
        if (expr.left.kind === "Identifier" &&
            this.isNeutralAggregateExpr(expr.right) &&
            !this.isMeaningfulAffinity(rightAff)) {
          const neutralAff: RuntimeAffinity = {
            runtime: this.defaultRuntime,
            confidence: "fallback",
            evidence: [{ type: "fallback", detail: "neutral declaration literal" }],
          };
          this.affinityMap.set(expr, { ...neutralAff });
          this.affinityMap.set(expr.left, { ...neutralAff });
          this.affinityMap.set(expr.right, { ...neutralAff });
          this.symbolTable.define(expr.left.name, {
            name: expr.left.name,
            affinity: { ...neutralAff },
            declNode: expr,
          });
          return neutralAff;
        }
        if (expr.left.kind === "Identifier" && rightAff.confidence !== "fallback") {
          this.affinityMap.set(expr.left, { ...rightAff });
          this.symbolTable.define(expr.left.name, {
            name: expr.left.name,
            affinity: { ...rightAff },
            declNode: expr,
          });
        } else {
          this.propagateExpr(expr.left);
        }
        this.ensureAffinity(expr, rightAff);
        return rightAff;

      case "Ternary":
        this.propagateExpr(expr.test);
        const consAff = this.propagateExpr(expr.consequent);
        this.propagateExpr(expr.alternate);
        this.ensureAffinity(expr, consAff);
        return this.getOrDefault(expr);

      case "Lambda":
        return this.propagateLambda(expr);

      case "ArrayLiteral":
        {
          let arrayAff: RuntimeAffinity | undefined;
          for (const el of expr.elements) {
            arrayAff = this.preferAggregateValueAffinity(arrayAff, this.propagateExpr(el));
          }
          if (this.isMeaningfulAffinity(arrayAff)) {
            const existing = this.affinityMap.get(expr);
            if (!existing || existing.confidence === "fallback" || this.isWeakScopeAffinity(existing)) {
              this.affinityMap.set(expr, {
                ...arrayAff,
                evidence: [
                  { type: "scope", detail: `array literal value: ${arrayAff.runtime}` },
                  ...arrayAff.evidence,
                ],
              });
            }
          }
        }
        return this.getOrDefault(expr);

      case "ObjectLiteral":
        {
          let objectAff: RuntimeAffinity | undefined;
          for (const prop of expr.properties) {
            objectAff = this.preferAggregateValueAffinity(objectAff, this.propagateExpr(prop.value));
          }
          if (this.isMeaningfulAffinity(objectAff)) {
            const existing = this.affinityMap.get(expr);
            if (!existing || existing.confidence === "fallback" || this.isWeakScopeAffinity(existing)) {
              this.affinityMap.set(expr, {
                ...objectAff,
                evidence: [
                  { type: "scope", detail: `object literal value: ${objectAff.runtime}` },
                  ...objectAff.evidence,
                ],
              });
            }
          }
        }
        return this.getOrDefault(expr);

      case "Spread":
        return this.propagateExpr(expr.argument);

      case "Yield":
        if (expr.value) this.propagateExpr(expr.value);
        return this.getOrDefault(expr);

      case "TypeAssertion":
        return this.propagateExpr(expr.expr);

      case "Index":
        const objectAff = this.propagateExpr(expr.object);
        this.propagateExpr(expr.index);
        if (objectAff.confidence !== "fallback") {
          this.ensureAffinity(expr, objectAff);
        }
        return this.getOrDefault(expr);

      case "ListComprehension":
        this.propagateExpr(expr.expression);
        this.propagateExpr(expr.iterable);
        if (expr.filter) this.propagateExpr(expr.filter);
        return this.getOrDefault(expr);

      case "Match":
        return this.propagateMatch(expr);

      case "RuntimeTag":
        this.propagateExpr(expr.expr);
        return this.getOrDefault(expr);

      case "Go":
        this.propagateExpr(expr.expr);
        return this.getOrDefault(expr);

      case "Identifier":
        return this.propagateIdentifier(expr);

      case "StringLiteral":
        return this.propagateStringLiteral(expr);

      default:
        return this.getOrDefault(expr);
    }
  }

  private propagateCall(node: AST.Call): RuntimeAffinity {
    const calleeAff = this.propagateExpr(node.callee);
    for (const arg of node.args) {
      this.propagateExpr(arg);
    }

    // If callee has a known affinity, the call inherits it
    const existing = this.affinityMap.get(node);
    if (calleeAff.confidence !== "fallback" && (!existing || existing.confidence === "fallback" ||
        (existing.confidence === "inferred" && existing.evidence[0]?.type === "scope"))) {
      this.affinityMap.set(node, {
        ...calleeAff,
        evidence: [
          { type: "scope", detail: `inherited from callee: ${calleeAff.runtime}` },
          ...calleeAff.evidence,
        ],
      });
    }

    // --- Syntactic Dominance Rule ---
    // If any argument has "syntax" evidence, that language wins the call context.
    // This handles cases like files.map(x => x) where files is Python-bound
    // but the arrow function is syntactically impossible in Python.
    const syntaxVotes = new Map<OmniRuntime, number>();
    for (const arg of node.args) {
      const argAff = this.affinityMap.get(arg);
      if (argAff) {
        const hasSyntaxEvidence = arg.kind !== "Identifier" && argAff.evidence[0]?.type === "syntax";
        if (hasSyntaxEvidence) {
          syntaxVotes.set(argAff.runtime, (syntaxVotes.get(argAff.runtime) || 0) + 1);
        }
      }
    }

    if (syntaxVotes.size === 1 && existing?.confidence !== "definite") {
      // Exactly one language has syntax evidence — it wins
      const [winnerRuntime] = syntaxVotes.keys();
      this.affinityMap.set(node, {
        runtime: winnerRuntime,
        confidence: "definite",
        evidence: [
          { type: "syntax", detail: "syntactic dominance: argument syntax overrides callee provenance" },
          ...calleeAff.evidence,
        ],
      });
    }
    // If zero syntax votes: callee provenance already set above
    // If multiple syntax votes (collision): callee provenance breaks tie (already set above)

    const nodeAff = this.getOrDefault(node);
    if (this.isMeaningfulAffinity(nodeAff)) {
      for (const arg of node.args) {
        if (arg.kind === "Identifier") {
          this.refineNeutralAggregateIdentifier(arg, nodeAff.runtime, {
            type: "scope",
            detail: `neutral aggregate adopted by ${nodeAff.runtime} call argument`,
          });
        }
      }
    }

    // Check for async: if this is an await expression wrapping a call
    // (handled at the Unary level for 'await' operator)

    // Insert bridge if callee runtime differs from call runtime
    if (calleeAff.runtime !== nodeAff.runtime) {
      this.insertBridge(calleeAff.runtime, nodeAff.runtime, this.inferMarshalKind(node));
    }

    // Check args for bridge crossings
    for (const arg of node.args) {
      const argAff = this.getOrDefault(arg);
      if (argAff.runtime !== nodeAff.runtime) {
        const marshalKind = arg.kind === "Lambda" ? MarshalKind.Callback : this.inferMarshalKind(arg);
        this.insertBridge(argAff.runtime, nodeAff.runtime, marshalKind);
      }
    }

    return nodeAff;
  }

  private propagateNewExpr(node: AST.NewExpr): RuntimeAffinity {
    const calleeAff = this.propagateExpr(node.callee);
    for (const arg of node.args) {
      this.propagateExpr(arg);
    }

    const existing = this.affinityMap.get(node);
    if (calleeAff.confidence !== "fallback" && (!existing || existing.confidence === "fallback" ||
        (existing.confidence === "inferred" && existing.evidence[0]?.type === "scope"))) {
      this.affinityMap.set(node, {
        ...calleeAff,
        evidence: [
          { type: "syntax", detail: `constructor expression: ${calleeAff.runtime}` },
          ...calleeAff.evidence,
        ],
      });
    }

    const nodeAff = this.getOrDefault(node);
    if (calleeAff.runtime !== nodeAff.runtime) {
      this.insertBridge(calleeAff.runtime, nodeAff.runtime, this.inferMarshalKind(node));
    }
    for (const arg of node.args) {
      const argAff = this.getOrDefault(arg);
      if (argAff.runtime !== nodeAff.runtime) {
        this.insertBridge(argAff.runtime, nodeAff.runtime, this.inferMarshalKind(arg));
      }
    }
    return nodeAff;
  }

  private propagateMember(node: AST.Member): RuntimeAffinity {
    const objAff = this.propagateExpr(node.object);
    const propertyAff = this.propagateMemberProperty(node);

    // Look up method name for runtime affinity evidence
    const methodName = this.getMemberPropertyName(node);
    const methodRuntime = methodName ? lookupMethodAffinity(methodName) : undefined;
    const qualifiedRuntime = lookupQualifiedGlobalAffinity(this.memberChainParts(node));

    const existing = this.affinityMap.get(node);
    const refinedObjectAff = methodName && methodRuntime
      ? this.refineNeutralAggregateOwnerFromMethod(node, methodName, methodRuntime)
      : undefined;
    const effectiveObjAff = refinedObjectAff || objAff;

    if (existing?.confidence === "definite" && existing.evidence.some(e => e.type === "syntax")) {
      if (effectiveObjAff.runtime !== existing.runtime) {
        this.insertBridge(effectiveObjAff.runtime, existing.runtime, this.inferMarshalKind(node));
      }
      return existing;
    }

    // Key rule: object provenance beats method name tables.
    // If `files` came from `os.listdir()` (Python), then `files.map()` should
    // stay Python — not flip to JS just because `.map` is in the JS method table.
    //
    // The method table only wins when:
    //   1. The object has no opinion (fallback confidence), OR
    //   2. The method's runtime MATCHES the object's runtime (reinforcing, not contradicting)
    const objIsKnown = effectiveObjAff.confidence !== "fallback" &&
      !(effectiveObjAff.confidence === "inferred" && effectiveObjAff.evidence[0]?.type === "scope" &&
        effectiveObjAff.evidence[0]?.detail.startsWith("scope majority"));

    if (qualifiedRuntime && (!existing || existing.confidence !== "definite")) {
      this.affinityMap.set(node, {
        runtime: qualifiedRuntime,
        confidence: "inferred",
        evidence: [{ type: "builtin", detail: `qualified global: ${this.memberChainParts(node).join(".")}` }],
      });
    } else if (objIsKnown) {
      // Object has a real runtime — inherit from object
      this.affinityMap.set(node, {
        runtime: effectiveObjAff.runtime,
        confidence: effectiveObjAff.confidence,
        evidence: [
          { type: "scope", detail: `inherited from object: ${effectiveObjAff.runtime}` },
          ...effectiveObjAff.evidence,
        ],
      });
    } else if (methodRuntime && (!existing || existing.confidence !== "definite") && !objIsKnown) {
      // Object is unknown (fallback) — method name provides the best evidence
      const aff = affinityFromEvidence(chooseRuntime([{
        runtime: methodRuntime,
        source: "method",
        weight: EVIDENCE_WEIGHTS.method,
        detail: `.${methodName}()`,
      }], this.defaultRuntime));
      this.affinityMap.set(node, {
        ...aff,
        evidence: [
          ...aff.evidence,
          ...effectiveObjAff.evidence,
        ],
      });
    } else if (propertyAff && propertyAff.confidence !== "fallback" &&
        (!existing || existing.confidence === "fallback" ||
        (existing.confidence === "inferred" && existing.evidence[0]?.type === "scope"))) {
      this.affinityMap.set(node, { ...propertyAff });
    } else if (!existing) {
      // Inherit from object
      this.ensureAffinity(node, objAff);
    }

    return this.getOrDefault(node);
  }

  private refineNeutralAggregateOwnerFromMethod(
    node: AST.Member,
    methodName: string,
    runtime: OmniRuntime,
  ): RuntimeAffinity | undefined {
    return node.object.kind === "Identifier"
      ? this.refineNeutralAggregateIdentifier(node.object, runtime, {
          type: "method",
          detail: `neutral aggregate adopted by .${methodName}()`,
        })
      : undefined;
  }

  private propagateBinary(node: AST.Binary): RuntimeAffinity {
    const leftAff = this.propagateExpr(node.left);
    const rightAff = this.propagateExpr(node.right);

    // If sides disagree, we need a bridge
    if (leftAff.runtime !== rightAff.runtime) {
      // Prefer the more confident side
      const winner = leftAff.confidence === "definite" ? leftAff :
                     rightAff.confidence === "definite" ? rightAff :
                     leftAff; // default to left
      this.ensureAffinity(node, winner);
      this.insertBridge(
        leftAff.runtime === winner.runtime ? rightAff.runtime : leftAff.runtime,
        winner.runtime,
        MarshalKind.Primitive,
      );
    } else {
      this.ensureAffinity(node, leftAff);
    }

    return this.getOrDefault(node);
  }

  private propagateUnary(node: AST.Unary): RuntimeAffinity {
    const argAff = this.propagateExpr(node.argument);

    // Async infection: await propagates async flag
    if (node.op === "await") {
      const existingAwait = this.affinityMap.get(node);
      if (!node.prefix && existingAwait?.confidence === "definite") {
        // Rust postfix `.await` — definite Pass 1 syntax evidence wins over
        // argument propagation (JS `await` is prefix-only).
        existingAwait.async = true;
        this.affinityMap.set(node, existingAwait);
        return existingAwait;
      }
      if (argAff.confidence !== "fallback") {
        this.affinityMap.set(node, { ...argAff });
      } else if (!this.affinityMap.has(node)) {
        const scopeRuntime = this.scopeRuntimeStack.length > 0
          ? this.scopeRuntimeStack[this.scopeRuntimeStack.length - 1]
          : OmniRuntime.JavaScript;
        this.ensureAffinity(node, {
          runtime: scopeRuntime,
          confidence: "inferred",
          evidence: [{ type: "node_type", detail: `await expression in ${scopeRuntime} scope` }],
          async: true,
        });
      }
      const aff = this.getOrDefault(node);
      aff.async = true;
      this.affinityMap.set(node, aff);
    }

    // Channel receive <-ch inherits Go affinity (set in Pass 1)
    if (!this.affinityMap.has(node)) {
      this.ensureAffinity(node, argAff);
    }

    return this.getOrDefault(node);
  }

  private propagateLambda(node: AST.Lambda): RuntimeAffinity {
    if ('kind' in node.body && (node.body as any).kind === "Block") {
      this.propagateBlock(node.body as AST.Block);
    } else {
      this.propagateExpr(node.body as AST.Expr);
    }

    // Async lambdas are JS
    if (node.async) {
      const aff = this.getOrDefault(node);
      if (aff.confidence === "fallback") {
        this.affinityMap.set(node, {
          runtime: OmniRuntime.JavaScript,
          confidence: "inferred",
          evidence: [{ type: "keyword", detail: "async lambda" }],
          async: true,
        });
      } else {
        aff.async = true;
        this.affinityMap.set(node, aff);
      }
    }

    return this.getOrDefault(node);
  }

  private propagateMatch(node: AST.Match): RuntimeAffinity {
    this.propagateExpr(node.expr);
    for (const arm of node.arms) {
      for (const pattern of arm.patterns) this.propagateExpr(pattern);
      if (arm.guard) this.propagateExpr(arm.guard);
      if ('kind' in arm.body && (arm.body as any).kind === "Block") {
        this.propagateBlock(arm.body as AST.Block);
      } else {
        this.propagateExpr(arm.body as AST.Expr);
      }
    }
    return this.getOrDefault(node);
  }

  private propagateStringLiteral(node: AST.StringLiteral): RuntimeAffinity {
    // Check interpolation parts for runtime tags
    for (const part of node.parts) {
      if (part.kind === "Interpolation" && typeof part.value === "string") {
        // Check for @py(), @js(), etc. in string interpolation
        const runtimeTagMatch = part.value.match(/^@(py|js|go|rb|java)\((.*)\)$/s);
        if (runtimeTagMatch) {
          // Mark this interpolation as needing a specific runtime
          // This info will be used by the code generator
        }
      } else if (part.kind === "Interpolation" && typeof part.value !== "string") {
        this.propagateExpr(part.value as AST.Expr);
      }
    }
    return this.getOrDefault(node);
  }

  private propagateFuncDecl(node: AST.FuncDecl): RuntimeAffinity {
    for (const decorator of node.decorators || []) {
      this.propagateDecorator(decorator);
    }

    // Push the function's declared runtime so body statements inherit it
    const funcAff = this.affinityMap.get(node);
    if (funcAff && funcAff.confidence !== "fallback") {
      this.scopeRuntimeStack.push(funcAff.runtime);
    }
    this.symbolTable.pushScope();
    this.propagateBlock(node.body);
    this.symbolTable.popScope();
    if (funcAff && funcAff.confidence !== "fallback") {
      this.scopeRuntimeStack.pop();
    }

    // Async infection for async functions
    const aff = this.getOrDefault(node);
    if (node.async) {
      aff.async = true;
      this.affinityMap.set(node, aff);
    }

    return aff;
  }

  private propagateBlock(block: AST.Block): RuntimeAffinity {
    let blockAffinity: RuntimeAffinity | undefined;

    for (const stmt of block.statements) {
      const stmtAff = this.propagateNode(stmt);
      if (!blockAffinity || this.isWeakScopeAffinity(blockAffinity)) {
        blockAffinity = stmtAff;
      }
    }

    return blockAffinity || this.getOrDefault(block);
  }

  private propagateDecorator(decorator: AST.Decorator): RuntimeAffinity | undefined {
    if (decorator.expression) {
      return this.propagateExpr(decorator.expression);
    }
    const nameAff = this.propagateExpr(decorator.name);
    for (const arg of decorator.args || []) this.propagateExpr(arg);
    return nameAff;
  }

  private propagateClassFromDecorators(node: AST.ClassDecl): void {
    const existing = this.affinityMap.get(node);
    if (existing && existing.confidence !== "fallback") return;

    for (const decorator of node.decorators || []) {
      const aff = decorator.expression
        ? this.affinityMap.get(decorator.expression)
        : this.affinityMap.get(decorator.name);
      if (aff && aff.confidence !== "fallback") {
        this.affinityMap.set(node, {
          runtime: aff.runtime,
          confidence: aff.confidence,
          evidence: [{ type: "scope", detail: `class decorator: ${aff.runtime}` }, ...aff.evidence],
        });
        return;
      }
    }
    for (const member of node.members) {
      for (const decorator of member.decorators || []) {
        const aff = decorator.expression
          ? this.affinityMap.get(decorator.expression)
          : this.affinityMap.get(decorator.name);
        if (aff && aff.confidence !== "fallback") {
          this.affinityMap.set(node, {
            runtime: aff.runtime,
            confidence: aff.confidence,
            evidence: [{ type: "scope", detail: `member decorator: ${aff.runtime}` }, ...aff.evidence],
          });
          return;
        }
      }
    }
  }

  private propagateCatchClause(clause: AST.CatchClause): RuntimeAffinity {
    const catchAff = this.affinityMap.get(clause.body);
    const scopedCatch = catchAff && catchAff.confidence !== "fallback" ? catchAff : undefined;

    if (scopedCatch) {
      this.scopeRuntimeStack.push(scopedCatch.runtime);
    }
    this.symbolTable.pushScope();

    if (clause.param && scopedCatch) {
      this.ensureAffinity(clause.param, scopedCatch);
      this.symbolTable.define(clause.param.name, {
        name: clause.param.name,
        affinity: { ...scopedCatch },
      });
    }

    const bodyAff = this.propagateBlock(clause.body);
    this.symbolTable.popScope();
    if (scopedCatch) {
      this.scopeRuntimeStack.pop();
    }

    return bodyAff;
  }

  // --- Helpers ---

  private propagateIdentifier(node: AST.Identifier): RuntimeAffinity {
    const symbol = this.symbolTable.lookup(node.name);
    const existing = this.affinityMap.get(node);
    if (symbol && symbol.affinity.confidence !== "fallback" &&
        (!existing || existing.confidence === "fallback" ||
        (existing.confidence === "inferred" && existing.evidence[0]?.type === "scope"))) {
      this.affinityMap.set(node, { ...symbol.affinity });
    } else if (!symbol) {
      const globalRuntime = lookupGlobalAffinity(node.name);
      if (globalRuntime && (!existing || existing.confidence === "fallback" ||
          (existing.confidence === "inferred" && existing.evidence[0]?.type === "scope"))) {
        const aff = affinityFromEvidence(chooseRuntime([{
          runtime: globalRuntime,
          source: "global",
          weight: EVIDENCE_WEIGHTS.global,
          detail: `global: ${node.name}`,
        }], this.defaultRuntime));
        this.affinityMap.set(node, aff);
      }
    }
    return this.getOrDefault(node);
  }

  private propagateMemberProperty(node: AST.Member): RuntimeAffinity | undefined {
    const property = node.property as unknown;
    if (property && typeof property === "object" && "kind" in property && property.kind !== "Identifier") {
      return this.propagateExpr(property as AST.Expr);
    }
    return undefined;
  }

  private getMemberPropertyName(node: AST.Member): string | undefined {
    const property = node.property as unknown;
    if (property && typeof property === "object" && "kind" in property && property.kind === "Identifier") {
      return (property as AST.Identifier).name;
    }
    return undefined;
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

  private defineDeclaredNames(
    names: AST.Identifier[],
    affinity: RuntimeAffinity,
    declNode: AST.Decl | AST.Stmt | AST.Expr,
  ): void {
    for (const name of names) {
      this.symbolTable.define(name.name, {
        name: name.name,
        affinity: { ...affinity },
        declNode,
      });
    }
  }

  private ensureAffinity(node: AST.Decl | AST.Stmt | AST.Expr, affinity: RuntimeAffinity): void {
    if (!this.affinityMap.has(node)) {
      this.affinityMap.set(node, { ...affinity });
    }
  }

  private preferRuntimeAffinity(
    primary: RuntimeAffinity | undefined,
    secondary: RuntimeAffinity | undefined,
  ): RuntimeAffinity | undefined {
    if (primary && primary.confidence !== "fallback" && !this.isWeakScopeAffinity(primary)) {
      return primary;
    }
    if (secondary && secondary.confidence !== "fallback" && !this.isWeakScopeAffinity(secondary)) {
      return secondary;
    }
    return primary || secondary;
  }

  private preferAggregateValueAffinity(
    current: RuntimeAffinity | undefined,
    candidate: RuntimeAffinity | undefined,
  ): RuntimeAffinity | undefined {
    if (!this.isMeaningfulAffinity(candidate)) {
      return current;
    }
    if (!this.isMeaningfulAffinity(current)) {
      return candidate;
    }

    const candidateDirect = this.hasDirectRuntimeEvidence(candidate);
    const currentDirect = this.hasDirectRuntimeEvidence(current);
    if (candidateDirect && !currentDirect) {
      return candidate;
    }
    if (candidateDirect === currentDirect && candidate.confidence === "definite" && current.confidence !== "definite") {
      return candidate;
    }
    return current;
  }

  private hasDirectRuntimeEvidence(affinity: RuntimeAffinity): boolean {
    const first = affinity.evidence[0];
    return first?.type === "syntax" || first?.type === "builtin" || first?.type === "keyword";
  }

  private isWeakScopeAffinity(affinity: RuntimeAffinity | undefined): boolean {
    return !!affinity &&
      affinity.confidence === "inferred" &&
      affinity.evidence[0]?.type === "scope" &&
      affinity.evidence[0]?.detail.startsWith("scope majority");
  }

  private isMeaningfulAffinity(affinity: RuntimeAffinity | undefined): affinity is RuntimeAffinity {
    return !!affinity && affinity.confidence !== "fallback" && !this.isWeakScopeAffinity(affinity);
  }

  private isNeutralDeclarationAffinity(affinity: RuntimeAffinity | undefined): boolean {
    return !!affinity &&
      affinity.confidence === "fallback" &&
      affinity.evidence[0]?.type === "fallback" &&
      affinity.evidence[0]?.detail === "neutral declaration literal";
  }

  private refineNeutralAggregateIdentifier(
    identifier: AST.Identifier,
    runtime: OmniRuntime,
    evidence: AffinityEvidence,
  ): RuntimeAffinity | undefined {
    const symbol = this.symbolTable.lookup(identifier.name);
    if (!symbol?.declNode || !this.isNeutralAggregateBinding(symbol.declNode, identifier.name)) {
      return undefined;
    }

    if (!this.isNeutralDeclarationAffinity(symbol.affinity)) {
      return undefined;
    }

    const affinity: RuntimeAffinity = {
      runtime,
      confidence: "inferred",
      evidence: [evidence],
    };

    this.affinityMap.set(symbol.declNode, { ...affinity });
    const value = this.singleDeclarationValue(symbol.declNode);
    if (value) {
      this.affinityMap.set(value, {
        ...affinity,
        evidence: [
          { type: "scope", detail: `neutral aggregate declaration adopted by ${runtime}` },
          ...affinity.evidence,
        ],
      });
    }
    this.affinityMap.set(identifier, { ...affinity });
    this.symbolTable.update(identifier.name, {
      ...symbol,
      affinity: { ...affinity },
    });

    return affinity;
  }

  private isNeutralAggregateDeclaration(
    node: AST.Decl | AST.Stmt | AST.Expr,
  ): node is AST.VarDecl | AST.ConstDecl {
    if (node.kind !== "VarDecl" && node.kind !== "ConstDecl") {
      return false;
    }
    if (node.names.length !== 1 || !node.values || node.values.length !== 1) {
      return false;
    }
    const value = node.values[0];
    return this.isNeutralAggregateExpr(value);
  }

  private isNeutralAggregateBinding(
    node: AST.Decl | AST.Stmt | AST.Expr,
    name: string,
  ): node is AST.VarDecl | AST.ConstDecl | AST.Assign {
    if (node.kind === "Assign") {
      return node.left.kind === "Identifier" &&
        node.left.name === name &&
        this.isNeutralAggregateExpr(node.right);
    }
    return this.isNeutralAggregateDeclaration(node) && node.names[0].name === name;
  }

  private singleDeclarationValue(node: AST.VarDecl | AST.ConstDecl | AST.Assign): AST.Expr | undefined {
    if (node.kind === "Assign") {
      return node.right;
    }
    return node.values && node.values.length === 1 ? node.values[0] : undefined;
  }

  private isNeutralAggregateExpr(expr: AST.Expr): boolean {
    return expr.kind === "ArrayLiteral" || expr.kind === "ObjectLiteral";
  }

  private getOrDefault(node: AST.Decl | AST.Stmt | AST.Expr): RuntimeAffinity {
    const existing = this.affinityMap.get(node);
    if (existing) return existing;

    // Prefer the enclosing function's runtime over symbol-table scope majority
    const scopeRuntime = this.scopeRuntimeStack.length > 0
      ? this.scopeRuntimeStack[this.scopeRuntimeStack.length - 1]
      : undefined;
    const scopeAff = scopeRuntime
      ? { runtime: scopeRuntime, confidence: "inferred" as const, evidence: [{ type: "scope" as const, detail: `enclosing function: ${scopeRuntime}` }] }
      : this.symbolTable.getScopeAffinity();
    const defaultAff: RuntimeAffinity = scopeAff || {
      runtime: this.defaultRuntime,
      confidence: "fallback",
      evidence: [{ type: "fallback", detail: "default runtime" }],
    };

    this.affinityMap.set(node, defaultAff);
    return defaultAff;
  }

  private insertBridge(from: OmniRuntime, to: OmniRuntime, marshalKind: MarshalKind): void {
    if (from === to) return;

    const bridge: BridgeDescriptor = {
      from,
      to,
      marshalKind,
      cost: computeBridgeCost(marshalKind),
    };

    this.bridges.push(bridge);
  }

  private inferMarshalKind(node: AST.Expr): MarshalKind {
    switch (node.kind) {
      case "NumericLiteral":
      case "StringLiteral":
      case "BooleanLiteral":
      case "NullLiteral":
        return MarshalKind.Primitive;
      case "ArrayLiteral":
        return MarshalKind.Array;
      case "ObjectLiteral":
        return MarshalKind.Object;
      case "Lambda":
        return MarshalKind.Callback;
      default:
        return MarshalKind.Unknown;
    }
  }

  private isExpr(node: any): boolean {
    const exprKinds = new Set([
      "NumericLiteral", "StringLiteral", "RegexLiteral", "BooleanLiteral",
      "NullLiteral", "Identifier", "NewExpr", "Call", "Index", "Member", "Unary",
      "Binary", "Assign", "Lambda", "Ternary", "ArrayLiteral", "SetLiteral",
      "ObjectLiteral", "ListComprehension", "Spread", "Yield", "TypeAssertion",
      "JSXElement", "JSXFragment", "Match", "RuntimeTag",
    ]);
    return exprKinds.has(node.kind);
  }
}
