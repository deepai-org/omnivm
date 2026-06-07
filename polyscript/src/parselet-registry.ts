/**
 * Parselet Registry — Keyword-dispatched parsing (Chunk 11).
 *
 * Replaces the giant if/else chains in parseStatement() and parseDeclaration()
 * with a registry that maps keywords to parselet functions.
 *
 * v1 is modest: statement and declaration registries only.
 * Pratt infix/postfix stays hardcoded for now.
 */

import * as AST from './ast';

// ============ Types ============

/** A registered statement parselet — called after the keyword is matched. */
export type StatementParselet<Host> = (host: Host) => AST.Stmt;

/** A registered declaration parselet — called after the keyword is matched. */
export type DeclarationParselet<Host> = (host: Host) => AST.Decl;

/** A registered prefix expression parselet — called when the token appears in prefix position. */
export type PrefixParselet<Host> = (host: Host) => AST.Expr;

// ============ Registry ============

export class ParseletRegistry<Host> {
  private statements = new Map<string, StatementParselet<Host>>();
  private declarations = new Map<string, DeclarationParselet<Host>>();
  private prefixExprs = new Map<string, PrefixParselet<Host>>();

  // ---- Statement registration ----

  registerStatement(keyword: string, parselet: StatementParselet<Host>): this {
    this.statements.set(keyword, parselet);
    return this;
  }

  getStatement(keyword: string): StatementParselet<Host> | undefined {
    return this.statements.get(keyword);
  }

  hasStatement(keyword: string): boolean {
    return this.statements.has(keyword);
  }

  // ---- Declaration registration ----

  registerDeclaration(keyword: string, parselet: DeclarationParselet<Host>): this {
    this.declarations.set(keyword, parselet);
    return this;
  }

  getDeclaration(keyword: string): DeclarationParselet<Host> | undefined {
    return this.declarations.get(keyword);
  }

  hasDeclaration(keyword: string): boolean {
    return this.declarations.has(keyword);
  }

  // ---- Prefix expression registration ----

  registerPrefix(token: string, parselet: PrefixParselet<Host>): this {
    this.prefixExprs.set(token, parselet);
    return this;
  }

  getPrefix(token: string): PrefixParselet<Host> | undefined {
    return this.prefixExprs.get(token);
  }

  hasPrefix(token: string): boolean {
    return this.prefixExprs.has(token);
  }

  // ---- Introspection ----

  get statementKeywords(): string[] {
    return [...this.statements.keys()];
  }

  get declarationKeywords(): string[] {
    return [...this.declarations.keys()];
  }

  get prefixTokens(): string[] {
    return [...this.prefixExprs.keys()];
  }

  /** Create a shallow copy of this registry (for dialect layering). */
  clone(): ParseletRegistry<Host> {
    const copy = new ParseletRegistry<Host>();
    for (const [k, v] of this.statements) copy.statements.set(k, v);
    for (const [k, v] of this.declarations) copy.declarations.set(k, v);
    for (const [k, v] of this.prefixExprs) copy.prefixExprs.set(k, v);
    return copy;
  }
}
