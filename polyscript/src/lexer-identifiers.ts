/**
 * Identifier, keyword, and sigil scanners.
 */

import { TokenType } from './lexer';
import { ScanHost } from './lexer-cursor';

const KEYWORDS = new Set([
  'await', 'break', 'case', 'catch', 'class', 'const', 'continue',
  'default', 'defer', 'def', 'do', 'done', 'elif', 'else', 'end',
  'enum', 'export', 'extends', 'false', 'fi', 'final', 'for', 'fun', 'func',
  'function', 'go', 'if', 'import', 'in', 'interface', 'let', 'loop',
  'match', 'new', 'null', 'nil', 'of', 'package', 'return', 'struct', 'switch',
  'then', 'this', 'throw', 'trait', 'true', 'try', 'type', 'until',
  'unsafe', 'using', 'var', 'when', 'while', 'with', 'yield',
  'typeof', 'void', 'delete', 'instanceof', 'async', 'auto',
  'immutable', 'require', 'fn', 'foreach', 'echo', 'print',
  'except', 'rescue', 'finally', 'undefined', 'elseif', 'esac',
  'begin', 'as', 'from', 'is', 'not', 'or', 'and', 'lambda',
  'global', 'nonlocal', 'pass', 'raise', 'assert', 'del'
]);

export function isKeyword(value: string): boolean {
  return KEYWORDS.has(value);
}

export function scanIdentifier(h: ScanHost): void {
  const start = h.position - 1;
  const startLine = h.line;
  const startColumn = h.column - 1;
  let value = h.source[start];

  while (/[a-zA-Z0-9_]/.test(h.peek())) {
    value += h.advance();
  }

  // In MemberAccess mode, all keywords become identifiers
  let type: TokenType;
  if (h.state.memberAccess) {
    type = TokenType.Identifier;
    h.state.memberAccess = false;
  } else if (h.state.decorator) {
    type = TokenType.Identifier;
    h.state.decorator = false;
  } else {
    type = isKeyword(value) ? TokenType.Keyword : TokenType.Identifier;
  }

  h.addTokenEx(type, value, start, h.position, startLine, startColumn);
}

export function scanSigilIdentifier(h: ScanHost): void {
  const start = h.position - 1;
  const startLine = h.line;
  const startColumn = h.column - 1;
  let value = '$';

  while (/[a-zA-Z0-9_]/.test(h.peek())) {
    value += h.advance();
  }

  h.addTokenEx(TokenType.SigilIdentifier, value, start, h.position, startLine, startColumn);
}
