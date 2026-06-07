/**
 * Control Flow Parselet — Extracted from Parser (Chunk 8).
 *
 * If, loops, try/catch, break/continue/return, yield, go, defer, etc.
 * Depends on a narrow ControlFlowHost interface.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError } from '../parser-cursor';
import * as Decls from './declarations';
import * as Functions from './functions';

export interface ControlFlowHost {
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
  synchronize(): void;

  parseExpression(): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseExpressionList(): AST.Expr[];
  parseIdentifier(): AST.Identifier;
  parseType(): AST.TypeNode;
  parseBlock(): AST.Block;
  parseBlockOrStatement(): AST.Block;
  parseIndentBlock(): AST.Block;
  parseKeywordBlock(keyword: string): AST.Block;
  parseBashTestExpression(): AST.Expr;
  parseIfThenBlock(): AST.Block;
  parseStatement(): AST.Stmt;
  parseDeclaration(): AST.Decl;
  parseTopLevel(): AST.Decl | AST.Stmt | null;
  isDeclStart(): boolean;
  parseExprStmt(): AST.ExprStmt | AST.If;
  checkIndentBlock(): boolean;
}

export function parseIf(host: ControlFlowHost, ): AST.If {
  const start = host.current - 1;
  const arms: AST.IfArm[] = [];
  let isBashStyle = false;
  
  // First if condition
  // Check if this is a bash-style test expression with [ ... ]
  let test: AST.Expr;
  if (host.check("[")) {
    // Bash test expression
    test = host.parseBashTestExpression();
  } else if (host.check("echo") || host.check("test") || host.check("command")) {
    // Bash-style: if command ...; then — consume tokens until ; or then
    const cmdStart = host.current;
    while (!host.isAtEnd() && !host.check(";") && !host.check("then") && !host.peek().virtualSemi) {
      host.advance();
    }
    test = {
      kind: "Identifier",
      name: host.tokens.slice(cmdStart, host.current).map((t: any) => t.value).join(" "),
      span: host.createSpan(cmdStart, host.current - 1),
    } as AST.Identifier;
  } else if (host.check("(")) {
    // Parenthesized condition — explicitly parse (expr) without postfix
    // to avoid if (cond) (body) being parsed as if (cond(body))
    host.advance(); // consume '('
    test = host.parseExpression();
    host.consume(")", "Expected ')' after if condition");
  } else {
    test = host.parseExpression();
  }

  // Go-style if with init statement: if init; condition { ... }
  if (host.check(";") && host.peekNext()?.value !== "then") {
    // The test we just parsed is actually the init statement; parse the real condition
    host.advance(); // consume ;
    if (host.check("[")) {
      test = host.parseBashTestExpression();
    } else {
      test = host.parseExpression();
    }
  }

  // Check for Python-style colon
  if (host.match(":")) {
    // Indent-based block
    const body = host.parseIndentBlock();
    arms.push({ test, body, span: host.createSpan(start, host.current - 1) });
  } else if (host.check(";") && host.peekNext()?.value === "then") {
    // Bash-style: if condition; then ... fi
    host.advance(); // consume ;
    host.consume("then", "Expected 'then' after if condition in bash-style");
    const body = host.parseIfThenBlock();
    arms.push({ test, body, span: host.createSpan(start, host.current - 1) });
    isBashStyle = true;
  } else if (host.match("then")) {
    // Bash-style without semicolon: if condition then ... fi
    const body = host.parseIfThenBlock();
    arms.push({ test, body, span: host.createSpan(start, host.current - 1) });
    isBashStyle = true;
  } else {
    // Regular block or single statement
    const body = host.parseBlockOrStatement();
    arms.push({ test, body, span: host.createSpan(start, host.current - 1) });
  }
  
  // elif/elseif clauses
  while (host.match("elif", "elseif")) {
    const elifTest = host.parseExpression();
    
    let elifBody: AST.Block;
    if (host.match(":")) {
      elifBody = host.parseIndentBlock();
    } else if (isBashStyle && (host.match(";") || host.match("then"))) {
      if (host.previous()?.value === ";") {
        host.consume("then", "Expected 'then' after elif condition");
      }
      elifBody = host.parseIfThenBlock();
    } else {
      // Block or single statement
      elifBody = host.parseBlockOrStatement();
    }
    
    arms.push({ 
      test: elifTest, 
      body: elifBody, 
      span: host.createSpan(host.current - 1, host.current) 
    });
  }
  
  // else clause
  let elseBody: AST.Block | undefined;
  if (host.match("else")) {
    // Check for 'else if' (two separate keywords)
    if (host.match("if")) {
      // Handle 'else if' as another if statement
      // parseIf expects 'if' to have been already matched
      const elseIf = parseIf(host);
      // Wrap the else-if in a block
      elseBody = {
        kind: "Block",
        statements: [elseIf],
        span: elseIf.span
      };
    } else if (host.match(":")) {
      elseBody = host.parseIndentBlock();
    } else if (isBashStyle) {
      elseBody = host.parseIfThenBlock();
    } else {
      // Block or single statement
      elseBody = host.parseBlockOrStatement();
    }
  }
  
  // Consume 'fi' for bash-style if statements
  if (isBashStyle) {
    host.consume("fi", "Expected 'fi' to close if statement");
  }
  
  return {
    kind: "If",
    arms,
    elseBody,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseDoStatement(host: ControlFlowHost, ): AST.Stmt {
  const start = host.current - 1;
  const startToken = host.tokens[start - 1]; // The 'do' token

  // Ruby-style block: do |params| body end
  if (host.check("|")) {
    host.advance(); // consume '|'
    const params: AST.Param[] = [];
    if (!host.check("|")) {
      do {
        params.push(Functions.parseParameter(host as any));
      } while (host.match(","));
    }
    host.consume("|", "Expected '|' after block parameters");
    // Parse body until 'end' or 'done'
    const stmts: AST.Stmt[] = [];
    while (!host.check("end") && !host.check("done") && !host.isAtEnd()) {
      while (host.peek().virtualSemi) host.advance();
      if (host.check("end") || host.check("done")) break;
      stmts.push(host.parseTopLevel() as AST.Stmt);
    }
    if (!host.match("end") && !host.match("done")) {
      throw host.error(host.peek(), "Expected 'end' to close do block");
    }
    const body: AST.Block = { kind: "Block", statements: stmts, span: host.createSpan(start, host.current - 1) };
    // Emit as a Lambda expression statement (the block is a Ruby block/lambda)
    const lambda: AST.Lambda = {
      kind: "Lambda",
      params,
      body,
      span: host.createSpan(start, host.current - 1)
    };
    return { kind: "ExprStmt", expression: lambda, span: lambda.span } as any;
  }

  // Parse the block — use custom loop that accepts both 'done' (bash) and 'end' (Ruby)
  let body: AST.Block;
  if (host.check("{")) {
    body = host.parseBlock();
  } else {
    const stmts: (AST.Decl | AST.Stmt)[] = [];
    while (!host.check("done") && !host.check("end") && !host.check("while") && !host.isAtEnd()) {
      if (host.peek().virtualSemi) { host.advance(); continue; }
      const beforePos = host.current;
      try {
        const stmt = host.parseTopLevel();
        if (stmt) stmts.push(stmt);
      } catch (error) {
        if (error instanceof ParseError) {
          host.errors.push(error as any);
          host.synchronize();
        } else { throw error; }
      }
      if (host.current === beforePos) host.advance();
    }
    body = { kind: "Block", statements: stmts, span: host.createSpan(start, host.current - 1) };
  }

  // Check what comes after the block
  if (host.match("while")) {
    // This is a JavaScript-style do-while loop
    host.consume("(", "Expected '(' after 'while' in do-while loop");
    const test = host.parseExpression();
    host.consume(")", "Expected ')' after condition in do-while loop");

    // Optional semicolon after do-while
    host.consumeSemicolon();

    return {
      kind: "Loop",
      mode: "do-while",
      body,
      test,
      span: host.createSpan(start, host.current - 1)
    };
  } else if (host.check("done")) {
    // This would be a bash-style do-done block
    // For now, treat it as a simple block statement
    host.consume("done", "Expected 'done' to close do block");
    return {
      kind: "Block",
      statements: body.statements,
      span: host.createSpan(start, host.current - 1)
    };
  } else if (host.check("end")) {
    // Ruby-style do-end block (e.g., fork do ... end)
    host.consume("end", "Expected 'end' to close do block");
    return {
      kind: "Block",
      statements: body.statements,
      span: host.createSpan(start, host.current - 1)
    };
  } else {
    // Error: 'do' must be followed by either 'while', 'done', or 'end'
    throw host.error(host.peek(), "Expected 'while' or 'done' after do block");
  }
}

export function parseLoop(host: ControlFlowHost, ): AST.Loop {
  const start = host.current - 1;
  const keyword = host.previous()?.value || "";
  
  if (keyword === "loop") {
    // Infinite loop
    const body = host.parseBlock();
    return {
      kind: "Loop",
      mode: "infinite",
      body,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  if (keyword === "for") {
    // Check for await
    const isAwait = host.match("await");
    
    // Check for foreach-style
    if (host.peek().type === TokenType.Identifier || (isAwait && host.check("("))) {
      if (isAwait) {
        // for await (const item of stream)
        host.consume("(", "Expected '(' after 'for await'");
        
        // Parse variable declaration (const/let/var item)
        let variable: AST.Identifier | AST.ArrayPattern | AST.ObjectPattern;
        if (host.match("const", "let", "var")) {
          if (host.check("[") || host.check("{")) {
            variable = Decls.parseDestructuringPattern(host as any);
          } else {
            variable = host.parseIdentifier();
          }
        } else {
          variable = host.parseIdentifier();
        }
        
        host.consume("of", "Expected 'of' in for-await loop");
        const iterable = host.parseExpression();
        host.consume(")", "Expected ')' after for-await");
        const body = host.parseBlock();
        
        return {
          kind: "Loop",
          mode: "foreach",
          variable,
          iterable,
          iterationKind: "of",
          body,
          await: true,
          span: host.createSpan(start, host.current - 1)
        };
      } else {
        const checkpoint = host.current;
        const id = host.advance();
        if (host.match(",")) {
          // Multi-variable: for key, value of/in ...
          const firstVar: AST.Identifier = {
            kind: "Identifier",
            name: id.value,
            span: host.createSpanFrom(id)
          };
          const secondId = host.peek();
          if (secondId.type === TokenType.Identifier || secondId.type === TokenType.Keyword) {
            host.advance();
            const secondVar: AST.Identifier = {
              kind: "Identifier",
              name: secondId.value,
              span: host.createSpanFrom(secondId)
            };
            if (host.match("of", "in")) {
              const iterType = host.previous()?.value === "in" ? "in" : "of";
              const variable: AST.ArrayPattern = {
                kind: "ArrayPattern",
                elements: [firstVar, secondVar],
                span: host.createSpan(checkpoint, host.current - 1)
              };
              const iterable = host.parseExpression();
              host.match(":");
              const body = host.parseBlockOrStatement();
              return {
                kind: "Loop",
                mode: "foreach",
                variable,
                iterable,
                iterationKind: iterType,
                body,
                span: host.createSpan(start, host.current - 1)
              };
            }
          }
          // Didn't match multi-variable pattern, backtrack
          host.current = checkpoint;
        } else if (host.match("in")) {
          // foreach style: for x in collection
          const variable: AST.Identifier = {
            kind: "Identifier",
            name: id.value,
            span: host.createSpanFrom(id)
          };
          host.noRubyBlock = true;
          const iterable = host.parseExpression();
          host.noRubyBlock = false;
          // Consume optional Python-style colon (for x in items:)
          let body: AST.Block;
          if (host.match(":")) {
            body = host.parseIndentBlock();
          } else if (host.match("do")) {
            body = host.parseKeywordBlock("do");
          } else {
            body = host.parseBlockOrStatement();
          }
          return {
            kind: "Loop",
            mode: "foreach",
            variable,
            iterable,
            iterationKind: "in",
            body,
            span: host.createSpan(start, host.current - 1)
          };
        }
        host.current = checkpoint;
      }
    }
    
    // Check for Go-style for loop (without parentheses)
    // Look for pattern: identifier :=
    if (host.peek().type === TokenType.Identifier && host.peekAt(1)?.value === ":=") {
      // Go-style for loop without parentheses
      // Parse init statement manually to avoid consuming semicolon
      const initStart = host.current;
      const name = host.parseIdentifier();
      host.consume(":=", "Expected ':=' in for init");

      // Check for Go for-range: `for task := range iterable { ... }`
      if (host.peek().type === TokenType.Identifier && host.peek().value === "range") {
        host.advance(); // consume 'range'
        const iterable = host.parseExpression();
        const body = host.parseBlockOrStatement();
        return {
          kind: "Loop",
          mode: "foreach",
          variable: name,
          iterable,
          body,
          span: host.createSpan(start, host.current - 1)
        };
      }

      const expr = host.parseExpression();
      const init: AST.ShortDecl = {
        kind: "ShortDecl",
        pairs: [{ name, expr }],
        span: host.createSpan(initStart, host.current - 1)
      };

      host.consume(";", "Expected ';' after init");
      
      let test: AST.Expr | undefined;
      if (!host.check(";")) {
        test = host.parseExpression();
      }
      host.consume(";", "Expected ';' after loop condition");
      
      let step: AST.Expr | undefined;
      if (!host.check("{")) {
        step = host.parseExpression();
      }
      
      const body = host.parseBlockOrStatement();
      
      return {
        kind: "Loop",
        mode: "for",
        init,
        test,
        step,
        body,
        span: host.createSpan(start, host.current - 1)
      };
    }
    
    // Go-style condition-only for: for condition { body }
    if (!host.check("(")) {
      const test = host.parseExpression();
      let body: AST.Block;
      if (host.match(":")) {
        body = host.parseIndentBlock();
      } else {
        body = host.parseBlockOrStatement();
      }
      return {
        kind: "Loop",
        mode: "while",
        test,
        body,
        span: host.createSpan(start, host.current - 1)
      };
    }

    // Traditional for loop with parentheses
    host.consume("(", "Expected '(' after 'for'");
    
    // Check for for-of/for-in loops with const/let/var
    if (host.check("const") || host.check("let") || host.check("var")) {
      const checkpoint = host.current;
      const declKeyword = host.advance(); // consume const/let/var
      
      if (host.peek().type === TokenType.Identifier || host.peek().type === TokenType.Keyword || host.check("[") || host.check("{")) {
        let variable: AST.Identifier | AST.ArrayPattern | AST.ObjectPattern;
        if (host.check("[") || host.check("{")) {
          variable = Decls.parseDestructuringPattern(host as any);
        } else {
          // Accept keywords as variable names (e.g., for (const fn of items))
          const tok = host.advance();
          variable = {
            kind: "Identifier",
            name: tok.value,
            span: host.createSpanFrom(tok)
          };
        }

        // Check for 'of' or 'in'
        if (host.match("of", "in")) {
          const iterType = host.previous()?.value === "in" ? "in" : "of";
          const iterable = host.parseExpression();
          host.consume(")", "Expected ')' after for-of/for-in");
          const body = host.parseBlockOrStatement();

          return {
            kind: "Loop",
            mode: "foreach",
            variable,
            iterable,
            iterationKind: iterType,
            body,
            span: host.createSpan(start, host.current - 1)
          };
        }
      }
      
      // Not a for-of/for-in, restore position and parse normally
      host.current = checkpoint;
    }
    
    // Check for for-of/for-in without const/let/var (just identifier or destructuring)
    if (host.peek().type === TokenType.Identifier) {
      const checkpoint = host.current;
      
      // Check if this might be a destructuring pattern
      const firstId = host.parseIdentifier();
      
      // Check for destructuring pattern like a, b (note: we're already inside parentheses)
      if (host.check(",")) {
        // This is a destructuring pattern
        const elements: AST.Identifier[] = [firstId];
        while (host.match(",")) {
          elements.push(host.parseIdentifier());
        }
        
        // Check if followed by 'in' or 'of'
        if (host.check(")") && (host.peekNext()?.value === "in" || host.peekNext()?.value === "of")) {
          host.advance(); // consume )
          host.advance(); // consume in/of
          const iterType = host.previous()?.value === "in" ? "in" : "of";
          const iterable = host.parseExpression();
          const body = host.parseBlockOrStatement();
          
          // Create a pseudo-identifier for the destructured pattern
          const variable: AST.Identifier = {
            kind: "Identifier",
            name: `(${elements.map(e => e.name).join(", ")})`,
            originalSpelling: `(${elements.map(e => e.name).join(", ")})`,
            span: host.createSpan(checkpoint, checkpoint + elements.length - 1)
          };
          
          return {
            kind: "Loop",
            mode: "foreach",
            variable,
            iterable,
            iterationKind: iterType,
            body,
            span: host.createSpan(start, host.current - 1)
          };
        }
      } else if (host.match("of", "in")) {
        // Simple identifier with in/of
        const iterType = host.previous()?.value === "in" ? "in" : "of";
        const iterable = host.parseExpression();
        host.consume(")", "Expected ')' after for-of/for-in");
        const body = host.parseBlockOrStatement();
        
        return {
          kind: "Loop",
          mode: "foreach",
          variable: firstId,
          iterable,
          iterationKind: iterType,
          body,
          span: host.createSpan(start, host.current - 1)
        };
      }
      
      // Not a for-of/for-in, restore position
      host.current = checkpoint;
    }
    
    let init: AST.Stmt | AST.Decl | undefined;
    if (!host.check(";")) {
      if (host.isDeclStart()) {
        init = host.parseDeclaration();
      } else {
        init = host.parseExprStmt();
      }
    } else {
      host.advance(); // consume ';'
    }
    
    // Skip virtual semicolons that might appear
    while (host.peek().virtualSemi) {
      host.advance();
    }
    
    let test: AST.Expr | undefined;
    if (!host.check(";")) {
      test = host.parseExpression();
    }
    host.consume(";", "Expected ';' after loop condition");
    
    let step: AST.Expr | undefined;
    if (!host.check(")")) {
      step = host.parseExpression();
    }
    
    host.consume(")", "Expected ')' after for clauses");
    const body = host.parseBlockOrStatement();
    
    return {
      kind: "Loop",
      mode: "for",
      init,
      test,
      step,
      body,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  if (keyword === "while") {
    // Check if this is a bash-style test expression with [ ... ]
    let test: AST.Expr;
    if (host.check("[")) {
      // Bash test expression
      test = host.parseBashTestExpression();
    } else {
      test = host.parseExpression();
    }
    
    // Skip optional semicolon before do (bash style: while [ test ]; do)
    if (host.check(";") && host.peekNext()?.value === "do") {
      host.advance(); // consume semicolon
    }
    
    // Check for keyword block (do...done)
    let body: AST.Block;
    if (host.match("do")) {
      body = host.parseKeywordBlock("do");
    } else if (host.match(":")) {
      // Python-style colon block
      body = host.parseIndentBlock();
    } else if (host.peek().virtualSemi && !host.check("{")) {
      // Ruby-style: while cond \n body \n end
      body = host.parseKeywordBlock("while");
    } else {
      body = host.parseBlockOrStatement();
    }

    return {
      kind: "Loop",
      mode: "while",
      test,
      body,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  if (keyword === "until") {
    // Check if this is a bash-style test expression with [ ... ]
    let test: AST.Expr;
    if (host.check("[")) {
      // Bash test expression
      test = host.parseBashTestExpression();
    } else {
      test = host.parseExpression();
    }
    
    // Skip optional semicolon before do (bash style: until [ test ]; do)
    if (host.check(";") && host.peekNext()?.value === "do") {
      host.advance(); // consume semicolon
    }
    
    // Check for keyword block (do...done)
    let body: AST.Block;
    if (host.match("do")) {
      body = host.parseKeywordBlock("do");
    } else {
      body = host.parseBlockOrStatement();
    }
    return {
      kind: "Loop",
      mode: "until",
      test,
      body,
      span: host.createSpan(start, host.current - 1)
    };
  }
  
  throw host.error(host.peek(), "Invalid loop type");
}

export function parseForeach(host: ControlFlowHost, ): AST.Loop {
  const start = host.current - 1;
  const variable = host.parseIdentifier();
  host.consume("in", "Expected 'in' in foreach loop");
  // Suppress Ruby block consumption so factory.deps do ... done doesn't
  // get parsed as Ruby method call (items.each do |x| ... end)
  host.noRubyBlock = true;
  const iterable = host.parseExpression();
  host.noRubyBlock = false;

  // Check for do/done keyword block
  let body: AST.Block;
  if (host.match("do")) {
    body = host.parseKeywordBlock("do");
  } else {
    body = host.parseBlockOrStatement();
  }
  
  return {
    kind: "Loop",
    mode: "foreach",
    variable,
    iterable,
    body,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseTry(host: ControlFlowHost, ): AST.Try {
  const start = host.current - 1;
  
  // Check for Python-style try with colon
  let body: AST.Block;
  if (host.match(":")) {
    // Python-style - parse single statement or indented block
    if (host.checkIndentBlock()) {
      body = host.parseIndentBlock();
    } else {
      // Single statement on same line
      const stmt = host.parseStatement();
      body = {
        kind: "Block",
        statements: [stmt],
        span: stmt.span
      };
    }
  } else if (host.check("(")) {
    // Java-style try-with-resources: try (resource = expr) { body }
    // Skip the resource declaration and parse as regular try body
    host.advance(); // consume '('
    // Parse resource declarations (skip until closing paren)
    let parenDepth = 1;
    while (parenDepth > 0 && !host.isAtEnd()) {
      if (host.check("(")) parenDepth++;
      if (host.check(")")) parenDepth--;
      if (parenDepth > 0) host.advance();
    }
    host.consume(")", "Expected ')' after try-with-resources");
    body = host.parseBlock();
  } else {
    body = host.parseBlock();
  }

  const catches: AST.CatchClause[] = [];
  
  while (host.match("catch", "except", "rescue")) {
    const clauseType = host.previous()?.value; // Remember which keyword we matched
    let param: AST.Identifier | undefined;
    let type: AST.TypeNode | undefined;
    
    // Python-style except: except Type: except Type as var: except (T1, T2) as var:
    // But NOT except (e) { ... } which uses parenthesized catch below
    if (clauseType === "except" && !host.check("{")) {
      // Handle parenthesized exception tuple: except (ValueError, TypeError) as e:
      // Distinguish from JS-style except (e) { ... } by looking for comma or uppercase type after (
      if (host.check("(")) {
        // Lookahead: if token after ( contains a comma before ), it's a Python tuple
        let pos = host.current + 1; // skip past (
        let depth = 1;
        let hasComma = false;
        while (pos < host.tokens.length && depth > 0) {
          const v = host.tokens[pos].value;
          if (v === "(") depth++;
          else if (v === ")") depth--;
          else if (v === "," && depth === 1) hasComma = true;
          pos++;
        }
        // Check what follows the closing paren
        const afterParen = pos < host.tokens.length ? host.tokens[pos - 1] : undefined;
        const tokenAfterParen = pos < host.tokens.length ? host.tokens[pos] : undefined;
        const isPythonTuple = hasComma || (tokenAfterParen && (tokenAfterParen.value === "as" || tokenAfterParen.value === ":"));

        if (isPythonTuple && !(!hasComma && tokenAfterParen?.value === "{")) {
          host.advance(); // consume '('
          type = host.parseType();
          while (host.match(",")) {
            host.parseType();
          }
          host.consume(")", "Expected ')' after exception types");
          if (host.match("as")) {
            param = host.parseIdentifier();
          }
          if (host.match(":")) {
            let catchBody: AST.Block;
            if (host.checkIndentBlock()) {
              catchBody = host.parseIndentBlock();
            } else {
              const stmt = host.parseStatement();
              catchBody = { kind: "Block", statements: [stmt], span: stmt.span };
            }
            catches.push({ param, type, body: catchBody, span: host.createSpan(host.current - 1, host.current) });
          }
          continue;
        }
      }
    }
    if (clauseType === "except" && !host.check("(") && !host.check("{")) {
      // Parse optional exception type(s) and binding
      if (host.peek().type === TokenType.Identifier && !host.check(":")) {
        type = host.parseType();
        if (host.match("as")) {
          param = host.parseIdentifier();
        }
      }
      // Expect colon for Python-style, or handle brace-style
      if (host.match(":")) {
        let catchBody: AST.Block;
        if (host.checkIndentBlock()) {
          catchBody = host.parseIndentBlock();
        } else {
          const stmt = host.parseStatement();
          catchBody = {
            kind: "Block",
            statements: [stmt],
            span: stmt.span
          };
        }
        catches.push({
          param,
          type,
          body: catchBody,
          span: host.createSpan(host.current - 1, host.current)
        });
      } else {
        // Fall through to brace-style catch body
        const catchBody = host.parseBlock();
        catches.push({ param, type, body: catchBody, span: host.createSpan(host.current - 1, host.current) });
      }
    } else if (clauseType === "rescue") {
      // Ruby-style rescue Type => var or just rescue
      if (host.peek().type === TokenType.Identifier && !host.check("=>")) {
        type = host.parseType();
      }
      
      if (host.match("=>")) {
        param = host.parseIdentifier();
      }
      
      // Parse rescue body (statements until next rescue/ensure/end)
      const rescueStatements: (AST.Decl | AST.Stmt)[] = [];
      while (!host.check("rescue") && !host.check("ensure") && !host.check("end") && 
             !host.check("finally") && !host.check("except") && !host.isAtEnd()) {
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
        // Prevent infinite loop - if we didn't advance, force advance
        if (host.current === beforePos) {
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
    } else if (host.match("(")) {
      // Traditional catch with parentheses
      if (!host.check(")")) {
        // Java-style: catch (Type variable) — type comes first
        // JS-style: catch (variable) — no type, or catch (variable: Type)
        const afterFirst = host.peekAt(1);
        if (looksLikeJavaCatchParameter(host)) {
          type = host.parseType();
          param = host.parseIdentifier();
        } else if (afterFirst && afterFirst.type === TokenType.Identifier && host.peekAt(2)?.value === ")") {
          // Java: catch (ExceptionType varName)
          type = host.parseType();
          param = host.parseIdentifier();
        } else if (afterFirst && afterFirst.type === TokenType.Identifier && host.peekAt(2)?.value !== ")") {
          // Could be Java with qualified type: catch (java.io.IOException e)
          type = host.parseType();
          if (host.peek().type === TokenType.Identifier && !host.check(")")) {
            param = host.parseIdentifier();
          }
        } else {
          param = host.parseIdentifier();
          if (host.match(":")) {
            type = host.parseType();
          }
        }
      }
      host.consume(")", "Expected ')' after catch clause");
      
      const catchBody = host.parseBlock();
      catches.push({
        param,
        type,
        body: catchBody,
        span: host.createSpan(host.current - 1, host.current)
      });
    } else {
      // No parentheses or colon, parse block directly
      const catchBody = host.parseBlock();
      catches.push({
        param,
        type,
        body: catchBody,
        span: host.createSpan(host.current - 1, host.current)
      });
    }
  }
  
  let finallyBody: AST.Block | undefined;
  if (host.match("finally")) {
    // Check for Python-style finally with colon
    if (host.match(":")) {
      if (host.checkIndentBlock()) {
        finallyBody = host.parseIndentBlock();
      } else {
        const stmt = host.parseStatement();
        finallyBody = {
          kind: "Block",
          statements: [stmt],
          span: stmt.span
        };
      }
    } else {
      finallyBody = host.parseBlock();
    }
  }
  
  return {
    kind: "Try",
    body,
    catches,
    finallyBody,
    span: host.createSpan(start, host.current - 1)
  };
}

function looksLikeJavaCatchParameter(host: ControlFlowHost): boolean {
  let pos = host.current;
  let depth = 0;
  const clauseTokens: Token[] = [];

  while (pos < host.tokens.length) {
    const token = host.tokens[pos];
    if (token.value === "(") {
      depth++;
    } else if (token.value === ")") {
      if (depth === 0) break;
      depth--;
    }
    if (depth === 0) {
      clauseTokens.push(token);
    }
    pos++;
  }

  if (clauseTokens.length < 2) return false;
  if (clauseTokens.some(token => token.value === ":" || token.value === ",")) return false;

  const last = clauseTokens[clauseTokens.length - 1];
  if (last.type !== TokenType.Identifier && last.type !== TokenType.Keyword) return false;

  const typeTokens = clauseTokens.slice(0, -1);
  if (typeTokens.length === 1 && typeTokens[0].type === TokenType.Identifier) {
    return /^[A-Z_$]/.test(typeTokens[0].value);
  }

  return typeTokens.some(token =>
    token.value === "." || token.value === "|" || token.value === "<" || token.value === "[" ||
    (token.type === TokenType.Identifier && /^[A-Z_$]/.test(token.value))
  );
}

export function parseUsing(host: ControlFlowHost, ): AST.Using {
  const start = host.current - 1;

  let resource: AST.Expr | AST.Decl;

  // C#/Java-style: using (Type var = expr) { ... }
  const hasParen = host.match("(");

  // Inside using (...), check for typed declaration: Type Name = Expr or var Name = Expr
  const isUsingDecl = hasParen && (
    // Type Name = Expr pattern (e.g., StreamReader sr = new StreamReader("file"))
    (host.peek().type === TokenType.Identifier && host.peekAt(1)?.type === TokenType.Identifier) ||
    // var/let/const Name = Expr pattern (e.g., var response = await ...)
    ((host.peek().value === "var" || host.peek().value === "let" || host.peek().value === "const") &&
     host.peekAt(1)?.type === TokenType.Identifier)
  );
  if (isUsingDecl) {
    // Skip var/let/const or type name
    const firstToken = host.advance();
    const varName = host.parseIdentifier();
    host.consume("=", "Expected '=' in using declaration");
    const value = host.parseExpression();
    resource = {
      kind: "VarDecl",
      names: [varName],
      values: [value],
      span: host.createSpan(start, host.current - 1)
    } as AST.VarDecl;
  } else {
    // Parse the resource expression. The general expression parser treats
    // `as` as a TypeScript type assertion; in a context-manager header it is
    // Python's alias binder, so unwrap that shape here.
    const expr = host.parseExpression();
    const contextAliasAssertion =
      expr.kind === "TypeAssertion" && expr.type.kind === "SimpleType"
        ? (expr as AST.TypeAssertion & { type: AST.SimpleType })
        : undefined;
    const assertedAlias = contextAliasAssertion?.type.id;

    // Check for Python-style 'as' alias
    if (assertedAlias) {
      resource = {
        kind: "VarDecl",
        names: [assertedAlias],
        values: [contextAliasAssertion.expr],
        span: host.createSpan(start, host.current - 1)
      } as AST.VarDecl;
    } else if (host.match("as")) {
      const alias = host.parseIdentifier();
      // Create a variable declaration for the alias
      resource = {
        kind: "VarDecl",
        names: [alias],
        values: [expr],
        span: host.createSpan(start, host.current - 1)
      } as AST.VarDecl;
    } else if (host.isDeclStart()) {
      // Rewind and parse as declaration
      host.current = start + 1;
      resource = host.parseDeclaration();
    } else {
      resource = expr;
    }
  }
  
  // Consume closing paren for using (...) form
  if (hasParen) {
    host.consume(")", "Expected ')' after using declaration");
  }

  // Parse body - check for Python-style colon or explicit block
  let body: AST.Block | undefined;
  if (host.match(":")) {
    if (host.checkIndentBlock()) {
      body = host.parseIndentBlock();
    } else {
      // Single statement on same line
      const stmt = host.parseStatement();
      body = {
        kind: "Block",
        statements: [stmt],
        span: stmt.span
      };
    }
  } else if (host.check("{")) {
    // Explicit block
    body = host.parseBlock();
  } else {
    // C#-style using without explicit body - applies to rest of scope
    // Create an empty block placeholder
    body = {
      kind: "Block",
      statements: [],
      span: host.createSpan(host.current, host.current)
    };
  }
  
  return {
    kind: "Using",
    resource,
    body,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseDefer(host: ControlFlowHost, ): AST.Defer {
  const start = host.current - 1;
  let body: AST.Block | AST.Expr;
  
  if (host.check("{")) {
    body = host.parseBlock();
  } else {
    body = host.parseExpression();
    host.consumeSemicolon();
  }
  
  return {
    kind: "Defer",
    body,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseBreak(host: ControlFlowHost, ): AST.Break {
  const start = host.current - 1;
  let label: AST.Identifier | undefined;
  
  // Check if next token is an identifier that could be a label
  // Don't treat keywords like "default", "case", "}", etc. as labels
  const next = host.peek();
  if (next.type === TokenType.Identifier && !host.check(";")) {
    // Parse as label identifier
    const token = host.advance();
    label = {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
  } else if (next.type === TokenType.Keyword && 
             !host.check(";") &&
             !host.check("default") && 
             !host.check("case") &&
             !host.check("}") &&
             !host.check("else") &&
             !host.check("catch") &&
             !host.check("finally")) {
    // Only treat certain keywords as potential labels, not control flow keywords
    const token = host.advance();
    label = {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
  }
  
  host.consumeSemicolon();
  
  return {
    kind: "Break",
    label,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseContinue(host: ControlFlowHost, ): AST.Continue {
  const start = host.current - 1;
  let label: AST.Identifier | undefined;
  
  // Check if next token is an identifier that could be a label
  // Don't treat keywords like "default", "case", "}", etc. as labels
  const next = host.peek();
  if (next.type === TokenType.Identifier && !host.check(";")) {
    // Parse as label identifier
    const token = host.advance();
    label = {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
  } else if (next.type === TokenType.Keyword && 
             !host.check(";") &&
             !host.check("default") && 
             !host.check("case") &&
             !host.check("}") &&
             !host.check("else") &&
             !host.check("catch") &&
             !host.check("finally")) {
    // Only treat certain keywords as potential labels, not control flow keywords
    const token = host.advance();
    label = {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
  }
  
  host.consumeSemicolon();
  
  return {
    kind: "Continue",
    label,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseReturn(host: ControlFlowHost, ): AST.Return {
  const start = host.current - 1;
  const values: AST.Expr[] = [];
  
  if (!host.checkSemicolon() && !host.isAtEnd() && !host.check("}")) {
    values.push(...host.parseExpressionList());
  }
  
  host.consumeSemicolon();
  
  return {
    kind: "Return",
    values,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseAssert(host: ControlFlowHost, ): AST.ExprStmt {
  const start = host.current - 1;
  
  // Parse the condition expression
  const condition = host.parseExpression();
  
  // Optional: parse comma and message (Python style: assert x == 1, "message")
  let message: AST.Expr | undefined;
  if (host.match(",")) {
    message = host.parseExpression();
  }
  
  host.consumeSemicolon();
  
  // Create an assert as a call expression wrapped in an expression statement
  const assertCall: AST.Call = {
    kind: "Call",
    callee: {
      kind: "Identifier",
      name: "assert",
      span: host.createSpan(start, start)
    },
    args: message ? [condition, message] : [condition],
    span: host.createSpan(start, host.current - 1)
  };
  
  return {
    kind: "ExprStmt",
    expr: assertCall,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseEcho(host: ControlFlowHost, ): AST.Echo {
  const start = host.current - 1;
  const values = host.parseExpressionList();
  host.consumeSemicolon();
  
  return {
    kind: "Echo",
    values,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseThrow(host: ControlFlowHost, ): AST.Throw {
  const start = host.current - 1;
  const keyword = host.previous()?.value;
  const value = host.parseExpression();
  const cause = keyword === "raise" && host.match("from")
    ? host.parseExpression()
    : undefined;
  host.consumeSemicolon();
  
  return {
    kind: "Throw",
    value,
    ...(cause ? { cause } : {}),
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseYield(host: ControlFlowHost, ): AST.Yield {
  const start = host.current - 1;
  let value: AST.Expr | undefined;
  let delegate = false;
  
  if (host.match("*")) {
    delegate = true;
  }
  
  if (!host.checkSemicolon() && !host.isAtEnd()) {
    value = host.parseExpression();
  }
  
  host.consumeSemicolon();
  
  return {
    kind: "Yield",
    value,
    delegate,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseYieldExpression(host: ControlFlowHost, ): AST.Yield {
  const start = host.current - 1;
  let value: AST.Expr | undefined;
  let delegate = false;
  
  // Check for yield* or yield from
  if (host.match("*")) {
    delegate = true;
  } else if (host.match("from")) {
    delegate = true;
  }
  
  // In expression context, parse the value if there is one
  if (!host.check(";") && !host.check(",") && !host.check(")") && 
      !host.check("]") && !host.check("}") && !host.isAtEnd()) {
    value = host.parseAssignmentExpression();
  }
  
  return {
    kind: "Yield",
    value,
    delegate,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseGo(host: ControlFlowHost, ): AST.Go {
  const start = host.current - 1;
  const expr = host.parseExpression();
  host.consumeSemicolon();
  
  return {
    kind: "Go",
    expr,
    span: host.createSpan(start, host.current - 1)
  };
}

/**
 * Check if 'func' keyword is followed by Go-style typed params: func(name type, ...)
 * or func Name(name type, ...). Peeks ahead without consuming tokens.
 */
export function hasGoTypedParams(host: ControlFlowHost, ): boolean {
  // Current token is 'func'. Skip optional name identifier to find '('
  let idx = host.current + 1;
  // Skip function name if present: func Name(...)
  if (idx < host.tokens.length && host.tokens[idx].type === TokenType.Identifier) {
    idx++;
  }
  // Must have '(' next
  if (idx >= host.tokens.length || host.tokens[idx].value !== "(") return false;
  const firstParam = idx + 1;
  if (firstParam >= host.tokens.length) return false;
  // Empty params func() → not Go-typed
  if (host.tokens[firstParam].value === ")") return false;
  // Check if first param is identifier followed by a type token (Go-style: name type)
  if (host.tokens[firstParam].type !== TokenType.Identifier) return false;
  const afterFirst = firstParam + 1;
  if (afterFirst >= host.tokens.length) return false;
  const afterTok = host.tokens[afterFirst];
  // Go type can be: identifier, keyword type (interface, struct, map, chan, func), pointer (*), slice ([])
  if (afterTok.type === TokenType.Identifier) return true;
  if (afterTok.type === TokenType.Keyword &&
      ["interface", "struct", "map", "chan", "func"].includes(afterTok.value)) return true;
  if (afterTok.value === "*" || afterTok.value === "[") return true;
  return false;
}

export function parsePass(host: ControlFlowHost, ): AST.Pass {
  const start = host.current - 1;
  host.consumeSemicolon();
  
  return {
    kind: "Pass",
    span: host.createSpan(start, host.current - 1)
  };
}
