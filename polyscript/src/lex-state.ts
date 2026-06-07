/**
 * LexState — Single flat mutable state for lexer context.
 *
 * Replaces both LexerMode stack and ContextTracker with cheap
 * mutable fields. No snapshots, no JSON cloning.
 */

export interface LexState {
  // Mode flags (replaces modeStack)
  memberAccess: boolean;
  bashCondition: boolean;
  bashBracketDepth: number;
  decorator: boolean;

  // JSX tracking (replaces ContextTracker JSX fields)
  jsxDepth: number;
  inJSXClosingTag: boolean;
  inJSXText: boolean;
  inJSXExpression: boolean;

  // Type tracking (replaces ContextTracker type fields)
  genericDepth: number;
  inTypeAnnotation: boolean;
  typeDepth: number;

  // Position flags (replaces ContextPosition — only the ones actually read)
  afterReturn: boolean;
  afterEquals: boolean;
  afterArrow: boolean;
  afterOpenParen: boolean;
  afterOpenBrace: boolean;
  afterComma: boolean;
  afterColon: boolean;
  afterOperator: boolean;
  afterDot: boolean;
  lineStart: boolean;
}

export function createLexState(): LexState {
  return {
    memberAccess: false,
    bashCondition: false,
    bashBracketDepth: 0,
    decorator: false,

    jsxDepth: 0,
    inJSXClosingTag: false,
    inJSXText: false,
    inJSXExpression: false,

    genericDepth: 0,
    inTypeAnnotation: false,
    typeDepth: 0,

    afterReturn: false,
    afterEquals: false,
    afterArrow: false,
    afterOpenParen: false,
    afterOpenBrace: false,
    afterComma: false,
    afterColon: false,
    afterOperator: false,
    afterDot: false,
    lineStart: true,
  };
}

// ============ Queries ============

export function isInJSX(s: LexState): boolean {
  return s.jsxDepth > 0;
}

export function isInJSXText(s: LexState): boolean {
  return s.inJSXText;
}

export function isInType(s: LexState): boolean {
  return s.inTypeAnnotation || s.genericDepth > 0;
}

export function shouldPreserveWhitespace(s: LexState): boolean {
  if (s.inJSXExpression) return false;
  return s.inJSXText;
}

export function canBeJSX(s: LexState): boolean {
  if (isInType(s)) return false;
  return s.afterReturn || s.afterEquals || s.afterArrow ||
         s.afterOpenParen || s.afterOpenBrace || s.afterComma ||
         s.lineStart || s.afterColon || s.afterOperator;
}

export function canBeGeneric(s: LexState): boolean {
  return isInType(s) || s.afterDot ||
         (!s.afterOperator && !s.afterEquals);
}

// ============ Transitions ============

export function enterJSX(s: LexState): void {
  s.jsxDepth++;
  s.inJSXClosingTag = false;
}

export function exitJSX(s: LexState): void {
  if (s.jsxDepth > 0) {
    s.jsxDepth--;
    s.inJSXText = false;
    s.inJSXClosingTag = false;
  }
}

export function enterJSXText(s: LexState): void {
  s.inJSXText = true;
}

export function exitJSXText(s: LexState): void {
  s.inJSXText = false;
}

export function enterJSXExpression(s: LexState): void {
  s.inJSXExpression = true;
  s.inJSXText = false;
}

export function exitJSXExpression(s: LexState): void {
  s.inJSXExpression = false;
}

export function enterTypeAnnotation(s: LexState): void {
  s.inTypeAnnotation = true;
  s.typeDepth++;
}

export function exitTypeAnnotation(s: LexState): void {
  if (s.typeDepth > 0) {
    s.typeDepth--;
    s.inTypeAnnotation = s.typeDepth > 0;
  }
}

export function enterGeneric(s: LexState): void {
  s.genericDepth++;
}

export function exitGeneric(s: LexState): void {
  if (s.genericDepth > 0) {
    s.genericDepth--;
  }
}

export function updatePosition(s: LexState, tokenValue: string): void {
  // Reset all position flags
  s.afterReturn = false;
  s.afterEquals = false;
  s.afterArrow = false;
  s.afterOpenParen = false;
  s.afterOpenBrace = false;
  s.afterComma = false;
  s.afterColon = false;
  s.afterOperator = false;
  s.afterDot = false;
  s.lineStart = false;

  switch (tokenValue) {
    case 'return': case 'yield': case 'throw': case 'await': case 'new':
    case 'delete': case 'void': case 'typeof': case 'case': case 'in':
      s.afterReturn = true; break;
    case '=': s.afterEquals = true; break;
    case '=>': s.afterArrow = true; break;
    case '(': s.afterOpenParen = true; break;
    case '{': s.afterOpenBrace = true; break;
    case ',': s.afterComma = true; break;
    case ':': s.afterColon = true; break;
    case '.': s.afterDot = true; break;
    case ';': break; // afterSemicolon not needed
  }

  // Check if operator
  const operators = ['+', '-', '*', '/', '%', '**', '&', '|', '^', '~',
                    '<<', '>>', '>>>', '&&', '||', '??', '!',
                    '<', '>', '<=', '>=', '==', '!=', '===', '!=='];
  if (operators.includes(tokenValue)) {
    s.afterOperator = true;
  }
}
