/**
 * Functions Parselet — Extracted from Parser (Chunk 4).
 *
 * Function declarations, parameter lists, lambdas, arrow functions.
 * Depends on a FunctionHost interface — no direct Parser import.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError } from '../parser-cursor';
import * as Types from './types';

export interface FunctionHost {
  tokens: Token[];
  current: number;
  errors: ParseError[];

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
  must(expected: string, options?: { recoverWithSynthetic?: boolean }): boolean;
  attempt<T>(fn: () => T): T | null;

  parseIdentifier(): AST.Identifier;
  parseType(): AST.TypeNode;
  parseExpression(minPrecedence?: number): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseBlock(): AST.Block;
  parseIndentBlock(): AST.Block;
  parseExpressionBody(): AST.Block;
  parseStatement(): AST.Stmt;
  parseTopLevel(): AST.Decl | AST.Stmt | null;
  parseDestructuringPattern(): AST.ArrayPattern | AST.ObjectPattern;
  synchronize(): void;
}

// ============ Function Declarations ============

export function parseFuncDecl(
  host: FunctionHost,
  async = false,
  unsafe = false,
  generator = false,
  decorators?: AST.Expr[],
  decoratorStart?: number
): AST.FuncDecl {
  const previousIndex = host.current - 1;
  const keywordStart = host.tokens[previousIndex]?.value === "*" &&
    host.tokens[previousIndex - 1]?.value === "function"
    ? previousIndex - 1
    : previousIndex;
  const start = decoratorStart ?? keywordStart;

  const declKeywordValue = host.tokens[keywordStart]?.value;
  const declKeyword = (declKeywordValue === "def" || declKeywordValue === "fn" ||
                       declKeywordValue === "fun" || declKeywordValue === "func" ||
                       declKeywordValue === "function") ? declKeywordValue as AST.FuncDecl["declKeyword"] : undefined;

  const isRubyDef = declKeywordValue === "def";

  // Ruby/C++ operator overload: def +(other), operator+(...)
  let name: AST.Identifier;
  const isOperatorName = isRubyDef && host.peek().type === TokenType.Operator &&
    ["+", "-", "*", "/", "%", "==", "!=", "<", ">", "<=", ">=", "<<", ">>",
     "[]", "[]=", "<=>", "**", "&", "|", "^", "~", "!"].includes(host.peek().value);
  if (isOperatorName) {
    const opToken = host.advance();
    name = {
      kind: "Identifier",
      name: opToken.value,
      span: host.createSpanFrom(opToken)
    };
  } else {
    name = host.parseIdentifier();
  }

  // Parse generic parameters if present (<T> or Python 3.12+ [T])
  let genericParams: AST.Identifier[] | undefined;
  const genericOpen = host.match("<") ? ">" : host.match("[") ? "]" : null;
  if (genericOpen) {
    genericParams = [];
    do {
      genericParams.push(host.parseIdentifier());
      if (host.match("extends", "super")) {
        host.parseType();
      } else if (host.check(":") && !host.check("::")) {
        host.advance();
        host.parseType();
        while (host.match("+")) {
          host.parseType();
        }
      }
    } while (host.match(","));
    host.consume(genericOpen, `Expected '${genericOpen}' after generic parameters`);
  }

  // For Ruby def, parameters are optional
  let params: AST.Param[] = [];
  if (host.check("(")) {
    params = parseParameterList(host);
  } else if (isRubyDef && !host.check(":") && !host.check("{") &&
             !host.check("=>") && !host.peek().virtualSemi &&
             host.peek().line === name.span.line) {
    do {
      params.push(parseParameter(host));
    } while (host.match(","));
  }

  let returnType: AST.TypeNode | undefined;

  if (host.match("->")) {
    returnType = host.parseType();
  } else if (declKeywordValue === "func" && host.check("(") && !host.check("(=")) {
    const goRetStart = host.current;
    const parsed = host.attempt(() => {
      host.advance(); // consume '('
      const types: AST.TypeNode[] = [];
      if (!host.check(")")) {
        do {
          types.push(Types.parseGoTypeAnnotation(host));
        } while (host.match(","));
      }
      host.consume(")", "Expected ')' after return types");
      return types;
    });
    if (parsed) {
      if (parsed.length === 1) {
        returnType = parsed[0];
      } else if (parsed.length > 1) {
        returnType = { kind: "UnionType", types: parsed, span: host.createSpan(goRetStart, host.current - 1) } as any;
      }
    }
  } else if (declKeywordValue === "func" && !host.check("{") && !host.peek().virtualSemi &&
             (host.peek().type === TokenType.Identifier || host.check("[") || host.check("*") || host.check("&") || host.check("map"))) {
    returnType = Types.parseGoTypeAnnotation(host);
  } else if (host.check(":")) {
    const checkpoint = host.current;
    host.advance(); // consume ':'

    const nextToken = host.peek();
    const prevIndent = host.tokens[checkpoint]?.indentCol ?? 0;
    const nextIndent = nextToken.indentCol;

    if (nextToken.virtualSemi ||
        (nextIndent !== undefined && nextIndent > prevIndent)) {
      // Python-style indented block
    } else if (host.check("return") || host.check("pass") ||
               host.check("raise") || host.check("yield") ||
               host.check("this") || host.check("super")) {
      // Single-line Python function body or statement
    } else {
      returnType = host.parseType();
    }
  }

  let body: AST.Block;
  if (host.match("=>")) {
    body = host.parseExpressionBody();
  } else if (host.match(":") || host.previous()?.value === ":") {
    const currentIndent = host.current > 0 ? (host.tokens[host.current - 1]?.indentCol ?? 0) : 0;
    const peekIndent = host.peek().indentCol;
    if (host.peek().virtualSemi ||
        (peekIndent !== undefined && peekIndent > currentIndent)) {
      body = host.parseIndentBlock();
    } else {
      const stmt = host.parseStatement();
      body = {
        kind: "Block",
        statements: stmt ? [stmt] : [],
        span: host.createSpanFrom(stmt || host.previous())
      };
    }
  } else if (host.check("{")) {
    body = host.parseBlock();
  } else if (isRubyDef) {
    body = parseRubyFuncBody(host, keywordStart);
  } else {
    if (host.peek().virtualSemi || host.isAtEnd()) {
      body = {
        kind: "Block",
        statements: [],
        span: host.createSpanFrom(host.previous() || { span: { start: 0, end: 0, line: 0, column: 0 } })
      };
    } else {
      const stmt = host.parseStatement();
      body = {
        kind: "Block",
        statements: stmt ? [stmt] : [],
        span: host.createSpanFrom(stmt || host.previous())
      };
    }
  }

  const funcDecl: AST.FuncDecl = {
    kind: "FuncDecl",
    name,
    genericParams,
    params,
    returnType,
    async,
    unsafe,
    generator,
    declKeyword,
    body: body as AST.Block,
    span: host.createSpan(start, host.current - 1)
  };

  if (decorators && decorators.length > 0) {
    funcDecl.decorators = decorators.map(expr => ({
      kind: "Decorator" as const,
      name: expr.kind === "Identifier" ? expr :
            expr.kind === "Call" && expr.callee.kind === "Identifier" ? expr.callee :
            { kind: "Identifier" as const, name: "unknown", span: expr.span },
      expression: expr,
      args: expr.kind === "Call" ? expr.args : undefined,
      span: expr.span
    }));
  }

  return funcDecl;
}

function parseRubyFuncBody(host: FunctionHost, start: number): AST.Block {
  const statements: (AST.Decl | AST.Stmt)[] = [];
  let endCount = 1;

  while (!host.isAtEnd() && endCount > 0) {
    if (host.peek().virtualSemi) {
      host.advance();
      continue;
    }

    const token = host.peek();
    if (token.value === "def" || token.value === "class" || token.value === "module") {
      endCount++;
    } else if (token.value === "do") {
      if (host.current > 0) {
        const prevToken = host.tokens[host.current - 1];
        if (prevToken.type === TokenType.Identifier ||
            prevToken.value === ")" || prevToken.value === "|") {
          endCount++;
        }
      }
    } else if (token.value === "if" || token.value === "unless" ||
               token.value === "while" || token.value === "until") {
      const nextIdx = host.current + 1;
      let isRubyStyle = false;
      for (let i = nextIdx; i < host.tokens.length && i < nextIdx + 10; i++) {
        if (host.tokens[i].value === "then" ||
            (host.tokens[i].value === ":" && host.tokens[i].type === TokenType.Operator)) {
          isRubyStyle = true;
          break;
        }
        if (host.tokens[i].virtualSemi || host.tokens[i].value === "{") break;
      }
      if (isRubyStyle) endCount++;
    } else if (token.value === "case") {
      const nextIdx = host.current + 1;
      let isRubyStyle = false;
      for (let i = nextIdx; i < host.tokens.length && i < nextIdx + 20; i++) {
        if (host.tokens[i].value === "when") {
          isRubyStyle = true;
          break;
        }
        if (host.tokens[i].value === "in" || host.tokens[i].value === ")") {
          break;
        }
      }
      if (isRubyStyle) endCount++;
    } else if (token.value === "try") {
      const nextIdx = host.current + 1;
      let isRubyStyle = false;
      for (let i = nextIdx; i < host.tokens.length && i < nextIdx + 50; i++) {
        if (host.tokens[i].value === "rescue") {
          isRubyStyle = true;
          break;
        }
        if (host.tokens[i].value === "catch" || host.tokens[i].value === "except") {
          break;
        }
      }
      if (isRubyStyle) endCount++;
    } else if (token.value === "end") {
      endCount--;
      if (endCount === 0) {
        break;
      }
    }

    try {
      const stmt = host.parseTopLevel();
      if (stmt) statements.push(stmt);
    } catch (error) {
      if (error instanceof ParseError) {
        host.errors.push(error);
        host.synchronize();
      } else {
        throw error;
      }
    }
  }

  host.consume("end", "Expected 'end' to close function");

  return {
    kind: "Block",
    statements,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseFuncDeclWithReturnTypeBefore(
  host: FunctionHost,
  decorators?: AST.Expr[],
  decoratorStart?: number
): AST.FuncDecl {
  const typeStart = host.current;
  const start = decoratorStart ?? typeStart;
  const returnType = host.parseType();
  const name = host.parseIdentifier();
  const params = parseParameterList(host);

  const body = host.match("=>") ?
    host.parseExpressionBody() :
    host.parseBlock();

  const funcDecl: AST.FuncDecl = {
    kind: "FuncDecl",
    name,
    genericParams: undefined,
    params,
    returnType,
    async: false,
    unsafe: false,
    body: body as AST.Block,
    span: host.createSpan(start, host.current - 1)
  };

  if (decorators && decorators.length > 0) {
    funcDecl.decorators = decorators.map(expr => ({
      kind: "Decorator" as const,
      name: expr.kind === "Identifier" ? expr :
            expr.kind === "Call" && expr.callee.kind === "Identifier" ? expr.callee :
            { kind: "Identifier" as const, name: "unknown", span: expr.span },
      expression: expr,
      args: expr.kind === "Call" ? expr.args : undefined,
      span: expr.span
    }));
  }

  return funcDecl;
}

// ============ Parameters ============

export function parseParameterList(host: FunctionHost): AST.Param[] {
  host.consume("(", "Expected '(' before parameters");
  const params: AST.Param[] = [];

  while (host.peek().virtualSemi) {
    host.advance();
  }

  if (!host.check(")")) {
    do {
      // Skip virtual semicolons before each param
      while (host.peek().virtualSemi) host.advance();
      // Trailing comma support: stop if we see )
      if (host.check(")")) break;
      params.push(parseParameter(host));
      while (host.peek().virtualSemi) {
        host.advance();
      }
    } while (host.match(","));
  }

  while (host.peek().virtualSemi) {
    host.advance();
  }

  host.consume(")", "Expected ')' after parameters");
  return params;
}

export function parseParameter(host: FunctionHost): AST.Param {
  const start = host.current;

  // Parse parameter decorators
  const decorators: AST.Decorator[] = [];
  while (host.check("@")) {
    host.advance(); // consume @

    if (host.peek().type === TokenType.Identifier) {
      const name = host.parseIdentifier();
      let args: AST.Expr[] | undefined;

      if (host.check("(")) {
        host.advance(); // consume (
        args = [];

        if (!host.check(")")) {
          do {
            args.push(host.parseExpression());
          } while (host.match(","));
        }

        host.consume(")", "Expected ')' after decorator arguments");
      }

      decorators.push({
        kind: "Decorator",
        name,
        args,
        span: host.createSpan(start, host.current - 1)
      });
    }
  }

  // TypeScript visibility modifiers in constructor params
  let visibility: "public" | "private" | "protected" | undefined;
  if (host.match("public", "private", "protected")) {
    visibility = host.previous()!.value as any;
  }

  let readonly = false;
  if (host.match("readonly")) {
    readonly = true;
  }

  // Ruby-style block parameter (&param)
  let isBlockParam = false;
  if (host.match("&")) {
    isBlockParam = true;
  }

  // Spread parameter (...param), Python *args, **kwargs
  let isSpread = false;
  if (host.match("...")) {
    isSpread = true;
  } else if (host.check("**")) {
    host.advance();
    isSpread = true;
  } else if (host.check("*") &&
             (host.peekNext()?.type === TokenType.Identifier || host.peekNext()?.type === TokenType.Keyword)) {
    host.advance();
    isSpread = true;
  }

  // Parse parameter name
  let name: AST.Identifier | AST.ArrayPattern | AST.ObjectPattern;
  const token = host.peek();

  if (token.value === "{" || token.value === "[") {
    name = host.parseDestructuringPattern();
  } else if (token.type === TokenType.Identifier ||
      token.type === TokenType.Keyword ||
      token.type === TokenType.SigilIdentifier) {
    host.advance();
    const isSigil = token.type === TokenType.SigilIdentifier;
    let nameStr = token.value;
    // Rust macro repetition modifier: $(...)*  $(...)+
    if (isSigil && (host.check("*") || host.check("+"))) {
      nameStr += host.advance().value;
    }
    name = {
      kind: "Identifier",
      name: nameStr,
      originalSpelling: nameStr,
      span: host.createSpanFrom(token)
    };
  } else {
    name = host.parseIdentifier();
  }

  let optional = false;
  if (host.match("?")) {
    optional = true;
  }

  let type: AST.TypeNode | undefined;
  if (host.match(":")) {
    type = host.parseType();
  } else if (name.kind === "Identifier" && !name.originalSpelling?.startsWith("$") &&
             !host.check(",") && !host.check(")") && !host.check("=") && !host.check("?") && !host.check("|") &&
             (host.peek().type === TokenType.Identifier ||
              host.check("interface") || host.check("struct") || host.check("chan") ||
              host.check("map") || host.check("*") || host.check("&") ||
              (host.check("[") && host.peekAt(1)?.value === "]"))) {
    type = Types.parseGoTypeAnnotation(host);
  }

  // C++ reference type: `const Vec& other` or `Vec& other`
  // After parsing name=const type=Vec, if next is `&` + identifier, re-interpret
  if (type && host.check("&") && host.peekAt(1)?.type === TokenType.Identifier) {
    host.advance(); // consume &
    const realName = host.advance(); // consume actual param name
    name = {
      kind: "Identifier",
      name: realName.value,
      originalSpelling: realName.value,
      span: host.createSpanFrom(realName)
    };
    // type stays as-is (the C++ type before &)
  }

  // Java varargs: Type... paramName — ... comes after the type
  // At this point, 'name' might be the type and '...' + identifier is the real param
  if (host.check("...") && !isSpread) {
    const afterDots = host.peekAt(1);
    if (afterDots && (afterDots.type === TokenType.Identifier || afterDots.type === TokenType.Keyword)) {
      // Reinterpret: current 'name' was actually the type
      type = { kind: "SimpleType", id: name as AST.Identifier, span: (name as AST.Identifier).span } as AST.TypeNode;
      host.advance(); // consume ...
      isSpread = true;
      const realName = host.advance();
      name = {
        kind: "Identifier",
        name: realName.value,
        originalSpelling: realName.value,
        span: host.createSpanFrom(realName)
      };
    }
  }

  let defaultValue: AST.Expr | undefined;
  if (host.match("=")) {
    defaultValue = host.parseAssignmentExpression();
  }

  const param: AST.Param = {
    name,
    type,
    defaultValue,
    visibility,
    readonly,
    spread: isSpread,
    blockParam: isBlockParam,
    span: host.createSpan(start, host.current - 1)
  };

  if (decorators.length > 0) {
    param.decorators = decorators;
  }

  return param;
}

// ============ Lambda / Arrow Functions ============

export function parseLambda(host: FunctionHost): AST.Lambda {
  const start = host.current - 1; // Account for already consumed (

  let params: AST.Param[] = [];

  if (!host.check(")")) {
    do {
      params.push(parseParameter(host));
    } while (host.match(","));
  }
  host.consume(")", "Expected ')' after lambda parameters");

  let returnType: AST.TypeNode | undefined;
  if (host.match(":")) {
    returnType = host.parseType();
  }

  host.consume("=>", "Expected '=>' in lambda");

  while (host.peek().virtualSemi) {
    host.advance();
  }

  const body = host.check("{") ? host.parseBlock() : host.parseAssignmentExpression();

  return {
    kind: "Lambda",
    params,
    returnType,
    body,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseAsyncLambda(host: FunctionHost, start: number): AST.Lambda {
  if (host.check("{")) {
    const block = host.parseBlock();
    return {
      kind: "Lambda",
      params: [],
      returnType: undefined,
      body: block,
      async: true,
      span: host.createSpan(start, host.current - 1)
    } as AST.Lambda;
  }

  let params: AST.Param[] = [];

  if (host.match("(")) {
    if (!host.check(")")) {
      do {
        params.push(parseParameter(host));
      } while (host.match(","));
    }
    host.consume(")", "Expected ')' after lambda parameters");
  } else if (host.peek().type === TokenType.Identifier) {
    const name = host.parseIdentifier();
    params.push({
      name,
      type: undefined,
      defaultValue: undefined,
      span: name.span
    });
  }

  let returnType: AST.TypeNode | undefined;
  if (host.match(":")) {
    returnType = host.parseType();
  }

  host.consume("=>", "Expected '=>' in async lambda");

  const body = host.check("{") ? host.parseBlock() : host.parseAssignmentExpression();

  return {
    kind: "Lambda",
    params,
    returnType,
    body,
    async: true,
    span: host.createSpan(start, host.current - 1)
  } as AST.Lambda;
}

export function checkLambda(host: FunctionHost): boolean {
  if (host.check("(")) {
    const checkpoint = host.current;
    host.advance(); // (

    let depth = 1;
    while (depth > 0 && !host.isAtEnd()) {
      if (host.check("(")) depth++;
      if (host.check(")")) depth--;
      host.advance();
    }

    let hasArrow = host.check("=>");
    if (!hasArrow && host.check(":")) {
      const savePos = host.current;
      host.advance(); // skip :
      let typeDepth = 0;
      while (!host.isAtEnd()) {
        if (host.check("<") || host.check("[")) {
          typeDepth++;
        } else if (host.check(">") || host.check("]")) {
          typeDepth--;
        } else if (typeDepth === 0 && host.check("=>")) {
          hasArrow = true;
          break;
        } else if (typeDepth === 0 && (host.check("{") || host.check(";") || host.check(",") || host.check(")") || host.isAtEnd())) {
          break;
        }
        host.advance();
      }
      host.current = savePos;
    }
    host.current = checkpoint;
    return hasArrow;
  }

  if (host.peek().type === TokenType.Identifier) {
    const next = host.peekNext();
    return next?.value === "=>" ||
           (next?.value === ":" && host.peekAt(2)?.value === "=>");
  }

  return false;
}

export function checkParenthesizedLambda(host: FunctionHost): boolean {
  // We already consumed the opening paren

  if (host.peek().type === TokenType.Identifier) {
    const next = host.peekNext();
    if (next && next.value === ":") {
      let depth = 1;
      let pos = host.current + 2;

      while (depth > 0 && pos < host.tokens.length) {
        const tok = host.tokens[pos];
        if (tok.value === "(") depth++;
        else if (tok.value === ")") {
          depth--;
          if (depth === 0) {
            const nextTok = host.tokens[pos + 1];
            if (nextTok && nextTok.value === "=>") return true;
            if (nextTok && nextTok.value === ":") {
              let rpos = pos + 2;
              let rtypeDepth = 0;
              while (rpos < host.tokens.length) {
                const rt = host.tokens[rpos];
                if (rt.value === "<" || rt.value === "[") rtypeDepth++;
                else if (rt.value === ">" || rt.value === "]") rtypeDepth--;
                else if (rtypeDepth === 0 && rt.value === "=>") return true;
                else if (rtypeDepth === 0 && (rt.value === "{" || rt.value === ";" || rt.value === ",")) break;
                rpos++;
              }
            }
            return false;
          }
        }
        pos++;
      }
    }
  }

  // Fallback: scan to closing paren and check for =>
  let depth = 1;

  while (depth > 0 && !host.isAtEnd()) {
    if (host.check("(")) {
      depth++;
      host.advance();
    } else if (host.check(")")) {
      depth--;
      host.advance();
    } else {
      host.advance();
    }
  }

  if (host.check("=>")) {
    return true;
  }

  if (host.check(":")) {
    const savePos = host.current;
    host.advance(); // consume :

    let typeDepth = 0;
    while (!host.isAtEnd()) {
      if (host.check("<")) {
        typeDepth++;
      } else if (host.check(">")) {
        typeDepth--;
      } else if (typeDepth === 0 && host.check("=>")) {
        host.current = savePos;
        return true;
      } else if (typeDepth === 0 && (host.check("{") || host.check(";") || host.check(",") || host.isAtEnd())) {
        break;
      }
      host.advance();
    }
    host.current = savePos;
  }

  return false;
}
