/**
 * MASI — Max-Accept Semicolon Insertion
 *
 * Postpass over raw tokens that inserts virtual semicolons at line
 * boundaries where the grammar would otherwise be ambiguous.
 *
 * Extracted from lexer.ts to separate layout postprocessing from
 * raw character scanning.
 */

import { Token, TokenType } from './lexer';

/**
 * Insert virtual semicolons between tokens at line boundaries.
 * Pure function: takes a token array, returns a new array with
 * virtual semicolons inserted where appropriate.
 */
export function applyMASI(tokens: Token[]): Token[] {
  const result: Token[] = [];
  let jsxDepth = 0;
  let braceDepth = 0;
  let parenDepth = 0;
  let bracketDepth = 0;
  let curlyDepth = 0;

  for (let i = 0; i < tokens.length; i++) {
    const token = tokens[i];
    result.push(token);

    // Track grouping depth for vsemi suppression
    if (token.value === '(') parenDepth++;
    else if (token.value === ')') parenDepth = Math.max(0, parenDepth - 1);
    else if (token.value === '[') bracketDepth++;
    else if (token.value === ']') bracketDepth = Math.max(0, bracketDepth - 1);
    else if (token.value === '{') curlyDepth++;
    else if (token.value === '}') curlyDepth = Math.max(0, curlyDepth - 1);

    // Track JSX context
    if (token.value === '<' && i + 1 < tokens.length) {
      const next = tokens[i + 1];
      const prev = i > 0 ? tokens[i - 1] : null;
      const isGeneric = prev && (prev.type === TokenType.Identifier || prev.type === TokenType.Keyword) &&
        !['return', 'yield', 'throw', 'case', 'in', 'of', 'typeof', 'instanceof', 'new', 'delete', 'void', 'await'].includes(prev.value);
      if (!isGeneric && (next.type === TokenType.Identifier ||
          next.value === '>' ||
          (next.value === '/' && i + 2 < tokens.length &&
           (tokens[i + 2].type === TokenType.Identifier || tokens[i + 2].value === '>')))) {
        jsxDepth++;
      }
    } else if (token.value === '>' && jsxDepth > 0) {
      if (i > 0 && tokens[i - 1].value === '/') {
        jsxDepth--;
      } else if (i > 2 && tokens[i - 2].value === '<' && tokens[i - 1].value === '/') {
        jsxDepth--;
      }
    } else if (token.value === '{' && jsxDepth > 0) {
      braceDepth++;
    } else if (token.value === '}' && jsxDepth > 0 && braceDepth > 0) {
      braceDepth--;
    }

    // Check if we should insert virtual semicolon after this token
    if (i < tokens.length - 1) {
      const nextToken = tokens[i + 1];

      // Check if there's a line break between tokens
      let tokenEndLine = token.line;
      if (token.value && token.value.includes('\n')) {
        tokenEndLine = token.line + (token.value.match(/\n/g) || []).length;
      }
      if (nextToken.line > tokenEndLine) {
        const inJSXContext = jsxDepth > 0;
        const isJSXExpressionClose = token.value === '}' && braceDepth >= 0 && jsxDepth > 0;
        const prevToken = i > 0 ? tokens[i - 1] : null;

        if (parenDepth > 0 || bracketDepth > 0) {
          // Suppress vsemis inside () and [] (Python/multi-line expressions)
        } else if (!shouldSuppressVirtualSemi(token, nextToken, inJSXContext || isJSXExpressionClose, prevToken)) {
          const virtualSemi: Token = {
            type: TokenType.VirtualSemi,
            value: ';',
            start: token.end,
            end: token.end,
            line: token.line,
            column: token.column + token.value.length,
            virtualSemi: true
          };
          result.push(virtualSemi);
        }
      }
    }
  }

  return result;
}

function shouldSuppressVirtualSemi(current: Token, next: Token, inJSX: boolean, prev: Token | null): boolean {
  // Some keywords always get a virtual semicolon when alone on a line
  const alwaysTerminateKeywords = ['return', 'break', 'continue', 'throw', 'yield'];
  if (current.type === TokenType.Keyword && alwaysTerminateKeywords.includes(current.value)) {
    return false;
  }

  // Never insert virtual semicolons inside JSX content
  if (inJSX) {
    return true;
  }

  // ? after ) or ] is likely the try operator, not ternary start
  if (current.value === '?' && prev && (prev.value === ')' || prev.value === ']')) {
    return false;
  }

  // Suppress after line-continuation characters
  const suppressChars = ['.', ',', ':', ';', '+', '-', '*', '/', '%', '&', '|', '^', '<', '>', '=', '!', '~', '?', '(', '[', '{', '`'];
  if (suppressChars.includes(current.value)) {
    return true;
  }

  // Suppress before continuation operators on next line
  const continuationOps = ['?.', '|>', '..', '::', '||', '&&', '??', '->'];
  if (continuationOps.includes(next.value)) {
    return true;
  }

  const singleCharContinuation = ['.', '?', '|', '&', '+', '-', '*', '/', '%', '^'];
  if (singleCharContinuation.includes(next.value)) {
    return true;
  }

  // Suppress if next line is more indented
  if (next.indentCol !== undefined && current.indentCol !== undefined) {
    if (next.indentCol > current.indentCol) {
      return true;
    }
  }

  return false;
}
