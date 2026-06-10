/**
 * Expression Postfix Parselet — Extracted from Parser (Chunk 12b).
 *
 * Handles postfix expression parsing: function calls, member/index access,
 * optional chaining, Ruby blocks, generic call suffix, scope resolution,
 * postfix unary operators (++, --, !, ?, .await).
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError } from '../parser-cursor';
import * as Types from './types';
import * as Literals from './literals';

export interface PostfixHost {
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
  must(expected: string, options?: { recoverWithSynthetic?: boolean }): boolean;

  parseExpression(minPrecedence?: number): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseIdentifier(): AST.Identifier;
  parseType(): AST.TypeNode;
  parseStatement(): AST.Stmt;
  isUnaryOp(token: Token): boolean;
  isBinaryOp(token: Token): boolean;
  isStatementKeyword(keyword: string): boolean;
  tryParseGenericArgs(): AST.TypeNode[] | null;
}

export function parsePostfix(host: PostfixHost, expr: AST.Expr): AST.Expr {
  // Note: Generic arguments are now parsed in parsePrimary and attached to the identifier
  // They will be in expr._genericArgs if present
  
  while (true) {
    // Python implicit string concatenation: "a" "b" → "a" + "b"
    if ((expr.kind === "StringLiteral") && host.peek().type === TokenType.StringLiteral) {
      const right = Literals.parseStringLiteral(host as any);
      expr = {
        kind: "Binary",
        op: "+",
        left: expr,
        right,
        span: host.createSpanFrom(expr)
      } as any;
      continue;
    }

    // Check for generic arguments after member access (e.g., React.forwardRef<T1, T2>)
    // This handles cases where generics appear after a member access operation
    if (host.check("<") && !host.check("<-") && !host.check("<<") && !host.check("<=") && expr.kind === "Member") {
      // Skip if next token after < is a numeric literal — definitely a comparison
      const afterLt = host.peekAt(1);
      if (afterLt && afterLt.type === TokenType.NumericLiteral) {
        // Fall through to binary operator handling below
      } else {
      const checkpoint = host.current;
      const genericArgs = tryParseGenericArgs(host);
      
      if (genericArgs) {
        // Store generic arguments for the next call expression
        (expr as any)._genericArgs = genericArgs;
      } else {
        // Not generic arguments, restore position
        host.current = checkpoint;
      }
      }
    }
    
    // Function call
    if (host.match("(")) {
      // Special case for make() with Go types (channels, slices, maps)
      if (expr.kind === "Identifier" && expr.name === "make") {
        // Check if the next token suggests a Go type
        if (host.check("<-") || host.peek().value === "chan" ||
            (host.check("[") && host.peekAt(1)?.value === "]") ||
            host.peek().value === "map") {
          // Use parseGoTypeAnnotation for Go-specific types (slices, maps)
          // Use parseType for chan (supports angle bracket generics like chan<T>)
          const useGoType = (host.check("[") && host.peekAt(1)?.value === "]") || host.peek().value === "map";
          const typeNode = useGoType ? Types.parseGoTypeAnnotation(host as any) : host.parseType();
          
          // If it's a GenericType with chan base, keep it as a structured expression
          let typeExpr: AST.Expr;
          if (typeNode.kind === "GenericType" && typeNode.base.name === "chan") {
            // Keep the GenericType structure accessible by wrapping in a special node
            // For now, we'll still convert to string but mark it specially
            typeExpr = {
              kind: "Identifier",
              name: Types.typeNodeToString(typeNode),
              originalSpelling: Types.typeNodeToString(typeNode),
              span: typeNode.span,
              // Store the original type info in a non-standard field for test access
              _typeNode: typeNode
            } as any;
          } else {
            typeExpr = {
              kind: "Identifier",
              name: Types.typeNodeToString(typeNode),
              originalSpelling: Types.typeNodeToString(typeNode),
              span: typeNode.span
            };
          }
          
          // Handle optional size/capacity arguments
          const args: AST.Expr[] = [typeExpr];
          while (host.match(",")) {
            args.push(host.parseAssignmentExpression());
          }
          
          host.must(")", { recoverWithSynthetic: true });
          
          // Create the Call node, and if we have a GenericType, store it
          const callExpr: AST.Call = {
            kind: "Call",
            callee: expr,
            args,
            span: host.createSpanFrom(expr)
          };
          
          // Store the type node in a non-standard field for test access
          if ((typeExpr as any)._typeNode) {
            (callExpr as any)._typeNode = (typeExpr as any)._typeNode;
          }
          
          expr = callExpr;
          continue;
        }
      }
      
      const args = parseArguments(host);
      host.must(")", { recoverWithSynthetic: true });
      
      // Check if we have stored generic arguments
      const genericArgs = (expr as any)._genericArgs;
      
      const callExpr: AST.Call = {
        kind: "Call",
        callee: expr,
        args,
        span: host.createSpanFrom(expr)
      };
      
      // Add generic arguments if they exist
      if (genericArgs) {
        callExpr.typeArgs = genericArgs;
        // Clean up the temporary storage
        delete (expr as any)._genericArgs;
      }
      // Rust turbofish spelling marks the call as definite Rust.
      if ((expr as any)._turbofish) {
        callExpr.turbofish = true;
        delete (expr as any)._turbofish;
      }

      expr = callExpr;
      
      // Check for Ruby block after function call
      if (host.check("do") && !host.noRubyBlock) {
        const blockStart = host.current;
        host.advance(); // consume 'do'
        
        // Parse block parameters if present |x, y|
        let blockParams: AST.Identifier[] = [];
        if (host.match("|")) {
          do {
            blockParams.push(host.parseIdentifier());
          } while (host.match(","));
          host.consume("|", "Expected '|' after block parameters");
        }
        
        // Parse block body until 'end' or 'done'
        const blockStatements: (AST.Stmt | AST.Decl)[] = [];
        while (!host.isAtEnd() && host.peek().value !== "end" && host.peek().value !== "done") {
          if (host.peek().virtualSemi) {
            host.advance();
            continue;
          }

          const stmt = host.parseStatement();
          if (stmt) blockStatements.push(stmt);
        }

        if (!host.match("end") && !host.match("done")) {
          host.consume("end", "Expected 'end' to close Ruby block");
        }

        // Add block as a special property on the call (not in standard AST)
        (callExpr as any).rubyBlock = {
          params: blockParams,
          body: blockStatements,
          span: host.createSpan(blockStart, host.current - 1)
        };
      }

      continue;
    }

    // Check for Ruby block after member access (Ruby method calls without parens)
    // e.g., items.each do |item| ... end
    if (host.check("do") && expr.kind === "Member" && !host.noRubyBlock) {
      // Convert the member access to a call with no arguments
      const callExpr: AST.Call = {
        kind: "Call",
        callee: expr,
        args: [],
        span: expr.span
      };

      const blockStart = host.current;
      host.advance(); // consume 'do'

      // Parse block parameters if present |x, y|
      let blockParams: AST.Identifier[] = [];
      if (host.match("|")) {
        do {
          blockParams.push(host.parseIdentifier());
        } while (host.match(","));
        host.consume("|", "Expected '|' after block parameters");
      }

      // Parse block body until 'end' or 'done'
      const blockStatements: (AST.Stmt | AST.Decl)[] = [];
      while (!host.isAtEnd() && host.peek().value !== "end" && host.peek().value !== "done") {
        if (host.peek().virtualSemi) {
          host.advance();
          continue;
        }

        const stmt = host.parseStatement();
        if (stmt) blockStatements.push(stmt);
      }

      if (!host.match("end") && !host.match("done")) {
        host.consume("end", "Expected 'end' to close Ruby block");
      }
      
      // Add block as a special property on the call
      (callExpr as any).rubyBlock = {
        params: blockParams,
        body: blockStatements,
        span: host.createSpan(blockStart, host.current - 1)
      };
      
      expr = callExpr;
      continue;
    }
    
    // Ruby block with curly braces: expr { |x, y| body }
    // Only when { is followed by | (to disambiguate from object literals/blocks)
    if (host.check("{") && host.peekNext()?.value === "|" && !host.noRubyBlock &&
        (expr.kind === "Call" || expr.kind === "Member" || expr.kind === "Identifier")) {
      // Convert to call if needed
      let callExpr: AST.Call;
      if (expr.kind === "Call") {
        callExpr = expr as AST.Call;
      } else {
        callExpr = {
          kind: "Call",
          callee: expr,
          args: [],
          span: expr.span
        };
      }

      const blockStart = host.current;
      host.advance(); // consume '{'

      // Parse block parameters |x, y|
      let blockParams: AST.Identifier[] = [];
      if (host.match("|")) {
        do {
          blockParams.push(host.parseIdentifier());
        } while (host.match(","));
        host.consume("|", "Expected '|' after block parameters");
      }

      // Parse block body until '}'
      const blockStatements: (AST.Stmt | AST.Decl)[] = [];
      while (!host.isAtEnd() && !host.check("}")) {
        if (host.peek().virtualSemi) {
          host.advance();
          continue;
        }

        const stmt = host.parseStatement();
        if (stmt) blockStatements.push(stmt);
      }

      host.consume("}", "Expected '}' to close Ruby block");

      (callExpr as any).rubyBlock = {
        params: blockParams,
        body: blockStatements,
        span: host.createSpan(blockStart, host.current - 1)
      };

      expr = callExpr;
      continue;
    }

    // Member access and optional chaining
    // Check for ?. first to handle both ?.property and ?.[index]
    if (host.peek().value === "?.") {
      const next = host.peekNext();
      if (next?.value === "[") {
        // Optional chaining with bracket notation (?.[)
        host.advance(); // consume ?.
        host.advance(); // consume [
        const index = host.parseExpression();
        host.consume("]", "Expected ']' after index");
        expr = {
          kind: "Index",
          object: expr,
          index,
          optional: true,
          span: host.createSpanFrom(expr)
        };
        continue;
      } else if (next?.value === "(") {
        // Optional call (?.())
        host.advance(); // consume ?.
        host.advance(); // consume (
        const args: AST.Expr[] = [];
        if (!host.check(")")) {
          do {
            args.push(host.parseAssignmentExpression());
          } while (host.match(","));
        }
        host.consume(")", "Expected ')' after optional call arguments");
        expr = {
          kind: "Call",
          callee: expr,
          args,
          optional: true,
          span: host.createSpanFrom(expr)
        };
        continue;
      } else if (next && (next.type === TokenType.Identifier || next.type === TokenType.Keyword)) {
        // Optional chaining with property access (?.property)
        host.advance(); // consume ?.
        const property = host.parseIdentifier();
        expr = {
          kind: "Member",
          object: expr,
          property,
          optional: true,
          span: host.createSpanFrom(expr)
        };
        continue;
      }
      // If ?. is not followed by [, (, or identifier, don't consume it
    }
    
    // Regular index access (including Python slices)
    if (host.match("[")) {
      // Python slice: [start:end] [:end] [start:] [start:end:step]
      let index: AST.Expr;
      if (host.check(":") || host.check("::")) {
        // [:end] or [:end:step] or [::step]
        const isDoubleColon = host.check("::");
        host.advance(); // consume ':' or '::'
        let end: AST.Expr | undefined;
        let step: AST.Expr | undefined;
        if (isDoubleColon) {
          // [::step]
          step = host.check("]") ? undefined : host.parseAssignmentExpression();
        } else {
          end = host.check("]") || host.check(":") ? undefined : host.parseAssignmentExpression();
          if (host.match(":")) {
            step = host.check("]") ? undefined : host.parseAssignmentExpression();
          }
        }
        index = { kind: "Call", callee: { kind: "Identifier", name: "__slice__", span: host.createSpan(host.current - 1, host.current - 1) } as any,
          args: [end, step].filter(Boolean) as AST.Expr[], span: host.createSpan(host.current - 1, host.current - 1) } as any;
      } else {
        index = host.parseExpression();
        // Check for slice notation after first expression: [start:end] [start:end:step] [start::step]
        if (host.check(":") || host.check("::")) {
          if (host.check("::")) {
            host.advance(); // consume '::'
            const step = host.check("]") ? undefined : host.parseAssignmentExpression();
            index = { kind: "Binary", op: ":", left: index, right: step ?? { kind: "Identifier", name: "", span: index.span } as any, span: host.createSpanFrom(index) } as any;
          } else {
            host.advance(); // consume ':'
            const end = host.check("]") || host.check(":") ? undefined : host.parseAssignmentExpression();
            let step: AST.Expr | undefined;
            if (host.match(":")) {
              step = host.check("]") ? undefined : host.parseAssignmentExpression();
            }
            index = { kind: "Binary", op: ":", left: index, right: end ?? { kind: "Identifier", name: "", span: index.span } as any, span: host.createSpanFrom(index) } as any;
          }
        }
      }
      host.consume("]", "Expected ']' after index");
      expr = {
        kind: "Index",
        object: expr,
        index,
        optional: false,
        span: host.createSpanFrom(expr)
      };
      continue;
    }
    
    // Scope resolution operator (C++/Rust style)
    if (host.match("::")) {
      // Rust turbofish: `expr::<T, U>(args)` — type args spelled after `::`.
      if (host.check("<")) {
        const turbofishArgs = host.tryParseGenericArgs();
        if (turbofishArgs) {
          (expr as any)._genericArgs = turbofishArgs;
          (expr as any)._turbofish = true;
          continue;
        }
      }
      // After ::, keywords can be used as identifiers
      const next = host.peek();
      let property: AST.Identifier;
      
      if (next.type === TokenType.Identifier || next.type === TokenType.Keyword) {
        property = {
          kind: "Identifier",
          name: next.value,
          originalSpelling: next.value,
          span: host.createSpan(host.current, host.current)
        };
        host.advance();
      } else {
        property = host.parseIdentifier();
      }
      
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
    
    // Regular member access (including pointer dereference, force unwrap, and PHP arrow)
    if (host.match(".", ".*", "!.", "->")) {
      const op = host.previous()?.value;
      const deref = op === ".*";
      const forceUnwrap = op === "!.";
      const phpArrow = op === "->";
      
      // Special case for .*. pattern (pointer member access)
      if (deref && host.match(".")) {
        const property = host.parseIdentifier();
        // Create a compound member access: (*obj).field
        expr = {
          kind: "Member",
          object: {
            kind: "Unary",
            op: "*",
            argument: expr,
            prefix: true,
            span: expr.span
          },
          property,
          optional: false,
          span: host.createSpanFrom(expr)
        };
      } else if (!deref) {
        // Check for .await syntax (Rust-style)
        if (host.peek().value === "await") {
          host.advance(); // consume await
          expr = {
            kind: "Unary",
            op: "await",
            argument: expr,
            prefix: false,
            span: host.createSpanFrom(expr)
          };
          continue;
        }

        // Go type assertion: expr.(Type) or expr.(type)
        if (host.check("(")) {
          host.advance(); // consume (
          if (host.check("type")) {
            // Go type switch: expr.(type)
            host.advance(); // consume 'type'
            host.consume(")", "Expected ')' after type assertion");
            expr = {
              kind: "TypeAssertion",
              expr,
              type: {
                kind: "SimpleType",
                id: { kind: "Identifier", name: "type", span: host.createSpanFrom(expr) },
                span: host.createSpanFrom(expr)
              } as AST.TypeNode,
              span: host.createSpanFrom(expr)
            };
          } else {
            // Go type assertion: expr.(ConcreteType)
            const type = Types.parseGoTypeAnnotation(host as any);
            host.consume(")", "Expected ')' after type assertion");
            expr = {
              kind: "TypeAssertion",
              expr,
              type,
              span: host.createSpanFrom(expr)
            };
          }
          continue;
        }

        // Regular member access, force unwrap, or PHP arrow
        const property = host.parseIdentifier();
        
        // If it was force unwrap, wrap the object in a non-null assertion
        if (forceUnwrap) {
          expr = {
            kind: "Member",
            object: {
              kind: "Unary",
              op: "!",
              argument: expr,
              prefix: false,
              span: expr.span
            },
            property,
            optional: false,
            span: host.createSpanFrom(expr)
          };
        } else {
          // Regular member access (. or ->)
          expr = {
            kind: "Member",
            object: expr,
            property,
            optional: false,
            span: host.createSpanFrom(expr)
          };
        }
      }
      continue;
    } else if (host.check(".") && host.peekNext()?.value === "*") {
      // Handle case where .* was lexed as two separate tokens
      host.advance(); // consume .
      host.advance(); // consume *
      if (host.match(".")) {
        const property = host.parseIdentifier();
        // Create a compound member access: (*obj).field
        expr = {
          kind: "Member",
          object: {
            kind: "Unary",
            op: "*",
            argument: expr,
            prefix: true,
            span: expr.span
          },
          property,
          optional: false,
          span: host.createSpanFrom(expr)
        };
      } else {
        // Just pointer dereference
        expr = {
          kind: "Unary",
          op: "*",
          argument: expr,
          prefix: true,
          span: host.createSpanFrom(expr)
        };
      }
      continue;
    }
    
    // Postfix increment/decrement
    if (host.match("++", "--")) {
      const op = host.previous();
      expr = {
        kind: "Unary",
        op: op?.value || "",
        argument: expr,
        prefix: false,
        span: host.createSpanFrom(expr)
      };
      continue;
    }
    
    // Non-null assertion operator (TypeScript)
    if (host.check("!")) {
      const next = host.peekNext();
      if (!next || !host.isUnaryOp(next)) {
        // Only treat ! as postfix if not followed by another unary operator
        host.advance(); // consume !
        expr = {
          kind: "Unary",
          op: "!",
          argument: expr,
          prefix: false,
          span: host.createSpanFrom(expr)
        };
        continue;
      }
    }
    
    // Try operator (Rust) - only at end of expression or before certain tokens
    if (host.check("?")) {
      const next = host.peekNext();
      // Treat as postfix ? if followed by:
      // - End of statement (; or newline)
      // - Closing bracket/paren
      // - Comma
      // - Binary operators (but not :)
      const isPostfix = !next ||
                       next.type === TokenType.EOF ||
                       next.value === ";" ||
                       next.value === ")" ||
                       next.value === "]" ||
                       next.value === "}" ||
                       next.value === "," ||
                       next.value === "." ||
                       next.virtualSemi ||
                       (host.isBinaryOp(next) && next.value !== ":" && next.value !== "<") ||
                       (next.type === TokenType.Keyword && host.isStatementKeyword(next.value));
      
      if (isPostfix) {
        host.advance(); // consume ?
        expr = {
          kind: "Unary",
          op: "?",
          argument: expr,
          prefix: false,
          span: host.createSpanFrom(expr)
        };
        // Rust try-then-chain across lines: `.send().await?\n  .text()`.
        // MASI inserts a vsemi after `?` (following `)`/`]`); skip it when
        // the next line continues the member chain with `.`.
        if (host.peek().type === TokenType.VirtualSemi && host.peekNext()?.value === ".") {
          host.advance(); // skip virtual semicolon
        }
        continue;
      }
    }
    
    break;
  }
  
  return expr;
}

export function parseArguments(host: PostfixHost, ): AST.Expr[] {
  const args: AST.Expr[] = [];
  
  // Skip virtual semicolons before first argument
  while (host.peek().virtualSemi) {
    host.advance();
  }
  
  if (!host.check(")")) {
    do {
      // Check for spread operator
      if (host.match("...")) {
        const spreadStart = host.current - 1;
        const argument = host.parseAssignmentExpression();
        args.push({
          kind: "Spread",
          argument,
          optional: false,
          span: host.createSpan(spreadStart, host.current - 1)
        });
      } else {
        // Parse expression but stop at comma at this level
        const expr = host.parseAssignmentExpression();

        if (host.check(":") && !host.check("::") && expr.span.end === host.peek().start) {
          host.advance();
          const value = host.parseAssignmentExpression();
          args.push({
            kind: "Binary",
            op: ":",
            left: expr,
            right: value,
            span: host.createSpanFrom(expr)
          } as any);
          while (host.peek().virtualSemi) {
            host.advance();
          }
          if (!host.match(",")) {
            break;
          }
          while (host.peek().virtualSemi) {
            host.advance();
          }
          if (host.check(")")) {
            break;
          }
          continue;
        }

        // Python generator expression inside function call: func(expr for x in items)
        while (host.peek().virtualSemi) host.advance();
        if (host.check("for") && args.length === 0) {
          // Parse inline generator: expr for var in iterable [if cond]
          const comprehensions: any[] = [];
          while (host.match("for")) {
            const variables: AST.Identifier[] = [];
            variables.push(host.parseIdentifier());
            while (host.match(",")) variables.push(host.parseIdentifier());
            host.consume("in", "Expected 'in' in generator comprehension");
            const iterable = host.parseAssignmentExpression();
            let condition: AST.Expr | undefined;
            if (host.match("if")) condition = host.parseAssignmentExpression();
            comprehensions.push({ variables, iterable, condition });
            if (!host.check("for")) break;
          }
          // Use first comprehension to build proper ListComprehension AST node
          const firstComp = comprehensions[0];
          args.push({
            kind: "ListComprehension",
            expression: expr,
            targets: firstComp.variables,
            iterable: firstComp.iterable,
            filter: firstComp.condition,
            span: host.createSpan(host.current - 1, host.current - 1)
          } as any);
          return args;
        }
        args.push(expr);
      }
      
      // Skip virtual semicolons after argument
      while (host.peek().virtualSemi) {
        host.advance();
      }

      // Python implicit string concatenation: adjacent string literals without comma
      while (host.peek().type === TokenType.StringLiteral && !host.check(")")) {
        const right = host.parseAssignmentExpression();
        const left = args[args.length - 1];
        args[args.length - 1] = {
          kind: "Binary",
          op: "+",
          left,
          right,
          span: host.createSpan((left.span as any).start ?? host.current - 2, host.current - 1)
        } as any;
        while (host.peek().virtualSemi) {
          host.advance();
        }
      }

      if (!host.match(",")) {
        break;
      }
      
      // Skip virtual semicolons after comma
      while (host.peek().virtualSemi) {
        host.advance();
      }
      
      // Tolerate trailing comma
      if (host.check(")")) {
        break;
      }
    } while (true);
  }
  
  return args;
}

export function tryParseGenericArgs(host: PostfixHost, ): AST.TypeNode[] | null {
  if (!host.check("<")) return null;

  const checkpoint = host.current;
  const errorCheckpoint = host.errors.length;

  try {
    host.advance(); // <
    const args: AST.TypeNode[] = [];

    do {
      args.push(host.parseType());
    } while (host.match(","));

    // Handle >> and >>> as closing brackets
    if (host.check(">>")) {
      // Treat >> as a single > and leave the second > for the next parse
      const originalToken = host.tokens[host.current];
      // Replace >> with > at current position
      host.tokens[host.current] = { ...originalToken, value: ">" };
      // Insert another > after it
      const syntheticToken = { ...originalToken, value: ">" };
      host.tokens.splice(host.current + 1, 0, syntheticToken);
      // Now consume the first >
      host.advance();
    } else if (host.check(">>>")) {
      // Treat >>> as a single > and leave >> for the next parse
      const originalToken = host.tokens[host.current];
      // Replace >>> with > at current position
      host.tokens[host.current] = { ...originalToken, value: ">" };
      // Insert >> after it
      const syntheticToken = { ...originalToken, value: ">>" };
      host.tokens.splice(host.current + 1, 0, syntheticToken);
      // Now consume the first >
      host.advance();
    } else {
      host.consume(">", "Expected '>' after generic arguments");
    }

    // Check if followed by valid continuation
    const next = host.peek();
    if (next.value === "(" || next.value === "[" || next.value === "{" ||
        next.value === ">" || next.value === ">>" || next.value === ">>>" ||
        next.value === ":" || next.value === "extends" ||
        next.value === "implements" || next.value === "where") {
      return args;
    }

    // Not a valid generic argument list — restore errors from failed attempt
    host.errors.length = errorCheckpoint;
    host.current = checkpoint;
    return null;
  } catch {
    // Failed to parse as generic args — restore errors from failed attempt
    host.errors.length = errorCheckpoint;
    host.current = checkpoint;
    return null;
  }
}
