/**
 * ParserCursor — Transactional token cursor for the PolyScript parser.
 *
 * Provides token navigation, span creation, error handling, semicolon policy,
 * and transactional combinators (attempt, choice, withContext).
 *
 * The Parser class extends this to inherit all cursor functionality.
 */

import { Token, TokenType } from './lexer';
import * as AST from './ast';

export class ParseError extends Error {
  constructor(
    message: string,
    public token: Token,
    public quickFix?: string
  ) {
    super(message);
    this.name = 'ParseError';
  }
}

export type KeywordBlockKind = "do" | "case" | "begin" | "if" | "for" | "while" | "function";

export interface ParserContext {
  braceDepth: number;
  indentStack: number[];
  keywordStack: KeywordBlockKind[];
  insideSwitch: boolean;
  noRubyBlock: boolean;
}

export class ParserCursor {
  public tokens: Token[] = [];
  public current = 0;
  public errors: ParseError[] = [];

  // Block delimiter stacks
  public braceDepth = 0;
  public indentStack: number[] = [];
  public keywordStack: KeywordBlockKind[] = [];

  // Error recovery state
  protected syntheticTokenCounter = 0;

  // Context tracking
  public insideSwitch = false;
  public noRubyBlock = false;

  // Directives
  protected nextStmtGenericMode: "on" | "off" | "auto" = "auto";
  protected fileRuntimeDirective?: string;

  /** Raw source text, when provided — used by the Rust item raw scanner. */
  protected sourceText?: string;

  constructor(tokens: Token[], source?: string) {
    this.sourceText = source;
    // Scan raw source text for runtime directive (comments are skipped by lexer)
    if (source) {
      const match = source.match(/\/\/\s*@runtime\s+(\S+)/);
      if (match) {
        this.fileRuntimeDirective = match[1];
      }
    }
    this.tokens = tokens.filter(t =>
      t.type !== TokenType.Comment &&
      t.type !== TokenType.Whitespace
    );
  }

  getErrors(): ParseError[] {
    return this.errors;
  }

  // ============ Transactional Combinators ============

  /**
   * Run `fn`, rolling back cursor position on failure (thrown error or null return).
   * Returns the result on success, or null on failure.
   * Does NOT roll back errors by default — use attemptClean() for that.
   */
  public attempt<T>(fn: () => T): T | null {
    const checkpoint = this.current;
    try {
      const result = fn();
      if (result === null || result === undefined) {
        this.current = checkpoint;
        return null;
      }
      return result;
    } catch {
      this.current = checkpoint;
      return null;
    }
  }

  /**
   * Like attempt(), but also rolls back errors on failure.
   * Use when speculative parsing should leave no error trace.
   */
  protected attemptClean<T>(fn: () => T): T | null {
    const checkpoint = this.current;
    const errorCheckpoint = this.errors.length;
    try {
      const result = fn();
      if (result === null || result === undefined) {
        this.current = checkpoint;
        this.errors.length = errorCheckpoint;
        return null;
      }
      return result;
    } catch {
      this.current = checkpoint;
      this.errors.length = errorCheckpoint;
      return null;
    }
  }

  /**
   * Try each function in order. Return the first successful result.
   * Each failed attempt rolls back cursor position.
   */
  protected choice<T>(...fns: (() => T)[]): T | null {
    for (const fn of fns) {
      const result = this.attempt(fn);
      if (result !== null && result !== undefined) {
        return result;
      }
    }
    return null;
  }

  /**
   * Temporarily patch context flags, run `fn`, then restore them.
   */
  protected withContext<T>(patch: Partial<ParserContext>, fn: () => T): T {
    const saved: Partial<ParserContext> = {};

    // Save current values for patched keys
    if ('braceDepth' in patch) { saved.braceDepth = this.braceDepth; this.braceDepth = patch.braceDepth!; }
    if ('indentStack' in patch) { saved.indentStack = this.indentStack; this.indentStack = patch.indentStack!; }
    if ('keywordStack' in patch) { saved.keywordStack = this.keywordStack; this.keywordStack = patch.keywordStack!; }
    if ('insideSwitch' in patch) { saved.insideSwitch = this.insideSwitch; this.insideSwitch = patch.insideSwitch!; }
    if ('noRubyBlock' in patch) { saved.noRubyBlock = this.noRubyBlock; this.noRubyBlock = patch.noRubyBlock!; }

    try {
      return fn();
    } finally {
      if ('braceDepth' in saved) this.braceDepth = saved.braceDepth!;
      if ('indentStack' in saved) this.indentStack = saved.indentStack!;
      if ('keywordStack' in saved) this.keywordStack = saved.keywordStack!;
      if ('insideSwitch' in saved) this.insideSwitch = saved.insideSwitch!;
      if ('noRubyBlock' in saved) this.noRubyBlock = saved.noRubyBlock!;
    }
  }

  /**
   * Assert that progress was made in a parsing loop.
   * Call with the cursor position from before the loop body.
   * If no progress, advances one token to prevent infinite loops.
   */
  protected assertProgress(before: number, context: string): void {
    if (this.current === before && !this.isAtEnd()) {
      this.advance();
    }
  }

  // ============ Token Navigation ============

  public peek(): Token {
    if (this.isAtEnd()) {
      return this.tokens[this.tokens.length - 1];
    }
    return this.tokens[this.current];
  }

  public peekNext(): Token | undefined {
    return this.tokens[this.current + 1];
  }

  public peekAt(offset: number): Token | undefined {
    return this.tokens[this.current + offset];
  }

  /** Peek past consecutive virtualSemi tokens to find the next real token */
  protected peekPastVsemis(): Token | undefined {
    let pos = this.current;
    while (pos < this.tokens.length && this.tokens[pos].virtualSemi) {
      pos++;
    }
    return pos < this.tokens.length ? this.tokens[pos] : undefined;
  }

  public previous(): Token | undefined {
    return this.tokens[this.current - 1];
  }

  public hasWhitespaceBefore(): boolean {
    const prev = this.previous();
    const curr = this.peek();

    if (!prev || !curr) return false;

    if (prev.end !== undefined && curr.start !== undefined) {
      return curr.start > prev.end;
    }

    return false;
  }

  public advance(): Token {
    if (!this.isAtEnd()) this.current++;
    return this.previous()!;
  }

  public isAtEnd(): boolean {
    if (this.current >= this.tokens.length) return true;
    const token = this.tokens[this.current];
    return token && token.type === TokenType.EOF;
  }

  public check(value: string): boolean {
    if (this.isAtEnd()) return false;
    return this.peek().value === value;
  }

  public match(...values: string[]): boolean {
    for (const value of values) {
      if (this.check(value)) {
        this.advance();
        return true;
      }
    }
    return false;
  }

  public consume(expected: TokenType | string, message: string): Token {
    const token = this.peek();

    if (typeof expected === "string") {
      if (token.value === expected) {
        return this.advance();
      }
    } else {
      if (token.type === expected) {
        return this.advance();
      }
    }

    throw this.error(token, message);
  }

  /** Multi-token lookahead helper */
  protected lookahead(n: number): Token | null {
    const pos = this.current + n;
    return pos < this.tokens.length ? this.tokens[pos] : null;
  }

  // ============ Semicolon Handling ============

  protected consumeDirectives(): void {
    while (this.peek().type === TokenType.Comment) {
      const comment = this.advance();
      if (comment.value.startsWith("// @generics")) {
        this.nextStmtGenericMode = "on";
      } else if (comment.value.startsWith("// @nogenerics")) {
        this.nextStmtGenericMode = "off";
      }
    }
  }

  public consumeSemicolon(): void {
    if (this.match(";") || this.peek().virtualSemi) {
      if (this.peek().virtualSemi) {
        return;
      }
    }
  }

  public checkSemicolon(): boolean {
    return this.check(";") || this.peek().virtualSemi || false;
  }

  protected skipSemicolons(): void {
    while (this.check(";") || this.peek().virtualSemi) {
      this.advance();
    }
  }

  // ============ Error & Recovery ============

  /**
   * Synchronize after an error by advancing to the next statement boundary.
   * Override in Parser to add isDeclStart() check.
   */
  public synchronize(): void {
    this.advance();

    while (!this.isAtEnd()) {
      if (this.previous()?.type === TokenType.Operator &&
          this.previous()?.value === ";") {
        return;
      }

      const token = this.peek();
      if (token.value === "fi" || token.value === "esac" || token.value === "done" ||
          token.value === "end" || token.value === "}" || token.value === "elif" ||
          token.value === "else" || token.value === "elseif" || token.value === "rescue" ||
          token.value === "ensure" || token.value === "except" || token.value === "finally") {
        return;
      }

      this.advance();
    }
  }

  public error(token: Token, message: string): ParseError {
    return new ParseError(message, token);
  }

  protected createSyntheticToken(type: TokenType, value: string): Token {
    const pos = this.current > 0 ? this.tokens[this.current - 1].end : 0;
    return {
      type,
      value,
      start: pos,
      end: pos,
      line: this.current > 0 ? this.tokens[this.current - 1].line : 1,
      column: this.current > 0 ? this.tokens[this.current - 1].column + 1 : 1,
      synthetic: true
    } as Token;
  }

  protected createMissingExpr(): AST.Expr {
    const span = this.current > 0 ?
      this.createSpan(this.current - 1, this.current - 1) :
      this.createSpan(0, 0);

    return {
      kind: "Identifier",
      name: "__missing__",
      span
    };
  }

  protected createMissingIdentifier(): AST.Identifier {
    const span = this.current > 0 ?
      this.createSpan(this.current - 1, this.current - 1) :
      this.createSpan(0, 0);

    return {
      kind: "Identifier",
      name: "__missing__",
      span
    };
  }

  public must(expected: string, options?: { recoverWithSynthetic?: boolean }): boolean {
    while (this.peek().virtualSemi) {
      this.advance();
    }

    if (this.check(expected)) {
      this.advance();
      return true;
    }

    if (options?.recoverWithSynthetic) {
      this.errors.push(new ParseError(
        `Expected '${expected}' but got '${this.peek().value}'`,
        this.peek(),
        `Insert '${expected}'`
      ));
      return true;
    }

    throw this.error(this.peek(), `Expected '${expected}'`);
  }

  // ============ Span Creation ============

  public createSpan(start: number, end: number): AST.Span {
    const startToken = this.tokens[start] || this.tokens[0];
    const endToken = this.tokens[end] || this.tokens[this.tokens.length - 1];

    return {
      start: startToken.start,
      end: endToken.end,
      line: startToken.line,
      column: startToken.column
    };
  }

  public createSpanFrom(node: { span: AST.Span } | Token): AST.Span {
    if ('span' in node) {
      return {
        ...node.span,
        end: this.previous()?.end || node.span.end
      };
    }

    return {
      start: node.start,
      end: node.end,
      line: node.line,
      column: node.column
    };
  }
}
