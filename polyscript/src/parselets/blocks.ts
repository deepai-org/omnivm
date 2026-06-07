/**
 * Blocks + Switch/Match Parselet — Extracted from Parser (Chunk 9).
 *
 * Block parsing (braces, indent, keyword), switch/match statements,
 * bash case/esac, select statements, bash test expressions.
 * Depends on a narrow BlockHost interface.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError, KeywordBlockKind } from '../parser-cursor';
import * as Types from './types';
import * as Literals from './literals';

export interface BlockHost {
  tokens: Token[];
  current: number;
  errors: ParseError[];
  braceDepth: number;
  indentStack: number[];
  keywordStack: KeywordBlockKind[];
  insideSwitch: boolean;

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
  synchronize(): void;
  attempt<T>(fn: () => T): T | null;

  parseExpression(): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseIdentifier(): AST.Identifier;
  parseType(): AST.TypeNode;
  parsePrimary(): AST.Expr;
  parseStatement(): AST.Stmt;
  parseDeclaration(): AST.Decl;
  parseTopLevel(): AST.Decl | AST.Stmt | null;
  isDeclStart(): boolean;
}

export function parseBlockOrStatement(host: BlockHost, ): AST.Block {
  // Helper to parse either a block or single statement
  if (host.check("{")) {
    return parseBlock(host);
  } else {
    // Single statement without braces
    const stmt = host.parseStatement();
    return {
      kind: "Block",
      statements: stmt ? [stmt] : [],
      span: host.createSpanFrom(stmt || host.previous())
    };
  }
}

export function parseBlock(host: BlockHost, ): AST.Block {
  const start = host.current;
  const openToken = host.peek();
  
  if (host.match("{")) {
    host.braceDepth++;
    const statements: (AST.Decl | AST.Stmt)[] = [];
    let loopCount = 0;
    while (!host.check("}") && !host.isAtEnd()) {
      loopCount++;
      if (loopCount > 1000) {
        break;
      }

      const beforePos = host.current;
      try {
        // Skip virtual semicolons
        while (host.peek().virtualSemi) {
          host.advance();
        }

        let stmt: AST.Decl | AST.Stmt | null = null;

        if (host.isDeclStart()) {
          stmt = host.parseDeclaration();
        } else if (!host.check("}")) {
          stmt = host.parseStatement();
        }

        if (stmt) {
          statements.push(stmt);
        }
      } catch (error) {
        if (error instanceof ParseError) {
          host.errors.push(error);
          host.synchronize();
        } else {
          throw error;
        }
      }
      // Prevent infinite loop
      if (host.current === beforePos && !host.check("}")) {
        host.advance();
      }
    }
    
    if (!host.match("}")) {
      throw host.error(host.peek(), "Expected '}'");
    }
    host.braceDepth--;
    
    return {
      kind: "Block",
      statements,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  // Check for indent block
  if (checkIndentBlock(host)) {
    return parseIndentBlock(host);
  }
  
  // Check for keyword block
  if (checkKeywordBlock(host)) {
    return parseKeywordBlock(host);
  }
  
  throw host.error(host.peek(), "Expected block");
}

export function checkIndentBlock(host: BlockHost, ): boolean {
  // Check if previous token was a header ending with ':'
  if (host.current > 0) {
    const prev = host.tokens[host.current - 1];
    if (prev.type === TokenType.Operator && prev.value === ":") {
      // Check if next line is more indented
      const nextToken = host.peek();
      if (nextToken.indentCol !== undefined && 
          (host.indentStack.length === 0 || 
           nextToken.indentCol > host.indentStack[host.indentStack.length - 1])) {
        return true;
      }
    }
  }
  return false;
}

export function parseSelectStatement(host: BlockHost, ): AST.Select {
  const start = host.current - 1;
  const cases: AST.SwitchCase[] = [];
  let defaultCase: AST.Block | undefined;
  
  host.consume("{", "Expected '{' after select");
  
  while (!host.check("}") && !host.isAtEnd()) {
    // Skip virtual semicolons
    while (host.peek().virtualSemi) {
      host.advance();
    }
    
    if (host.check("}") || host.isAtEnd()) {
      break;
    }
    
    if (host.match("default")) {
      host.consume(":", "Expected ':' after default");
      defaultCase = parseCaseBody(host);
      continue;
    }
    
    if (host.match("case")) {
      // Parse channel operation (e.g., x := <-ch or ch <- value)
      // Parse as expression to get the channel operation directly
      const pattern = host.parseExpression();
      host.consume(":", "Expected ':' after case");
      const body = parseCaseBody(host);
      
      cases.push({
        patterns: [pattern], // Channel operation as pattern
        body,
        fallthrough: false,
        span: host.createSpan(start, host.current - 1)
      });
      continue;
    }
    
    // If we get here, there's an unexpected token
    throw host.error(host.peek(), "Expected 'case' or 'default' in select statement");
  }
  
  host.consume("}", "Expected '}' after select body");
  
  // Create a pseudo-discriminant for select (since it doesn't have one)
  return {
    kind: "Select",
    cases,
    defaultCase,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseCaseStatement(host: BlockHost, ): AST.Switch {
  const start = host.current - 1;
  // Parse the discriminant - just a primary expression to avoid consuming 'in'
  const discriminant = host.parsePrimary();
  // Accept both 'in' (bash) and 'when' (Ruby) as the keyword after case discriminant
  if (!host.match("in") && !host.match("when")) {
    host.consume("in", "Expected 'in' or 'when' after case expression");
  }
  return parseCaseEsac(host, start, discriminant);
}

export function parseCaseEsac(host: BlockHost, start: number, discriminant: AST.Expr): AST.Switch {
  const cases: AST.SwitchCase[] = [];
  let defaultCase: AST.Block | undefined;
  
  while (!host.check("esac") && !host.isAtEnd()) {
    // Skip any virtual semicolons
    while (host.peek().virtualSemi) {
      host.advance();
    }
    
    if (host.check("esac")) break;
    
    // Parse case pattern
    const patterns: AST.Expr[] = [];
    
    // Check for default case (*)
    if (host.match("*")) {
      host.consume(")", "Expected ')' after case pattern");

      // Parse case body — stop at ;;, esac, or new pattern followed by )
      const statements: (AST.Decl | AST.Stmt)[] = [];
      while (!host.isAtEnd()) {
        while (host.peek().virtualSemi) host.advance();
        if (host.check(";;") || host.check("esac")) break;
        if (isNewCaseArm(host)) break;
        const beforePos = host.current;
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
        if (host.current === beforePos) host.advance();
      }

      const body: AST.Block = {
        kind: "Block",
        statements,
        span: host.createSpan(host.current, host.current)
      };

      // Skip vsemis before checking for ;;
      while (host.peek().virtualSemi) host.advance();
      // Check for fallthrough (no ;;)
      const fallthrough = !host.match(";;");

      // Add default case to cases array with empty patterns to indicate default
      cases.push({
        patterns: [], // Empty patterns indicate default case
        body,
        fallthrough,
        span: host.createSpan(host.current, host.current)
      });

      // Also set defaultCase for backward compatibility
      defaultCase = body;
    } else {
      // Parse pattern (number, string, etc.)
      patterns.push(host.parseExpression());

      host.consume(")", "Expected ')' after case pattern");

      // Parse case body — stop at ;;, esac, or new pattern followed by )
      const statements: (AST.Decl | AST.Stmt)[] = [];
      while (!host.isAtEnd()) {
        while (host.peek().virtualSemi) host.advance();
        if (host.check(";;") || host.check("esac")) break;
        // Detect start of a new case arm (implicit arm boundary without ;;)
        if (isNewCaseArm(host)) break;
        const beforePos = host.current;
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
        if (host.current === beforePos) host.advance();
      }

      const body: AST.Block = {
        kind: "Block",
        statements,
        span: host.createSpan(host.current, host.current)
      };

      // Skip vsemis before checking for ;;
      while (host.peek().virtualSemi) host.advance();
      // Check for fallthrough (no ;;)
      const fallthrough = !host.match(";;");

      cases.push({
        patterns,
        body,
        fallthrough,
        span: host.createSpan(host.current, host.current)
      });
    }
  }
  
  host.consume("esac", "Expected 'esac' to close case statement");
  
  return {
    kind: "Switch",
    discriminant,
    cases,
    defaultCase,
    span: host.createSpan(start, host.current - 1)
  };
}

/**
 * Detect if current position starts a new bash case arm pattern.
 * Uses conservative lookahead to avoid false positives.
 * Checks for: literal/identifier/_ followed by ), or range pattern like N..M)
 */
export function isNewCaseArm(host: BlockHost, ): boolean {
  const t = host.peek();
  // Simple pattern: value )
  if ((t.type === TokenType.NumericLiteral || t.type === TokenType.StringLiteral ||
       t.type === TokenType.Identifier || (t.type === TokenType.Keyword && t.value === "_")) &&
      host.peekAt(1)?.value === ")") {
    return true;
  }
  // Range pattern: N..M ) or N..M)
  if ((t.type === TokenType.NumericLiteral || t.type === TokenType.Identifier) &&
      host.peekAt(1)?.value === ".." &&
      host.peekAt(2) &&
      host.peekAt(3)?.value === ")") {
    return true;
  }
  // Wildcard pattern: * )
  if (t.value === "*" && host.peekAt(1)?.value === ")") {
    return true;
  }
  return false;
}

export function parseBashTestExpression(host: BlockHost, ): AST.Expr {
  // Parse [ ... ] as a special test expression
  const start = host.current;
  host.consume("[", "Expected '[' for bash test");
  
  // Collect everything until ] as a test expression
  // For now, we'll represent it as a special call expression
  const args: AST.Expr[] = [];
  
  while (!host.check("]") && !host.isAtEnd()) {
    // Skip virtual semicolons
    if (host.peek().virtualSemi) {
      host.advance();
      continue;
    }
    
    // Handle operators as identifiers in bash test context
    if (host.peek().type === TokenType.Operator) {
      const op = host.advance();
      
      // Handle test operators like -gt, -lt, etc.
      if (op.value === "-" && host.peek().type === TokenType.Identifier) {
        const flag = host.advance();
        // Create a special test operator expression
        args.push({
          kind: "Identifier",
          name: op.value + flag.value,
          span: host.createSpan(host.current - 2, host.current - 1)
        });
      } else {
        // Other operators like =, !=, etc. are test operators
        args.push({
          kind: "Identifier", 
          name: op.value,
          span: host.createSpanFrom(op)
        });
      }
    } else if (host.peek().type === TokenType.StringLiteral ||
               host.peek().type === TokenType.NumericLiteral ||
               host.peek().type === TokenType.Identifier ||
               host.peek().type === TokenType.SigilIdentifier) {
      // Parse literals and identifiers
      args.push(host.parsePrimary());
    } else {
      // Skip unexpected tokens
      host.advance();
    }
  }
  
  host.consume("]", "Expected ']' after bash test");
  
  // Return as a special call expression representing the test
  return {
    kind: "Call",
    callee: {
      kind: "Identifier",
      name: "test",
      span: host.createSpan(start, start)
    },
    args,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseIfThenBlock(host: BlockHost, ): AST.Block {
  // Parse statements until we hit fi, elif, or else
  const start = host.current;
  const statements: (AST.Decl | AST.Stmt)[] = [];
  
  while (!host.isAtEnd()) {
    // Skip virtual semicolons before checking terminators
    while (host.peek().virtualSemi) host.advance();
    if (host.check("fi") || host.check("elif") || host.check("elseif") ||
        host.check("else") || host.isAtEnd()) break;
    const beforePos = host.current;
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
    // Prevent infinite loop
    if (host.current === beforePos) {
      host.advance();
    }
  }
  
  return {
    kind: "Block",
    statements,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseIndentBlock(host: BlockHost, ): AST.Block {
  const start = host.current;
  
  // Skip any virtual semicolons after the colon
  while (host.peek().virtualSemi) {
    host.advance();
  }
  
  // Get the base indent level
  const baseIndent = host.indentStack.length > 0 ? 
    host.indentStack[host.indentStack.length - 1] : 0;
  
  // The first statement should be more indented
  // Use the indentCol of the next non-whitespace token
  const blockIndent = host.peek().indentCol ?? 0;
  if (blockIndent <= baseIndent && !host.isAtEnd()) {
    // Empty block or same-line statement
    return {
      kind: "Block",
      statements: [],
      span: host.createSpan(start, host.current)
    };
  }
  
  host.indentStack.push(blockIndent);
  const statements: (AST.Decl | AST.Stmt)[] = [];
  
  while (!host.isAtEnd()) {
    // Skip virtual semicolons before checking indentation
    while (host.peek().virtualSemi) {
      host.advance();
    }
    if (host.isAtEnd()) break;

    const nextIndent = host.peek().indentCol ?? 0;

    // Check if we've dedented back to or past the base level
    if (nextIndent < blockIndent) {
      break;
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
  
  host.indentStack.pop();
  
  return {
    kind: "Block",
    statements,
    span: host.createSpan(start, host.current - 1)
  };
}

export function checkKeywordBlock(host: BlockHost, ): boolean {
  const value = host.previous()?.value;
  return value === "do" || value === "case" || value === "begin" ||
         (value === "if" && isBashOrRubyStyle(host)) ||
         (value === "for" && isRubyStyle(host)) ||
         (value === "while" && isRubyStyle(host)) ||
         (value === "function" && isBashStyle(host));
}

export function parseBeginBlock(host: BlockHost, ): AST.Try | AST.Block {
  const start = host.current - 1;
  const statements: (AST.Decl | AST.Stmt)[] = [];
  
  // Parse the main body until rescue/ensure/end
  while (!host.check("rescue") && !host.check("ensure") && !host.check("end") && !host.isAtEnd()) {
    // Skip virtual semicolons
    if (host.peek().virtualSemi) {
      host.advance();
      continue;
    }
    
    const beforePos = host.current;
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
    // Prevent infinite loop
    if (host.current === beforePos) {
      host.advance();
    }
  }
  
  // If we hit 'end' directly without rescue/ensure, it's just a block
  if (host.check("end")) {
    host.advance(); // consume 'end'
    return {
      kind: "Block",
      statements,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  const body: AST.Block = {
    kind: "Block",
    statements,
    span: host.createSpan(start, host.current - 1)
  };
  
  const catches: AST.CatchClause[] = [];
  
  // Handle rescue clauses
  while (host.match("rescue")) {
    let param: AST.Identifier | undefined;
    let type: AST.TypeNode | undefined;
    
    // Check for rescue Type => var pattern
    if (host.peek().type === TokenType.Identifier && !host.check("=>")) {
      type = host.parseType();
    }
    
    if (host.match("=>")) {
      param = host.parseIdentifier();
    }
    
    const rescueStatements: (AST.Decl | AST.Stmt)[] = [];
    while (!host.check("rescue") && !host.check("ensure") && !host.check("end") && !host.isAtEnd()) {
      if (host.peek().virtualSemi) {
        host.advance();
        continue;
      }
      
      const beforePos = host.current;
      try {
        const stmt = host.parseTopLevel();
        if (stmt) rescueStatements.push(stmt);
      } catch (error) {
        if (error instanceof ParseError) {
          host.errors.push(error);
          host.synchronize();
        } else {
          throw error;
        }
      }
      // Prevent infinite loop
      if (host.current === beforePos && !host.check("rescue") && !host.check("ensure") && !host.check("end")) {
        host.advance();
      }
    }
    
    catches.push({
      param,
      type,
      body: {
        kind: "Block",
        statements: rescueStatements,
        span: host.createSpan(host.current - 1, host.current)
      },
      span: host.createSpan(host.current - 1, host.current)
    });
  }
  
  // Handle ensure clause
  let finallyBlock: AST.Block | undefined;
  if (host.match("ensure")) {
    const ensureStatements: (AST.Decl | AST.Stmt)[] = [];
    while (!host.check("end") && !host.isAtEnd()) {
      if (host.peek().virtualSemi) {
        host.advance();
        continue;
      }
      
      try {
        const stmt = host.parseTopLevel();
        if (stmt) ensureStatements.push(stmt);
      } catch (error) {
        if (error instanceof ParseError) {
          host.errors.push(error);
          host.synchronize();
        } else {
          throw error;
        }
      }
    }
    
    finallyBlock = {
      kind: "Block",
      statements: ensureStatements,
      span: host.createSpan(host.current - 1, host.current)
    };
  }
  
  host.consume("end", "Expected 'end' to close begin block");
  
  return {
    kind: "Try",
    body,
    catches,
    finallyBody: finallyBlock,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseKeywordBlock(host: BlockHost, keyword?: string): AST.Block {
  const start = host.current;
  const actualKeyword = keyword || host.previous()?.value || "do";
  host.keywordStack.push(actualKeyword as any);
  
  const statements: (AST.Decl | AST.Stmt)[] = [];
  
  const endKeyword = getEndKeyword(host, actualKeyword);
  
  while (!host.check(endKeyword) && !host.isAtEnd()) {
    // Skip virtual semicolons
    if (host.peek().virtualSemi) {
      host.advance();
      continue;
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
  
  if (!host.match(endKeyword)) {
    throw host.error(host.peek(), `Expected '${endKeyword}'`);
  }
  
  host.keywordStack.pop();
  
  return {
    kind: "Block",
    statements,
    span: host.createSpan(start, host.current - 1)
  };
}

export function getEndKeyword(host: BlockHost, keyword: string): string {
  switch (keyword) {
    case "do": return "done";
    case "case": return "esac";
    case "begin": return "end";
    case "if": return "fi";
    case "for":
    case "while":
    case "function": return isBashStyle(host) ? "done" : "end";
    default: return "end";
  }
}

export function isBashStyle(host: BlockHost, ): boolean {
  // Heuristic: check for bash-style constructs
  return false; // Implement based on context
}

export function isRubyStyle(host: BlockHost, ): boolean {
  // Heuristic: check for ruby-style constructs
  return false; // Implement based on context
}

export function isBashOrRubyStyle(host: BlockHost, ): boolean {
  return isBashStyle(host) || isRubyStyle(host);
}

export function parseSwitch(host: BlockHost, ): AST.Switch | AST.Match {
  const start = host.current - 1;
  const prevVal = host.previous()?.value;
  const isMatch = prevVal === "match" || prevVal === "when";
  const discriminant = host.parseExpression();
  const cases: AST.SwitchCase[] = [];
  let defaultCase: AST.Block | undefined;
  
  // Set flag to indicate we're inside a switch
  const wasInsideSwitch = host.insideSwitch;
  host.insideSwitch = true;
  
  // Support both { } and : styles
  const isPythonStyle = host.check(":");
  let baseIndent = 0;
  
  if (isPythonStyle) {
    host.consume(":", "Expected ':' after match expression");
    // Get the indentation level for Python-style match
    baseIndent = host.tokens[host.current - 1]?.indentCol ?? 0;
    
    // Skip to next line if there's a virtual semicolon
    while (host.peek().virtualSemi) {
      host.advance();
    }
  } else {
    host.consume("{", "Expected '{' after switch expression");
    // We've consumed a brace but didn't increment braceDepth
    // Track this so we know when we're back at the match level
  }
  
  // Track the initial brace depth (before parsing any arms)
  const matchBraceDepth = host.braceDepth;
  
  // Loop condition depends on style
  while (!host.isAtEnd()) {
    // Skip virtual semicolons first
    while (host.peek().virtualSemi) {
      host.advance();
    }

    // For Python style, check if we've dedented back
    if (isPythonStyle) {
      const currentIndent = host.peek().indentCol;
      if (currentIndent !== undefined && currentIndent <= baseIndent) {
        // We've dedented, exit the match block
        break;
      }
    } else {
      // For brace style, check for closing brace
      // We should only exit if we're back at the same brace depth as when we started
      if (host.check("}") && host.braceDepth === matchBraceDepth) {
        break;
      }
    }
    
    if (host.isAtEnd()) {
      break;
    }
    
    const caseStart = host.current;
    
    // Handle default case (also `else` for Kotlin when-style)
    if (host.match("default") || (isMatch && host.match("else"))) {
      // Match uses => while switch uses :
      if (isMatch && !isPythonStyle) {
        host.consume("=>", "Expected '=>' after default");
        defaultCase = parseMatchCaseBody(host);
      } else {
        host.consume(":", "Expected ':' after default");
        defaultCase = parseCaseBody(host);
      }
      
      // Check for comma in match expressions
      if (isMatch && host.check(",")) {
        host.advance();
      }
      continue;
    }
    
    // Handle regular case
    const patterns: AST.Expr[] = [];
    
    // Check for wildcard default in match expressions
    if (isMatch && host.check("_")) {
      const wildcardStart = host.current;
      host.advance(); // consume _
      
      // Skip virtual semicolons
      while (host.peek().virtualSemi) {
        host.advance();
      }
      
      if (host.check("=>")) {
        // This is a wildcard default case
        host.consume("=>", "Expected '=>' after wildcard pattern");
        defaultCase = parseMatchCaseBody(host);
        
        // Check for comma in match expressions
        if (host.check(",")) {
          host.advance();
        }
        continue;
      } else {
        // Not a wildcard, backtrack
        host.current = wildcardStart;
      }
    }
    
    if (host.match("case")) {
      // Go type switch case: case []map[string]string: or case map[K]V:
      if ((host.check("[") && host.peekAt(1)?.value === "]") ||
          (host.check("map") && host.peekAt(1)?.value === "[")) {
        const goType = Types.parseGoTypeAnnotation(host as any);
        // Convert type to an identifier expression for case matching
        const typeName = goType.kind === "SimpleType" ? ((goType as any).id?.name || "GoType") :
                         goType.kind === "GenericType" ? Types.typeNodeToString(goType) : "GoType";
        patterns.push({
          kind: "Identifier",
          name: typeName,
          span: goType.span
        } as AST.Identifier);
      } else {
        // Traditional switch case
        patterns.push(host.parseExpression());
      }
      while (host.match(",")) {
        patterns.push(host.parseExpression());
      }
    } else if (isMatch && !host.check("}")) {
      // Match expression case - parse pattern directly
      patterns.push(parseMatchPattern(host));
      
      // Handle alternative patterns with |
      while (host.match("|")) {
        patterns.push(parseMatchPattern(host));
      }
    } else {
      // No more cases
      break;
    }
    
    // Check for guard clause (if condition)
    let guard: AST.Expr | undefined;
    if (host.match("if")) {
      guard = host.parseExpression();
    }
    
    // Match can use either => or : depending on style
    if (isMatch && !isPythonStyle) {
      host.consume("=>", "Expected '=>' after match pattern");
    } else {
      host.consume(":", "Expected ':' after case pattern");
    }
    
    const body = isMatch ? parseMatchCaseBody(host) : parseCaseBody(host);
    
    // Check for fallthrough
    const fallthrough = checkFallthrough(host);
    
    cases.push({
      patterns,
      guard,
      body,
      fallthrough,
      span: host.createSpan(caseStart, host.current - 1)
    });
    
    
    // In match expressions, cases can be separated by commas
    if (isMatch && host.check(",")) {
      host.advance(); // consume comma
      // Continue to next case
    }
  }
  
  // Only expect closing brace for non-Python style
  if (!isPythonStyle) {
    host.consume("}", "Expected '}' after switch body");
  }
  
  // Restore switch context flag
  host.insideSwitch = wasInsideSwitch;
  
  // Return Match for match expressions, Switch for switch statements
  if (isMatch) {
    // Convert SwitchCase to MatchArm
    const arms: AST.MatchArm[] = cases.map(c => ({
      patterns: c.patterns,
      guard: c.guard,
      body: c.body
    }));
    
    // Add default case as a catch-all arm if present
    if (defaultCase) {
      arms.push({
        patterns: [{ kind: "Identifier", name: "_", span: defaultCase.span }],
        body: defaultCase
      });
    }
    
    return {
      kind: "Match",
      expr: discriminant,
      arms,
      style: isPythonStyle ? "python" : "rust",
      span: host.createSpan(start, host.current - 1)
    } as AST.Match;
  }
  
  return {
    kind: "Switch",
    discriminant,
    cases,
    defaultCase,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseMatchPattern(host: BlockHost, ): AST.Expr {
  const start = host.current;
  
  // Skip virtual semicolons
  while (host.peek().virtualSemi) {
    host.advance();
  }
  
  // Check for wildcard pattern _
  if (host.match("_")) {
    return {
      kind: "Identifier",
      name: "_",
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  // Check for literal patterns (numbers, strings, booleans)
  if (host.peek().type === TokenType.NumericLiteral) {
    return Literals.parseNumericLiteral(host as any);
  }
  
  if (host.peek().type === TokenType.StringLiteral) {
    return Literals.parseStringLiteral(host as any);
  }
  
  if (host.match("true", "false")) {
    const token = host.previous();
    return {
      kind: "BooleanLiteral",
      value: token?.value === "true",
      span: host.createSpanFrom(token!)
    };
  }
  
  if (host.match("null", "undefined", "nil", "None")) {
    return {
      kind: "NullLiteral",
      span: host.createSpanFrom(host.previous()!)
    };
  }
  
  // Check for array/list patterns [head, ...tail]
  if (host.match("[")) {
    return Literals.parseArrayLiteral(host as any);
  }
  
  // Check for object patterns {type: "user", name}
  if (host.match("{")) {
    return Literals.parseObjectLiteral(host as any);
  }
  
  // Parse constructor pattern like Some(v) or simple identifier
  let id: AST.Expr = host.parseIdentifier();
  
  // Check for qualified pattern like Pattern::Regex
  if (host.check("::")) {
    host.advance(); // consume ::
    const property = host.parseIdentifier();
    id = {
      kind: "Member",
      object: id,
      property,
      computed: false,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  // Check for constructor pattern with arguments
  if (host.match("(")) {
    const args: AST.Expr[] = [];
    
    if (!host.check(")")) {
      do {
        // Parse binding variable or nested pattern
        args.push(host.parseExpression());
      } while (host.match(","));
    }
    
    host.consume(")", "Expected ')' after constructor arguments");
    
    return {
      kind: "Call",
      callee: id,
      args,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  return id;
}

export function parseCaseBody(host: BlockHost, ): AST.Block {
  const statements: (AST.Decl | AST.Stmt)[] = [];

  while (!host.isAtEnd() && !host.check("}") && !host.check("default")) {
    // Skip virtual semicolons
    while (host.peek().virtualSemi) {
      host.advance();
    }

    // Check terminators after skipping vsemis
    if (host.check("}") || host.check("default") || host.isAtEnd()) {
      break;
    }

    // Stop at 'case' unless it's a bash case...in statement
    if (host.check("case")) {
      // Look ahead: case <pattern> in => bash case statement (parse as body statement)
      // case <pattern> : => next switch case (stop)
      let isBashCase = false;
      const checkpoint = host.current;
      // Peek past 'case' and the pattern to see 'in' vs ':'
      let scan = host.current + 1; // skip 'case'
      // skip the pattern expression tokens until we hit 'in', ':', or '=>'
      while (scan < host.tokens.length) {
        const t = host.tokens[scan];
        if (t.value === "in") { isBashCase = true; break; }
        if (t.value === ":" || t.value === "=>" || t.value === "}" ||
            t.type === TokenType.EOF || t.virtualSemi) break;
        scan++;
      }
      if (!isBashCase) break;
    }
    
    // Parse statements directly, not using parseTopLevel
    let stmt: AST.Decl | AST.Stmt | null = null;
    if (host.isDeclStart()) {
      stmt = host.parseDeclaration();
    } else {
      stmt = host.parseStatement();
    }
    
    if (stmt) statements.push(stmt);
    
    // Check for break statement
    if (stmt && stmt.kind === "Break") {
      break;
    }
  }
  
  return {
    kind: "Block",
    statements,
    span: host.createSpanFrom(statements[0] || host.previous())
  };
}

export function parseMatchCaseBody(host: BlockHost, ): AST.Block {
  // For match expressions, the body is typically a single expression
  // It can be a block {...} or just an expression
  if (host.check("{")) {
    return parseBlock(host);
  }

  // Handle return/break/continue statements in match arms (Rust allows this)
  if (host.check("return") || host.check("break") || host.check("continue")) {
    const stmt = host.parseStatement();
    return {
      kind: "Block",
      statements: stmt ? [stmt] : [],
      span: stmt?.span || host.createSpan(host.current, host.current)
    };
  }

  // Parse single expression but not comma operator at this level
  // In match cases, comma separates cases, not expressions
  const expr = host.parseAssignmentExpression();
  
  // Check for comma (next case) but don't consume it
  // The main loop will handle advancing past the comma
  if (host.check(",")) {
    // Don't consume - let the main loop handle it
  }
  
  return {
    kind: "Block",
    statements: [{
      kind: "ExprStmt",
      expr,
      span: expr.span
    }],
    span: expr.span
  };
}

export function checkFallthrough(host: BlockHost, ): boolean {
  // Check for fallthrough token or comment
  const next = host.peek();
  if (next.type === TokenType.Keyword && next.value === "fallthrough") {
    host.advance();
    host.consumeSemicolon();
    return true;
  }
  
  // Check for // fallthrough comment
  // This would need to be tracked during lexing
  return false;
}
