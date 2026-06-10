/**
 * Expression Prefix Parselet — Extracted from Parser (Chunk 12a).
 *
 * Handles primary expression parsing: literals, identifiers, function expressions,
 * async/yield, JSX entry, type assertions, unary prefix operators, new, match/when,
 * runtime tags, Go composite literals, Ruby symbols/percent literals.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError } from '../parser-cursor';
import * as JSX from './jsx';
import * as Types from './types';
import * as Functions from './functions';
import * as Literals from './literals';
import * as Blocks from './blocks';
import * as ControlFlow from './control-flow';

export interface PrefixHost {
  tokens: Token[];
  current: number;
  errors: ParseError[];
  noRubyBlock: boolean;

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
  consumeSemicolon(): void;
  checkSemicolon(): boolean;
  attempt<T>(fn: () => T): T | null;
  must(expected: string, options?: { recoverWithSynthetic?: boolean }): boolean;
  hasWhitespaceBefore(): boolean;

  parseExpression(minPrecedence?: number): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseIdentifier(): AST.Identifier;
  parseType(): AST.TypeNode;
  parseStatement(): AST.Stmt;
  parsePostfix(expr: AST.Expr): AST.Expr;
  isStatementKeyword(keyword: string): boolean;
  isUnaryOp(token: Token): boolean;
  isAssignmentOp(token: Token): boolean;
  tryParseGenericArgs(): AST.TypeNode[] | null;
  parseArguments(): AST.Expr[];
}

export function parsePrimary(host: PrefixHost, ): AST.Expr {
  // Rust closures: |x| expr, |x, y| { ... }, or empty || closures.
  // An expression cannot otherwise begin with `|`, so this is additive.
  if (host.check("|") || host.check("||")) {
    const closure = tryParseRustClosure(host);
    if (closure) return closure;
  }

  // Handle function expressions (including async)
  // But not when func() looks like a function call (no body follows the parens)
  if ((host.peek().value === "function" || host.peek().value === "func") &&
      !looksLikeFuncCall(host) &&
      // func must be followed by (, *, or identifier (function name) to be a function expression
      (host.peekAt(1)?.value === "(" || host.peekAt(1)?.value === "*" ||
       host.peekAt(1)?.type === TokenType.Identifier)) {
    const start = host.current;
    host.advance(); // consume 'function' or 'func'

    // Check for generator function*
    const isGenerator = host.match("*");

    // Function expressions can be anonymous
    let name: AST.Identifier | undefined = undefined;
    if (host.peek().type === TokenType.Identifier) {
      name = host.parseIdentifier();
    }

    // Parse parameters
    const params = Functions.parseParameterList(host as any);

    // Parse return type if present
    let returnType: AST.TypeNode | undefined = undefined;
    if (host.match(":")) {
      returnType = host.parseType();
    }

    // Parse body
    const body = Blocks.parseBlock(host as any);

    // Return as a Lambda expression (anonymous function)
    // Allow postfix operations (like IIFE calls: func(){}())
    const lambda: AST.Expr = {
      kind: "Lambda",
      params,
      returnType,
      body,
      span: host.createSpan(start, host.current - 1)
    };
    if (isGenerator) (lambda as any).generator = true;
    return host.parsePostfix(lambda);
  }
  
  // Handle Python lambda: lambda x, y: expr
  if (host.peek().value === "lambda" &&
      host.peekAt(1)?.value !== "." && host.peekAt(1)?.value !== "[" &&
      host.peekAt(1)?.value !== ")" && host.peekAt(1)?.value !== "," &&
      host.peekAt(1)?.value !== ";" && host.peekAt(1)?.value !== "}" &&
      host.peekAt(1)?.value !== "&&" && host.peekAt(1)?.value !== "||" &&
      host.peekAt(1)?.value !== "as" && host.peekAt(1)?.value !== "!" &&
      host.peekAt(1)?.value !== "=" && host.peekAt(1)?.value !== "?") {
    const start = host.current;
    host.advance(); // consume 'lambda'

    // Parse parameters (comma-separated identifiers until ':')
    const params: AST.Param[] = [];
    while (!host.check(":") && !host.isAtEnd()) {
      const paramName = host.parseIdentifier();
      let defaultValue: AST.Expr | undefined;
      if (host.match("=")) {
        defaultValue = host.parseAssignmentExpression();
      }
      params.push({ name: paramName, defaultValue, span: paramName.span });
      if (!host.match(",")) break;
    }
    host.consume(":", "Expected ':' after lambda parameters");

    // Parse body expression (not a block — single expression)
    const bodyExpr = host.parseAssignmentExpression();
    const body: AST.Block = {
      kind: "Block",
      statements: [{ kind: "ExprStmt", expr: bodyExpr, span: bodyExpr.span } as any],
      span: bodyExpr.span,
    };

    return {
      kind: "Lambda",
      params,
      body,
      span: host.createSpan(start, host.current - 1),
    } as AST.Lambda;
  }

  // Handle async lambda/function expressions
  if (host.peek().value === "async") {
    // Look ahead to see if this is really an async function/lambda
    const next = host.peekNext();
    const isAsyncFunction = next && (
      next.value === "(" ||  // async () => 
      next.value === "{" ||  // async { ... }
      next.value === "move" || // async move { ... } (Rust-style)
      (next.type === TokenType.Identifier && host.peekAt(2)?.value === "=>") || // async x =>
      next.value === "function" // async function
    );
    
    if (isAsyncFunction) {
      host.advance(); // consume 'async'
      const start = host.current - 1;
      
      // Check for async function expression
      if (host.match("function")) {
        // Parse async function expression
        const isGenerator = host.match("*");
        
        // Function expressions can be anonymous
        let name: AST.Identifier | undefined = undefined;
        if (host.peek().type === TokenType.Identifier) {
          name = host.parseIdentifier();
        }
        
        // Parse parameters
        const params = Functions.parseParameterList(host as any);
        
        // Parse return type if present
        let returnType: AST.TypeNode | undefined = undefined;
        if (host.match(":")) {
          returnType = host.parseType();
        }
        
        // Parse body
        const body = Blocks.parseBlock(host as any);
        
        // Return as an async Lambda expression
        const lambda: any = {
          kind: "Lambda",
          params,
          returnType,
          body,
          span: host.createSpan(start, host.current - 1)
        };
        lambda.async = true;
        if (isGenerator) lambda.generator = true;
        return lambda;
      }
      
      // Check for 'move' keyword (Rust-style async move block)
      const hasMove = host.match("move");
      
      // Check for lambda or async block
      if (host.check("(") || host.peek().type === TokenType.Identifier || host.check("{")) {
        // Parse as async lambda or async block
        const lambda = Functions.parseAsyncLambda(host as any, start);
        // Mark if it has move semantics
        if (hasMove) {
          (lambda as any).move = true;
        }
        // Allow postfix operations on the lambda (like calls)
        return host.parsePostfix(lambda);
      }
      // Otherwise it's an error
      throw host.error(host.peek(), "Expected lambda or block after async");
    }
    // Otherwise, treat 'async' as a regular identifier
  }
  
  // Handle yield expression
  if (host.match("yield")) {
    return ControlFlow.parseYieldExpression(host as any);
  }

  // Handle Go spawn expressions: const h = go worker(1)
  if (host.match("go")) {
    const start = host.current - 1;
    const expr = host.parseExpression();
    return {
      kind: "Go",
      expr,
      span: host.createSpan(start, host.current - 1)
    } as AST.Go;
  }
  
  // Handle channel receive operator
  if (host.check("<-")) {
    const op = host.advance();
    const argument = parsePrimary(host);
    return {
      kind: "Unary",
      op: "<-",
      argument,
      prefix: true,
      span: host.createSpan(host.current - 1, host.current)
    };
  }
  
  // Handle JSX elements and fragments (spec 10.6)
  const token = host.peek();
  if (token.type === TokenType.JSXTagStart || token.value === "<") {
    // Check if we're in a valid JSX expression context
    if (JSX.isInJSXExpressionContext(host as any)) {
      const next = host.peekNext();

      // Check for JSX fragment <>
      if (next && next.value === ">") {
        return JSX.parseJSXFragment(host as any);
      }

      // Check for JSX closing tag </
      if (next && next.value === "/") {
        // This shouldn't happen in primary expression position
        throw host.error(host.peek(), "Unexpected JSX closing tag");
      }

      // Check if this looks like JSX element using new disambiguation
      if (next && (next.type === TokenType.Identifier || next.value === ">")) {
        if (JSX.isJSXElement(host as any)) {
          return JSX.parseJSXElement(host as any);
        }
      }
    }
    
    // Not JSX - try other interpretations
    const next = host.peekNext();
    if (next && next.type === TokenType.Identifier) {
      // Try type assertion or generic parsing
      
      // Try type assertion <Type>expr
      const checkpoint = host.current;
      host.advance(); // consume '<'
      
      // Try to parse a type (which could be a complex generic type)
      try {
        const type = host.parseType();
        
        // Look for closing '>' - for generic types, this is already consumed
        // For simple types, we need to consume it
        // Also handle >> token that might remain after generic parsing
        if (host.peek().value === ">") {
          host.advance();
        } else if (host.peek().value === ">>") {
          // Split >> into two > tokens for proper handling
          host.advance(); // consume >>
          // Inject a synthetic > token back
          const syntheticToken = { ...host.tokens[host.current - 1] };
          syntheticToken.value = ">";
          host.tokens.splice(host.current, 0, syntheticToken);
        }
        
        // Now we should have a complete type, parse the expression
        // But first check we're at a valid position for an expression
        if (host.isAtEnd() || host.peek().type === TokenType.EOF) {
          throw new Error("Expected expression after type assertion");
        }
        const expr = parsePrimary(host);
        
        return {
          kind: "TypeAssertion",
          expr,
          type,
          span: host.createSpan(checkpoint, host.current - 1)
        };
      } catch (e) {
        // Not a valid type assertion, restore position
        host.current = checkpoint;
        // Re-throw if it's not a parsing error we can recover from
        if (e instanceof Error && e.message && !e.message.includes("Unexpected")) {
          throw e;
        }
      }
    }
  }
  
  // Handle prefix operators
  if (host.isUnaryOp(host.peek())) {
    const op = host.advance();
    // Rust mutable borrow: &mut expr
    let opValue = op.value;
    if (opValue === "&" && host.check("mut") && host.peekNext()?.type === TokenType.Identifier) {
      host.advance(); // consume 'mut'
      opValue = "&mut";
    }
    const argument = parsePrimary(host);
    return {
      kind: "Unary",
      op: opValue,
      argument,
      prefix: true,
      span: host.createSpan(host.current - 1, host.current)
    };
  }
  
  // Literals
  if (host.peek().type === TokenType.NumericLiteral) {
    return host.parsePostfix(Literals.parseNumericLiteral(host as any));
  }
  
  if (host.peek().type === TokenType.StringLiteral) {
    return host.parsePostfix(Literals.parseStringLiteral(host as any));
  }
  
  if (host.peek().type === TokenType.TemplateLiteral) {
    // Check if this should be reinterpreted as an identifier
    if (shouldReinterpretAsIdentifier(host)) {
      return parseBacktickIdentifier(host);
    }
    return host.parsePostfix(Literals.parseTemplateLiteral(host as any));
  }

  if (host.peek().type === TokenType.RegexLiteral) {
    return host.parsePostfix(Literals.parseRegexLiteral(host as any));
  }
  
  if (host.match("true", "false")) {
    const token = host.previous();
    return {
      kind: "BooleanLiteral",
      value: token?.value === "true",
      span: host.createSpanFrom(token!)
    };
  }
  
  if (host.match("null", "undefined", "nil")) {
    return {
      kind: "NullLiteral",
      span: host.createSpanFrom(host.previous()!)
    };
  }
  
  // this and super keywords
  if (host.match("this", "super")) {
    const token = host.previous()!;
    const id: AST.Identifier = {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
    return host.parsePostfix(id);
  }
  
  // Runtime tag expressions: @py(expr), @js(expr), @go(expr), @rb(expr), @java(expr), @rs(expr)
  if (host.check("@")) {
    const runtimeNames = ["py", "js", "go", "rb", "java", "rs"];
    const nextToken = host.peekNext();
    if (nextToken && runtimeNames.includes(nextToken.value)) {
      const runtimeTag = host.attempt(() => {
        const tagStart = host.current;
        host.advance(); // consume @
        const runtimeName = host.advance(); // consume runtime name
        if (!host.check("(")) return null;
        host.advance(); // consume (
        const expr = host.parseExpression();
        host.consume(")", "Expected ')' after runtime-tagged expression");
        return host.parsePostfix({
          kind: "RuntimeTag",
          runtime: runtimeName.value as AST.RuntimeTag["runtime"],
          expr,
          span: host.createSpan(tagStart, host.current - 1)
        });
      });
      if (runtimeTag) return runtimeTag;
    }
  }

  // Kotlin-style `when` as match expression
  // But not if when(...) is followed by `do` (Elixir-style macro def)
  if (host.peek().value === "when" &&
      (host.peekAt(1)?.value === "(" || host.peekAt(1)?.value === "{")) {
    let triggerMatch = true;
    if (host.peekAt(1)?.value === "(") {
      let scanPos = host.current + 2, depth = 1;
      while (scanPos < host.tokens.length && depth > 0) {
        if (host.tokens[scanPos].value === "(") depth++;
        if (host.tokens[scanPos].value === ")") depth--;
        scanPos++;
      }
      while (scanPos < host.tokens.length && host.tokens[scanPos].virtualSemi) scanPos++;
      if (host.tokens[scanPos]?.value === "do") triggerMatch = false;
    }
    if (triggerMatch) {
      host.advance(); // consume 'when'
      const switchExpr = Blocks.parseSwitch(host as any);
      return switchExpr as any;
    }
  }

  // Go composite literals: map[K]V{...}, []Type{...}
  if (host.check("map") && host.peekAt(1)?.value === "[") {
    return host.parsePostfix(parseGoCompositeLiteral(host));
  }
  if (host.check("[") && host.peekAt(1)?.value === "]" &&
      !host.peekAt(2)?.newline &&
      (host.peekAt(2)?.type === TokenType.Identifier ||
       (host.peekAt(2)?.type === TokenType.Keyword &&
        ["map", "interface", "struct", "chan"].includes(host.peekAt(2)?.value || "")))) {
    // Only parse as Go slice literal if { follows the type (lookahead to confirm)
    const checkpoint = host.current;
    try {
      host.advance(); // [
      host.advance(); // ]
      // For simple identifiers (byte, int, string, etc.), just skip one token
      // to avoid parseType() over-consuming the following '(' as function type params
      if (host.peek().type === TokenType.Identifier) {
        host.advance();
      } else {
        Types.parseGoTypeAnnotation(host as any); // handle map[K]V, interface{}, etc.
      }
      if (host.check("{") || host.check("(")) {
        host.current = checkpoint;
        return host.parsePostfix(parseGoCompositeLiteral(host));
      }
    } catch {}
    host.current = checkpoint;
  }

  // C++20 requires expression: requires(T a, T b) { constraints }
  if (host.peek().value === "requires" && host.peekAt(1)?.value === "(") {
    const reqStart = host.current;
    host.advance(); // consume 'requires'
    const params = Functions.parseParameterList(host as any);
    const body = Blocks.parseBlock(host as any);
    return {
      kind: "Lambda",
      params,
      body,
      span: host.createSpan(reqStart, host.current - 1)
    } as AST.Lambda;
  }

  // Identifiers and sigil identifiers
  // Also allow keywords as identifiers in expression context when they can't start a statement
  if (host.peek().type === TokenType.Identifier ||
      host.peek().type === TokenType.SigilIdentifier ||
      (host.peek().type === TokenType.Keyword && !host.isStatementKeyword(host.peek().value))) {
    const token = host.peek();
    let id: AST.Identifier;
    
    if (token.type === TokenType.Keyword) {
      // Keywords used as identifiers
      host.advance();
      id = {
        kind: "Identifier",
        name: token.value,
        span: host.createSpanFrom(token)
      };
    } else if (token.type === TokenType.SigilIdentifier) {
      // Sigil identifiers ($var, $(...))
      host.advance();
      let name = token.value.slice(1); // Strip $ prefix
      let originalSpelling = token.value;
      // Rust macro repetition modifier: $(...)*  $(...)+
      if (host.check("*") || host.check("+")) {
        const mod = host.advance().value;
        name += mod;
        originalSpelling += mod;
      }
      id = {
        kind: "Identifier",
        name,
        originalSpelling,
        span: host.createSpanFrom(token)
      };
    } else {
      id = host.parseIdentifier();
    }
    
    // Check for generic type arguments only in specific contexts
    // Per spec: generics only when < follows identifier with NO whitespace
    if (host.peek().value === "<" && !host.hasWhitespaceBefore()) {
      const next = host.peekNext();
      // Only try to parse generics if not followed by a number or obvious non-type token
      if (next && next.type !== TokenType.NumericLiteral && 
          next.type === TokenType.Identifier) {
        const genericArgs = host.tryParseGenericArgs();
        if (genericArgs) {
          // Store generic arguments for potential call expression
          // This will be picked up by parsePostfix if followed by ()
          (id as any)._genericArgs = genericArgs;
        }
      }
    }
    
    return host.parsePostfix(id);
  }
  
  // Parenthesized expression, lambda, or generator comprehension
  if (host.match("(")) {
    // Check if this is a lambda parameter list
    const checkpoint = host.current;
    const isLambda = Functions.checkParenthesizedLambda(host as any);
    host.current = checkpoint;
    
    if (isLambda) {
      return Functions.parseLambda(host as any);
    }
    
    const start = host.current - 1;

    // Skip virtual semicolons and JSX whitespace after opening parenthesis
    while (host.peek().virtualSemi) {
      host.advance();
    }
    JSX.skipJSXWhitespace(host as any);

    // Empty tuple/parens: ()
    if (host.check(")")) {
      host.advance();
      return host.parsePostfix({
        kind: "ArrayLiteral",
        elements: [],
        span: host.createSpan(start, host.current - 1)
      } as any);
    }

    const expr = host.parseExpression();
    
    // Skip virtual semicolons and JSX whitespace before closing parenthesis
    while (host.peek().virtualSemi) {
      host.advance();
    }
    JSX.skipJSXWhitespace(host as any);
    
    // Check for generator comprehension: (expr for var in iterable)
    if (host.check("for")) {
      return Literals.parseGeneratorComprehension(host as any, expr, start);
    }
    
    host.must(")", { recoverWithSynthetic: true });
    return host.parsePostfix(expr);
  }
  
  // Array literal
  if (host.match("[")) {
    return host.parsePostfix(Literals.parseArrayLiteral(host as any));
  }

  // Object literal
  if (host.match("{")) {
    return host.parsePostfix(Literals.parseObjectLiteral(host as any));
  }
  
  // Lambda/arrow function
  if (Functions.checkLambda(host as any)) {
    return Functions.parseLambda(host as any);
  }
  
  // new expression
  if (host.match("new")) {
    return parseNewExpression(host);
  }
  
  // throw expression (JavaScript/TypeScript)
  if (host.match("throw")) {
    const start = host.current - 1;
    const argument = host.parseExpression();
    return {
      kind: "Unary",
      op: "throw",
      argument,
      prefix: true,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  // match expression — but not when followed by assignment (e.g. match = str =~ rx)
  // or { (e.g. if match { ... } where match is a variable name used as condition)
  if (host.check("match") && !host.isAssignmentOp(host.peekAt(1)!) && host.peekAt(1)?.value !== "{" &&
      host.peekAt(1)?.value !== ")" && host.peekAt(1)?.value !== "," && host.peekAt(1)?.value !== ";" &&
      host.peekAt(1)?.value !== "&&" && host.peekAt(1)?.value !== "||" && host.peekAt(1)?.value !== "?" &&
      host.peekAt(1)?.value !== "]" && host.peekAt(1)?.value !== "[" && host.peekAt(1)?.value !== "!" &&
      host.peekAt(1)?.value !== "." &&
      host.peekAt(1)?.type !== TokenType.EOF) {
    host.advance();
    const switchExpr = Blocks.parseSwitch(host as any);
    return switchExpr as any;
  }
  // match as identifier (assignment target or variable reference)
  if (host.check("match")) {
    const token = host.advance();
    const id: AST.Identifier = {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
    return host.parsePostfix(id);
  }

  // Ruby symbol literals: :identifier or :keyword
  if (host.check(":") && !host.check("::")) {
    const next = host.peekAt(1);
    if (next && (next.type === TokenType.Identifier || next.type === TokenType.Keyword)) {
      const start = host.current;
      host.advance(); // consume ':'
      const name = host.advance(); // consume identifier/keyword
      return host.parsePostfix({
        kind: "StringLiteral",
        parts: [{ kind: "Text", value: ":" + name.value }],
        flags: {},
        delimiter: ":",
        span: host.createSpan(start, host.current - 1)
      });
    }
  }

  // Ruby percent literals: %r{pattern}flags, %w{word list}, %i{symbol list}
  if (host.check("%")) {
    const next = host.peekAt(1);
    if (next && next.type === TokenType.Identifier && /^[rwiqQxs]$/.test(next.value)) {
      const nextNext = host.peekAt(2);
      if (nextNext && (nextNext.value === "{" || nextNext.value === "[" || nextNext.value === "(")) {
        const start = host.current;
        host.advance(); // consume '%'
        const kind = host.advance(); // consume letter (r, w, etc.)
        const open = host.advance(); // consume opening delimiter
        const close = open.value === "{" ? "}" : open.value === "[" ? "]" : ")";
        // Consume content until matching close delimiter
        let depth = 1;
        while (!host.isAtEnd() && depth > 0) {
          if (host.peek().value === open.value) depth++;
          else if (host.peek().value === close) depth--;
          if (depth > 0) host.advance();
        }
        if (!host.isAtEnd()) host.advance(); // consume closing delimiter
        // Consume optional trailing flags (e.g., 'i', 'g', 'mix')
        if (!host.isAtEnd() && host.peek().type === TokenType.Identifier &&
            /^[igmxsue]+$/.test(host.peek().value)) {
          host.advance();
        }
        return {
          kind: "RegexLiteral",
          pattern: "%" + kind.value + open.value + "..." + close,
          flags: "",
          span: host.createSpan(start, host.current - 1)
        } as any;
      }
    }
  }

  throw host.error(host.peek(), "Unexpected token in expression");
}


/**
 * Try to parse a Rust closure starting at `|` or `||`.
 * Returns null (without consuming tokens) when the pipes do not look like
 * a closure parameter list.
 */
function tryParseRustClosure(host: PrefixHost): AST.Expr | null {
  const start = host.current;
  const params: AST.Param[] = [];

  if (host.check("||")) {
    host.advance(); // consume || (empty parameter list)
  } else {
    // Scan ahead: the opening `|` must be closed by another `|` with only
    // parameter-list tokens in between (identifiers, commas, simple types).
    let scan = host.current + 1;
    let sawCloser = false;
    let guard = 0;
    while (scan < host.tokens.length && guard++ < 64) {
      const t = host.tokens[scan];
      if (t.value === "|") { sawCloser = true; break; }
      if (t.virtualSemi || t.type === TokenType.EOF) break;
      const isWordLike = t.type === TokenType.Identifier || t.type === TokenType.Keyword;
      const isParamPunct = [",", ":", "&", "<", ">", "[", "]", "(", ")"].includes(t.value);
      if (!isWordLike && !isParamPunct) break;
      scan++;
    }
    if (!sawCloser) return null;

    host.advance(); // consume opening |
    if (!host.check("|")) {
      do {
        params.push(Functions.parseParameter(host as any));
      } while (host.match(","));
    }
    if (!host.match("|")) {
      host.current = start;
      return null;
    }
  }

  // Optional Rust return type: |x| -> T { ... }
  let returnType: AST.TypeNode | undefined;
  if (host.check("->")) {
    host.advance();
    returnType = host.parseType();
  }

  const body = host.check("{")
    ? Blocks.parseBlock(host as any)
    : host.parseAssignmentExpression();

  const lambda: AST.Lambda = {
    kind: "Lambda",
    params,
    returnType,
    body,
    span: host.createSpan(start, host.current - 1)
  };
  (lambda as any).rustClosure = true;
  return lambda;
}

export function parseGoCompositeLiteral(host: PrefixHost, ): AST.Expr {
  const start = host.current;
  // Parse the Go type
  const goType = Types.parseGoTypeAnnotation(host as any);

  // If followed by { ... }, parse the composite literal body
  if (host.check("{")) {
    host.advance(); // consume {
    // Skip virtual semicolons
    while (host.peek().virtualSemi) host.advance();

    // Check if this is a map-like type (has key-value pairs) or slice-like (has values)
    const entries: AST.ObjectProperty[] = [];
    const elements: AST.Expr[] = [];
    let isMap = false;

    while (!host.check("}") && !host.isAtEnd()) {
      while (host.peek().virtualSemi) host.advance();
      if (host.check("}")) break;

      const expr = host.parseAssignmentExpression();

      if (host.match(":")) {
        // key: value pair (map literal)
        isMap = true;
        const value = host.parseAssignmentExpression();
        entries.push({
          key: expr,
          value,
          span: host.createSpanFrom(expr)
        });
      } else {
        // Just a value (slice literal)
        elements.push(expr);
      }

      host.match(","); // optional trailing comma
      while (host.peek().virtualSemi) host.advance();
    }
    host.consume("}", "Expected '}' in Go composite literal");

    if (isMap || entries.length > 0) {
      return {
        kind: "ObjectLiteral",
        properties: entries,
        span: host.createSpan(start, host.current - 1)
      } as AST.ObjectLiteral;
    } else {
      return {
        kind: "ArrayLiteral",
        elements,
        span: host.createSpan(start, host.current - 1)
      } as AST.ArrayLiteral;
    }
  }

  // No { after type — treat as a type conversion: []byte(data) etc.
  // Parse it as a call-like expression
  if (host.check("(")) {
    host.advance(); // consume (
    const args: AST.Expr[] = [];
    if (!host.check(")")) {
      do {
        args.push(host.parseExpression());
      } while (host.match(","));
    }
    host.consume(")", "Expected ')' in Go type conversion");
    const typeName: AST.Identifier = {
      kind: "Identifier",
      name: goType.kind === "SimpleType" ? ((goType as any).id?.name || "unknown") :
            goType.kind === "GenericType" ? ("[]" + ((goType as any).args?.[0]?.id?.name || "")) : "GoType",
      span: host.createSpan(start, host.current - 1)
    };
    return {
      kind: "Call",
      callee: typeName,
      args,
      span: host.createSpan(start, host.current - 1)
    } as AST.Call;
  }

  // Fallback: just return the type name as an identifier
  const typeName: AST.Identifier = {
    kind: "Identifier",
    name: goType.kind === "SimpleType" ? ((goType as any).id?.name || "GoType") : "GoType",
    span: host.createSpan(start, host.current - 1)
  };
  return typeName;
}


export function parseNewExpression(host: PrefixHost, ): AST.Expr {
  const start = host.current - 1;
  const callee = parseNewCallee(host);

  let args: AST.Expr[] = [];
  if (host.match("(")) {
    args = host.parseArguments();
    host.must(")", { recoverWithSynthetic: true });
  }

  const typeArgs = (callee as any)._genericArgs as AST.TypeNode[] | undefined;
  if (typeArgs) delete (callee as any)._genericArgs;

  return host.parsePostfix({
    kind: "NewExpr",
    callee,
    args,
    ...(typeArgs ? { typeArgs } : {}),
    span: host.createSpan(start, host.current - 1)
  });
}

function parseNewCallee(host: PrefixHost): AST.Expr {
  let expr = parseNewCalleeAtom(host);

  while (true) {
    if (host.check("<") && !host.check("<-") && !host.check("<<") && !host.check("<=") && !host.hasWhitespaceBefore()) {
      const checkpoint = host.current;
      const genericArgs = host.tryParseGenericArgs();
      if (genericArgs) {
        (expr as any)._genericArgs = genericArgs;
        continue;
      }
      host.current = checkpoint;
    }

    if (host.match(".", "::")) {
      const property = parseNewCalleeIdentifier(host);
      expr = {
        kind: "Member",
        object: expr,
        property,
        computed: false,
        optional: false,
        span: host.createSpanFrom(expr)
      };
      continue;
    }

    break;
  }

  return expr;
}

function parseNewCalleeAtom(host: PrefixHost): AST.Expr {
  if (host.match("(")) {
    const expr = host.parseExpression();
    host.must(")", { recoverWithSynthetic: true });
    return expr;
  }

  if (host.match("this", "super")) {
    const token = host.previous()!;
    return {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
  }

  return parseNewCalleeIdentifier(host);
}

function parseNewCalleeIdentifier(host: PrefixHost): AST.Identifier {
  const token = host.peek();
  if (token.type === TokenType.Identifier || token.type === TokenType.Keyword) {
    host.advance();
    return {
      kind: "Identifier",
      name: token.value,
      originalSpelling: token.value,
      span: host.createSpanFrom(token)
    };
  }

  throw host.error(token, "Expected constructor name after 'new'");
}

export function shouldReinterpretAsIdentifier(host: PrefixHost, ): boolean {
  // Check if we're in an identifier position
  const prev = host.previous();
  
  if (!prev) return false;
  
  // Declaration contexts
  if (prev.value === "def" || prev.value === "fun" || prev.value === "fn" ||
      prev.value === "function" || prev.value === "class" || prev.value === "struct" ||
      prev.value === "interface" || prev.value === "trait" || prev.value === "type" ||
      prev.value === "enum" || prev.value === "let" || prev.value === "var" ||
      prev.value === "const" || prev.value === "auto" || prev.value === "final" ||
      prev.value === "immutable") {
    return true;
  }
  
  // Import/export contexts
  if (prev.value === "import" || prev.value === "as" || prev.value === "export") {
    return true;
  }
  
  // Member access
  if (prev.value === "." || prev.value === "?.") {
    return true;
  }
  
  // Object property keys
  if (prev.value === "{" || prev.value === ",") {
    // Could be in an object literal context
    return true;
  }
  
  return false;
}


export function parseBacktickIdentifier(host: PrefixHost, ): AST.Identifier {
  const token = host.advance();
  
  // For template literals that contain interpolations (${...}),
  // treat the entire literal as a string literal instead of an identifier
  if (token.value.includes('${')) {
    // This is actually a template literal with interpolations, not a backtick identifier
    // Return it as-is to be handled as a string literal
    host.current--; // Put the token back
    return Literals.parseTemplateLiteral(host as any) as any; // Treat as expression
  }
  
  // Extract content from backticks
  const content = token.value.slice(1, -1);
  
  // Validate it matches identifier pattern
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(content)) {
    // If not a valid identifier, treat it as a string literal
    host.current--; // Put the token back
    return Literals.parseTemplateLiteral(host as any) as any;
  }
  
  return {
    kind: "Identifier",
    name: content,
    originalSpelling: token.value,
    span: host.createSpanFrom(token)
  };
}


export function looksLikeFuncCall(host: PrefixHost, ): boolean {
  const kw = host.peek();
  if (kw.value !== "func" && kw.value !== "function") return false;
  const next = host.peekAt(1);
  if (!next || next.value !== "(") return false;
  // func( — scan past matched parens to see if a body follows
  let depth = 0;
  let pos = host.current + 1; // start at (
  while (pos < host.tokens.length) {
    const t = host.tokens[pos];
    if (t.value === "(") depth++;
    else if (t.value === ")") {
      depth--;
      if (depth === 0) {
        // Check what follows the closing paren
        const after = host.tokens[pos + 1];
        if (!after) return true; // EOF after func() = call
        // If body follows, it's a function expression
        if (after.value === "{" || after.value === ":" || after.value === "->") return false;
        // Otherwise it's a call
        return true;
      }
    }
    pos++;
  }
  return false;
}
