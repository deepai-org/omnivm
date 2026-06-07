/**
 * LexerCursor — Canonical character consumption and token emission.
 *
 * All position state (line, column, indent) is maintained here.
 * Scanners call advance()/peek() and emitToken() without manually
 * tracking position. The Lexer class extends this.
 */

import { Token, TokenType } from './lexer';
import * as LS from './lex-state';

/**
 * ScanHost — the interface scanner functions use to interact with the cursor.
 * Exposed so extracted scanner modules can read/write position state.
 */
export interface ScanHost {
  source: string;
  position: number;
  line: number;
  column: number;
  tokens: Token[];
  lastNonWSToken: Token | null;
  currentIndent: number;
  lineStart: boolean;
  state: LS.LexState;
  advance(): string;
  peek(): string;
  peekNext(): string;
  peekAt(offset: number): string;
  isAtEnd(): boolean;
  peekIdentifier(): string;
  peekPastGenericParams(startPos: number): number;
  peekNextNonWhitespace(): string;
  addToken(type: TokenType, value: string, start: number, end: number): void;
  addTokenEx(type: TokenType, value: string, start: number, end: number, line: number, column: number): void;
}

export class LexerCursor {
  source: string;
  position = 0;
  line = 1;
  column = 1;
  tokens: Token[] = [];
  lastNonWSToken: Token | null = null;
  currentIndent = 0;
  lineStart = true;
  state: LS.LexState;

  constructor(source: string) {
    this.source = source;
    this.state = LS.createLexState();
  }

  // ============ Character Navigation ============

  /** Advance one character. Returns the consumed character. */
  advance(): string {
    const char = this.source[this.position];
    this.position++;
    this.column++;
    return char;
  }

  peek(): string {
    if (this.isAtEnd()) return '\0';
    return this.source[this.position];
  }

  peekNext(): string {
    if (this.position + 1 >= this.source.length) return '\0';
    return this.source[this.position + 1];
  }

  peekAt(offset: number): string {
    if (this.position + offset >= this.source.length) return '\0';
    return this.source[this.position + offset];
  }

  isAtEnd(): boolean {
    return this.position >= this.source.length;
  }

  // ============ Lookahead Helpers ============

  /** Peek an identifier starting at current position (non-consuming). */
  peekIdentifier(): string {
    let pos = this.position;
    if (!this.source[pos] || !/[a-zA-Z_]/.test(this.source[pos])) {
      return '';
    }
    let result = '';
    while (pos < this.source.length && /[a-zA-Z0-9_]/.test(this.source[pos])) {
      result += this.source[pos];
      pos++;
    }
    return result;
  }

  /** Find position after matching '>' of generic params starting at `startPos`. */
  peekPastGenericParams(startPos: number): number {
    let pos = startPos;
    if (this.source[pos] !== '<') return pos;
    let depth = 1;
    pos++;
    while (pos < this.source.length && depth > 0) {
      const char = this.source[pos];
      if (char === '<') depth++;
      else if (char === '>') depth--;
      pos++;
    }
    return pos;
  }

  /** Peek the next non-whitespace character (non-consuming). */
  peekNextNonWhitespace(): string {
    let offset = 0;
    while (this.position + offset < this.source.length) {
      const char = this.source[this.position + offset];
      if (!/\s/.test(char)) return char;
      offset++;
    }
    return '\0';
  }

  // ============ Token Emission ============

  /** Emit a token using current line/column for position. */
  addToken(type: TokenType, value: string, start: number, end: number): void {
    const token: Token = {
      type,
      value,
      start,
      end,
      line: this.line,
      column: this.column - (end - start)
    };
    this.finalizeToken(token, type, value);
  }

  /** Emit a token with explicit line/column (for scanners that save start position). */
  addTokenEx(type: TokenType, value: string, start: number, end: number, line: number, column: number): void {
    const token: Token = {
      type,
      value,
      start,
      end,
      line,
      column
    };
    this.finalizeToken(token, type, value);
    // Also run type context transitions for addTokenEx
    if (type !== TokenType.Whitespace && type !== TokenType.Comment) {
      this.updateTypeContext(value);
    }
  }

  private finalizeToken(token: Token, type: TokenType, value: string): void {
    // Set indentation for tokens at start of line
    if (this.lineStart && type !== TokenType.Whitespace) {
      token.indentCol = this.currentIndent;
      token.newline = true;
      this.lineStart = false;
    }

    this.tokens.push(token);

    if (type !== TokenType.Whitespace && type !== TokenType.Comment) {
      this.lastNonWSToken = token;
      LS.updatePosition(this.state, value);
    }
  }

  /** Handle type context transitions after token emission. */
  protected updateTypeContext(value: string): void {
    if (value === ':' && !LS.isInJSX(this.state)) {
      LS.enterTypeAnnotation(this.state);
    } else if (value === '<' && LS.canBeGeneric(this.state)) {
      LS.enterGeneric(this.state);
    } else if (value === 'extends' || value === 'implements' ||
               value === 'as' || value === 'typeof' || value === 'keyof') {
      LS.enterTypeAnnotation(this.state);
    }

    if (value === '>' && this.state.genericDepth > 0) {
      LS.exitGeneric(this.state);
    } else if ((value === ',' || value === ')' || value === ';' || value === '=' ||
                value === '{' || value === '}') && LS.isInType(this.state)) {
      LS.exitTypeAnnotation(this.state);
    }
  }
}
