/**
 * Comment and shebang scanners.
 */

import { ScanHost } from './lexer-cursor';

export function skipShebang(h: ScanHost): void {
  while (h.peek() !== '\n' && !h.isAtEnd()) {
    h.advance();
  }
  if (h.peek() === '\n') {
    h.advance();
    h.line++;
    h.column = 1;
  }
}

export function skipLineComment(h: ScanHost): void {
  while (h.peek() !== '\n' && !h.isAtEnd()) {
    h.advance();
  }
}

/**
 * MULTI-LINE Rust attribute skipper. Called with the `#` already consumed
 * and `h.peek()` at `!` or `[`. When the position starts a Rust attribute
 * (`#[...]` / `#![...]`) whose balanced bracket group spans MORE than one
 * line, consume the whole group as comment trivia and return true.
 *
 * Single-line attributes keep the legacy hash-line-comment behavior (return
 * false; caller consumes to end of line) so Python/Ruby/Bash `#` comments —
 * including ones that happen to contain brackets — are completely
 * unaffected. A multi-line consume only happens when the group really
 * balances, scanning with Rust string/char/comment awareness; an unbalanced
 * `#[...` (e.g. a Python comment) falls back to the line comment.
 */
export function trySkipMultilineRustAttribute(h: ScanHost): boolean {
  const src = h.source;
  let q = h.position; // at '!' or '['
  if (src[q] === '!') q++;
  if (src[q] !== '[') return false;
  // Attribute content must start with an identifier (cfg, derive, doc, ...).
  let r = q + 1;
  while (src[r] === ' ' || src[r] === '\t') r++;
  if (!/[A-Za-z_]/.test(src[r] || '')) return false;

  // String/char/comment-aware balanced scan for the matching `]`.
  const cap = Math.min(src.length, q + 20000);
  let depth = 0;
  let end = -1;
  let i = q;
  while (i < cap) {
    const c = src[i];
    if (c === '"') {
      i++;
      while (i < src.length) {
        if (src[i] === '\\') { i += 2; continue; }
        if (src[i] === '"') { i++; break; }
        i++;
      }
      continue;
    }
    if (c === "'") {
      // Char literal ('x', '\n') or lifetime ('a) — same rule as the Rust
      // item scanner. Also protects against a stray `]` inside a
      // single-quoted string in non-Rust content.
      if (src[i + 1] === '\\') {
        i += 2;
        while (i < src.length && src[i] !== "'") i++;
        i++;
      } else if (src[i + 1] !== undefined && src[i + 2] === "'") {
        i += 3;
      } else {
        i++;
        while (i < src.length && /[A-Za-z0-9_]/.test(src[i])) i++;
      }
      continue;
    }
    if (c === '/' && src[i + 1] === '/') {
      const nl = src.indexOf('\n', i);
      i = nl === -1 ? src.length : nl;
      continue;
    }
    if (c === '/' && src[i + 1] === '*') {
      const close = src.indexOf('*/', i + 2);
      i = close === -1 ? src.length : close + 2;
      continue;
    }
    if (c === '[') depth++;
    else if (c === ']') {
      depth--;
      if (depth === 0) { end = i + 1; break; }
    }
    i++;
  }
  // Only MULTI-line groups are consumed here (including attributes whose
  // only newlines sit inside a multi-line string argument, e.g.
  // `#[deprecated = "\n..."]`); single-line attributes keep the legacy
  // hash-line-comment behavior.
  if (end === -1 || !src.slice(q, end).includes('\n')) return false;

  // Consume through the closing `]`, keeping line/column bookkeeping
  // (same convention as skipBlockComment).
  while (h.position < end && !h.isAtEnd()) {
    if (h.peek() === '\n') {
      h.line++;
      h.column = 0;
    }
    h.advance();
  }
  return true;
}

export function skipBlockComment(h: ScanHost): void {
  h.advance(); // consume second /
  while (!h.isAtEnd()) {
    if (h.peek() === '*' && h.peekNext() === '/') {
      h.advance(); // *
      h.advance(); // /
      break;
    }
    if (h.peek() === '\n') {
      h.line++;
      h.column = 0;
    }
    h.advance();
  }
}

export function skipHTMLComment(h: ScanHost): void {
  h.advance(); // !
  h.advance(); // -
  h.advance(); // -

  while (!h.isAtEnd()) {
    if (h.peek() === '-' && h.peekNext() === '-' && h.peekAt(2) === '>') {
      h.advance(); // -
      h.advance(); // -
      h.advance(); // >
      break;
    }
    if (h.peek() === '\n') {
      h.line++;
      h.column = 0;
    }
    h.advance();
  }
}
