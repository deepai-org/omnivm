import { RuntimeAffinity, SymbolEntry, OmniRuntime } from './types';

/**
 * Scoped symbol table for tracking variable runtime affinities.
 * Supports nested scopes (function bodies, blocks, etc.)
 */
export class SymbolTable {
  private scopes: Map<string, SymbolEntry>[] = [new Map()];

  /**
   * Push a new scope (e.g., entering a function body).
   */
  pushScope(): void {
    this.scopes.push(new Map());
  }

  /**
   * Pop the current scope.
   */
  popScope(): void {
    if (this.scopes.length > 1) {
      this.scopes.pop();
    }
  }

  /**
   * Define a symbol in the current scope.
   */
  define(name: string, entry: SymbolEntry): void {
    this.currentScope().set(name, entry);
  }

  /**
   * Update the nearest existing symbol binding.
   */
  update(name: string, entry: SymbolEntry): boolean {
    for (let i = this.scopes.length - 1; i >= 0; i--) {
      if (this.scopes[i].has(name)) {
        this.scopes[i].set(name, entry);
        return true;
      }
    }
    return false;
  }

  /**
   * Look up a symbol, searching from innermost to outermost scope.
   */
  lookup(name: string): SymbolEntry | undefined {
    for (let i = this.scopes.length - 1; i >= 0; i--) {
      const entry = this.scopes[i].get(name);
      if (entry) return entry;
    }
    return undefined;
  }

  /**
   * Get the runtime affinity of the current scope.
   * Uses the most recently defined symbol's affinity as a heuristic,
   * or returns undefined if the scope has no entries.
   */
  getScopeAffinity(): RuntimeAffinity | undefined {
    // Count runtime occurrences in current scope
    const counts = new Map<OmniRuntime, number>();
    const scope = this.currentScope();

    for (const entry of scope.values()) {
      const rt = entry.affinity.runtime;
      counts.set(rt, (counts.get(rt) || 0) + 1);
    }

    if (counts.size === 0) {
      // Check parent scopes
      for (let i = this.scopes.length - 2; i >= 0; i--) {
        for (const entry of this.scopes[i].values()) {
          const rt = entry.affinity.runtime;
          counts.set(rt, (counts.get(rt) || 0) + 1);
        }
        if (counts.size > 0) break;
      }
    }

    if (counts.size === 0) return undefined;

    // Return the runtime with the highest count
    let maxRuntime = OmniRuntime.JavaScript;
    let maxCount = 0;
    for (const [rt, count] of counts) {
      if (count > maxCount) {
        maxCount = count;
        maxRuntime = rt;
      }
    }

    return {
      runtime: maxRuntime,
      confidence: "inferred",
      evidence: [{ type: "scope", detail: `scope majority: ${maxRuntime}` }],
    };
  }

  /**
   * Get the current (innermost) scope.
   */
  private currentScope(): Map<string, SymbolEntry> {
    return this.scopes[this.scopes.length - 1];
  }

  /**
   * Get the depth of the scope stack.
   */
  get depth(): number {
    return this.scopes.length;
  }
}
