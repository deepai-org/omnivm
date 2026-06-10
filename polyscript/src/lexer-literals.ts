/**
 * Literal scanners: strings, numbers, templates, heredocs, regex.
 */

import { TokenType } from './lexer';
import { ScanHost } from './lexer-cursor';
import { scanIdentifier } from './lexer-identifiers';

export function scanPrefixedString(h: ScanHost): void {
  const start = h.position;
  const startLine = h.line;
  const startColumn = h.column;

  // Collect prefixes - support f, r, b, u, br, rb combinations, plus @ and $
  let prefixes = '';
  while (/[rbfuRBFU@$]/.test(h.peek())) {
    prefixes += h.advance();
  }

  const quote = h.advance();
  if (quote !== '"' && quote !== "'") {
    // Not a string, backtrack and scan as identifier
    h.position = start;
    // Need to re-advance past the first char since scanIdentifier expects it consumed
    h.advance();
    scanIdentifier(h);
    return;
  }

  // Check for triple quotes
  let isTriple = false;
  if (h.peek() === quote && h.peekNext() === quote) {
    isTriple = true;
    h.advance();
    h.advance();
  }

  let value = prefixes + quote;
  if (isTriple) value += quote + quote;

  while (!h.isAtEnd()) {
    if (isTriple) {
      if (h.peek() === quote && h.peekNext() === quote && h.peekAt(2) === quote) {
        value += h.advance() + h.advance() + h.advance();
        break;
      }
    } else {
      if (h.peek() === quote) {
        // Count consecutive backslashes before the quote
        let backslashCount = 0;
        let checkPos = h.position - 1;
        while (checkPos >= 0 && h.source[checkPos] === '\\') {
          backslashCount++;
          checkPos--;
        }
        // If even number of backslashes (including 0), the quote is not escaped
        if (backslashCount % 2 === 0) {
          value += h.advance();
          break;
        }
      }
    }

    if (h.peek() === '\n') {
      if (!isTriple) {
        // Error: unterminated string
        break;
      }
      h.line++;
      h.column = 1;
    }

    value += h.advance();
  }

  h.addTokenEx(TokenType.StringLiteral, value, start, h.position, startLine, startColumn);
}


export function scanTemplateLiteral(h: ScanHost): void {
  const start = h.position - 1;
  const startLine = h.line;
  const startColumn = h.column - 1;
  let value = '`';

  while (!h.isAtEnd() && h.peek() !== '`') {
    if (h.peek() === '\\') {
      value += h.advance();
      if (!h.isAtEnd()) {
        value += h.advance();
      }
    } else if (h.peek() === '$' && h.peekNext() === '{') {
      // Handle template expression
      value += h.advance() + h.advance();
      let depth = 1;
      while (!h.isAtEnd() && depth > 0) {
        const char = h.advance();
        value += char;
        if (char === '{') depth++;
        else if (char === '}') depth--;
      }
    } else {
      if (h.peek() === '\n') {
        h.line++;
        h.column = 0;
      }
      value += h.advance();
    }
  }

  if (h.peek() === '`') {
    value += h.advance();
  }

  h.addTokenEx(TokenType.TemplateLiteral, value, start, h.position, startLine, startColumn);
}

export function scanNumber(h: ScanHost): void {
  const start = h.position - 1;
  const startLine = h.line;
  const startColumn = h.column - 1;
  let value = h.source[start];

  // Rust tuple indexing (`pair.0`): the numeric literal consumes the
  // MemberAccess mode the `.` opened, so the NEXT identifier-like token is
  // not demoted from keyword to identifier.
  if (h.state.memberAccess) {
    h.state.memberAccess = false;
  }

  // Check for hex, octal, binary
  if (value === '0') {
    const next = h.peek();
    if (next === 'x' || next === 'X') {
      value += h.advance();
      while (/[0-9a-fA-F_]/.test(h.peek())) {
        value += h.advance();
      }
    } else if (next === 'o' || next === 'O') {
      value += h.advance();
      while (/[0-7_]/.test(h.peek())) {
        value += h.advance();
      }
    } else if (next === 'b' || next === 'B') {
      value += h.advance();
      while (/[01_]/.test(h.peek())) {
        value += h.advance();
      }
    }
  }

  // Decimal number
  while (/[\d_]/.test(h.peek())) {
    value += h.advance();
  }

  // Float
  if (h.peek() === '.' && /\d/.test(h.peekNext())) {
    value += h.advance();
    while (/[\d_]/.test(h.peek())) {
      value += h.advance();
    }
  }

  // Exponent
  if (/[eE]/.test(h.peek())) {
    value += h.advance();
    if (/[+-]/.test(h.peek())) {
      value += h.advance();
    }
    while (/\d/.test(h.peek())) {
      value += h.advance();
    }
  }

  // Suffix
  if (/[nulfiULFI]/.test(h.peek())) {
    while (/[a-zA-Z0-9]/.test(h.peek())) {
      value += h.advance();
    }
  }

  h.addTokenEx(TokenType.NumericLiteral, value, start, h.position, startLine, startColumn);
}

/** Returns false if no delimiter found (caller should fall back to operator scan). */
export function scanHeredoc(h: ScanHost): boolean {
  const start = h.position - 2; // Account for '<<' already consumed
  const startLine = h.line;
  const startColumn = h.column - 2;

  // Read the delimiter (max 20 chars for safety)
  let delimiter = '';
  let delimiterCount = 0;
  while (!h.isAtEnd() && /[A-Z_0-9]/i.test(h.peek()) && delimiterCount < 20) {
    delimiter += h.advance();
    delimiterCount++;
  }

  if (delimiter === '') {
    // No delimiter found, treat as operator
    h.position = start + 2; // Position after <<
    return false;
  }

  // Skip to next line
  while (!h.isAtEnd() && h.peek() !== '\n') {
    h.advance();
  }
  if (h.peek() === '\n') {
    h.advance();
    h.line++;
    h.column = 0;
  }

  // Collect heredoc content until we find delimiter on its own line
  let content = '';
  let currentLine = '';
  let linesRead = 0;
  const maxLines = 1000; // Safety limit

  while (!h.isAtEnd() && linesRead < maxLines) {
    const char = h.peek();

    if (char === '\n') {
      // Check if current line is the delimiter
      if (currentLine.trim() === delimiter) {
        // Found end delimiter
        break;
      }
      // Add line to content
      content += currentLine + '\n';
      currentLine = '';
      linesRead++;
      h.advance();
      h.line++;
      h.column = 0;
    } else {
      currentLine += char;
      h.advance();
    }
  }

  // Check final line
  if (currentLine.trim() === delimiter) {
    // Consume the delimiter line
    while (!h.isAtEnd() && h.peek() !== '\n') {
      h.advance();
    }
  }

  // Create the heredoc token
  const heredocValue = `<<${delimiter}\n${content}${delimiter}`;
  h.addTokenEx(TokenType.StringLiteral, heredocValue, start, h.position, startLine, startColumn);
  return true;
}

export function scanRegex(h: ScanHost): void {
  const start = h.position - 1;
  const startLine = h.line;
  const startColumn = h.column - 1;
  let value = '/';

  let inCharClass = false;
  while (!h.isAtEnd() && (h.peek() !== '/' || inCharClass)) {
    if (h.peek() === '\\') {
      value += h.advance();
      if (!h.isAtEnd()) {
        value += h.advance();
      }
    } else if (h.peek() === '\n') {
      // Error: unterminated regex
      break;
    } else {
      const ch = h.peek();
      if (ch === '[') inCharClass = true;
      else if (ch === ']') inCharClass = false;
      value += h.advance();
    }
  }

  if (h.peek() === '/') {
    value += h.advance();

    // Flags
    while (/[gimsuy]/.test(h.peek())) {
      value += h.advance();
    }
  }

  h.addTokenEx(TokenType.RegexLiteral, value, start, h.position, startLine, startColumn);
}
