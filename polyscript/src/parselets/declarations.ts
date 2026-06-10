/**
 * Declaration Parselets — var/const/short decl, type decl, package, export.
 *
 * Extracted from parser.ts to keep the Pratt expression core separate
 * from declaration-specific logic.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError } from '../parser-cursor';

// ============ Host Interface ============

export interface DeclHost {
  // Token navigation
  peek(): Token;
  peekNext(): Token | undefined;
  peekAt(offset: number): Token | undefined;
  advance(): Token;
  check(value: string): boolean;
  match(...values: string[]): boolean;
  consume(expected: TokenType | string, message: string): Token;
  previous(): Token | undefined;
  isAtEnd(): boolean;
  current: number;
  tokens: Token[];
  errors: ParseError[];

  // Span creation
  createSpan(start: number, end: number): AST.Span;
  createSpanFrom(node: { span: AST.Span } | Token): AST.Span;

  // Semicolons
  consumeSemicolon(): void;

  // Sub-parsers
  parseIdentifier(): AST.Identifier;
  parseExpression(minPrecedence?: number): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseExpressionList(): AST.Expr[];
  parseType(): AST.TypeNode;
  parseDeclaration(): AST.Decl;
  isDeclStart(): boolean;

  // Error
  error(token: Token, message: string): ParseError;
}

// ============ Variable Declarations ============

export function parseVarDecl(host: DeclHost): AST.VarDecl {
  const start = host.current - 1;

  // Rust-style: let mut x = ...
  if (host.peek().value === "mut") {
    host.advance();
  }

  // Destructuring: var/let { a, b } = ... or var/let [a, b] = ...
  if (host.check("{") || host.check("[")) {
    const pattern = parseDestructuringPattern(host);
    let type: AST.TypeNode | undefined;
    if (host.match(":")) {
      type = host.parseType();
    }
    let values: AST.Expr[] | undefined;
    if (host.match("=")) {
      values = host.parseExpressionList();
    }
    host.consumeSemicolon();
    const names = extractPatternNames(pattern);
    return { kind: "VarDecl", names, type, values, destructurePattern: pattern, span: host.createSpan(start, host.current - 1) } as any;
  }

  // Tuple destructuring: let/var (a, b) = ... vs Go grouped var: var ( name = value \n name2 = value2 )
  if (host.check("(")) {
    // Lookahead: if there's a comma before ) at depth 1, treat as tuple destructuring
    let isTupleDestructure = false;
    {
      let pos = host.current + 1; // after '('
      let depth = 1;
      while (pos < host.tokens.length && depth > 0) {
        const v = host.tokens[pos].value;
        if (v === "(") depth++;
        else if (v === ")") { depth--; if (depth === 0) break; }
        else if (v === "," && depth === 1) { isTupleDestructure = true; break; }
        pos++;
      }
      // Also check: if ')' is immediately followed by '=', it's destructuring even without comma
      if (!isTupleDestructure && depth === 0 && pos + 1 < host.tokens.length && host.tokens[pos + 1].value === "=") {
        isTupleDestructure = true;
      }
    }
    if (isTupleDestructure) {
      // Parse as tuple destructuring: (a, b) = expr
      host.advance(); // consume '('
      const names: AST.Identifier[] = [];
      names.push(host.parseIdentifier());
      while (host.match(",")) {
        names.push(host.parseIdentifier());
      }
      host.consume(")", "Expected ')' after tuple destructuring");
      let type: AST.TypeNode | undefined;
      if (host.match(":")) {
        type = host.parseType();
      }
      let values: AST.Expr[] | undefined;
      if (host.match("=")) {
        values = host.parseExpressionList();
      }
      host.consumeSemicolon();
      return {
        kind: "VarDecl",
        names,
        type,
        values,
        span: host.createSpan(start, host.current - 1)
      };
    }
    host.advance();
    const allNames: AST.Identifier[] = [];
    const allValues: AST.Expr[] = [];
    while (!host.check(")") && !host.isAtEnd()) {
      while (host.peek().virtualSemi) host.advance();
      if (host.check(")")) break;
      const name = host.parseIdentifier();
      allNames.push(name);
      // Skip Go type annotation (e.g., `limiters sync.Map`)
      if (!host.check("=") && !host.check(")") && !host.isAtEnd() && !host.peek().virtualSemi) {
        const next = host.peek();
        if (next.type === "Identifier" || next.type === "Keyword" || next.value === "[" || next.value === "*") {
          host.parseType();
        }
      }
      if (host.match("=")) {
        allValues.push(host.parseAssignmentExpression());
      }
      while (host.peek().virtualSemi) host.advance();
    }
    host.consume(")", "Expected ')' after grouped var declarations");
    host.consumeSemicolon();
    return {
      kind: "VarDecl",
      names: allNames,
      values: allValues.length > 0 ? allValues : undefined,
      span: host.createSpan(start, host.current - 1)
    };
  }

  const names = parseIdentifierList(host);

  let type: AST.TypeNode | undefined;
  if (host.match(":")) {
    type = host.parseType();
  } else if (!host.check("=") && !host.check(";") && !host.isAtEnd() && !host.peek().virtualSemi) {
    // Go-style: var count int = 10 (type follows name without colon)
    const next = host.peek();
    if (next.type === "Identifier" || next.type === "Keyword") {
      const val = next.value;
      // Only parse as type if it looks like a type name, not an operator or value
      if (val !== "=" && val !== "in" && val !== "of") {
        type = host.parseType();
      }
    } else if (next.value === "[" || next.value === "*") {
      // Go slice []type or pointer *type
      type = host.parseType();
    }
  }

  let values: AST.Expr[] | undefined;
  if (host.match("=")) {
    values = host.parseExpressionList();
  }

  host.consumeSemicolon();

  return {
    kind: "VarDecl",
    names,
    type,
    values,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseConstDecl(host: DeclHost): AST.ConstDecl {
  const start = host.current - 1;

  // Destructuring: const { a, b } = ... or const [a, b] = ...
  if (host.check("{") || host.check("[")) {
    const pattern = parseDestructuringPattern(host);
    let type: AST.TypeNode | undefined;
    if (host.match(":")) {
      type = host.parseType();
    }
    if (!host.match("=") && !host.match(":=")) {
      host.consume("=", "Const declaration requires initialization");
    }
    const values = host.parseExpressionList();
    host.consumeSemicolon();
    // Extract names from pattern for ConstDecl compatibility
    const names = extractPatternNames(pattern);
    return { kind: "ConstDecl", names, type, values, destructurePattern: pattern, span: host.createSpan(start, host.current - 1) } as any;
  }

  const names = parseIdentifierList(host);

  let type: AST.TypeNode | undefined;
  if (host.match(":")) {
    type = host.parseType();
  }

  if (!host.match("=") && !host.match(":=")) {
    host.consume("=", "Const declaration requires initialization");
  }
  const values = host.parseExpressionList();

  host.consumeSemicolon();

  return {
    kind: "ConstDecl",
    names,
    type,
    values,
    span: host.createSpan(start, host.current - 1)
  };
}

// ============ Short Declarations ============

export function parseShortDecl(host: DeclHost): AST.ShortDecl {
  const start = host.current;
  const targets: (AST.Identifier | AST.ArrayPattern | AST.ObjectPattern)[] = [];

  do {
    if (host.check("[") || host.check("{")) {
      targets.push(parseDestructuringPattern(host));
    } else if (host.peek().type === TokenType.Identifier) {
      targets.push(host.parseIdentifier());
    } else {
      throw host.error(host.peek(), "Expected identifier or destructuring pattern");
    }
  } while (host.match(","));

  host.consume(":=", "Expected ':=' in short declaration");

  const value = host.parseExpression();

  host.consumeSemicolon();

  // Simple single-identifier case — old format for compatibility
  if (targets.length === 1 && targets[0].kind === "Identifier") {
    return {
      kind: "ShortDecl",
      pairs: [{ name: targets[0], expr: value }],
      span: host.createSpan(start, host.current - 1)
    };
  }

  return {
    kind: "ShortDecl",
    targets,
    value,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseDestructuringShortDecl(host: DeclHost): AST.ShortDecl {
  const start = host.current;

  const pattern = parseDestructuringPattern(host);

  host.consume(":=", "Expected ':=' in destructuring declaration");

  const value = host.parseExpression();

  host.consumeSemicolon();

  return {
    kind: "ShortDecl",
    targets: [pattern],
    value,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseDestructuringPattern(host: DeclHost): AST.ArrayPattern | AST.ObjectPattern {
  const start = host.current;
  const token = host.peek();

  if (token.value === "[") {
    host.advance();
    const elements: (AST.Identifier | AST.ArrayPattern | AST.ObjectPattern | AST.ArrayPatternElement | null)[] = [];

    while (!host.check("]") && !host.isAtEnd()) {
      while (host.peek().virtualSemi) host.advance();

      if (host.check(",")) {
        elements.push(null);
        host.advance();
        continue;
      }

      const elementStart = host.current;
      let rest = false;
      if (host.check("...")) {
        host.advance();
        rest = true;
      }

      let element: AST.Identifier | AST.ArrayPattern | AST.ObjectPattern;
      if (host.check("[") || host.check("{")) {
        element = parseDestructuringPattern(host);
      } else if (host.peek().type === TokenType.Identifier) {
        element = host.parseIdentifier();
      } else {
        break;
      }

      let defaultValue: AST.Expr | undefined;
      if (!rest && host.match("=")) {
        defaultValue = host.parseAssignmentExpression();
      }

      if (rest || defaultValue) {
        elements.push({
          kind: "ArrayPatternElement",
          value: element,
          rest,
          defaultValue,
          span: host.createSpan(elementStart, host.current - 1)
        });
      } else {
        elements.push(element);
      }

      while (host.peek().virtualSemi) host.advance();

      if (rest) {
        break;
      }

      if (!host.match(",")) {
        break;
      }
    }

    host.consume("]", "Expected ']' after array pattern");

    return {
      kind: "ArrayPattern",
      elements,
      span: host.createSpan(start, host.current - 1)
    };
  } else {
    host.advance();
    const properties: AST.ObjectPatternProperty[] = [];

    while (!host.check("}") && !host.isAtEnd()) {
      while (host.peek().virtualSemi) host.advance();

      const propStart = host.current;

      // Rest element: ...identifier
      if (host.check("...")) {
        host.advance(); // consume ...
        const restId = host.parseIdentifier();
        properties.push({
          key: restId,
          value: restId,
          shorthand: true,
          rest: true,
          span: host.createSpan(propStart, host.current - 1)
        } as any);
        // Rest must be last
        break;
      }

      const key = host.parseIdentifier();

      let value: AST.Identifier | AST.ArrayPattern | AST.ObjectPattern = key;
      let shorthand = true;

      if (host.match(":")) {
        shorthand = false;
        if (host.check("[") || host.check("{")) {
          value = parseDestructuringPattern(host);
        } else {
          value = host.parseIdentifier();
        }
      }

      // Default value: { trailing = true }
      let defaultValue: AST.Expr | undefined;
      if (host.match("=")) {
        defaultValue = host.parseAssignmentExpression();
      }

      properties.push({
        key,
        value,
        shorthand,
        defaultValue,
        span: host.createSpan(propStart, host.current - 1)
      } as any);

      while (host.peek().virtualSemi) host.advance();

      if (!host.match(",")) {
        break;
      }
    }

    host.consume("}", "Expected '}' after object pattern");

    return {
      kind: "ObjectPattern",
      properties,
      span: host.createSpan(start, host.current - 1)
    };
  }
}

// ============ Type/Package/Export Declarations ============

export function parseTypeDecl(host: DeclHost): AST.TypeDecl {
  const start = host.current - 1;
  const name = host.parseIdentifier();

  let genericParams: AST.Identifier[] | undefined;
  if (host.match("<")) {
    genericParams = [];
    do {
      genericParams.push(host.parseIdentifier());
    } while (host.match(","));
    host.consume(">", "Expected '>' after generic parameters");
  }

  host.consume("=", "Expected '=' in type declaration");
  const definition = host.parseType();
  host.consumeSemicolon();

  return {
    kind: "TypeDecl",
    name,
    genericParams,
    definition,
    span: host.createSpan(start, host.current - 1)
  };
}

/**
 * Rust-style struct declaration: `struct Name { field: Type, ... }`,
 * `struct Name<T> { ... }`, tuple struct `struct Name(T, U);`, or unit
 * struct `struct Name;`. The body tokens are skipped — the node's span
 * preserves the raw source for verbatim Rust reconstruction.
 * The `struct` keyword has already been consumed by the caller.
 */
export function parseStructDecl(host: DeclHost): AST.TypeDecl {
  const start = host.current - 1;
  const name = host.parseIdentifier();

  let genericParams: AST.Identifier[] | undefined;
  if (host.match("<")) {
    genericParams = [];
    do {
      // Tolerate lifetimes ('a) and bounds by skipping non-identifier tokens
      if (host.peek().type === TokenType.Identifier) {
        genericParams.push(host.parseIdentifier());
      } else {
        host.advance();
      }
    } while (host.match(","));
    host.consume(">", "Expected '>' after generic parameters");
  }

  const skipBalanced = (open: string, close: string) => {
    host.advance(); // consume opening delimiter
    let depth = 1;
    while (depth > 0 && !host.isAtEnd()) {
      if (host.check(open)) depth++;
      else if (host.check(close)) depth--;
      host.advance();
    }
  };

  if (host.check("{")) {
    skipBalanced("{", "}");
  } else if (host.check("(")) {
    skipBalanced("(", ")");
    host.consumeSemicolon();
  } else {
    host.consumeSemicolon();
  }

  const span = host.createSpan(start, host.current - 1);
  return {
    kind: "TypeDecl",
    name,
    genericParams,
    definition: {
      kind: "SimpleType",
      id: { kind: "Identifier", name: "struct", span },
      span,
    } as AST.TypeNode,
    structDecl: true,
    span,
  };
}

export function parsePackageDecl(host: DeclHost): AST.PackageDecl {
  const start = host.current - 1;
  const nameToken = host.peek().type === TokenType.Identifier ?
    host.advance() :
    host.consume(TokenType.StringLiteral, "Expected package name");

  const name: AST.Identifier = {
    kind: "Identifier",
    name: nameToken.value,
    span: host.createSpanFrom(nameToken)
  };

  host.consumeSemicolon();

  return {
    kind: "PackageDecl",
    name,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseExportDecl(host: DeclHost): AST.ExportDecl {
  const start = host.current - 1;

  // Handle 'export default'
  if (host.match("default")) {
    let declaration: AST.Decl | undefined;

    if (host.isDeclStart()) {
      declaration = host.parseDeclaration();
    } else {
      const expr = host.parseExpression();
      host.consumeSemicolon();
    }

    return {
      kind: "ExportDecl",
      declaration,
      isDefault: true,
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Handle 'export type'
  if (host.match("type")) {
    if (host.check("{")) {
      const specifiers = parseExportSpecifiers(host);

      let source: string | undefined;
      if (host.match("from")) {
        if (host.peek().type === TokenType.StringLiteral) {
          source = host.advance().value.slice(1, -1);
        }
      }

      host.consumeSemicolon();

      return {
        kind: "ExportDecl",
        specifiers,
        source,
        span: host.createSpan(start, host.current - 1)
      };
    } else {
      const declaration = parseTypeDecl(host);
      return {
        kind: "ExportDecl",
        declaration,
        span: host.createSpan(start, host.current - 1)
      };
    }
  }

  // Export declaration (export function foo() {})
  if (host.isDeclStart()) {
    const declaration = host.parseDeclaration();
    return {
      kind: "ExportDecl",
      declaration,
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Export specifiers (export { foo, bar })
  if (host.check("{")) {
    const specifiers = parseExportSpecifiers(host);

    let source: string | undefined;
    if (host.match("from")) {
      if (host.peek().type === TokenType.StringLiteral) {
        source = host.advance().value.slice(1, -1);
      }
    }

    host.consumeSemicolon();

    return {
      kind: "ExportDecl",
      specifiers,
      source,
      span: host.createSpan(start, host.current - 1)
    };
  }

  // export * from / export * as X from
  if (host.check("*")) {
    host.advance();
    let namespaceAlias: string | undefined;
    if (host.match("as")) {
      namespaceAlias = host.advance().value;
    }
    if (host.match("from")) {
      let source: string | undefined;
      if (host.peek().type === TokenType.StringLiteral) {
        source = host.advance().value.slice(1, -1);
      }
      host.consumeSemicolon();

      const span = host.createSpan(start, host.current - 1);
      const specifiers: AST.ExportSpecifier[] | undefined = namespaceAlias
        ? [{
            local: { kind: "Identifier", name: "*", span } as AST.Identifier,
            exported: { kind: "Identifier", name: namespaceAlias, span } as AST.Identifier,
            span
          }]
        : undefined;

      return {
        kind: "ExportDecl",
        specifiers,
        source,
        span: host.createSpan(start, host.current - 1)
      };
    }
  }

  // Simple export: export Name
  if (host.peek().type === TokenType.Identifier) {
    host.advance();
    host.consumeSemicolon();

    return {
      kind: "ExportDecl",
      span: host.createSpan(start, host.current - 1)
    };
  }

  throw host.error(host.peek(), "Invalid export declaration");
}

export function parseExportSpecifiers(host: DeclHost): AST.ExportSpecifier[] {
  host.consume("{", "Expected '{'");
  const specifiers: AST.ExportSpecifier[] = [];

  if (!host.check("}")) {
    do {
      // Skip 'type' keyword in type-only exports (e.g., export { type Foo })
      host.match("type");
      const local = host.parseIdentifier();
      let exported: AST.Identifier | undefined;

      if (host.match("as")) {
        exported = host.parseIdentifier();
      }

      specifiers.push({
        local,
        exported,
        span: host.createSpanFrom(local)
      });
    } while (host.match(","));
  }

  host.consume("}", "Expected '}'");
  return specifiers;
}

// ============ Helpers ============

/** Look ahead past a destructuring pattern to check if a specific token follows. */
export function peekAhead(host: DeclHost, value: string): boolean {
  const checkpoint = host.current;
  let depth = 0;

  if (host.peek().value === "{" || host.peek().value === "[") {
    const openBracket = host.peek().value;
    const closeBracket = openBracket === "{" ? "}" : "]";
    host.advance();
    depth = 1;

    while (depth > 0 && !host.isAtEnd()) {
      if (host.peek().value === openBracket) depth++;
      else if (host.peek().value === closeBracket) depth--;
      host.advance();
    }

    const found = host.peek().value === value;
    host.current = checkpoint;
    return found;
  }

  host.current = checkpoint;
  return false;
}

function extractPatternNames(pattern: AST.ArrayPattern | AST.ObjectPattern): AST.Identifier[] {
  const names: AST.Identifier[] = [];
  if (pattern.kind === "ArrayPattern") {
    for (const el of pattern.elements) {
      if (!el) continue;
      if (el.kind === "Identifier") names.push(el);
      else if (el.kind === "ArrayPatternElement") {
        if (el.value.kind === "Identifier") names.push(el.value);
        else names.push(...extractPatternNames(el.value));
      }
      else names.push(...extractPatternNames(el));
    }
  } else {
    for (const prop of pattern.properties) {
      if (prop.value.kind === "Identifier") names.push(prop.value);
      else if (prop.value.kind === "ArrayPattern" || prop.value.kind === "ObjectPattern") {
        names.push(...extractPatternNames(prop.value));
      }
    }
  }
  return names;
}

function parseIdentifierList(host: DeclHost): AST.Identifier[] {
  const ids: AST.Identifier[] = [host.parseIdentifier()];

  while (host.match(",")) {
    ids.push(host.parseIdentifier());
  }

  return ids;
}
