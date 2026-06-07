/**
 * Type Parselet — Extracted from Parser (Chunk 3).
 *
 * All type parsing: simple types, generic types, union types, function types,
 * nullable types, channel types, Go type annotations, object type literals,
 * tuple types, type predicates, keyof, impl/dyn traits.
 *
 * Depends on a narrow TypeHost interface — no direct Parser import.
 */

import { Token, TokenType } from '../lexer';
import * as AST from '../ast';

export interface TypeHost {
  tokens: Token[];
  current: number;

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

  parseIdentifier(): AST.Identifier;
}

// ============ Main Entry Points ============

export function parseType(host: TypeHost): AST.TypeNode {
  // Check for type predicates: paramName is Type
  if (host.peek().type === TokenType.Identifier) {
    const checkpoint = host.current;
    const paramName = host.advance();

    if (host.peek().value === "is") {
      host.advance(); // consume 'is'
      const predicateType = parseSimpleType(host);

      return {
        kind: "PredicateType",
        param: {
          kind: "Identifier",
          name: paramName.value,
          span: host.createSpan(checkpoint, checkpoint)
        },
        type: predicateType,
        span: host.createSpan(checkpoint, host.current - 1)
      } as any;
    } else {
      host.current = checkpoint;
    }
  }

  // Leading | for union types: type X = | A | B
  if (host.check("|")) {
    host.advance(); // consume leading |
    const types: AST.TypeNode[] = [];
    do {
      types.push(parseSimpleType(host));
    } while (host.match("|"));
    return {
      kind: "UnionType",
      types,
      span: host.createSpan(host.current - 1, host.current - 1)
    } as AST.TypeNode;
  }

  let type = parseSimpleType(host);

  // Handle array type suffix: Type[] or indexed access Type["property"]
  while (host.check("[")) {
    const checkpoint = host.current;
    host.advance(); // consume [

    if (host.check("]")) {
      host.advance(); // consume ]
      type = {
        kind: "GenericType",
        base: {
          kind: "Identifier",
          name: "Array",
          span: host.createSpan(checkpoint, host.current - 1)
        } as AST.Identifier,
        args: [type],
        span: host.createSpanFrom(type)
      };
    } else if (host.peek().type === TokenType.StringLiteral) {
      const indexToken = host.advance();
      host.consume("]", "Expected ']' after indexed access property");

      type = {
        kind: "IndexedAccessType",
        object: type,
        index: indexToken.value,
        span: host.createSpanFrom(type)
      } as any;
    } else if (type.kind === "SimpleType" || type.kind === "GenericType") {
      // Python-style generic: Result[List[T], str]
      try {
        const args: AST.TypeNode[] = [];
        args.push(parseType(host));
        while (host.check(",")) {
          host.advance();
          args.push(parseType(host));
        }
        host.consume("]", "Expected ']' after type arguments");
        type = {
          kind: "GenericType",
          base: type.kind === "SimpleType" ? (type as any).id : (type as any).base,
          args,
          span: host.createSpanFrom(type)
        };
      } catch {
        host.current = checkpoint;
        break;
      }
    } else {
      host.current = checkpoint;
      break;
    }
  }

  // Handle nullable types
  if (host.match("?")) {
    type = {
      kind: "NullableType",
      inner: type,
      span: host.createSpanFrom(type)
    };
  }

  // Handle function types with -> (right-associative)
  if (host.check("->")) {
    host.advance(); // consume ->
    const ret = parseType(host);
    type = {
      kind: "FuncType",
      params: [{
        type: type,
        span: type.span
      }],
      ret,
      span: host.createSpanFrom(type)
    };
  }

  // Handle union types
  else if (host.match("|")) {
    const types: AST.TypeNode[] = [type];
    do {
      types.push(parseSimpleType(host));
    } while (host.match("|"));

    type = {
      kind: "UnionType",
      types,
      span: host.createSpanFrom(types[0])
    };
  }

  // Handle intersection types: Type & Type
  while (host.check("&") && host.peekAt(1)?.value !== "&") {
    host.advance(); // consume &
    const types: AST.TypeNode[] = [type];
    types.push(parseSimpleType(host));
    while (host.check("&") && host.peekAt(1)?.value !== "&") {
      host.advance();
      types.push(parseSimpleType(host));
    }
    type = {
      kind: "IntersectionType",
      types,
      span: host.createSpanFrom(types[0])
    } as any;
  }

  return type;
}

export function parseSimpleType(host: TypeHost): AST.TypeNode {
  const start = host.current;

  // Python ellipsis as type (e.g., tuple[int, ...])
  if (host.check("...")) {
    host.advance();
    return { kind: "SimpleType", id: { kind: "Identifier", name: "...", span: host.createSpan(start, host.current - 1) }, span: host.createSpan(start, host.current - 1) } as AST.TypeNode;
  }

  // Reference type prefix: &Type, &mut Type, &[T] (Rust)
  if (host.check("&") && host.peekNext()?.value !== "&") {
    host.advance(); // consume &
    const isMut = host.check("mut");
    if (isMut) host.advance();
    // &[T] — reference to slice
    if (host.check("[")) {
      host.advance(); // [
      const elem = parseSimpleType(host);
      host.consume("]", "Expected ']' after slice type");
      const prefix = isMut ? "&mut " : "&";
      return {
        kind: "GenericType",
        base: { kind: "Identifier", name: prefix + "[]", span: host.createSpan(start, host.current - 1) },
        args: [elem],
        span: host.createSpan(start, host.current - 1),
      } as AST.TypeNode;
    }
    const inner = parseSimpleType(host);
    const prefix = isMut ? "&mut " : "&";
    const innerName = inner.kind === "SimpleType" ? (inner as any).id.name : "";
    return {
      kind: "SimpleType",
      id: { kind: "Identifier", name: prefix + innerName, span: host.createSpan(start, host.current - 1) },
      span: host.createSpan(start, host.current - 1),
    } as any;
  }

  // Array type prefix: []Type
  if (host.check("[") && host.peekNext()?.value === "]") {
    host.advance(); // consume [
    host.advance(); // consume ]
    const elementType = parseSimpleType(host);

    return {
      kind: "GenericType",
      base: {
        kind: "Identifier",
        name: "Array",
        span: host.createSpan(start, start + 1)
      },
      args: [elementType],
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Tuple type: [T1, T2, ...]
  if (host.check("[") && host.peekNext()?.value !== "]") {
    host.advance(); // consume [
    const elements: AST.TypeNode[] = [];
    if (!host.check("]")) {
      elements.push(parseType(host));
      while (host.match(",")) {
        elements.push(parseType(host));
      }
    }
    host.consume("]", "Expected ']' after tuple type");
    return {
      kind: "TupleType",
      elements,
      span: host.createSpan(start, host.current - 1)
    } as any;
  }

  // String literal type
  if (host.peek().type === TokenType.StringLiteral) {
    const literal = host.advance();
    return {
      kind: "SimpleType",
      id: { kind: "Identifier", name: literal.value, span: host.createSpan(start, start) },
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Numeric literal type (e.g., version: 1)
  if (host.peek().type === TokenType.NumericLiteral) {
    const literal = host.advance();
    return {
      kind: "SimpleType",
      id: { kind: "Identifier", name: literal.value, span: host.createSpan(start, start) },
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Boolean literal type (true/false)
  if (host.peek().value === "true" || host.peek().value === "false") {
    const literal = host.advance();
    return {
      kind: "SimpleType",
      id: { kind: "Identifier", name: literal.value, span: host.createSpan(start, start) },
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Channel type: <-chan T or chan<- T
  if (host.match("<-")) {
    host.consume("chan", "Expected 'chan' after '<-'");
    let elementType: AST.TypeNode | undefined;
    if (host.peek().type === TokenType.Identifier || host.check("(")) {
      elementType = parseSimpleType(host);
    }
    return {
      kind: "ChanType",
      direction: "receive",
      elementType,
      span: host.createSpan(start, host.current - 1)
    };
  }

  if (host.peek().value === "chan") {
    host.advance(); // consume 'chan'
    if (host.match("<-")) {
      let elementType: AST.TypeNode | undefined;
      if (host.peek().type === TokenType.Identifier || host.check("(")) {
        elementType = parseSimpleType(host);
      }
      return {
        kind: "ChanType",
        direction: "send",
        elementType,
        span: host.createSpan(start, host.current - 1)
      };
    } else if (host.check("<")) {
      // Generic channel syntax: chan<T>
      host.advance(); // consume <
      const args: AST.TypeNode[] = [];
      args.push(parseType(host));

      while (host.match(",")) {
        args.push(parseType(host));
      }

      consumeClosingAngleBracket(host);

      return {
        kind: "GenericType",
        base: {
          kind: "Identifier",
          name: "chan",
          originalSpelling: "chan",
          span: host.createSpan(start, start)
        },
        args,
        span: host.createSpan(start, host.current - 1)
      };
    } else {
      // Bidirectional channel: chan T
      let elementType: AST.TypeNode | undefined;
      if (host.peek().type === TokenType.Identifier || host.check("(")) {
        elementType = parseSimpleType(host);
      }
      return {
        kind: "ChanType",
        direction: "both",
        elementType,
        span: host.createSpan(start, host.current - 1)
      };
    }
  }

  // Object type literal: { prop: type, ... }
  if (host.check("{")) {
    host.advance(); // consume {

    const properties: AST.ObjectTypeProperty[] = [];

    while (!host.check("}") && !host.isAtEnd()) {
      while (host.peek().virtualSemi) {
        host.advance();
      }

      if (host.check("}")) break;

      let readonly = false;
      if (host.match("readonly")) {
        readonly = true;
      }

      let name: string;
      if (host.check("[")) {
        // TypeScript mapped type: [K in T]: ValueType or [K in T as Name]: ValueType
        host.advance(); // consume [
        const parts: string[] = [];
        let depth = 1;
        while (depth > 0 && !host.isAtEnd()) {
          if (host.check("[")) depth++;
          else if (host.check("]")) { depth--; if (depth === 0) break; }
          parts.push(host.advance().value);
        }
        host.consume("]", "Expected ']' after mapped type key");
        name = `[${parts.join(' ')}]`;
      } else if (host.peek().type === TokenType.StringLiteral) {
        const token = host.advance();
        name = token.value.slice(1, -1);
      } else if (host.peek().type === TokenType.Keyword) {
        name = host.advance().value;
      } else {
        name = host.parseIdentifier().name;
      }

      let optional = false;
      if (host.match("?")) {
        optional = true;
      }

      // Skip generic params in method signatures: set<K extends T>(...)
      if (host.check("<")) {
        host.advance();
        let depth = 1;
        while (depth > 0 && !host.isAtEnd()) {
          if (host.check("<")) depth++;
          else if (host.check(">")) depth--;
          host.advance();
        }
      }

      // Skip method params: name(...): ReturnType
      if (host.check("(")) {
        host.advance();
        let depth = 1;
        while (depth > 0 && !host.isAtEnd()) {
          if (host.check("(")) depth++;
          else if (host.check(")")) depth--;
          host.advance();
        }
      }

      host.consume(":", "Expected ':' after property name in object type");
      const type = parseType(host);

      properties.push({
        name,
        type,
        optional,
        readonly
      });

      if (host.match(",", ";")) {
        // consumed
      }

      while (host.peek().virtualSemi) {
        host.advance();
      }
    }

    host.consume("}", "Expected '}' in object type literal");

    return {
      kind: "ObjectType",
      properties,
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Function type with parenthesized parameters or parenthesized type
  if (host.check("(")) {
    const checkpoint = host.current;
    host.advance(); // (

    // Skip to matching )
    let depth = 1;
    while (depth > 0 && !host.isAtEnd()) {
      if (host.check("(")) depth++;
      if (host.check(")")) depth--;
      host.advance();
    }

    const isFuncType = host.check("=>") || host.check("->");
    const arrow = host.check("=>") ? "=>" : "->";
    host.current = checkpoint;

    if (isFuncType) {
      host.advance(); // (
      const params: AST.FuncTypeParam[] = [];

      if (!host.check(")")) {
        do {
          const paramStart = host.current;
          let name: AST.Identifier | undefined;
          let optional = false;

          if (host.peek().type === TokenType.Identifier) {
            const next = host.peekNext();
            if (next?.value === ":" || next?.value === "?") {
              name = host.parseIdentifier();

              if (host.match("?")) {
                optional = true;
              }

              if (host.match(":")) {
                // Type annotation follows
              } else if (!optional) {
                throw host.error(host.peek(), "Expected ':' after parameter name");
              }
            }
          }

          const type = parseType(host);

          params.push({
            name,
            type,
            optional,
            span: host.createSpan(paramStart, host.current - 1)
          });
        } while (host.match(","));
      }

      host.consume(")", "Expected ')' in function type");
      host.consume(arrow, `Expected '${arrow}' in function type`);
      const ret = parseType(host);

      return {
        kind: "FuncType",
        params,
        ret,
        span: host.createSpan(start, host.current - 1)
      };
    } else {
      // Not a function type - parenthesized type or tuple type
      host.advance(); // consume '('

      if (host.check(")")) {
        host.advance();
        return {
          kind: "SimpleType",
          id: { kind: "Identifier", name: "()", span: host.createSpan(start, host.current - 1) },
          span: host.createSpan(start, host.current - 1)
        };
      }

      const firstType = parseType(host);

      if (host.match(",")) {
        const elements: AST.TypeNode[] = [firstType];
        do {
          elements.push(parseType(host));
        } while (host.match(","));

        host.consume(")", "Expected ')' after tuple type");

        const tupleStr = `(${elements.map(e => typeNodeToString(e)).join(", ")})`;

        return {
          kind: "SimpleType",
          id: { kind: "Identifier", name: tupleStr, span: host.createSpan(start, host.current - 1) },
          span: host.createSpan(start, host.current - 1)
        };
      } else {
        host.consume(")", "Expected ')' after parenthesized type");
        return firstType;
      }
    }
  }

  // Go map[K]V type
  if (host.check("map") && host.peekNext()?.value === "[") {
    host.advance(); // map
    host.advance(); // [
    const keyType = parseSimpleType(host);
    host.consume("]", "Expected ']' after map key type");
    const valType = parseSimpleType(host);
    return {
      kind: "GenericType",
      base: { kind: "Identifier", name: "map", span: host.createSpan(start, start) },
      args: [keyType, valType],
      span: host.createSpan(start, host.current - 1)
    } as AST.TypeNode;
  }

  // Go interface{} / struct{} as type
  if ((host.check("interface") || host.check("struct")) && host.peekNext()?.value === "{") {
    const kw = host.advance().value; // interface or struct
    host.advance(); // {
    host.consume("}", `Expected '}' after ${kw}{}`);
    return {
      kind: "SimpleType",
      id: { kind: "Identifier", name: `${kw}{}`, span: host.createSpan(start, host.current - 1) },
      span: host.createSpan(start, host.current - 1)
    } as any;
  }

  // Simple or generic type
  let id: AST.Identifier;
  const token = host.peek();
  if (token.type === TokenType.Keyword &&
      (token.value === "void" || token.value === "undefined" ||
       token.value === "number" || token.value === "boolean" ||
       token.value === "string" || token.value === "object" ||
       token.value === "any" || token.value === "never" ||
       token.value === "unknown" || token.value === "null" ||
       token.value === "this")) {
    host.advance();
    id = {
      kind: "Identifier",
      name: token.value,
      span: host.createSpanFrom(token)
    };
  } else {
    id = host.parseIdentifier();
  }

  // Handle qualified type names
  let qualifiedId = id;
  while (host.match(".")) {
    const member = host.parseIdentifier();
    qualifiedId = {
      kind: "Identifier",
      name: `${qualifiedId.name}.${member.name}`,
      span: host.createSpanFrom(qualifiedId)
    };
  }

  // Handle "keyof Type" pattern
  if (qualifiedId.name === "keyof" && (host.peek().type === TokenType.Identifier || host.peek().type === TokenType.Keyword)) {
    const innerType = parseSimpleType(host);
    return {
      kind: "SimpleType",
      id: { kind: "Identifier", name: "keyof", span: host.createSpan(start, host.current - 1) },
      inner: innerType,
      span: host.createSpan(start, host.current - 1)
    } as any;
  }

  // Handle "impl Trait" pattern (Rust-style)
  if (qualifiedId.name === "impl" && host.peek().type === TokenType.Identifier) {
    const traitType = parseSimpleType(host);
    return {
      kind: "ImplType",
      trait: traitType,
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Handle "dyn Trait" pattern (Rust-style trait objects)
  if (qualifiedId.name === "dyn" && host.peek().type === TokenType.Identifier) {
    const traitType = parseSimpleType(host);
    return {
      kind: "DynType",
      trait: traitType,
      span: host.createSpan(start, host.current - 1)
    };
  }

  // Special handling for chan<T> syntax
  if (qualifiedId.name === "chan") {
    if (host.check("<")) {
      host.advance(); // consume '<'
      const args: AST.TypeNode[] = [];
      args.push(parseType(host));

      while (host.match(",")) {
        args.push(parseType(host));
      }

      consumeClosingAngleBracket(host);

      return {
        kind: "GenericType",
        base: qualifiedId,
        args,
        span: host.createSpan(start, host.current - 1)
      };
    } else {
      return {
        kind: "ChanType",
        direction: "both",
        elementType: undefined,
        span: host.createSpan(start, host.current - 1)
      };
    }
  }

  // Check for generic arguments
  if (host.match("<") || host.match("[")) {
    const closeBracket = host.previous()?.value === "<" ? ">" : "]";
    const args: AST.TypeNode[] = [];

    if (!host.check(closeBracket)) {
      do {
        const checkpoint = host.current;
        const firstType = parseType(host);

        if (host.check("=") && firstType.kind === "SimpleType") {
          host.advance(); // consume '='
          const constraintType = parseType(host);
          args.push(constraintType);
        } else {
          args.push(firstType);
        }
      } while (host.match(","));
    }

    if (closeBracket === ">") {
      consumeClosingAngleBracket(host);
    } else {
      host.consume(closeBracket, `Expected '${closeBracket}'`);
    }

    return {
      kind: "GenericType",
      base: qualifiedId,
      args,
      span: host.createSpan(start, host.current - 1)
    };
  }

  return {
    kind: "SimpleType",
    id: qualifiedId,
    span: qualifiedId.span
  };
}

// ============ Go Type Annotations ============

export function parseGoTypeAnnotation(host: TypeHost): AST.TypeNode {
  const start = host.current;
  const mkId = (n: string, span: AST.Span): AST.Identifier => ({ kind: "Identifier", name: n, span });
  const mkSpan = () => host.createSpan(start, host.current - 1);

  // Pointer type: *Type
  if (host.match("*")) {
    const inner = parseGoTypeAnnotation(host);
    return { kind: "SimpleType", id: mkId("*" + (inner.kind === "SimpleType" ? (inner as any).id.name : ""), inner.span), span: inner.span } as any;
  }

  // Reference type: &Type, &[T], &mut Type (Rust)
  if (host.match("&")) {
    const isMut = host.check("mut");
    if (isMut) host.advance();
    const inner = parseGoTypeAnnotation(host);
    const prefix = isMut ? "&mut " : "&";
    const innerName = inner.kind === "SimpleType" ? (inner as any).id.name : "";
    return { kind: "SimpleType", id: mkId(prefix + innerName, inner.span), span: mkSpan() } as any;
  }

  // Slice type: []Type
  if (host.check("[") && host.peekAt(1)?.value === "]") {
    host.advance(); // [
    host.advance(); // ]
    const elem = parseGoTypeAnnotation(host);
    return { kind: "GenericType", base: mkId("[]", mkSpan()), args: [elem], span: mkSpan() } as AST.TypeNode;
  }

  // map[K]V
  if (host.check("map") && host.peekAt(1)?.value === "[") {
    host.advance(); // map
    host.advance(); // [
    const keyType = parseGoTypeAnnotation(host);
    host.consume("]", "Expected ']' after map key type");
    const valType = parseGoTypeAnnotation(host);
    return { kind: "GenericType", base: mkId("map", mkSpan()), args: [keyType, valType], span: mkSpan() } as AST.TypeNode;
  }

  // chan<- Type, <-chan Type, chan Type
  if (host.check("chan")) {
    host.advance(); // chan
    if (host.match("<-")) { /* chan<- Type (send-only) */ }
    const chanType = parseGoTypeAnnotation(host);
    return { kind: "ChanType", direction: "both", elementType: chanType, span: mkSpan() } as any;
  }
  if (host.check("<-") && host.peekAt(1)?.value === "chan") {
    host.advance(); // <-
    host.advance(); // chan
    const chanType = parseGoTypeAnnotation(host);
    return { kind: "ChanType", direction: "recv", elementType: chanType, span: mkSpan() } as any;
  }

  // interface{}
  if (host.check("interface") && host.peekAt(1)?.value === "{") {
    host.advance(); host.advance();
    host.consume("}", "Expected '}' after interface{}");
    return { kind: "SimpleType", id: mkId("interface{}", mkSpan()), span: mkSpan() } as AST.TypeNode;
  }

  // struct{}
  if (host.check("struct") && host.peekAt(1)?.value === "{") {
    host.advance(); host.advance();
    host.consume("}", "Expected '}' after struct{}");
    return { kind: "SimpleType", id: mkId("struct{}", mkSpan()), span: mkSpan() } as AST.TypeNode;
  }

  // Fall back to regular type parsing
  return parseType(host);
}

// ============ Helpers ============

export function isType(host: TypeHost): boolean {
  const token = host.peek();
  return token.type === TokenType.Identifier && (
    token.value === "any" || token.value === "never" ||
    token.value === "bool" || token.value === "bytes" ||
    token.value === "string" || token.value === "char" ||
    token.value === "bigint" || token.value === "i8" ||
    token.value === "i16" || token.value === "i32" ||
    token.value === "i64" || token.value === "u8" ||
    token.value === "u16" || token.value === "u32" ||
    token.value === "u64" || token.value === "f32" ||
    token.value === "f64" || token.value === "chan" ||
    token.type === TokenType.Identifier
  );
}

export function typeNodeToString(type: AST.TypeNode): string {
  switch (type.kind) {
    case "SimpleType":
      return type.id.name;
    case "ChanType":
      const prefix = type.direction === "receive" ? "<-chan" :
                    type.direction === "send" ? "chan<-" : "chan";
      return type.elementType ? `${prefix} ${typeNodeToString(type.elementType)}` : prefix;
    case "NullableType":
      return `${typeNodeToString(type.inner)}?`;
    case "UnionType":
      return type.types.map(t => typeNodeToString(t)).join(" | ");
    case "GenericType":
      return `${type.base.name}<${type.args.map(a => typeNodeToString(a)).join(", ")}>`;
    case "FuncType":
      return `(${type.params.map(p => {
        const name = p.name ? p.name.name + ": " : "";
        return name + typeNodeToString(p.type);
      }).join(", ")}) => ${typeNodeToString(type.ret)}`;
    case "ImplType":
      return `impl ${typeNodeToString(type.trait)}`;
    case "DynType":
      return `dyn ${typeNodeToString(type.trait)}`;
    default:
      return "unknown";
  }
}

// ============ Internal Helpers ============

/**
 * Consume a closing > bracket, handling >> and >>> token splitting.
 */
function consumeClosingAngleBracket(host: TypeHost): void {
  if (host.check(">>")) {
    const originalToken = host.tokens[host.current];
    host.tokens[host.current] = { ...originalToken, value: ">" };
    const syntheticToken = { ...originalToken, value: ">" };
    host.tokens.splice(host.current + 1, 0, syntheticToken);
    host.advance();
  } else if (host.check(">>>")) {
    const originalToken = host.tokens[host.current];
    host.tokens[host.current] = { ...originalToken, value: ">" };
    const syntheticToken = { ...originalToken, value: ">>" };
    host.tokens.splice(host.current + 1, 0, syntheticToken);
    host.advance();
  } else {
    const actualToken = host.peek();
    if (actualToken.value !== ">") {
      throw host.error(actualToken, `Expected '>' but got '${actualToken.value}'`);
    }
    host.consume(">", "Expected '>'");
  }
}
