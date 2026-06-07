/**
 * Canonical Type System — The Unified Type IR
 *
 * Every type annotation from every language lowers to this representation.
 * The IR is based on algebraic data types:
 *   - Primitives (int, float, bool, string, void, never, any)
 *   - Products (structs/records/tuples/classes)
 *   - Sums (unions/enums/option/result)
 *   - Functions (callable signatures)
 *   - Collections (arrays, maps, sets, channels)
 *   - References (pointers, borrows, optionals)
 *   - Async wrappers (Promise/Future/Coroutine → Async<T>)
 */

// ============ Primitive Types ============

export type IntSize = 8 | 16 | 32 | 64 | "big";
export type FloatSize = 32 | 64;

export interface IntType {
  kind: "int";
  size: IntSize;
  signed: boolean;
}

export interface FloatType {
  kind: "float";
  size: FloatSize;
}

export interface BoolType { kind: "bool"; }
export interface StringType { kind: "string"; }
export interface BytesType { kind: "bytes"; }
export interface VoidType { kind: "void"; }
export interface NeverType { kind: "never"; }
export interface AnyType { kind: "any"; }
export interface NullType { kind: "null"; }

export type PrimitiveType =
  | IntType | FloatType | BoolType | StringType
  | BytesType | VoidType | NeverType | AnyType | NullType;

// ============ Product Types (Structs/Records/Tuples) ============

export interface Field {
  name: string;
  type: CanonicalType;
  optional?: boolean;
  mutable?: boolean;
  visibility?: "public" | "private" | "protected";
}

export interface StructType {
  kind: "struct";
  name?: string;              // Named vs anonymous
  fields: Field[];
  nominal?: boolean;          // true = Java/Rust nominal, false = TS/Go structural
  origin?: string;            // Source language for nominal checking
}

export interface TupleType {
  kind: "tuple";
  elements: CanonicalType[];
}

// ============ Sum Types (Unions/Enums/Option/Result) ============

export interface UnionType {
  kind: "union";
  members: CanonicalType[];
}

export interface EnumType {
  kind: "enum";
  name: string;
  variants: EnumVariant[];
}

export interface EnumVariant {
  name: string;
  payload?: CanonicalType;    // Rust-style enum with data
}

export interface OptionType {
  kind: "option";
  inner: CanonicalType;       // Option<T> / T? / T | null / Optional[T]
}

export interface ResultType {
  kind: "result";
  ok: CanonicalType;
  err: CanonicalType;         // Result<T, E> / (T, error) / throws
}

// ============ Function Types ============

export interface FuncType {
  kind: "func";
  params: FuncParam[];
  returns: CanonicalType;
  async?: boolean;
  throws?: CanonicalType;     // Error type if the function can throw/return error
  variadic?: boolean;
}

export interface FuncParam {
  name?: string;
  type: CanonicalType;
  optional?: boolean;
  defaultValue?: boolean;     // Has a default (we don't track the value)
}

// ============ Collection Types ============

export interface ArrayType {
  kind: "array";
  element: CanonicalType;
  fixedSize?: number;         // Fixed-size arrays ([4]int in Go, [u8; 4] in Rust)
}

export interface MapType {
  kind: "map";
  key: CanonicalType;
  value: CanonicalType;
}

export interface SetType {
  kind: "set";
  element: CanonicalType;
}

export interface ChannelType {
  kind: "channel";
  element: CanonicalType;
  direction?: "send" | "recv" | "both";
}

// ============ Reference / Wrapper Types ============

export interface RefType {
  kind: "ref";
  inner: CanonicalType;
  ownership: "owned" | "borrowed" | "shared" | "gc";
  mutable?: boolean;
}

export interface AsyncType {
  kind: "async";
  inner: CanonicalType;       // Promise<T> / Future<T> / Coroutine → Async<T>
}

// ============ Stream Type (Multi-Value Async) ============

/**
 * Stream<T> — a multi-value async sequence.
 *   Go:   <-chan T / chan T
 *   TS:   AsyncIterable<T> / ReadableStream<T>
 *   Rust: futures::Stream<Item = T> / tokio::sync::mpsc::Receiver<T>
 *
 * Distinct from Async<T> (single value) and Channel<T> (bidirectional primitive).
 * Channels lower to Stream when used in a receive-only cross-boundary context.
 */
export interface StreamType {
  kind: "stream";
  element: CanonicalType;
  backpressure?: boolean;     // true if the producer respects consumer speed
}

// ============ BufferView Type (Zero-Copy Memory) ============

/**
 * BufferView<T> — contiguous memory passed by reference, not copied.
 *   Go:   []byte
 *   TS:   Uint8Array / ArrayBuffer / TypedArray
 *   Rust: &[u8] / &mut [u8] / Vec<u8>
 *
 * The bridge passes a pointer + length instead of serializing.
 * Ownership semantics determine whether the receiver can mutate or must treat as read-only.
 */
export interface BufferViewType {
  kind: "buffer_view";
  element: CanonicalType;     // Usually u8, but could be i32 for Int32Array etc.
  ownership: "owned" | "borrowed" | "shared";
  mutable?: boolean;
}

// ============ Disposable Type (Cross-Boundary Resource Management) ============

/**
 * Disposable — a resource that must be explicitly released across boundaries.
 *   TS:   Symbol.dispose / using keyword
 *   Rust: Drop trait
 *   Go:   io.Closer / defer close()
 *
 * When a proxy (e.g. callback, interface impl) crosses a boundary,
 * the receiving runtime must signal when it's done so the source
 * runtime can release the reference. Without this, cross-boundary
 * proxies leak memory.
 */
export interface DisposableType {
  kind: "disposable";
  inner: CanonicalType;       // The underlying type being wrapped
  disposer?: string;          // Method name: "close", "dispose", "drop"
}

// ============ Generic / Parametric Types ============

export interface TypeVar {
  kind: "typevar";
  name: string;
  bound?: CanonicalType;      // T: Display + Clone → bound is intersection
}

export interface GenericType {
  kind: "generic";
  base: CanonicalType;
  args: CanonicalType[];
}

// ============ Interface / Trait Types ============

export interface InterfaceType {
  kind: "interface";
  name?: string;
  methods: MethodSig[];
  nominal?: boolean;
}

export interface MethodSig {
  name: string;
  type: FuncType;
}

// ============ Literal Types ============

export interface LiteralType {
  kind: "literal";
  value: string | number | boolean;
}

// ============ The Union of All Types ============

export type CanonicalType =
  | PrimitiveType
  | StructType
  | TupleType
  | UnionType
  | EnumType
  | OptionType
  | ResultType
  | FuncType
  | ArrayType
  | MapType
  | SetType
  | ChannelType
  | RefType
  | AsyncType
  | StreamType
  | BufferViewType
  | DisposableType
  | TypeVar
  | GenericType
  | InterfaceType
  | LiteralType;

// ============ Helpers ============

export const INT32: IntType = { kind: "int", size: 32, signed: true };
export const INT64: IntType = { kind: "int", size: 64, signed: true };
export const UINT8: IntType = { kind: "int", size: 8, signed: false };
export const FLOAT32: FloatType = { kind: "float", size: 32 };
export const FLOAT64: FloatType = { kind: "float", size: 64 };
export const BOOL: BoolType = { kind: "bool" };
export const STRING: StringType = { kind: "string" };
export const VOID: VoidType = { kind: "void" };
export const NEVER: NeverType = { kind: "never" };
export const ANY: AnyType = { kind: "any" };
export const NULL: NullType = { kind: "null" };

export function option(inner: CanonicalType): OptionType {
  return { kind: "option", inner };
}

export function array(element: CanonicalType): ArrayType {
  return { kind: "array", element };
}

export function map(key: CanonicalType, value: CanonicalType): MapType {
  return { kind: "map", key, value };
}

export function func(params: CanonicalType[], returns: CanonicalType): FuncType {
  return { kind: "func", params: params.map(t => ({ type: t })), returns };
}

export function async_(inner: CanonicalType): AsyncType {
  return { kind: "async", inner };
}

export function result(ok: CanonicalType, err: CanonicalType): ResultType {
  return { kind: "result", ok, err };
}

export function stream(element: CanonicalType, backpressure?: boolean): StreamType {
  return { kind: "stream", element, ...(backpressure ? { backpressure } : {}) };
}

export function bufferView(
  element: CanonicalType = UINT8,
  ownership: "owned" | "borrowed" | "shared" = "borrowed",
  mutable?: boolean,
): BufferViewType {
  return { kind: "buffer_view", element, ownership, ...(mutable ? { mutable } : {}) };
}

export function disposable(inner: CanonicalType, disposer?: string): DisposableType {
  return { kind: "disposable", inner, ...(disposer ? { disposer } : {}) };
}
