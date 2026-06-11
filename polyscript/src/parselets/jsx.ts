/**
 * JSX Parselet — Extracted from Parser (Chunk 2).
 *
 * All JSX parsing logic: element detection, disambiguation (type assertions vs JSX),
 * opening/closing elements, attributes, children, fragments, text, expressions.
 *
 * Depends on a narrow JSXHost interface — no direct Parser import.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';

// Narrow host interface — only what JSX parsing needs from the parser
export interface JSXHost {
  // Token array + cursor position (read/write for lookahead patterns)
  tokens: Token[];
  current: number;

  // Cursor methods
  peek(): Token;
  peekNext(): Token | undefined;
  peekAt(offset: number): Token | undefined;
  advance(): Token;
  check(value: string): boolean;
  match(...values: string[]): boolean;
  consume(expected: TokenType | string, message: string): Token;
  isAtEnd(): boolean;
  previous(): Token | undefined;
  error(token: Token, message: string): Error;
  createSpan(start: number, end: number): AST.Span;
  createSpanFrom(node: { span: AST.Span } | Token): AST.Span;

  // Parser methods needed by JSX
  parseExpression(): AST.Expr;
  parseType(): AST.TypeNode;
  parseStringLiteral(): AST.StringLiteral;
}

// ============ Detection / Disambiguation ============

export function isJSXElement(host: JSXHost): boolean {
  const saved = host.current;

  try {
    const token = host.peek();
    if (token.type !== TokenType.JSXTagStart && token.value !== "<") {
      return false;
    }
    host.advance(); // consume JSXTagStart or <

    const next = host.peek();

    // Pattern 1: Fragment <>
    if (next.value === ">") {
      host.current = saved;
      return true;
    }

    // Pattern 2: Closing tag </
    if (next.value === "/") {
      host.current = saved;
      return true;
    }

    // Pattern 3 & 4: Identifier (component or HTML element)
    if (next.type !== TokenType.Identifier) {
      host.current = saved;
      return false;
    }

    const elementName = next.value;

    // Pattern 3: Capital letter → JSX Component (but check for type assertion)
    if (/^[A-Z]/.test(elementName)) {
      host.current = saved;
      if (couldBeTypeAssertion(host)) {
        return false;
      }
      return isValidJSXContinuation(host);
    }

    // Pattern 4: HTML tag name → JSX Element
    if (isHTMLTag(elementName)) {
      host.current = saved;
      return isValidJSXContinuation(host);
    }

    // Pattern 5: Qualified name (namespace.component)
    host.advance(); // consume identifier
    if (host.peek().value === ".") {
      host.current = saved;
      return isValidJSXContinuation(host);
    }

    // Not a JSX pattern - check if it's a primitive type (non-JSX pattern)
    if (isPrimitiveType(elementName)) {
      host.current = saved;
      return false;
    }

    // For other identifiers, use lookahead to disambiguate
    host.current = saved;
    return isValidJSXContinuation(host);
  } catch {
    host.current = saved;
    return false;
  }
}

export function isPrimitiveType(name: string): boolean {
  const primitiveTypes = new Set([
    'string', 'number', 'boolean', 'object', 'undefined', 'null',
    'bigint', 'symbol', 'any', 'unknown', 'never', 'void'
  ]);
  return primitiveTypes.has(name);
}

export function couldBeTypeAssertion(host: JSXHost): boolean {
  const saved = host.current;

  try {
    host.advance(); // consume <
    const identifier = host.advance(); // consume identifier

    if (host.peek().value !== ">") {
      host.current = saved;
      return false;
    }

    host.advance(); // consume >

    const next = host.peek();

    // Check for JSX closing tag pattern first
    if (next.type === TokenType.Identifier ||
        next.type === TokenType.StringLiteral) {
      let checkPos = host.current;

      while (checkPos < host.tokens.length && checkPos < host.current + 10) {
        const tok = host.tokens[checkPos];

        if (tok.type === TokenType.StringLiteral && /^\s+$/.test(tok.value)) {
          checkPos++;
          continue;
        }

        if (tok.value === "<" &&
            checkPos + 1 < host.tokens.length &&
            host.tokens[checkPos + 1].value === "/") {
          host.current = saved;
          return false; // Definitely JSX
        }

        if (tok.value && tok.value.trim()) {
          break;
        }

        checkPos++;
      }
    }

    // Only consider it a type assertion if followed by clear expression starters
    if (next.type === TokenType.Identifier) {
      const afterIdent = host.tokens[host.current + 1];
      if (afterIdent && (
          afterIdent.value === "." ||
          afterIdent.value === "(" ||
          afterIdent.value === "[" ||
          afterIdent.value === ";" ||
          afterIdent.value === "," ||
          afterIdent.value === ")" ||
          afterIdent.type === TokenType.Operator)) {
        host.current = saved;
        return true;
      }
      host.current = saved;
      return false;
    }

    // Clear type assertion patterns
    if (next.value === "(" ||
        next.value === "[" ||
        next.type === TokenType.NumericLiteral) {
      host.current = saved;
      return true;
    }

    // Special case: { could be object literal OR JSX expression
    if (next.value === "{") {
      let checkPos = host.current;
      let braceDepth = 0;

      while (checkPos < host.tokens.length && checkPos < host.current + 20) {
        const tok = host.tokens[checkPos];

        if (tok.value === "{") braceDepth++;
        else if (tok.value === "}") {
          braceDepth--;
          if (braceDepth === 0) {
            if (checkPos + 1 < host.tokens.length) {
              const after = host.tokens[checkPos + 1];
              if (after.value === "<" &&
                  checkPos + 2 < host.tokens.length &&
                  host.tokens[checkPos + 2].value === "/") {
                host.current = saved;
                return false; // It's JSX
              }
            }
            break;
          }
        }

        checkPos++;
      }

      host.current = saved;
      return false;
    }

    // Default to JSX interpretation for ambiguous cases
    host.current = saved;
    return false;
  } catch {
    host.current = saved;
    return false;
  }
}

export function isInJSXExpressionContext(host: JSXHost): boolean {
  if (host.current === 0) return true;

  const meaningfulTokens: Token[] = [];

  for (let i = host.current - 1; i >= 0 && meaningfulTokens.length < 5; i--) {
    const token = host.tokens[i];

    if (token.type === TokenType.Whitespace ||
        token.type === TokenType.Comment ||
        token.virtualSemi) {
      continue;
    }

    meaningfulTokens.unshift(token);

    if (token.value === ';' || token.value === '}' || token.newline) {
      break;
    }
  }

  if (meaningfulTokens.length === 0) return true;

  const lastToken = meaningfulTokens[meaningfulTokens.length - 1];

  switch (lastToken.value) {
    case '=':
    case ':=':
    case '+=':
    case '-=':
    case '*=':
    case '/=':
    case 'return':
    case '?':
    case ':':
    case '&&':
    case '||':
    case '!':
    case '[':
    case '{':
    case ',':
    case '(':
    case '=>':
    case 'yield':
    case 'throw':
    case 'await':
      return true;

    case 'extends':
    case 'implements':
    case 'instanceof':
      return false;
  }

  if (meaningfulTokens.length >= 2) {
    const recent = meaningfulTokens.slice(-2);

    if (recent[0].type === TokenType.Identifier && recent[1].value === '?') {
      return true;
    }

    if (recent[0].value === ')' && recent[1].value === '?') {
      return true;
    }
  }

  return true;
}

export function isValidJSXContinuation(host: JSXHost): boolean {
  const saved = host.current;

  try {
    host.advance(); // consume <
    host.advance(); // consume identifier

    // Handle generic type parameters
    if (host.peek().value === "<") {
      host.advance(); // consume <
      let depth = 1;
      while (!host.isAtEnd() && depth > 0) {
        const token = host.peek();
        if (token.value === "<") depth++;
        else if (token.value === ">") depth--;
        host.advance();
      }
    }

    while (!host.isAtEnd()) {
      const token = host.peek();

      if (token.value === ">" || token.value === "/") {
        return true;
      }

      if (token.type === TokenType.Identifier || token.type === TokenType.Keyword || token.value === "{") {
        return true;
      }

      if (token.value === ".") {
        host.advance();
        if (host.peek().type === TokenType.Identifier) {
          host.advance();
          continue;
        }
        return false;
      }

      if (token.type === TokenType.Whitespace ||
          (token.type === TokenType.StringLiteral && /^\s+$/.test(token.value))) {
        host.advance();
        continue;
      }

      return false;
    }

    return false;
  } finally {
    host.current = saved;
  }
}

export function isHTMLTag(name: string): boolean {
  const htmlTags = new Set([
    'a', 'abbr', 'address', 'area', 'article', 'aside', 'audio',
    'b', 'base', 'bdi', 'bdo', 'blockquote', 'body', 'br', 'button',
    'canvas', 'caption', 'cite', 'code', 'col', 'colgroup',
    'data', 'datalist', 'dd', 'del', 'details', 'dfn', 'dialog', 'div', 'dl', 'dt',
    'em', 'embed',
    'fieldset', 'figcaption', 'figure', 'footer', 'form',
    'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'head', 'header', 'hgroup', 'hr', 'html',
    'i', 'iframe', 'img', 'input', 'ins',
    'kbd', 'label', 'legend', 'li', 'link',
    'main', 'map', 'mark', 'menu', 'meta', 'meter',
    'nav', 'noscript',
    'object', 'ol', 'optgroup', 'option', 'output',
    'p', 'param', 'picture', 'pre', 'progress',
    'q', 'rp', 'rt', 'ruby',
    's', 'samp', 'script', 'section', 'select', 'slot', 'small', 'source', 'span',
    'strong', 'style', 'sub', 'summary', 'sup', 'svg',
    'table', 'tbody', 'td', 'template', 'textarea', 'tfoot', 'th', 'thead', 'time',
    'title', 'tr', 'track',
    'u', 'ul',
    'var', 'video',
    'wbr'
  ]);

  return htmlTags.has(name.toLowerCase());
}

// ============ Element Parsing ============

export function parseJSXElement(host: JSXHost): AST.JSXElement {
  const start = host.current;
  const openingElement = parseJSXOpeningElement(host);

  if (openingElement.selfClosing) {
    return {
      kind: "JSXElement",
      openingElement,
      closingElement: null,
      children: [],
      span: host.createSpan(start, host.current - 1)
    };
  }

  const children = parseJSXChildren(host);
  const closingElement = parseJSXClosingElement(host);

  const openName = getJSXElementNameString(openingElement.name);
  const closeName = getJSXElementNameString(closingElement.name);

  if (openName !== closeName) {
    throw host.error(host.previous()!,
      `JSX closing tag </${closeName}> doesn't match opening tag <${openName}>`);
  }

  return {
    kind: "JSXElement",
    openingElement,
    closingElement,
    children,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseJSXFragment(host: JSXHost): AST.JSXFragment {
  const start = host.current;

  host.advance(); // consume '<'
  host.consume(">", "Expected '>'");

  const children = parseJSXChildren(host);

  // Consume </>
  host.consume("<", "Expected '</'");
  host.consume("/", "Expected '/'");
  host.consume(">", "Expected '>'");

  return {
    kind: "JSXFragment",
    children,
    span: host.createSpan(start, host.current - 1)
  };
}

function parseJSXOpeningElement(host: JSXHost): AST.JSXOpeningElement {
  const start = host.current;

  host.advance(); // consume '<'
  const name = parseJSXElementName(host);

  // Check for generic type arguments
  let typeArguments: AST.TypeNode[] | undefined;

  if (host.peek().value === "<") {
    const checkpoint = host.current;
    try {
      host.advance(); // consume '<'
      typeArguments = [];

      do {
        typeArguments.push(host.parseType());
      } while (host.match(","));

      if (host.peek().value === ">") {
        host.advance();
      } else if (host.peek().value === ">>") {
        const originalToken = host.tokens[host.current];
        host.tokens[host.current] = { ...originalToken, value: ">" };
        host.advance();
      } else if (host.peek().value === ">>>") {
        const originalToken = host.tokens[host.current];
        host.tokens[host.current] = { ...originalToken, value: ">" };
        host.advance();
      } else {
        host.current = checkpoint;
        typeArguments = undefined;
      }
    } catch {
      host.current = checkpoint;
      typeArguments = undefined;
    }
  }

  const attributes = parseJSXAttributes(host);

  skipJSXWhitespace(host);

  const selfClosing = host.match("/");
  host.consume(">", "Expected '>'");

  const result: any = {
    kind: "JSXOpeningElement",
    name,
    attributes,
    selfClosing,
    span: host.createSpan(start, host.current - 1)
  };

  if (typeArguments) {
    result.typeArguments = typeArguments;
  }

  return result;
}

function parseJSXClosingElement(host: JSXHost): AST.JSXClosingElement {
  const start = host.current;

  host.advance(); // consume '<'
  host.consume("/", "Expected '/'");
  const name = parseJSXElementName(host);
  host.consume(">", "Expected '>'");

  return {
    kind: "JSXClosingElement",
    name,
    span: host.createSpan(start, host.current - 1)
  };
}

function parseJSXElementName(host: JSXHost): AST.JSXElementName {
  const start = host.current;

  if (!host.peek() || host.peek().type !== TokenType.Identifier) {
    throw host.error(host.peek(), "Expected JSX element name");
  }

  let name: AST.JSXElementName = {
    kind: "JSXIdentifier",
    name: host.advance().value,
    span: host.createSpan(start, host.current - 1)
  };

  while (host.match(".")) {
    const propStart = host.current;
    if (host.peek().type !== TokenType.Identifier) {
      throw host.error(host.peek(), "Expected identifier after '.'");
    }

    const property: AST.JSXIdentifier = {
      kind: "JSXIdentifier",
      name: host.advance().value,
      span: host.createSpan(propStart, host.current - 1)
    };

    name = {
      kind: "JSXMemberExpression",
      object: name,
      property,
      span: host.createSpan(start, host.current - 1)
    };
  }

  return name;
}

// ============ Attributes ============

function parseJSXAttributes(host: JSXHost): AST.JSXAttribute[] {
  const attributes: AST.JSXAttribute[] = [];

  while (!host.isAtEnd() && !host.check(">") && !host.check("/")) {
    while (host.peek().virtualSemi) {
      host.advance();
    }

    skipJSXWhitespace(host);

    if (host.check(">") || host.check("/")) {
      break;
    }

    if (host.check("{")) {
      attributes.push(parseJSXSpreadAttribute(host));
    } else if (host.peek().type === TokenType.Identifier || host.peek().type === TokenType.Keyword) {
      attributes.push(parseJSXAttribute(host));
    } else {
      break;
    }
  }

  return attributes;
}

function parseJSXAttribute(host: JSXHost): AST.JSXNormalAttribute {
  const start = host.current;
  const name = parseJSXAttributeName(host);

  let value: AST.JSXAttributeValue | null = null;

  if (host.match("=")) {
    if (host.check("{")) {
      value = parseJSXExpressionContainer(host);
    } else if (host.peek().type === TokenType.StringLiteral) {
      value = host.parseStringLiteral();
    } else if (host.check("<")) {
      value = parseJSXElement(host);
    }
  }

  return {
    kind: "JSXAttribute",
    name,
    value,
    span: host.createSpan(start, host.current - 1)
  };
}

function parseJSXAttributeName(host: JSXHost): AST.JSXIdentifier | AST.JSXNamespacedName {
  const start = host.current;

  const token = host.peek();
  if (token.type !== TokenType.Identifier && token.type !== TokenType.Keyword) {
    throw host.error(host.peek(), "Expected attribute name");
  }

  const namespace: AST.JSXIdentifier = {
    kind: "JSXIdentifier",
    name: host.advance().value,
    span: host.createSpan(start, host.current - 1)
  };

  // Namespaced attribute like xmlns:xlink
  if (host.match(":")) {
    const nameStart = host.current;
    if (host.peek().type !== TokenType.Identifier && host.peek().type !== TokenType.Keyword) {
      throw host.error(host.peek(), "Expected identifier after ':'");
    }

    const name: AST.JSXIdentifier = {
      kind: "JSXIdentifier",
      name: host.advance().value,
      span: host.createSpan(nameStart, host.current - 1)
    };

    return {
      kind: "JSXNamespacedName",
      namespace,
      name,
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Hyphenated attributes like data-testid
  if (host.match("-")) {
    let fullName = namespace.name + "-";
    while (host.peek().type === TokenType.Identifier) {
      fullName += host.advance().value;
      if (!host.match("-")) break;
      fullName += "-";
    }

    return {
      kind: "JSXIdentifier",
      name: fullName,
      span: host.createSpan(start, host.current - 1)
    };
  }

  return namespace;
}

function parseJSXSpreadAttribute(host: JSXHost): AST.JSXSpreadAttribute {
  const start = host.current;

  host.consume("{", "Expected '{'");
  host.consume("...", "Expected '...'");
  const argument = host.parseExpression();
  host.consume("}", "Expected '}'");

  return {
    kind: "JSXSpreadAttribute",
    argument,
    span: host.createSpan(start, host.current - 1)
  };
}

// ============ Children ============

function parseJSXChildren(host: JSXHost): AST.JSXChild[] {
  const children: AST.JSXChild[] = [];

  while (!host.isAtEnd()) {
    const beforePos = host.current;
    if (host.check("<") && host.peekNext()?.value === "/") {
      break;
    }

    while (host.peek().virtualSemi) {
      host.advance();
    }

    if (host.check("{")) {
      if (host.peekNext()?.value === "...") {
        const start = host.current;
        host.advance(); // consume {
        host.advance(); // consume ...
        const expression = host.parseExpression();
        host.consume("}", "Expected '}'");

        children.push({
          kind: "JSXSpreadChild",
          expression,
          span: host.createSpan(start, host.current - 1)
        });
      } else {
        children.push(parseJSXExpressionContainer(host));
      }
    } else if (host.check("<")) {
      if (host.peekNext()?.value === ">") {
        children.push(parseJSXFragment(host));
      } else {
        children.push(parseJSXElement(host));
      }
    } else {
      const text = parseJSXText(host);
      if (text) {
        children.push(text);
      }
    }
    if (host.current === beforePos) {
      // No progress — not actually JSX content (e.g. a Rust qualified path
      // `<A as B>::C` routed here). Bail instead of spinning forever.
      break;
    }
  }

  return children;
}

function parseJSXText(host: JSXHost): AST.JSXText | null {
  const start = host.current;
  let text = "";
  let raw = "";
  let lastTokenEnd = -1;

  while (!host.isAtEnd()) {
    const token = host.peek();

    if (token.value === "<" || token.value === "{") {
      break;
    }

    if (token.virtualSemi) {
      host.advance();
      continue;
    }

    if (lastTokenEnd >= 0 && token.start > lastTokenEnd) {
      text += " ";
      raw += " ";
    }

    if (token.type === TokenType.StringLiteral && token.value.includes("</")) {
      if (splitStringLiteralBeforeJSXClosingTag(host, token)) {
        continue;
      }
    }

    if (token.type === TokenType.Identifier ||
        token.type === TokenType.Keyword ||
        token.type === TokenType.NumericLiteral ||
        token.type === TokenType.StringLiteral) {
      text += token.value;
      raw += token.value;
      lastTokenEnd = token.end;
      host.advance();
    } else if (token.value === ">" || token.value === "}") {
      break;
    } else {
      text += token.value;
      raw += token.value;
      lastTokenEnd = token.end;
      host.advance();
    }
  }

  if (text === "") {
    return null;
  }

  return {
    kind: "JSXText",
    value: text,
    raw,
    span: host.createSpan(start, host.current - 1)
  };
}

function splitStringLiteralBeforeJSXClosingTag(host: JSXHost, token: Token): boolean {
  const closeOffset = token.value.indexOf("</");
  if (closeOffset <= 0) return false;

  const before = token.value.slice(0, closeOffset);
  const rest = token.value.slice(closeOffset);
  const closeMatch = rest.match(/^<\/([A-Za-z_$][\w$.-]*)(>)(.*)$/);
  if (!closeMatch) return false;

  const closeStart = token.start + closeOffset;
  const tagName = closeMatch[1];
  const after = closeMatch[3] || "";
  const synthetic: Token[] = [
    { ...token, type: TokenType.Operator, value: "<", start: closeStart, end: closeStart + 1 },
    { ...token, type: TokenType.Operator, value: "/", start: closeStart + 1, end: closeStart + 2 },
    { ...token, type: TokenType.Identifier, value: tagName, start: closeStart + 2, end: closeStart + 2 + tagName.length },
    { ...token, type: TokenType.Operator, value: ">", start: closeStart + 2 + tagName.length, end: closeStart + 3 + tagName.length },
  ];

  const afterStart = closeStart + 3 + tagName.length;
  for (let i = 0; i < after.length; i++) {
    const value = after[i];
    if (!value.trim()) continue;
    synthetic.push({
      ...token,
      type: TokenType.Operator,
      value,
      start: afterStart + i,
      end: afterStart + i + 1,
    });
  }

  host.tokens[host.current] = {
    ...token,
    value: before,
    end: token.start + closeOffset,
  };
  host.tokens.splice(host.current + 1, 0, ...synthetic);
  for (let i = host.current + 1 + synthetic.length; i < host.tokens.length; i++) {
    const nextToken = host.tokens[i];
    if (nextToken?.type !== TokenType.StringLiteral || !/^\s*$/.test(nextToken.value)) continue;
    if (nextToken.value.includes("\n")) {
      host.tokens[i] = {
        ...nextToken,
        type: TokenType.VirtualSemi,
        value: ";",
        virtualSemi: true,
      };
    } else {
      host.tokens.splice(i, 1);
      i--;
    }
  }
  return true;
}

// ============ Expression Containers ============

function parseJSXExpressionContainer(host: JSXHost): AST.JSXExpressionContainer {
  const start = host.current;

  host.consume("{", "Expected '{'");

  skipJSXWhitespace(host);

  if (host.check("}")) {
    host.advance();
    return {
      kind: "JSXExpressionContainer",
      expression: {
        kind: "JSXEmptyExpression",
        span: host.createSpan(start + 1, host.current - 1)
      },
      span: host.createSpan(start, host.current - 1)
    };
  }

  const expression = host.parseExpression();

  skipJSXWhitespace(host);
  host.consume("}", "Expected '}'");

  return {
    kind: "JSXExpressionContainer",
    expression,
    span: host.createSpan(start, host.current - 1)
  };
}

// Dead code in original — kept for reference but not exported
function parseJSXExpression(host: JSXHost): AST.Expr {
  const originalTokens = host.tokens;
  const originalCurrent = host.current;

  const filteredTokens: Token[] = [];
  const indexMap: number[] = [];

  for (let i = originalCurrent; i < originalTokens.length; i++) {
    const token = originalTokens[i];

    if (token.value === "}" && token.type === TokenType.Operator) {
      filteredTokens.push(token);
      indexMap.push(i);
      break;
    }

    if (token.type === TokenType.StringLiteral && /^\s+$/.test(token.value)) {
      continue;
    }

    filteredTokens.push(token);
    indexMap.push(i);
  }

  host.tokens = [...originalTokens.slice(0, originalCurrent), ...filteredTokens];
  host.current = originalCurrent;

  try {
    const expr = host.parseExpression();
    const movedInFiltered = host.current - originalCurrent;
    host.tokens = originalTokens;
    host.current = indexMap[movedInFiltered - 1] + 1;
    return expr;
  } catch (error) {
    host.tokens = originalTokens;
    host.current = originalCurrent;
    throw error;
  }
}

// ============ Helpers ============

export function getJSXElementNameString(name: AST.JSXElementName): string {
  switch (name.kind) {
    case "JSXIdentifier":
      return name.name;
    case "JSXMemberExpression":
      return getJSXElementNameString(name.object) + "." + name.property.name;
    case "JSXNamespacedName":
      return name.namespace.name + ":" + name.name.name;
  }
}

export function skipJSXWhitespace(host: JSXHost): void {
  while (host.peek().type === TokenType.StringLiteral && /^\s*$/.test(host.peek().value)) {
    host.advance();
  }
}
