/**
 * Class/Interface/Impl/Enum Parselet — Extracted from Parser (Chunk 7).
 *
 * Handles class declarations, interface declarations, impl blocks,
 * where clauses, and enum declarations.
 * Depends on a narrow ClassHost interface — no direct Parser import.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';
import { ParseError } from '../parser-cursor';
import * as Functions from './functions';

export interface ClassHost {
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
  consumeSemicolon(): void;
  checkSemicolon(): boolean;
  attempt<T>(fn: () => T): T | null;
  must(expected: string, options?: { recoverWithSynthetic?: boolean }): boolean;

  parseIdentifier(): AST.Identifier;
  parseExpression(): AST.Expr;
  parsePostfix(expr: AST.Expr): AST.Expr;
  parseAssignmentExpression(): AST.Expr;
  parseType(): AST.TypeNode;
  parseBlock(): AST.Block;
}

function decoratorFromExpr(expr: AST.Expr, span: AST.Span): AST.Decorator {
  return {
    kind: "Decorator",
    name: expr.kind === "Identifier" ? expr :
          expr.kind === "Call" && expr.callee.kind === "Identifier" ? expr.callee :
          { kind: "Identifier" as const, name: "unknown", span: expr.span },
    expression: expr,
    args: expr.kind === "Call" ? expr.args : undefined,
    span,
  };
}

function isHeaderTerminator(token: Token | undefined): boolean {
  return !token ||
    token.type === TokenType.EOF ||
    token.virtualSemi ||
    token.value === ";" ||
    token.value === "{" ||
    token.value === ":";
}

function looksLikeTypeParameterList(host: ClassHost): boolean {
  if (!host.check("<")) return false;

  let depth = 0;
  for (let i = host.current; i < host.tokens.length; i++) {
    const token = host.tokens[i];
    if (!token || token.type === TokenType.EOF || token.virtualSemi || token.value === ";") {
      return false;
    }
    if (token.value === "<") {
      depth++;
    } else if (token.value === ">") {
      depth--;
      if (depth === 0) return true;
    } else if (depth === 0 && (token.value === "{" || token.value === ":")) {
      return false;
    }
  }
  return false;
}

function parseRubySuperclass(host: ClassHost): AST.TypeNode | undefined {
  if (!host.match("<")) return undefined;

  const superStart = host.current;
  const parts: string[] = [];
  while (!isHeaderTerminator(host.peek())) {
    parts.push(host.advance().value);
  }

  if (parts.length === 0) return undefined;
  const span = host.createSpan(superStart, host.current - 1);
  const id: AST.Identifier = {
    kind: "Identifier",
    name: parts.join(""),
    span,
  };
  return {
    kind: "SimpleType",
    id,
    span,
  };
}

function consumeRubyClassBody(host: ClassHost): void {
  while (host.check(";") || host.peek().virtualSemi) {
    host.advance();
  }

  let depth = 1;
  while (!host.isAtEnd() && depth > 0) {
    const value = host.peek().value;

    if (value === "class" || value === "module" || value === "def" || value === "begin" || value === "do") {
      depth++;
      host.advance();
      continue;
    }

    if (value === "end") {
      depth--;
      host.advance();
      continue;
    }

    host.advance();
  }
}

export function parseClassDecl(host: ClassHost, decorators?: AST.Expr[]): AST.ClassDecl {
  const start = host.current - 1;
  const name = host.parseIdentifier();

  // Parse type parameters
  let typeParams: AST.Identifier[] | undefined;
  if (looksLikeTypeParameterList(host) && host.match("<")) {
    typeParams = [];
    do {
      typeParams.push(host.parseIdentifier());
      // Skip type constraints: extends Type, super Type, : Type
      if (host.match("extends", "super")) {
        host.parseType();
      } else if (host.check(":") && !host.check("::")) {
        host.advance();
        host.parseType();
        // Handle multiple constraints with + (e.g., <T: Clone + Debug>)
        while (host.match("+")) {
          host.parseType();
        }
      }
      // Skip default type: = DefaultType
      if (host.match("=")) {
        host.parseType();
      }
    } while (host.match(","));
    host.consume(">", "Expected '>' after type parameters");
  }

  // Python-style class inheritance: class Name(Parent, Mixin):
  let extendsType: AST.TypeNode | undefined;
  if (host.check("(") && !typeParams) {
    host.advance(); // consume '('
    if (!host.check(")")) {
      extendsType = host.parseType();
      // Additional parent classes (mixins) and keyword args (total=False)
      while (host.match(",")) {
        if (host.peek().type === TokenType.Identifier && host.peekNext()?.value === "=") {
          // Python keyword argument in class parents (e.g. total=False)
          host.advance(); // name
          host.advance(); // =
          host.parseExpression(); // value
        } else {
          host.parseType();
        }
      }
    }
    host.consume(")", "Expected ')' after parent classes");
  }

  // Parse extends clause
  if (!extendsType && host.match("extends")) {
    extendsType = host.parseType();
  }

  // Ruby-style class inheritance: class Name < Superclass
  if (!extendsType && host.check("<")) {
    extendsType = parseRubySuperclass(host);
  }

  // Parse implements clause
  let implementsTypes: AST.TypeNode[] | undefined;
  if (host.match("implements")) {
    implementsTypes = [];
    do {
      implementsTypes.push(host.parseType());
    } while (host.match(","));
  }

  // Parse with clause (mixins/traits)
  let withTypes: AST.TypeNode[] | undefined;
  if (host.match("with")) {
    withTypes = [];
    do {
      withTypes.push(host.parseType());
    } while (host.match(","));
  }

  // Parse class body - handle { }, Python-style :, and Ruby-style ... end
  const members: AST.ClassMember[] = [];
  let isPythonStyle = false;
  let isRubyStyle = false;
  let classIndent = -1;

  if (host.match(":")) {
    // Python-style class with colon
    isPythonStyle = true;
    // Skip virtual semicolons after colon
    while (host.peek().virtualSemi) {
      host.advance();
    }
    // Record the indentation level of the class body
    classIndent = host.peek().indentCol ?? 0;
  } else if (host.check("{")) {
    // Traditional braces style
    host.advance();
  } else {
    // Ruby classes are delimited by `end` and their bodies often contain
    // DSL macros that do not fit the generic class-member model.
    isRubyStyle = true;
    consumeRubyClassBody(host);
  }

  while (!isRubyStyle && !host.isAtEnd()) {
    // Skip virtual semicolons and regular semicolons before dedent checks.
    // Python class fields are separated by virtual semicolons; checking
    // indentation before consuming them makes the parser leave the second
    // and later field declarations at top level.
    while (host.check(";") || host.peek().virtualSemi) {
      host.advance();
    }

    // For Python style, check if we've dedented out of the class
    if (isPythonStyle) {
      const currentIndent = host.peek().indentCol ?? 0;
      if (currentIndent < classIndent) {
        // We've dedented out of the class
        break;
      }
    } else if (host.check("}")) {
      // For brace style, check for closing brace
      break;
    }

    if (host.check("}")) break;

    try {
      // Parse class member
      const memberStart = host.current;

      // Parse member decorators (e.g., @Input, @Output, @HostListener)
      const memberDecorators: AST.Decorator[] = [];
      const memberDecoratorExprs: AST.Expr[] = [];
      let firstDecoratorStart: number | undefined;
      while (host.check("@")) {
        const decoratorStart = host.current;
        if (firstDecoratorStart === undefined) firstDecoratorStart = decoratorStart;
        host.advance(); // consume @

        if (host.peek().type === TokenType.Identifier) {
          const base = host.parseIdentifier();
          const expression = host.parsePostfix(base);
          memberDecoratorExprs.push(expression);
          memberDecorators.push(decoratorFromExpr(expression, host.createSpan(decoratorStart, host.current - 1)));
        }

        // Skip virtual semicolons after decorators
        while (host.peek().virtualSemi) {
          host.advance();
        }
      }

      // Handle Ruby-style def...end methods
      if (host.match("def")) {
        const method = Functions.parseFuncDecl(host as any, false, false, false, memberDecoratorExprs, firstDecoratorStart);
        members.push(method as any);
        continue;
      }

      // Handle regular function declarations
      if (host.match("fn", "fun", "function", "func")) {
        const isGenerator = host.previous()?.value === "function" && host.match("*");
        const method = Functions.parseFuncDecl(host as any, false, false, isGenerator, memberDecoratorExprs, firstDecoratorStart);
        members.push(method as any);
        continue;
      }

      // Handle C++/C# operator overloads: operator+(args) { ... }
      if (host.peek().value === "operator" && host.peekNext()?.type === TokenType.Operator) {
        host.advance(); // consume 'operator'
        const opToken = host.advance(); // consume the operator symbol
        const opName: AST.Identifier = {
          kind: "Identifier",
          name: "operator" + opToken.value,
          span: host.createSpanFrom(opToken)
        };
        // Skip optional `const` qualifier after params (C++ const methods)
        const params = Functions.parseParameterList(host as any);
        if (host.peek().value === "const") host.advance();
        const body = host.parseBlock();
        members.push({
          kind: "Method",
          name: opName,
          params,
          body,
          span: host.createSpan(memberStart, host.current - 1)
        } as any);
        continue;
      }

      // Handle async functions
      if (host.match("async")) {
        // Check if followed by function keyword
        if (host.match("fn", "fun", "function", "func", "def")) {
          const isGenerator = host.previous()?.value === "function" && host.match("*");
          const method = Functions.parseFuncDecl(host as any, true, false, isGenerator, memberDecoratorExprs, firstDecoratorStart);
          members.push(method as any);
          continue;
        }

        // Check if followed by identifier (async method without function keyword)
        // e.g., async handle<T>() { }
        if (host.peek().type === TokenType.Identifier) {
          const methodName = host.parseIdentifier();

          // Parse generic parameters if present
          let genericParams: AST.Identifier[] | undefined;
          if (host.match("<")) {
            genericParams = [];
            do {
              genericParams.push(host.parseIdentifier());
              if (host.match("extends", "super")) {
                host.parseType();
              } else if (host.check(":") && !host.check("::")) {
                host.advance();
                host.parseType();
                while (host.match("+")) host.parseType();
              }
            } while (host.match(","));
            host.consume(">", "Expected '>' after generic parameters");
          }

          // Parse parameters
          const params = Functions.parseParameterList(host as any);

          // Parse return type if present
          let returnType: AST.TypeNode | undefined;
          if (host.match(":")) {
            returnType = host.parseType();
          }

          // Parse method body
          const body = host.parseBlock();

          const member: any = {
            kind: "Method",
            name: methodName,
            params,
            type: returnType,
            body,
            async: true,
            genericParams,
            span: host.createSpan(memberStart, host.current - 1)
          };
          if (memberDecorators.length > 0) {
            member.decorators = memberDecorators;
          }
          members.push(member);
          continue;
        }

        // If not followed by function keyword or identifier, restore position
        host.current = memberStart;
      }

      // Handle constructor
      if (host.peek().value === "constructor") {
        const name = host.parseIdentifier();

        // Constructor with parentheses
        if (host.check("(")) {
          const params = Functions.parseParameterList(host as any);

          // Constructor body
          const body = host.parseBlock();

          members.push({
            kind: "Constructor",
            params,
            body,
            span: host.createSpan(memberStart, host.current - 1)
          } as any);
          continue;
        }
      }

      // Handle TypeScript-style getters and setters
      // But only if get/set is not followed immediately by ( (then it's a method name)
      if ((host.peek().value === "get" || host.peek().value === "set") &&
          host.peekNext()?.value !== "(") {
        const isGetter = host.peek().value === "get";
        host.advance(); // consume get/set

        // Parse the accessor name
        const accessorName = host.parseIdentifier();

        // Parse parameters (setters have one parameter)
        const params = Functions.parseParameterList(host as any);

        // Parse return type if present
        let returnType: AST.TypeNode | undefined;
        if (host.match(":")) {
          returnType = host.parseType();
        }

        // Parse the body
        const body = host.parseBlock();

        const accessorMember: any = {
          kind: isGetter ? "Getter" : "Setter",
          name: accessorName,
          params,
          type: returnType,
          body,
          span: host.createSpan(memberStart, host.current - 1)
        };

        if (memberDecorators.length > 0) {
          accessorMember.decorators = memberDecorators;
        }

        members.push(accessorMember);
        continue;
      }

      // Collect all modifiers (known and unknown)
      let visibility: "public" | "private" | "protected" | undefined;
      let isStatic = false;
      let isReadonly = false;
      let unknownModifiers: string[] = [];

      // Keep collecting modifiers until we hit something that's definitely not a modifier
      while (host.peek().type === TokenType.Identifier || host.peek().type === TokenType.Keyword) {
        const token = host.peek();
        const value = token.value;

        // Check for known modifiers
        if (value === "public" || value === "private" || value === "protected") {
          visibility = value as any;
          host.advance();
        } else if (value === "static") {
          isStatic = true;
          host.advance();
        } else if (value === "readonly") {
          isReadonly = true;
          host.advance();
        } else if (value === "final" || value === "abstract" || value === "synchronized" ||
                   value === "native" || value === "volatile" || value === "transient" ||
                   value === "override") {
          unknownModifiers.push(value);
          host.advance();
        } else if (value === "async" || value === "const") {
          // These are handled elsewhere, stop collecting modifiers
          break;
        } else {
          // Check if this looks like it could be a modifier or a member name
          // If the next token is another identifier or a known type starter, it might be a modifier
          const next = host.peekNext();
          if (next && (next.type === TokenType.Identifier ||
                      next.value === ":" || next.value === "(" || next.value === "{" ||
                      next.value === "<" || next.value === "[")) {
            // Could be a modifier if followed by another identifier/type
            // or could be the member name if followed by : ( { < [
            if (next.type === TokenType.Identifier) {
              // Before treating as modifier, check for C# property pattern: type name { get/set }
              const twoAhead = host.peekAt(2);
              const threeAhead = host.peekAt(3);
              if (twoAhead?.value === "{" &&
                  (threeAhead?.value === "get" || threeAhead?.value === "set")) {
                // This is a type name, not a modifier - leave for C# property detection
                break;
              }
              // Likely a modifier (e.g., "volatile x")
              unknownModifiers.push(value);
              host.advance();
            } else {
              // Likely the member name, stop collecting modifiers
              break;
            }
          } else {
            // Probably the member name, stop collecting
            break;
          }
        }
      }

      // ES2022 static initializer block: static { ... }
      if (isStatic && host.check("{")) {
        const body = host.parseBlock();
        members.push({
          kind: "Method",
          name: { kind: "Identifier", name: "__static_init__", span: body.span } as AST.Identifier,
          params: [],
          body,
          static: true,
          span: host.createSpan(memberStart, host.current - 1)
        } as any);
        continue;
      }

      // Handle C#-style operator overloads after modifiers: public static Vec operator +(...)
      // At this point modifiers have been consumed; check for optional return type + operator keyword
      if (host.peek().value === "operator" && host.peekNext()?.type === TokenType.Operator) {
        host.advance(); // consume 'operator'
        const opToken = host.advance(); // consume the operator symbol
        const opName: AST.Identifier = {
          kind: "Identifier",
          name: "operator" + opToken.value,
          span: host.createSpanFrom(opToken)
        };
        const params = Functions.parseParameterList(host as any);
        if (host.peek().value === "const") host.advance();
        const body = host.parseBlock();
        const member: any = {
          kind: "Method",
          name: opName,
          params,
          body,
          static: isStatic,
          visibility,
          span: host.createSpan(memberStart, host.current - 1)
        };
        members.push(member);
        continue;
      }

      // Handle property declarations and methods
      // Allow keywords as method names (e.g., 'match' can be a method name)
      // Process any identifier or keyword as a potential field/method name
      if (host.peek().type === TokenType.Identifier || host.peek().type === TokenType.Keyword) {
        // Check if this might be a C# property with type first: public Type Name { get; set; }
        // We need to look ahead to distinguish between:
        // - public string Title { get; set; }  (C# property)
        // - public Title { ... }  (field/method named Title)

        let fieldType: AST.TypeNode | undefined;
        let memberName: AST.Identifier;

        // Save position for potential backtracking
        const checkpoint = host.current;

        // Try to parse as type + name pattern
        if (visibility) {
          // After visibility modifier, check if next two tokens could be type + name
          const firstToken = host.peek();
          const secondToken = host.peekNext();

          if (firstToken && secondToken &&
              (firstToken.type === TokenType.Identifier || firstToken.type === TokenType.Keyword) &&
              secondToken.type === TokenType.Identifier &&
              host.peekAt(2)?.value === "{") {
            // Check if the third token is { and fourth is "get" or "set"
            const thirdToken = host.peekAt(2);
            const fourthToken = host.peekAt(3);

            if (thirdToken?.value === "{" &&
                (fourthToken?.value === "get" || fourthToken?.value === "set")) {
              // This looks like a C# property: type name { get/set; }
              // Parse the type
              fieldType = host.parseType();
              // Parse the name
              memberName = host.parseIdentifier();

              // Now handle the { get; set; } part
              if (host.match("{")) {
                // Parse property accessors
                let getter: AST.PropertyAccessor | undefined;
                let setter: AST.PropertyAccessor | undefined;

                while (!host.check("}") && !host.isAtEnd()) {
                  const accessorStart = host.current;
                  let accessorVisibility: "public" | "private" | "protected" | undefined;

                  // Check for visibility modifier on accessor
                  if (host.match("public", "private", "protected")) {
                    accessorVisibility = host.previous()!.value as any;
                  }

                  if (host.match("get")) {
                    // Parse getter
                    if (host.match(";")) {
                      // Auto-property getter
                      getter = {
                        visibility: accessorVisibility,
                        span: host.createSpan(accessorStart, host.current - 1)
                      };
                    } else if (host.check("{")) {
                      // Getter with body
                      const body = host.parseBlock();
                      getter = {
                        visibility: accessorVisibility,
                        body,
                        span: host.createSpan(accessorStart, host.current - 1)
                      };
                    }
                  } else if (host.match("set")) {
                    // Parse setter
                    if (host.match(";")) {
                      // Auto-property setter
                      setter = {
                        visibility: accessorVisibility,
                        span: host.createSpan(accessorStart, host.current - 1)
                      };
                    } else if (host.check("{")) {
                      // Setter with body
                      const body = host.parseBlock();
                      setter = {
                        visibility: accessorVisibility,
                        body,
                        span: host.createSpan(accessorStart, host.current - 1)
                      };
                    }
                  } else if (!accessorVisibility) {
                    // Skip unexpected tokens (but we already consumed visibility)
                    host.advance();
                  }
                }
                host.consume("}", "Expected '}' after property accessors");

                // Create a property with accessors
                members.push({
                  kind: "Property",
                  name: memberName,
                  type: fieldType,
                  getter,
                  setter,
                  span: host.createSpan(memberStart, host.current - 1)
                } as any);
                continue;
              } else {
                // Not a C# property, parse normally
                host.current = checkpoint;
              }
            }
          }
        }

        // Java/C-style: ReturnType methodName(...) or Type fieldName = ...
        // Detect: current is identifier/keyword, next is also identifier, and after that is ( or = or ; or ,
        // This means current is a type and next is the actual name.
        if (!fieldType) {
          const cur = host.peek();
          const nxt = host.peekNext();
          if ((cur.type === TokenType.Identifier || cur.type === TokenType.Keyword) && nxt) {
            // Check if current token could be a return type:
            // pattern: Type Name( or Type Name = or Type Name ; or Type Name, or Type[] Name
            // Also handle generic types: Type<X> Name(
            let namePos = host.current + 1;
            // Skip generic args: Type<X, Y>
            if (nxt.value === "<") {
              let depth = 1;
              namePos = host.current + 2;
              while (namePos < host.tokens.length && depth > 0) {
                if (host.tokens[namePos].value === "<") depth++;
                else if (host.tokens[namePos].value === ">") depth--;
                namePos++;
              }
            }
            // Skip array brackets: Type[] or Type[][]
            while (namePos + 1 < host.tokens.length &&
                   host.tokens[namePos]?.value === "[" && host.tokens[namePos + 1]?.value === "]") {
              namePos += 2;
            }
            const possibleName = host.tokens[namePos];
            const afterName = host.tokens[namePos + 1];
            if (possibleName && (possibleName.type === TokenType.Identifier || possibleName.type === TokenType.Keyword) &&
                afterName && (afterName.value === "(" || afterName.value === "=" || afterName.value === ";" || afterName.value === ",")) {
              // Parse the type
              fieldType = host.parseType();
            }
          }
        }

        // Parse method/field name - allow keywords as identifiers in this context
        const nameToken = host.advance();
        memberName = {
          kind: "Identifier",
          name: nameToken.value,
          span: host.createSpanFrom(nameToken)
        };

        // Check for generic parameters on methods: methodName<T, U>() or methodName<K extends T>()
        let genericParams: AST.Identifier[] | undefined;
        if (host.check("<") && !host.check("<=") && !host.check("<<") && !host.check("<-")) {
          genericParams = host.attempt(() => {
            host.advance(); // consume <
            const params: AST.Identifier[] = [];
            do {
              params.push(host.parseIdentifier());
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
            host.consume(">", "Expected '>' after generic parameters");
            if (!host.check("(")) return null;
            return params;
          }) ?? undefined;
        }

        // Method with parentheses: methodName(params): returnType { body }
        if (host.check("(")) {
          const params = Functions.parseParameterList(host as any);

          // Java throws clause: method(params) throws Exception, IOException { }
          if (host.peek().value === "throws") {
            host.advance();
            do {
              host.parseType();
            } while (host.match(","));
          }

          // Optional return type
          let returnType: AST.TypeNode | undefined;
          if (host.match(":")) {
            try {
              returnType = host.parseType();
            } catch (error) {
              // If parsing the return type fails, skip to the opening brace
              if (error instanceof ParseError) {
                host.errors.push(error);
                // Skip to the opening brace of the method body
                while (!host.isAtEnd() && !host.check("{") && !host.check("}")) {
                  host.advance();
                }
              } else {
                throw error;
              }
            }
          }

          // Method body - handle both block and arrow function
          // But first check if this is just a signature (ends with semicolon)
          let body: AST.Block | undefined;

          if (host.check(";") || (host.checkSemicolon() && !host.check("{"))) {
            // Method signature without implementation
            host.consumeSemicolon();
            body = undefined;
          } else {
            try {
              if (host.match("=>")) {
                // Arrow function body
                const expr = host.parseExpression();
                body = {
                  kind: "Block",
                  statements: [{
                    kind: "Return",
                    values: [expr],
                    span: expr.span
                  }],
                  span: expr.span
                };
              } else {
                body = host.parseBlock();
              }
            } catch (error) {
              // If parsing the method body fails, create an empty block
              // and try to skip to the closing brace
              if (error instanceof ParseError) {
              host.errors.push(error);

              // Skip to the matching closing brace
              let depth = 1; // We're inside the method body
              while (depth > 0 && !host.isAtEnd()) {
                if (host.peek().value === "{") depth++;
                else if (host.peek().value === "}") {
                  depth--;
                  if (depth === 0) {
                    host.advance(); // consume the closing }
                    break;
                  }
                }
                host.advance();
              }

              // Skip any trailing semicolons or virtual semicolons after the method
              while (host.check(";") || host.peek().virtualSemi) {
                host.advance();
              }

              body = {
                kind: "Block",
                statements: [],
                span: host.createSpan(memberStart, host.current - 1)
              };
            } else {
                throw error;
              }
            }
          }

          const methodMember: any = {
            kind: "Method",
            name: memberName,
            params,
            type: returnType,
            body,
            genericParams,
            span: host.createSpan(memberStart, host.current - 1)
          };
          if (visibility) methodMember.visibility = visibility;
          if (isStatic) methodMember.static = isStatic;
          if (isReadonly) methodMember.readonly = isReadonly;
          if (memberDecorators.length > 0) {
            methodMember.decorators = memberDecorators;
          }
          if (unknownModifiers.length > 0) {
            methodMember.unknownModifiers = unknownModifiers;
          }
          members.push(methodMember);
          continue;
        }

        // Short declaration: name := value
        if (host.match(":=")) {
          const value = host.parseExpression();
          const member: any = {
            kind: "Field",
            name: memberName,
            value,
            span: host.createSpan(memberStart, host.current - 1)
          };
          if (memberDecorators.length > 0) {
            member.decorators = memberDecorators;
          }
          if (unknownModifiers.length > 0) {
            member.unknownModifiers = unknownModifiers;
          }
          members.push(member);
          continue;
        }

        // Type annotation: name: Type = value or name: Type;
        if (host.match(":")) {
          const type = host.parseType();
          let value: AST.Expr | undefined;
          if (host.match("=")) {
            value = host.parseExpression();
          }

          // Check if this might be a method signature without implementation
          if (host.check("(")) {
            // It's actually a method signature like: methodName: ReturnType(params)
            // But this is an unusual syntax - normally it would be methodName(params): ReturnType
            // For now, treat it as a field with a function type
            // Note: This might need adjustment based on language spec
          }

          // Create the field member
          const member: any = {
            kind: "Field",
            name: memberName,
            type,
            value,
            span: host.createSpan(memberStart, host.current - 1)
          };
          if (visibility) member.visibility = visibility;
          if (isStatic) member.static = isStatic;
          if (isReadonly) member.readonly = isReadonly;
          if (memberDecorators.length > 0) {
            member.decorators = memberDecorators;
          }
          if (unknownModifiers.length > 0) {
            member.unknownModifiers = unknownModifiers;
          }
          members.push(member);
          continue;
        }

        // Simple assignment: name = value
        if (host.match("=")) {
          const value = host.parseExpression();
          const member: any = {
            kind: "Field",
            name: memberName,
            value,
            span: host.createSpan(memberStart, host.current - 1)
          };
          if (memberDecorators.length > 0) {
            member.decorators = memberDecorators;
          }
          if (unknownModifiers.length > 0) {
            member.unknownModifiers = unknownModifiers;
          }
          members.push(member);
          continue;
        }

        // Field without value
        const fieldMember: any = {
          kind: "Field",
          name: memberName,
          span: host.createSpan(memberStart, host.current - 1)
        };
        if (memberDecorators.length > 0) {
          fieldMember.decorators = memberDecorators;
        }
        members.push(fieldMember);

        // Consume semicolon if present
        host.consumeSemicolon();
      } else {
        // Unknown member type - skip this token
        host.advance();
      }

    } catch (error) {
      if (error instanceof ParseError) {
        host.errors.push(error);
        // More aggressive error recovery - skip to next potential member
        // Look for visibility modifiers, method/field declarations, or closing brace
        let braceDepth = 0;
        while (!host.isAtEnd() && !host.check("}")) {
          const token = host.peek();

          // Track brace depth to skip nested blocks
          if (token.value === "{") {
            braceDepth++;
            host.advance();
            continue;
          } else if (token.value === "}" && braceDepth > 0) {
            braceDepth--;
            host.advance();
            continue;
          }

          // Only look for next member at brace depth 0
          if (braceDepth === 0) {
            // Check if we've reached what looks like the next member
            if (token.value === "public" || token.value === "private" ||
                token.value === "protected" || token.value === "static" ||
                token.value === "readonly" || token.value === "async" ||
                token.value === "constructor" || token.value === "override" ||
                token.value === "abstract" || token.value === "get" ||
                token.value === "set" || token.value === "declare") {
              // Found a potential next member
              break;
            }

            // Also check if we see "private methodName()" pattern
            if (token.value === "private" || token.value === "public" ||
                token.value === "protected") {
              // Look ahead one token
              const savedPos = host.current;
              host.advance();
              const next = host.peek();
              host.current = savedPos; // Restore position

              if (next?.type === TokenType.Identifier) {
                break; // This is likely a member declaration
              }
            }
          }

          // Also break if we see an identifier after a newline/semicolon
          // (could be a method/field without visibility modifier)
          if (host.checkSemicolon() || token.virtualSemi) {
            host.advance(); // consume the semicolon
            if (host.peek().type === TokenType.Identifier && !host.isAtEnd()) {
              break; // Next token could be a member
            }
          } else {
            host.advance();
          }
        }
      } else {
        throw error;
      }
    }
  }

  // Only consume closing brace for non-Python/non-Ruby style
  if (!isPythonStyle && !isRubyStyle) {
    host.consume("}", "Expected '}' after class body");
  }

  // Merge withTypes into implementsTypes since AST doesn't have separate field
  if (withTypes && withTypes.length > 0) {
    if (!implementsTypes) {
      implementsTypes = [];
    }
    implementsTypes.push(...withTypes);
  }

  const classDecl: AST.ClassDecl = {
    kind: "ClassDecl",
    name,
    typeParams,
    genericParams: typeParams, // Provide both for compatibility
    extends: extendsType,
    implements: implementsTypes,
    members,
    span: host.createSpan(start, host.current - 1)
  };

  // Add decorators if provided
  if (decorators && decorators.length > 0) {
    classDecl.decorators = decorators.map(expr => ({
      kind: "Decorator" as const,
      name: expr.kind === "Identifier" ? expr :
            expr.kind === "Call" && expr.callee.kind === "Identifier" ? expr.callee :
            { kind: "Identifier", name: "unknown", span: expr.span } as AST.Identifier,
      expression: expr,
      args: expr.kind === "Call" ? expr.args : undefined,
      span: expr.span
    }));
  }

  return classDecl;
}

export function parseInterfaceDecl(host: ClassHost): AST.InterfaceDecl {
  const start = host.current - 1;
  const name = host.parseIdentifier();

  // Parse type parameters
  let typeParams: AST.Identifier[] | undefined;
  if (host.match("<")) {
    typeParams = [];
    do {
      typeParams.push(host.parseIdentifier());
      // Skip constraints: extends Type, : Type
      if (host.match("extends", "super")) {
        host.parseType();
      } else if (host.check(":") && !host.check("::")) {
        host.advance();
        host.parseType();
        while (host.match("+")) host.parseType();
      }
      // Skip default type: = DefaultType
      if (host.match("=")) {
        host.parseType();
      }
    } while (host.match(","));
    host.consume(">", "Expected '>' after type parameters");
  }

  // Parse extends clause
  let extendsTypes: AST.TypeNode[] | undefined;
  if (host.match("extends")) {
    extendsTypes = [];
    do {
      extendsTypes.push(host.parseType());
    } while (host.match(","));
  }

  // Parse interface body
  host.consume("{", "Expected '{' before interface body");
  const members: AST.InterfaceMember[] = [];

  while (!host.check("}") && !host.isAtEnd()) {
    // Skip virtual semicolons and commas
    while (host.peek().virtualSemi || host.check(",")) {
      host.advance();
    }

    if (host.check("}")) break;

    // Parse interface member
    const memberStart = host.current;

    // Skip modifier keywords (async, static, readonly, etc.) before member name
    // But stop if the keyword IS the member name (e.g., async?: boolean)
    while (host.peek().type === TokenType.Keyword &&
           (host.peek().value === "async" || host.peek().value === "static" ||
            host.peek().value === "readonly" || host.peek().value === "abstract" ||
            host.peek().value === "fn" || host.peek().value === "def" ||
            host.peek().value === "fun" || host.peek().value === "func")) {
      // If followed by ? or : or (, this keyword is the member name, not a modifier
      const next = host.peekNext();
      if (next && (next.value === "?" || next.value === ":" || next.value === "(")) break;
      host.advance();
    }

    // Parse member name (allow keywords as identifiers here)
    let memberName: AST.Identifier;
    if (host.peek().type === TokenType.Keyword) {
      const kw = host.advance();
      memberName = { kind: "Identifier", name: kw.value, span: host.createSpanFrom(kw) };
    } else {
      memberName = host.parseIdentifier();
    }

    // Check for generic parameters on methods
    let genericParams: AST.Identifier[] | undefined;
    if (host.check("<")) {
      host.advance(); // consume <
      genericParams = [];
      do {
        genericParams.push(host.parseIdentifier());
        if (host.match("extends", "super")) {
          host.parseType();
        } else if (host.check(":") && !host.check("::")) {
          host.advance();
          host.parseType();
          while (host.match("+")) host.parseType();
        }
      } while (host.match(","));
      host.consume(">", "Expected '>' after generic parameters");
    }

    // Check if it's a method (has parentheses) or property
    if (host.check("(")) {
      // It's a method signature
      const params = Functions.parseParameterList(host as any);

      // Parse return type — but not if ':' is followed by a statement keyword (Python block)
      let returnType: AST.TypeNode | undefined;
      if (host.check(":") && host.peekNext()?.type === TokenType.Keyword &&
          (host.peekNext()?.value === "pass" || host.peekNext()?.value === "return" ||
           host.peekNext()?.value === "break" || host.peekNext()?.value === "continue")) {
        // Python-style colon block — skip the colon and block
        host.advance(); // consume ':'
        // Just skip the body statement
        while (host.peek().virtualSemi) host.advance();
        if (!host.check("}")) host.advance(); // skip the keyword (pass, etc.)
      } else if (host.match(":")) {
        returnType = host.parseType();
      } else if (host.match("->")) {
        returnType = host.parseType();
      }

      // Store as a method member with full signature
      members.push({
        name: memberName,
        kind: "Method",
        params,
        returnType,
        genericParams,
        optional: false,
        span: host.createSpan(memberStart, host.current - 1)
      });
    } else {
      // It's a property
      const optional = host.match("?");

      // Expect colon for type annotation
      host.consume(":", "Expected ':' after member name");

      // Parse the type
      const memberType = host.parseType();

      members.push({
        name: memberName,
        kind: "Property",
        type: memberType,
        optional,
        span: host.createSpan(memberStart, host.current - 1)
      });
    }

    // Skip optional comma or semicolon
    host.match(",") || host.match(";");
  }

  host.consume("}", "Expected '}' after interface body");

  return {
    kind: "InterfaceDecl",
    name,
    typeParams,
    extends: extendsTypes,
    members,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseImplBlock(host: ClassHost): AST.ImplDecl {
  // Parse Rust-style impl blocks
  // impl<T> Container<T> where T: Display { ... }
  // impl<T> Display for Container<T> where T: Display { ... }
  const start = host.current;
  host.advance(); // consume 'impl'

  // Parse generic parameters if present
  let typeParams: AST.Identifier[] | undefined;
  if (host.match("<")) {
    typeParams = [];
    do {
      typeParams.push(host.parseIdentifier());
      if (host.match("extends", "super")) {
        host.parseType();
      } else if (host.check(":") && !host.check("::")) {
        host.advance();
        host.parseType();
        while (host.match("+")) host.parseType();
      }
    } while (host.match(","));
    host.consume(">", "Expected '>' after generic parameters");
  }

  // Parse either "Type" or "Trait for Type"
  let trait: AST.TypeNode | undefined;
  let type: AST.TypeNode;

  // Parse the first type/identifier
  const firstType = host.parseType();

  // Check if this is "Trait for Type" pattern
  if (host.match("for")) {
    trait = firstType;
    type = host.parseType();
  } else {
    // Just "Type" without trait
    type = firstType;
  }

  // Parse where clause if present
  let whereClause: AST.WhereClause | undefined;
  if (host.peek().value === "where") {
    whereClause = parseWhereClause(host);
  }

  // Parse impl body — skip vsemis before opening brace
  while (host.peek().virtualSemi) host.advance();
  host.consume("{", "Expected '{' before impl body");
  const members: AST.ImplMember[] = [];

  while (!host.check("}") && !host.isAtEnd()) {
    // Skip semicolons and virtual semicolons
    while (host.check(";") || host.peek().virtualSemi) {
      host.advance();
    }

    if (host.check("}")) break;

    const memberStart = host.current;

    // Parse visibility modifiers
    let visibility: "public" | "private" | "protected" | undefined;
    if (host.match("pub", "public")) {
      visibility = "public";
    } else if (host.match("private", "priv")) {
      visibility = "private";
    } else if (host.match("protected")) {
      visibility = "protected";
    }

    // Parse impl members (methods, associated types, associated constants)
    if (host.match("const")) {
      // Check if this is "const fn" (const function)
      if (host.peek().value === "fn") {
        host.advance(); // consume 'fn'
        const func = Functions.parseFuncDecl(host as any, false, false, false);
        members.push({
          kind: "Method",
          name: func.name!,
          params: func.params,
          type: func.returnType,
          body: func.body,
          isConst: true,
          visibility,
          span: func.span
        });
      } else {
        // Associated constant: const SIZE: usize = 10;
        const name = host.parseIdentifier();
        let constType: AST.TypeNode | undefined;
        if (host.match(":")) {
          constType = host.parseType();
        }
        let value: AST.Expr | undefined;
        if (host.match("=")) {
          value = host.parseExpression();
        }
        host.consumeSemicolon();
        members.push({
          kind: "AssociatedConst",
          name,
          type: constType,
          value,
          visibility,
          span: host.createSpan(memberStart, host.current - 1)
        });
      }
    } else if (host.match("fn", "fun", "func", "function")) {
      const isGenerator = host.previous()?.value === "function" && host.match("*");
      const func = Functions.parseFuncDecl(host as any, false, false, isGenerator);
      members.push({
        kind: "Method",
        name: func.name!,
        params: func.params,
        type: func.returnType,
        body: func.body,
        visibility,
        span: func.span
      });
    } else if (host.match("type")) {
      // Associated type: type Item = T;
      const name = host.parseIdentifier();
      let assocType: AST.TypeNode | undefined;
      if (host.match("=")) {
        assocType = host.parseType();
      }
      host.consumeSemicolon();
      members.push({
        kind: "AssociatedType",
        name,
        type: assocType,
        visibility,
        span: host.createSpan(memberStart, host.current - 1)
      });
    } else if (host.peek().type === TokenType.Identifier) {
      // Try to parse as a method without fn keyword or as a field
      const name = host.parseIdentifier();
      if (host.check("(")) {
        // Method without fn keyword
        const params = Functions.parseParameterList(host as any);
        let returnType: AST.TypeNode | undefined;
        if (host.match("->", ":")) {
          returnType = host.parseType();
        }
        const body = host.parseBlock();
        members.push({
          kind: "Method",
          name,
          params,
          type: returnType,
          body,
          visibility,
          span: host.createSpan(memberStart, host.current - 1)
        });
      } else if (host.match(":")) {
        // Field with type annotation: field: Type = value
        const fieldType = host.parseType();
        let value: AST.Expr | undefined;
        if (host.match("=")) {
          value = host.parseExpression();
        }
        host.consumeSemicolon();
        members.push({
          kind: "Field",
          name,
          type: fieldType,
          value,
          visibility,
          span: host.createSpan(memberStart, host.current - 1)
        });
      } else {
        // Unknown member - store as unknown to preserve data
        // This could be macro invocations or other language features
        const tokens: Token[] = [];
        while (!host.checkSemicolon() && !host.check("}") && !host.isAtEnd()) {
          tokens.push(host.advance());
        }
        host.consumeSemicolon();
        members.push({
          kind: "Unknown",
          name,
          tokens,
          visibility,
          span: host.createSpan(memberStart, host.current - 1)
        });
      }
    } else {
      // Store unknown tokens as unknown member
      const tokens: Token[] = [];
      const unknownStart = host.current;
      while (!host.checkSemicolon() && !host.check("}") && !host.isAtEnd()) {
        tokens.push(host.advance());
      }
      host.consumeSemicolon();
      if (tokens.length > 0) {
        members.push({
          kind: "Unknown",
          tokens,
          visibility,
          span: host.createSpan(unknownStart, host.current - 1)
        });
      }
    }
  }

  host.consume("}", "Expected '}' after impl body");

  // Return as an ImplDecl
  return {
    kind: "ImplDecl",
    type,
    trait,
    typeParams,
    whereClause,
    members,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseWhereClause(host: ClassHost): AST.WhereClause {
  const start = host.current;
  host.advance(); // consume 'where'

  const constraints: AST.WhereConstraint[] = [];

  // Parse comma-separated constraints
  do {
    const constraintStart = host.current;

    // Parse the type being constrained (e.g., T or T::Item)
    const type = host.parseType();

    // Parse the bounds (e.g., : Display + Debug)
    const bounds: AST.TypeNode[] = [];
    if (host.match(":")) {
      // Parse first bound
      bounds.push(host.parseType());

      // Parse additional bounds separated by +
      while (host.match("+")) {
        bounds.push(host.parseType());
      }
    }

    constraints.push({
      type,
      bounds,
      span: host.createSpan(constraintStart, host.current - 1)
    });
  } while (host.match(","));

  return {
    constraints,
    span: host.createSpan(start, host.current - 1)
  };
}

export function parseEnumDecl(host: ClassHost): AST.EnumDecl {
  const start = host.current - 1;
  const name = host.parseIdentifier();

  host.consume("{", "Expected '{' before enum body");
  const members: AST.EnumMember[] = [];
  let hasPayloadVariants = false;

  while (!host.check("}") && !host.isAtEnd()) {
    // Skip virtual semicolons in enum body
    while (host.peek().virtualSemi) {
      host.advance();
    }

    // Check for closing brace after skipping virtual semicolons
    if (host.check("}")) {
      break;
    }

    const memberName = host.parseIdentifier();
    let value: AST.Expr | undefined;

    // Rust data-carrying variants: Name { field: Type, ... } or Name(Type, ...)
    // Payload tokens are skipped; the enum span preserves the raw source.
    if (host.check("{") || host.check("(")) {
      const open = host.peek().value;
      const close = open === "{" ? "}" : ")";
      host.advance(); // consume opening delimiter
      let depth = 1;
      while (depth > 0 && !host.isAtEnd()) {
        if (host.check(open)) depth++;
        else if (host.check(close)) depth--;
        host.advance();
      }
      hasPayloadVariants = true;
    } else if (host.match("=")) {
      value = host.parseAssignmentExpression();
    }

    members.push({
      name: memberName,
      value,
      span: host.createSpanFrom(memberName)
    });

    // Skip trailing virtual semicolons
    while (host.peek().virtualSemi) {
      host.advance();
    }

    if (!host.match(",")) {
      break;
    }
  }

  host.consume("}", "Expected '}' after enum body");

  return {
    kind: "EnumDecl",
    name,
    members,
    ...(hasPayloadVariants ? { payloadVariants: true } : {}),
    span: host.createSpan(start, host.current - 1)
  };
}
