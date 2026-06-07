/**
 * Literals Parselet — Extracted from Parser (Chunk 6).
 *
 * Numeric, string, template, regex literals. Array/object literals.
 * List/set/dict/generator comprehensions.
 *
 * Depends on a narrow LiteralHost interface — no direct Parser import.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError } from '../parser-cursor';
import * as Functions from './functions';

export interface LiteralHost {
  tokens: Token[];
  current: number;
  errors: ParseError[];

  peek(): Token;
  peekNext(): Token | undefined;
  advance(): Token;
  check(value: string): boolean;
  match(...values: string[]): boolean;
  consume(expected: TokenType | string, message: string): Token;
  isAtEnd(): boolean;
  previous(): Token | undefined;
  error(token: Token, message: string): Error;
  createSpan(start: number, end: number): AST.Span;
  createSpanFrom(node: { span: AST.Span } | Token): AST.Span;
  attempt<T>(fn: () => T): T | null;

  parseIdentifier(): AST.Identifier;
  parseExpression(minPrecedence?: number): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseSwitch(): AST.Switch | AST.Match;
  parseBlockOrStatement(): AST.Block;
}

// ============ Primitive Literals ============

export function parseNumericLiteral(host: LiteralHost): AST.NumericLiteral {
  const token = host.advance();
  let base: "decimal" | "hex" | "octal" | "binary" = "decimal";

  if (token.value.startsWith("0x") || token.value.startsWith("0X")) {
    base = "hex";
  } else if (token.value.startsWith("0o") || token.value.startsWith("0O")) {
    base = "octal";
  } else if (token.value.startsWith("0b") || token.value.startsWith("0B")) {
    base = "binary";
  }

  let suffix: string | undefined;
  const suffixMatch = token.value.match(/[nulfiULFI][\w]*$/);
  if (suffixMatch) {
    suffix = suffixMatch[0];
  }

  return {
    kind: "NumericLiteral",
    raw: token.value,
    base,
    suffix,
    span: host.createSpanFrom(token)
  };
}

export function parseStringLiteral(host: LiteralHost): AST.StringLiteral {
  const token = host.advance();

  let flags: AST.StringLiteral["flags"] = {};
  let delimiter = token.value[0];

  let prefixEnd = 0;
  for (let i = 0; i < token.value.length; i++) {
    const char = token.value[i];
    if (char === 'r') flags.raw = true;
    else if (char === 'b') flags.bytes = true;
    else if (char === 'f') flags.format = true;
    else if (char === 'c') flags.const = true;
    else if (char === '"' || char === "'" || char === '`') {
      prefixEnd = i;
      delimiter = char;
      break;
    }
  }

  const content = token.value.slice(prefixEnd + 1, -1);
  const parts: AST.StringPart[] = [];

  if (flags.format || delimiter === '`') {
    let current = "";
    let i = 0;

    while (i < content.length) {
      if ((flags.format && content[i] === '{' && content[i + 1] !== '{') ||
          (delimiter === '`' && content[i] === '$' && content[i + 1] === '{')) {
        if (current) {
          parts.push({ kind: "Text", value: current });
          current = "";
        }

        const start = flags.format ? i + 1 : i + 2;
        let depth = 1;
        let end = start;

        while (end < content.length && depth > 0) {
          if (content[end] === '{') depth++;
          else if (content[end] === '}') depth--;
          end++;
        }

        if (depth === 0) {
          const exprStr = content.slice(start, end - 1);
          parts.push({ kind: "Interpolation", value: exprStr });
          i = end;
        } else {
          current += content[i];
          i++;
        }
      } else if ((flags.format && content[i] === '{' && content[i + 1] === '{') ||
                 (flags.format && content[i] === '}' && content[i + 1] === '}')) {
        current += content[i];
        i += 2;
      } else {
        current += content[i];
        i++;
      }
    }

    if (current) {
      parts.push({ kind: "Text", value: current });
    }
  } else {
    parts.push({ kind: "Text", value: content });
  }

  return {
    kind: "StringLiteral",
    parts,
    flags,
    delimiter,
    span: host.createSpanFrom(token)
  };
}

export function parseTemplateLiteral(host: LiteralHost): AST.StringLiteral {
  const token = host.advance();

  const content = token.value.slice(1, -1);
  const parts: AST.StringPart[] = [];

  let current = "";
  let i = 0;

  while (i < content.length) {
    if (content[i] === '$' && content[i + 1] === '{') {
      if (current) {
        parts.push({ kind: "Text", value: current });
        current = "";
      }

      let depth = 1;
      let end = i + 2;

      while (end < content.length && depth > 0) {
        if (content[end] === '{') depth++;
        else if (content[end] === '}') depth--;
        end++;
      }

      if (depth === 0) {
        const exprStr = content.slice(i + 2, end - 1);
        parts.push({ kind: "Interpolation", value: exprStr });
        i = end;
      } else {
        current += content[i];
        i++;
      }
    } else if (content[i] === '\\' && i + 1 < content.length) {
      i++;
      switch (content[i]) {
        case 'n': current += '\n'; break;
        case 't': current += '\t'; break;
        case 'r': current += '\r'; break;
        case '\\': current += '\\'; break;
        case '`': current += '`'; break;
        default: current += content[i];
      }
      i++;
    } else {
      current += content[i];
      i++;
    }
  }

  if (current || parts.length === 0) {
    parts.push({ kind: "Text", value: current });
  }

  return {
    kind: "StringLiteral",
    parts,
    flags: { format: true },
    delimiter: "`",
    span: host.createSpanFrom(token)
  };
}

export function parseRegexLiteral(host: LiteralHost): AST.RegexLiteral {
  const token = host.advance();

  const lastSlash = token.value.lastIndexOf('/');
  const pattern = token.value.slice(1, lastSlash);
  const flags = token.value.slice(lastSlash + 1);

  return {
    kind: "RegexLiteral",
    pattern,
    flags,
    span: host.createSpanFrom(token)
  };
}

// ============ Array Literals + List Comprehensions ============

export function parseArrayLiteral(host: LiteralHost): AST.ArrayLiteral | AST.ListComprehension {
  const start = host.current - 1;
  const elements: AST.Expr[] = [];

  if (!host.check("]")) {
    let firstExpr: AST.Expr;

    if (host.match("...")) {
      const spreadStart = host.current - 1;
      const optional = host.match("?");
      const argument = host.parseAssignmentExpression();
      firstExpr = {
        kind: "Spread",
        argument,
        optional,
        span: host.createSpan(spreadStart, host.current - 1)
      };
      elements.push(firstExpr);
    } else {
      if (host.check("match")) {
        const matchResult = host.attempt(() => host.parseSwitch() as any as AST.Expr);
        if (matchResult) {
          firstExpr = matchResult;
        } else {
          firstExpr = host.parseAssignmentExpression();
        }
      } else {
        firstExpr = host.parseAssignmentExpression();
      }

      while (host.peek().virtualSemi) {
        host.advance();
      }

      if (host.check("for")) {
        return parseListComprehension(host, firstExpr, start);
      } else {
        elements.push(firstExpr);
      }
    }

    if (!host.check("for")) {
      while (host.match(",")) {
        while (host.peek().virtualSemi) {
          host.advance();
        }

        if (host.check("]")) break;

        if (host.match("...")) {
          const spreadStart = host.current - 1;
          const optional = host.match("?");
          const argument = host.parseAssignmentExpression();
          elements.push({
            kind: "Spread",
            argument,
            optional,
            span: host.createSpan(spreadStart, host.current - 1)
          });
        } else {
          elements.push(host.parseAssignmentExpression());
        }

        while (host.peek().virtualSemi) {
          host.advance();
        }
      }
    }
  }

  while (host.peek().virtualSemi) {
    host.advance();
  }

  host.consume("]", "Expected ']' after array elements");

  return {
    kind: "ArrayLiteral",
    elements,
    span: host.createSpan(start, host.current - 1)
  };
}

function parseListComprehension(host: LiteralHost, expr: AST.Expr, start: number): AST.ListComprehension {
  host.consume("for", "Expected 'for' in list comprehension");

  const targets: AST.Identifier[] = [];
  targets.push(host.parseIdentifier());

  while (host.match(",")) {
    targets.push(host.parseIdentifier());
  }

  host.consume("in", "Expected 'in' in list comprehension");

  const iterable = host.parseExpression();

  while (host.peek().virtualSemi) host.advance();

  let filter: AST.Expr | undefined;
  if (host.match("if")) {
    filter = host.parseExpression();
  }

  while (host.peek().virtualSemi) host.advance();

  host.consume("]", "Expected ']' after list comprehension");

  return {
    kind: "ListComprehension",
    expression: expr,
    targets,
    iterable,
    filter,
    span: host.createSpan(start, host.current - 1)
  };
}

// ============ Object Literals + Set/Dict Comprehensions ============

export function parseObjectLiteral(host: LiteralHost): AST.ObjectLiteral | AST.SetLiteral {
  const start = host.current - 1;

  if (!host.check("}")) {
    const checkpoint = host.current;

    try {
      if (host.peek().type === TokenType.Keyword && host.peekNext()?.value === ":") {
        // Likely object literal with keyword property — skip comprehension check
      } else {
        const errorCheckpoint = host.errors.length;
        const firstExpr = host.parseAssignmentExpression();

        if (host.check("for")) {
          return parseSetComprehension(host, firstExpr, start);
        }

        if (host.check(",") || host.check("}")) {
          const elements: AST.Expr[] = [firstExpr];
          while (host.match(",")) {
            while (host.peek().virtualSemi) host.advance();
            if (host.check("}")) break;
            elements.push(host.parseAssignmentExpression());
          }
          host.consume("}", "Expected '}' after set literal");
          host.errors.length = errorCheckpoint;
          return {
            kind: "SetLiteral",
            elements,
            span: host.createSpan(start, host.current - 1)
          };
        }
      }
    } catch {
      // Failed — continue as object literal
    }

    host.current = checkpoint;

    const properties: AST.ObjectProperty[] = [];

    do {
      while (host.peek().virtualSemi) {
        host.advance();
      }

      if (host.check("}")) {
        break;
      }

      const propStart = host.current;

      if (host.match("...")) {
        const optional = host.match("?");
        const argument = host.parseAssignmentExpression();

        properties.push({
          key: {
            kind: "Identifier",
            name: "...",
            span: host.createSpan(propStart, propStart)
          },
          value: {
            kind: "Spread",
            argument,
            optional,
            span: host.createSpan(propStart, host.current - 1)
          } as AST.Expr,
          shorthand: false,
          computed: false,
          span: host.createSpan(propStart, host.current - 1)
        });
      } else {
        let key: AST.Identifier | AST.StringLiteral | AST.NumericLiteral | AST.Expr;
        let computed = false;

        if (host.match("[")) {
          computed = true;
          key = host.parseExpression();
          host.consume("]", "Expected ']' after computed property");
        } else if (host.peek().type === TokenType.StringLiteral) {
          key = parseStringLiteral(host);
        } else if (host.peek().type === TokenType.NumericLiteral) {
          key = parseNumericLiteral(host);
        } else if (host.peek().type === TokenType.Keyword) {
          const keyToken = host.advance();
          key = {
            kind: "Identifier",
            name: keyToken.value,
            span: host.createSpanFrom(keyToken)
          };
        } else {
          key = host.parseIdentifier();
        }

        let value: AST.Expr;
        let shorthand = false;

        if (host.match(":", "=>")) {
          value = host.parseAssignmentExpression();

          if (host.check("for")) {
            const dictExpr = {
              kind: "ObjectLiteral" as const,
              properties: [{
                key,
                value,
                shorthand: false,
                computed,
                span: host.createSpan(propStart, host.current - 1)
              }],
              span: host.createSpan(propStart, host.current - 1)
            };
            return parseDictComprehension(host, dictExpr, start);
          }
        } else if (key.kind === "Identifier" && host.check("(")) {
          host.advance(); // consume '('
          const params: AST.Param[] = [];
          if (!host.check(")")) {
            do {
              params.push(Functions.parseParameter(host as any));
            } while (host.match(","));
          }
          host.consume(")", "Expected ')' after method parameters");
          const methodBody = host.parseBlockOrStatement();
          value = {
            kind: "Lambda",
            params,
            body: methodBody,
            span: host.createSpan(propStart, host.current - 1)
          } as AST.Lambda;
        } else if (key.kind === "Identifier") {
          shorthand = true;
          value = key;
        } else {
          throw host.error(host.peek(), "Expected ':' after property key");
        }

        properties.push({
          key,
          value,
          shorthand,
          computed,
          span: host.createSpan(propStart, host.current - 1)
        });
      }
    } while (host.match(","));

    while (host.peek().virtualSemi) {
      host.advance();
    }

    host.consume("}", "Expected '}' after object properties");

    return {
      kind: "ObjectLiteral",
      properties,
      span: host.createSpan(start, host.current - 1)
    };
  }

  while (host.peek().virtualSemi) {
    host.advance();
  }

  host.consume("}", "Expected '}' after object properties");

  return {
    kind: "ObjectLiteral",
    properties: [],
    span: host.createSpan(start, host.current - 1)
  };
}

function parseSetComprehension(host: LiteralHost, expr: AST.Expr, start: number): AST.SetLiteral {
  const comprehensions: any[] = [];

  while (host.match("for")) {
    const variables: AST.Identifier[] = [];
    variables.push(host.parseIdentifier());

    while (host.match(",")) {
      variables.push(host.parseIdentifier());
    }

    host.consume("in", "Expected 'in' in set comprehension");
    const iterable = host.parseExpression();

    let condition: AST.Expr | undefined;
    if (host.match("if")) {
      condition = host.parseExpression();
    }

    comprehensions.push({ variables, iterable, condition });

    if (!host.check("for")) {
      break;
    }
  }

  host.consume("}", "Expected '}' after set comprehension");

  return {
    kind: "SetLiteral",
    elements: [expr],
    span: host.createSpan(start, host.current - 1)
  };
}

function parseDictComprehension(host: LiteralHost, firstPair: AST.ObjectLiteral, start: number): AST.ObjectLiteral {
  const comprehensions: any[] = [];

  while (host.match("for")) {
    const variables: AST.Identifier[] = [];
    variables.push(host.parseIdentifier());

    while (host.match(",")) {
      variables.push(host.parseIdentifier());
    }

    host.consume("in", "Expected 'in' in dict comprehension");
    const iterable = host.parseExpression();

    let condition: AST.Expr | undefined;
    if (host.match("if")) {
      condition = host.parseExpression();
    }

    comprehensions.push({ variables, iterable, condition });

    if (!host.check("for")) {
      break;
    }
  }

  host.consume("}", "Expected '}' after dict comprehension");

  return {
    kind: "ObjectLiteral",
    properties: firstPair.properties,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseGeneratorComprehension(host: LiteralHost, expr: AST.Expr, start: number): AST.Call {
  const comprehensions: any[] = [];

  while (host.match("for")) {
    const variables: AST.Identifier[] = [];
    variables.push(host.parseIdentifier());

    while (host.match(",")) {
      variables.push(host.parseIdentifier());
    }

    host.consume("in", "Expected 'in' in generator comprehension");
    const iterable = host.parseExpression();

    let condition: AST.Expr | undefined;
    if (host.match("if")) {
      condition = host.parseExpression();
    }

    comprehensions.push({ variables, iterable, condition });

    if (!host.check("for")) {
      break;
    }
  }

  host.consume(")", "Expected ')' after generator comprehension");

  return {
    kind: "Call",
    callee: {
      kind: "Identifier",
      name: "__generator",
      span: host.createSpan(start, start)
    },
    args: [expr],
    span: host.createSpan(start, host.current - 1)
  };
}
