/**
 * Type Lowering — AST.TypeNode → CanonicalType
 *
 * Maps surface-level type syntax from any language into the unified IR.
 * This is intentionally tolerant: if we can't fully resolve a type, we
 * fall back to `any` rather than rejecting the program.
 */

import * as AST from '../ast';
import * as C from './canonical';

type Runtime = "javascript" | "typescript" | "python" | "go" | "rust" | "java" | "csharp" | "ruby" | "bash";

/**
 * Lower an AST TypeNode to a CanonicalType, given the runtime context.
 */
export function lowerType(node: AST.TypeNode, runtime?: Runtime): C.CanonicalType {
  switch (node.kind) {
    case "SimpleType":
      return lowerSimpleType(node.id.name, undefined, runtime);

    case "NullableType":
      return C.option(lowerType(node.inner, runtime));

    case "UnionType":
      return lowerUnion(node.types.map(t => lowerType(t, runtime)));

    case "GenericType":
      return lowerSimpleType(node.base.name, node.args.map(a => lowerType(a, runtime)), runtime);

    case "FuncType":
      return {
        kind: "func",
        params: node.params.map(p => ({
          name: p.name?.name,
          type: lowerType(p.type, runtime),
          optional: p.optional,
        })),
        returns: lowerType(node.ret, runtime),
      };

    case "ObjectType":
      return {
        kind: "struct",
        fields: node.properties.map(p => ({
          name: p.name,
          type: lowerType(p.type, runtime),
          optional: p.optional,
        })),
        nominal: false,
      };

    case "ChanType":
      return {
        kind: "channel",
        element: node.elementType ? lowerType(node.elementType, runtime) : C.ANY,
        direction: node.direction === "receive" ? "recv" : node.direction === "send" ? "send" : "both",
      };

    case "ImplType":
    case "DynType":
      return lowerType(node.trait, runtime);

    case "IndexedAccessType":
      return C.ANY; // T[K] — too complex for cross-runtime

    default:
      return C.ANY;
  }
}

/**
 * Lower a simple named type, respecting the source language.
 */
function lowerSimpleType(
  name: string,
  args?: C.CanonicalType[],
  runtime?: Runtime
): C.CanonicalType {

  switch (name) {
    // Booleans
    case "bool": case "boolean": case "Bool": case "Boolean":
      return C.BOOL;

    // Strings
    case "string": case "String": case "str": case "Str":
      return C.STRING;

    // Void / None / Unit
    case "void": case "None": case "NoneType": case "unit": case "Unit":
      return C.VOID;

    // Never / Bottom
    case "never": case "Never": case "Nothing": case "noreturn": case "NoReturn":
      return C.NEVER;

    // Any / Dynamic
    case "any": case "Any": case "object": case "Object": case "dynamic": case "unknown":
      return C.ANY;

    // Null
    case "null": case "nil": case "nullptr":
      return C.NULL;

    // === Integers ===
    case "i8": case "int8": case "Int8":
      return { kind: "int", size: 8, signed: true };
    case "i16": case "int16": case "Int16": case "short": case "Short":
      return { kind: "int", size: 16, signed: true };
    case "i32": case "int32": case "Int32":
      return C.INT32;
    case "i64": case "int64": case "Int64": case "long": case "Long":
      return C.INT64;
    case "u8": case "uint8": case "Uint8": case "byte": case "Byte":
      return C.UINT8;
    case "u16": case "uint16": case "Uint16": case "ushort":
      return { kind: "int", size: 16, signed: false };
    case "u32": case "uint32": case "Uint32": case "uint":
      return { kind: "int", size: 32, signed: false };
    case "u64": case "uint64": case "Uint64": case "ulong":
      return { kind: "int", size: 64, signed: false };
    case "int": case "Int":
      if (runtime === "python") return { kind: "int", size: "big", signed: true };
      if (runtime === "go") return C.INT64;
      return C.INT32;
    case "isize": case "usize":
      return { kind: "int", size: 64, signed: name === "isize" };

    // === Floats ===
    case "f32": case "float32": case "Float32":
      return C.FLOAT32;
    case "float": case "Float":
      if (runtime === "python") return C.FLOAT64;
      return C.FLOAT32;
    case "f64": case "float64": case "Float64": case "double": case "Double":
      return C.FLOAT64;
    case "number": case "Number":
      return C.FLOAT64;

    // === Bytes ===
    case "bytes": case "Bytes":
      return { kind: "bytes" };

    // === Option / Nullable ===
    case "Option": case "Optional": case "Maybe":
      if (args && args.length === 1) return C.option(args[0]);
      return C.option(C.ANY);

    // === Result / Error ===
    case "Result":
      if (args && args.length >= 2) return C.result(args[0], args[1]);
      if (args && args.length === 1) return C.result(args[0], C.ANY);
      return C.result(C.ANY, C.ANY);

    // === Collections ===
    case "Array": case "Vec": case "List": case "list": case "Slice":
    case "ArrayList": case "[]":
      if (args && args.length === 1) return C.array(args[0]);
      return C.array(C.ANY);

    case "Map": case "Dict": case "dict": case "HashMap": case "BTreeMap":
    case "map": case "Record":
      if (args && args.length === 2) return C.map(args[0], args[1]);
      return C.map(C.STRING, C.ANY);

    case "Set": case "set": case "HashSet": case "BTreeSet":
      if (args && args.length === 1) return { kind: "set", element: args[0] };
      return { kind: "set", element: C.ANY };

    // === Async ===
    case "Promise": case "Future": case "Coroutine": case "Task":
    case "CompletableFuture": case "Deferred":
      if (args && args.length === 1) return C.async_(args[0]);
      return C.async_(C.ANY);

    // === Channels ===
    case "chan": case "Channel":
      if (args && args.length === 1) return { kind: "channel", element: args[0], direction: "both" };
      return { kind: "channel", element: C.ANY, direction: "both" };
    case "Sender": case "mpsc_Sender":
      if (args && args.length === 1) return { kind: "channel", element: args[0], direction: "send" };
      return { kind: "channel", element: C.ANY, direction: "send" };
    case "Receiver": case "mpsc_Receiver":
      if (args && args.length === 1) return { kind: "channel", element: args[0], direction: "recv" };
      return { kind: "channel", element: C.ANY, direction: "recv" };

    // === Streams ===
    case "AsyncIterable": case "AsyncIterator": case "ReadableStream":
    case "Stream": case "AsyncGenerator":
      if (args && args.length === 1) return C.stream(args[0]);
      return C.stream(C.ANY);

    // === Buffer Views (Zero-Copy) ===
    case "Uint8Array": case "Buffer":
      return C.bufferView(C.UINT8, "owned");
    case "Int8Array":
      return C.bufferView({ kind: "int", size: 8, signed: true }, "owned");
    case "Int16Array":
      return C.bufferView({ kind: "int", size: 16, signed: true }, "owned");
    case "Int32Array":
      return C.bufferView(C.INT32, "owned");
    case "Float32Array":
      return C.bufferView(C.FLOAT32, "owned");
    case "Float64Array":
      return C.bufferView(C.FLOAT64, "owned");
    case "ArrayBuffer": case "SharedArrayBuffer":
      return C.bufferView(C.UINT8, name === "SharedArrayBuffer" ? "shared" : "owned");

    // === Disposable ===
    case "Disposable":
      if (args && args.length === 1) return C.disposable(args[0], "dispose");
      return C.disposable(C.ANY, "dispose");
    case "Closer": case "Closeable": case "AutoCloseable":
    case "java.io.Closeable": case "java.lang.AutoCloseable":
      if (args && args.length === 1) return C.disposable(args[0], "close");
      return C.disposable(C.ANY, "close");

    // === Generic with type args ===
    default:
      if (args && args.length > 0) {
        return {
          kind: "generic",
          base: { kind: "struct", name, fields: [], nominal: true, origin: runtime },
          args,
        };
      }
      return { kind: "struct", name, fields: [], nominal: true, origin: runtime };
  }
}

/**
 * Lower a union type, canonicalizing T | null → Option<T>
 */
function lowerUnion(types: C.CanonicalType[]): C.CanonicalType {
  const nonNull = types.filter(t => t.kind !== "null");
  const hasNull = nonNull.length < types.length;

  if (hasNull && nonNull.length === 1) {
    return C.option(nonNull[0]);
  }
  if (nonNull.length === 1 && !hasNull) {
    return nonNull[0];
  }

  return { kind: "union", members: hasNull ? [...nonNull, C.NULL] : types };
}
