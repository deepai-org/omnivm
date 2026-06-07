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
