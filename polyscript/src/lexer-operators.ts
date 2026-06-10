/**
 * Operator scanner + JSX/mode transition logic.
 */

import { TokenType } from './lexer';
import { ScanHost } from './lexer-cursor';
import * as LS from './lex-state';
import { OPERATOR_TRIE } from './operator-trie';

export function scanOperator(h: ScanHost, htmlTags: Set<string>): void {
  const start = h.position - 1;
  const startLine = h.line;
  const startColumn = h.column - 1;
  let value = h.source[start];

  // Handle JSX context detection for '<' and '>' per spec 10.6
  if (value === '<') {
    const nextChar = h.peek();

    if (nextChar === '/') {
      // JSX closing tag </tagname> - per spec
      // Don't enterJSX — the opening tag already incremented jsxDepth.
      // Just mark as closing tag so '>' knows to exitJSX.
      if (LS.isInJSXText(h.state)) {
        LS.exitJSXText(h.state);
      }
      h.state.inJSXClosingTag = true;
      // Emit JSXTagStart for closing tags too
      h.addTokenEx(TokenType.JSXTagStart, '<', start, h.position, startLine, startColumn);
      return;
    } else if (nextChar === '>') {
      // JSX Fragment <> - per spec
      if (LS.canBeJSX(h.state)) {
        h.addTokenEx(TokenType.JSXTagStart, '<', start, h.position, startLine, startColumn);
        LS.enterJSX(h.state);
        LS.enterJSXText(h.state);
        return;
      }
    } else if (nextChar && /[A-Z]/.test(nextChar)) {
      // Capital letter → JSX Component - per spec
      if (LS.canBeJSX(h.state)) {
        const identifier = h.peekIdentifier();
        const posAfterIdentifier = h.position + identifier.length;
        const charAfterIdentifier = h.source[posAfterIdentifier];

        if (charAfterIdentifier === '<') {
          const posAfterGeneric = h.peekPastGenericParams(posAfterIdentifier);
          const afterGeneric = h.source[posAfterGeneric];

          if (afterGeneric === '>' && h.source[posAfterGeneric + 1] === '{') {
            // <Type<...>>{ — type assertion with object literal, not JSX
          } else if (afterGeneric === '/' || afterGeneric === '>' ||
              (afterGeneric && /[a-zA-Z_]/.test(afterGeneric))) {
            h.addTokenEx(TokenType.JSXTagStart, '<', start, h.position, startLine, startColumn);
            LS.enterJSX(h.state);
            return;
          } else if (afterGeneric === ' ') {
            // Peek past whitespace: JSX has attribute names, not = or operators
            let peekPos = posAfterGeneric;
            while (peekPos < h.source.length && h.source[peekPos] === ' ') peekPos++;
            const afterSpace = h.source[peekPos];
            if (afterSpace && /[a-zA-Z_/>{]/.test(afterSpace)) {
              h.addTokenEx(TokenType.JSXTagStart, '<', start, h.position, startLine, startColumn);
              LS.enterJSX(h.state);
              return;
            }
          }
        } else if (charAfterIdentifier !== ',') {
          // `<I,` is a generic parameter list (`fn new<I, S>`), never JSX —
          // a JSX tag name is never directly followed by a comma.
          h.addTokenEx(TokenType.JSXTagStart, '<', start, h.position, startLine, startColumn);
          LS.enterJSX(h.state);
          return;
        }
      }
    } else if (nextChar && /[a-z]/.test(nextChar)) {
      // Lowercase identifier → potential HTML element - per spec
      if (LS.canBeJSX(h.state)) {
        const tagName = h.peekIdentifier();
        if (htmlTags.has(tagName.toLowerCase())) {
          const posAfterTag = h.position + tagName.length;
          const charAfterTag = h.source[posAfterTag];

          if (charAfterTag === '<') {
            const posAfterGeneric = h.peekPastGenericParams(posAfterTag);
            const afterGeneric = h.source[posAfterGeneric];

            if (afterGeneric === ' ' || afterGeneric === '/' || afterGeneric === '>' ||
                (afterGeneric && /[a-zA-Z_]/.test(afterGeneric))) {
              h.addTokenEx(TokenType.JSXTagStart, '<', start, h.position, startLine, startColumn);
              LS.enterJSX(h.state);
              return;
            }
          } else {
            h.addTokenEx(TokenType.JSXTagStart, '<', start, h.position, startLine, startColumn);
            LS.enterJSX(h.state);
            return;
          }
        }
      }
    }
  } else if (value === '>' && LS.isInJSX(h.state)) {
    // Handle JSX tag completion
    if (h.state.inJSXClosingTag) {
      LS.exitJSX(h.state);
    } else {
      const prevChar = h.position > 1 ? h.source[h.position - 2] : '';
      if (prevChar === '/') {
        LS.exitJSX(h.state);
      } else {
        LS.enterJSXText(h.state);
      }
    }
  } else if (value === '{' && LS.isInJSX(h.state)) {
    LS.enterJSXExpression(h.state);
  } else if (value === '}' && h.state.inJSXExpression) {
    LS.exitJSXExpression(h.state);
  }

  // Handle mode transitions
  if (value === '.' && h.peek() !== '.' && h.peek() !== '*') {
    h.state.memberAccess = true;
  } else if (value === '?' && h.peek() === '.') {
    h.state.memberAccess = true;
  } else if (value === '!' && h.peek() === '.') {
    h.state.memberAccess = true;
  } else if (value === '[' && !h.state.memberAccess && !h.state.decorator && !h.state.bashCondition) {
    const nextNonWs = h.peekNextNonWhitespace();
    if (nextNonWs === '$' || nextNonWs === '"' || nextNonWs === '`' ||
        nextNonWs === '-' || nextNonWs === '!') {
      h.state.bashCondition = true;
      h.state.bashBracketDepth = 1;
    }
  } else if (value === ']' && h.state.bashCondition) {
    h.state.bashBracketDepth--;
    if (h.state.bashBracketDepth === 0) {
      h.state.bashCondition = false;
    }
  } else if (value === '@' && !h.state.memberAccess && !h.state.decorator && !h.state.bashCondition) {
    const next = h.peek();
    if (/[a-zA-Z_]/.test(next)) {
      h.state.decorator = true;
    }
  }

  // Longest-match via trie
  value = OPERATOR_TRIE.longestMatch(h.source, start);
  h.position = start + value.length;
  h.column = startColumn + value.length;

  if ((value === '?.' || value === '!.') && (h.peek() === '(' || h.peek() === '[')) {
    h.state.memberAccess = false;
  }

  // Rust :: path separator — next identifier should not be treated as keyword
  if (value === '::') {
    h.state.memberAccess = true;
  }

  h.addTokenEx(TokenType.Operator, value, start, h.position, startLine, startColumn);
}

export function shouldBeRegex(h: ScanHost): boolean {
  if (!h.lastNonWSToken) return true;

  if (h.lastNonWSToken.value === '<') {
    return false;
  }

  const canEndExpression = [
    TokenType.Identifier, TokenType.SigilIdentifier,
    TokenType.NumericLiteral, TokenType.StringLiteral,
    TokenType.TemplateLiteral, TokenType.RegexLiteral
  ];

  if (canEndExpression.includes(h.lastNonWSToken.type)) {
    return false;
  }

  const endTokens = [']', ')', '}', '++', '--', '>'];
  if (endTokens.includes(h.lastNonWSToken.value)) {
    return false;
  }

  return true;
}
