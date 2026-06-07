/**
 * Cross-Runtime Coercion Rules
 *
 * Defines what happens when a value of type A crosses from runtime X to runtime Y.
 * Three outcomes:
 *   - Safe: no conversion needed (or lossless widening)
 *   - Coerce: conversion needed but always succeeds (e.g., i32 → f64)
 *   - Check: conversion that may fail at runtime (e.g., f64 → i32, narrowing)
 *
 * Philosophy: structural at boundaries (duck typing for cross-runtime), nominal within.
 */

import * as C from './canonical';

export type Compatibility = "safe" | "coerce" | "check" | "incompatible";

export interface CoercionResult {
  compat: Compatibility;
  reason?: string;
  bridgeOp?: BridgeOp;
  /** Runtime guard hint: code snippets OmniVM can use for runtime type checks. */
  guard?: RuntimeGuard;
}

/**
 * Runtime guard hint — generated code snippets for "check" compatibility results.
 * OmniVM can insert these at boundary points to validate values at runtime.
 */
export interface RuntimeGuard {
  js?: string;
  python?: string;
  go?: string;
}

/**
 * Bridge operations that OmniVM must perform at runtime boundaries.
 */
export type BridgeOp =
  | { op: "identity" }                         // No conversion
  | { op: "widen"; from: string; to: string }  // Lossless widening (i32 → i64)
  | { op: "narrow"; from: string; to: string } // Lossy narrowing (f64 → i32, may truncate)
  | { op: "wrap_option" }                      // T → Option<T>
  | { op: "unwrap_option" }                    // Option<T> → T (may panic)
  | { op: "wrap_result" }                      // T → Result<T, E> (exceptions → Result)
  | { op: "unwrap_result" }                    // Result<T, E> → T (may throw)
  | { op: "serialize"; format: "json" | "msgpack" | "arrow" }  // Complex type → bytes
  | { op: "deserialize"; format: "json" | "msgpack" | "arrow"; target: C.CanonicalType }
  | { op: "copy_array" }                       // Deep copy collection across boundary
  | { op: "proxy_iterator" }                   // Lazy proxy instead of copy
  | { op: "await_resolve" }                    // Async<T> → T at boundary
  | { op: "to_string" }                        // Anything → string
  | { op: "parse_int" }                        // string → int
  | { op: "parse_float" }                      // string → float
  | { op: "struct_to_dict" }                   // Named struct → dict/object
  | { op: "dict_to_struct"; target: string }   // dict/object → named struct
  | { op: "channel_bridge"; direction: "send" | "recv" | "both" }
  | { op: "stream_proxy"; backpressure?: boolean }         // Channel/Stream → AsyncIterable proxy
  | { op: "share_memory"; ownership: "owned" | "borrowed" | "shared"; mutable?: boolean }  // Zero-copy buffer pass
  | { op: "copy_buffer" }                                  // Fallback: copy buffer when ownership incompatible
  | { op: "throw_typed"; errorKind?: string }              // Result.Err → typed exception with metadata
  | { op: "catch_to_result" }                              // try/catch → Result (inverse of throw_typed)
  | { op: "proxy_with_finalizer"; disposer?: string }      // Cross-boundary proxy with GC release hook
  | { op: "attach_disposer"; disposer: string }            // Wrap in Disposable for resource management
  | { op: "proxy_callable" }                               // Cross-boundary function proxy (can't serialize closures)
  | { op: "tag_dispatch"; variants: string[] }             // Enum/tagged union → discriminated union mapping
  | { op: "struct_reshape"; fieldMap: Record<string, string> }  // Rename/reorder struct fields at boundary
  | { op: "compose"; steps: BridgeOp[] };                  // Composed chain of bridge ops for nested generics

/**
 * Check if a value of type `from` can flow into a slot of type `to`.
 * This is directional: from → to.
 */
export function checkCompatibility(from: C.CanonicalType, to: C.CanonicalType): CoercionResult {
  // Any accepts anything
  if (to.kind === "any") return safe();
  // Any can be used as anything (unsafe but tolerant)
  if (from.kind === "any") return coerce("any narrows to target type");

  // Never flows into anything (bottom type)
  if (from.kind === "never") return safe();
  // Nothing flows into never
  if (to.kind === "never") return incompatible("cannot assign to never");

  // Same kind — delegate to specific checker
  if (from.kind === to.kind) {
    return checkSameKind(from, to);
  }

  // Cross-kind compatibility rules
  return checkCrossKind(from, to);
}

function checkSameKind(from: C.CanonicalType, to: C.CanonicalType): CoercionResult {
  switch (from.kind) {
    case "int": {
      const f = from as C.IntType;
      const t = to as C.IntType;
      if (f.size === t.size && f.signed === t.signed) return safe();
      if (f.size === "big" || t.size === "big") {
        return t.size === "big" ? safe() : check("big int may not fit in fixed-size int");
      }
      const fBits = typeof f.size === "number" ? f.size : 64;
      const tBits = typeof t.size === "number" ? t.size : 64;
      if (fBits <= tBits && f.signed === t.signed) return coerce("integer widening");
      if (fBits <= tBits && !f.signed && t.signed) return coerce("unsigned to wider signed");
      return check("integer narrowing may truncate");
    }

    case "float": {
      const f = from as C.FloatType;
      const t = to as C.FloatType;
      if (f.size === t.size) return safe();
      if (f.size < t.size) return coerce("float widening");
      return check("float narrowing loses precision");
    }

    case "bool": case "string": case "bytes": case "void": case "null":
      return safe();

    case "array": {
      const f = from as C.ArrayType;
      const t = to as C.ArrayType;
      const elem = checkCompatibility(f.element, t.element);
      if (elem.compat === "incompatible") return incompatible("array element " + elem.reason);
      if (f.fixedSize && t.fixedSize && f.fixedSize !== t.fixedSize) {
        return incompatible("fixed-size array length mismatch");
      }
      return elem;
    }

    case "map": {
      const f = from as C.MapType;
      const t = to as C.MapType;
      const key = checkCompatibility(f.key, t.key);
      const val = checkCompatibility(f.value, t.value);
      if (key.compat === "incompatible") return incompatible("map key " + key.reason);
      if (val.compat === "incompatible") return incompatible("map value " + val.reason);
      return worst(key, val);
    }

    case "option": {
      const f = from as C.OptionType;
      const t = to as C.OptionType;
      return checkCompatibility(f.inner, t.inner);
    }

    case "result": {
      const f = from as C.ResultType;
      const t = to as C.ResultType;
      const ok = checkCompatibility(f.ok, t.ok);
      const err = checkCompatibility(f.err, t.err);
      return worst(ok, err);
    }

    case "async": {
      const f = from as C.AsyncType;
      const t = to as C.AsyncType;
      return checkCompatibility(f.inner, t.inner);
    }

    case "func": {
      const f = from as C.FuncType;
      const t = to as C.FuncType;
      // Contravariant params, covariant return
      const ret = checkCompatibility(f.returns, t.returns);
      if (ret.compat === "incompatible") return incompatible("return type " + ret.reason);
      // Check param count
      const requiredFrom = f.params.filter(p => !p.optional).length;
      const requiredTo = t.params.filter(p => !p.optional).length;
      if (requiredFrom > t.params.length) return incompatible("too many required params");
      // Check each param (contravariant: to-param must flow into from-param)
      let result: CoercionResult = ret;
      for (let i = 0; i < Math.min(f.params.length, t.params.length); i++) {
        const paramCompat = checkCompatibility(t.params[i].type, f.params[i].type);
        result = worst(result, paramCompat);
      }
      // Functions cannot be serialized — they always need a proxy at boundaries.
      // Even if types match perfectly, crossing a runtime boundary requires a callable proxy.
      return { compat: result.compat, reason: result.reason, bridgeOp: { op: "proxy_callable" } };
    }

    case "struct": {
      const f = from as C.StructType;
      const t = to as C.StructType;
      // Same nominal type in same runtime: exact match
      if (t.nominal && f.nominal && f.origin === t.origin) {
        if (f.name === t.name) return safe();
        // Different nominal types in same runtime: incompatible
        return incompatible(`nominal type mismatch: ${f.name} vs ${t.name}`);
      }
      // Cross-runtime or structural: use structural field matching
      // This is the key insight: at boundaries, everything is structural
      if (f.fields.length === 0 && t.fields.length === 0) {
        // Both are opaque named types with no known fields — coerce by name similarity
        if (f.name && t.name && f.name === t.name) return coerce("same-named struct across runtimes");
        if (f.name && t.name) return incompatible(`nominal type mismatch across runtimes: ${f.name} vs ${t.name}`);
        return coerce("opaque struct crossing");
      }
      return checkStructural(f, t);
    }

    case "interface": {
      const f = from as C.InterfaceType;
      const t = to as C.InterfaceType;
      // Check that f has all methods of t
      for (const method of t.methods) {
        const fMethod = f.methods.find(m => m.name === method.name);
        if (!fMethod) return incompatible(`missing method: ${method.name}`);
        const compat = checkCompatibility(fMethod.type, method.type);
        if (compat.compat === "incompatible") return compat;
      }
      return safe();
    }

    case "enum":
      return checkEnumToEnum(from as C.EnumType, to as C.EnumType);

    case "tuple": {
      const f = from as C.TupleType;
      const t = to as C.TupleType;
      if (f.elements.length !== t.elements.length) return incompatible("tuple length mismatch");
      let result: CoercionResult = safe();
      for (let i = 0; i < f.elements.length; i++) {
        const elem = checkCompatibility(f.elements[i], t.elements[i]);
        if (elem.compat === "incompatible") return incompatible(`tuple element ${i}: ${elem.reason}`);
        result = worst(result, elem);
      }
      return result;
    }

    case "channel": {
      const f = from as C.ChannelType;
      const t = to as C.ChannelType;
      const elem = checkCompatibility(f.element, t.element);
      if (elem.compat === "incompatible") return elem;
      // Direction compatibility
      if (t.direction === "both") return elem;
      if (f.direction === "both") return elem; // Narrowing direction is ok
      if (f.direction !== t.direction) return incompatible("channel direction mismatch");
      return elem;
    }

    case "stream": {
      const f = from as C.StreamType;
      const t = to as C.StreamType;
      const elem = checkCompatibility(f.element, t.element);
      if (elem.compat === "incompatible") return incompatible("stream element " + elem.reason);
      // Backpressure: target wants backpressure but source doesn't support it → warning
      if (t.backpressure && !f.backpressure) {
        return { compat: "coerce", reason: "source stream lacks backpressure support", bridgeOp: { op: "stream_proxy", backpressure: false } };
      }
      return elem;
    }

    case "buffer_view": {
      const f = from as C.BufferViewType;
      const t = to as C.BufferViewType;
      const elem = checkCompatibility(f.element, t.element);
      if (elem.compat === "incompatible") return incompatible("buffer element " + elem.reason);
      // Ownership: borrowed → owned is ok (copy), owned → borrowed is ok (lend)
      // Mutable: immutable → mutable requires copy
      if (t.mutable && !f.mutable) {
        return { compat: "coerce", reason: "must copy buffer to make mutable", bridgeOp: { op: "copy_buffer" } };
      }
      // Shared memory is safe if element types match
      return { compat: elem.compat, reason: elem.reason, bridgeOp: { op: "share_memory", ownership: t.ownership, mutable: t.mutable } };
    }

    case "disposable": {
      const f = from as C.DisposableType;
      const t = to as C.DisposableType;
      return checkCompatibility(f.inner, t.inner);
    }

    default:
      return coerce("same kind, assuming compatible");
  }
}

function checkCrossKind(from: C.CanonicalType, to: C.CanonicalType): CoercionResult {
  // int → float: always safe (i32 → f64 is lossless)
  if (from.kind === "int" && to.kind === "float") {
    const intSize = typeof (from as C.IntType).size === "number" ? (from as C.IntType).size as number : 64;
    if (intSize <= 32 && (to as C.FloatType).size === 64) return coerce("int to float64 is lossless");
    return check("large int to float may lose precision");
  }

  // float → int: check (truncation)
  if (from.kind === "float" && to.kind === "int") {
    return {
      compat: "check",
      reason: "float to int truncates",
      bridgeOp: { op: "narrow", from: `f${(from as C.FloatType).size}`, to: `i${(to as C.IntType).size}` },
      guard: {
        js: `Number.isInteger(value) && value >= ${intMin(to as C.IntType)} && value <= ${intMax(to as C.IntType)}`,
        python: `isinstance(value, (int, float)) and float(value).is_integer()`,
        go: `_, ok := value.(int64)`,
      },
    };
  }

  // T → Option<T>: always safe (wrapping)
  if (to.kind === "option") {
    const inner = checkCompatibility(from, (to as C.OptionType).inner);
    if (inner.compat !== "incompatible") {
      return { compat: inner.compat, bridgeOp: { op: "wrap_option" } };
    }
  }

  // Option<T> → U: check (may be null) + compose with inner bridge
  if (from.kind === "option" && to.kind !== "option") {
    const inner = checkCompatibility((from as C.OptionType).inner, to);
    if (inner.compat !== "incompatible") {
      const unwrapOp: BridgeOp = { op: "unwrap_option" };
      return {
        compat: "check",
        reason: "optional may be null",
        bridgeOp: composeOps(unwrapOp, inner),
      };
    }
  }

  // Result<T, E> → U: check (may be error) + compose with inner bridge
  // Use throw_typed when error is a named struct for fidelity
  if (from.kind === "result" && to.kind !== "result") {
    const f = from as C.ResultType;
    const inner = checkCompatibility(f.ok, to);
    if (inner.compat !== "incompatible") {
      const errOp: BridgeOp = (f.err.kind === "struct" && (f.err as C.StructType).name)
        ? { op: "throw_typed", errorKind: (f.err as C.StructType).name }
        : { op: "unwrap_result" };
      return {
        compat: "check",
        reason: f.err.kind === "struct" && (f.err as C.StructType).name ? "result may be typed error" : "result may be error",
        bridgeOp: composeOps(errOp, inner),
      };
    }
  }

  // T → Result<T, E>: safe (wrapping in Ok)
  if (to.kind === "result") {
    const inner = checkCompatibility(from, (to as C.ResultType).ok);
    if (inner.compat !== "incompatible") {
      return { compat: inner.compat, bridgeOp: { op: "wrap_result" } };
    }
  }

  // Async<T> → U: needs await + compose with inner bridge
  if (from.kind === "async") {
    const inner = checkCompatibility((from as C.AsyncType).inner, to);
    if (inner.compat !== "incompatible") {
      const awaitOp: BridgeOp = { op: "await_resolve" };
      const worstCompat = worst({ compat: "coerce" }, inner).compat;
      return {
        compat: worstCompat,
        reason: "must await",
        bridgeOp: composeOps(awaitOp, inner),
      };
    }
  }

  // Stream → Async: take first element (lossy)
  if (from.kind === "stream" && to.kind === "async") {
    const elem = checkCompatibility((from as C.StreamType).element, (to as C.AsyncType).inner);
    if (elem.compat !== "incompatible") {
      return { compat: "check", reason: "stream to single async takes only first element" };
    }
  }

  // Channel → Async: receive one value
  if (from.kind === "channel" && to.kind === "async") {
    const elem = checkCompatibility((from as C.ChannelType).element, (to as C.AsyncType).inner);
    if (elem.compat !== "incompatible") {
      return { compat: "coerce", reason: "channel recv as async", bridgeOp: { op: "await_resolve" } };
    }
  }

  // T → Async<T>: safe (already resolved)
  if (to.kind === "async") {
    return checkCompatibility(from, (to as C.AsyncType).inner);
  }

  // struct → interface: structural check
  if (from.kind === "struct" && to.kind === "interface") {
    return checkStructSatisfiesInterface(from as C.StructType, to as C.InterfaceType);
  }

  // struct → struct (cross-kind won't fire, but included for completeness via same-kind)

  // enum → union: tag dispatch (Rust enum → TS discriminated union)
  if (from.kind === "enum" && to.kind === "union") {
    return checkEnumToUnion(from as C.EnumType, to as C.UnionType);
  }

  // union → enum: reverse tag dispatch
  if (from.kind === "union" && to.kind === "enum") {
    return checkUnionToEnum(from as C.UnionType, to as C.EnumType);
  }

  // struct cross-runtime (different origins): structural check
  if (from.kind === "struct" && to.kind === "struct") {
    return checkStructural(from as C.StructType, to as C.StructType);
  }

  // null → option: safe
  if (from.kind === "null" && to.kind === "option") {
    return safe();
  }

  // string → int/float: parse
  if (from.kind === "string" && to.kind === "int") {
    return {
      compat: "check",
      reason: "string to int requires parsing",
      bridgeOp: { op: "parse_int" },
      guard: {
        js: `!isNaN(parseInt(value, 10))`,
        python: `value.lstrip("-").isdigit()`,
        go: `_, err := strconv.ParseInt(value, 10, 64); err == nil`,
      },
    };
  }
  if (from.kind === "string" && to.kind === "float") {
    return {
      compat: "check",
      reason: "string to float requires parsing",
      bridgeOp: { op: "parse_float" },
      guard: {
        js: `!isNaN(parseFloat(value))`,
        python: `try: float(value); True\nexcept: False`,
        go: `_, err := strconv.ParseFloat(value, 64); err == nil`,
      },
    };
  }

  // int/float/bool → string: to_string
  if ((from.kind === "int" || from.kind === "float" || from.kind === "bool") && to.kind === "string") {
    return { compat: "coerce", bridgeOp: { op: "to_string" } };
  }

  // Channel → Stream: semantic upgrade (multi-value async)
  if (from.kind === "channel" && to.kind === "stream") {
    const elem = checkCompatibility((from as C.ChannelType).element, (to as C.StreamType).element);
    if (elem.compat !== "incompatible") {
      return { compat: "coerce", reason: "channel to stream proxy", bridgeOp: { op: "stream_proxy", backpressure: (to as C.StreamType).backpressure } };
    }
  }

  // Stream → Channel: downgrade (stream into channel)
  if (from.kind === "stream" && to.kind === "channel") {
    const elem = checkCompatibility((from as C.StreamType).element, (to as C.ChannelType).element);
    if (elem.compat !== "incompatible") {
      return { compat: "coerce", reason: "stream to channel", bridgeOp: { op: "stream_proxy" } };
    }
  }

  // Array/bytes → BufferView: zero-copy view if element matches
  if ((from.kind === "array" || from.kind === "bytes") && to.kind === "buffer_view") {
    const fromElem = from.kind === "bytes" ? C.UINT8 : (from as C.ArrayType).element;
    const elem = checkCompatibility(fromElem, (to as C.BufferViewType).element);
    if (elem.compat !== "incompatible") {
      return { compat: "coerce", reason: "array to buffer view", bridgeOp: { op: "share_memory", ownership: (to as C.BufferViewType).ownership } };
    }
  }

  // BufferView → Array/bytes: copy out
  if (from.kind === "buffer_view" && (to.kind === "array" || to.kind === "bytes")) {
    const toElem = to.kind === "bytes" ? C.UINT8 : (to as C.ArrayType).element;
    const elem = checkCompatibility((from as C.BufferViewType).element, toElem);
    if (elem.compat !== "incompatible") {
      return { compat: "coerce", reason: "buffer view to array (copy)", bridgeOp: { op: "copy_buffer" } };
    }
  }

  // T → Disposable<T>: wrap with finalizer
  if (to.kind === "disposable") {
    const inner = checkCompatibility(from, (to as C.DisposableType).inner);
    if (inner.compat !== "incompatible") {
      return { compat: "coerce", reason: "wrapping in disposable", bridgeOp: { op: "attach_disposer", disposer: (to as C.DisposableType).disposer || "dispose" } };
    }
  }

  // Disposable<T> → T: unwrap (resource management responsibility transfers)
  if (from.kind === "disposable") {
    const inner = checkCompatibility((from as C.DisposableType).inner, to);
    if (inner.compat !== "incompatible") {
      return { compat: "check", reason: "unwrapping disposable — receiver must manage lifecycle" };
    }
  }

  // array ↔ set: coerce
  if (from.kind === "array" && to.kind === "set") {
    return checkCompatibility((from as C.ArrayType).element, (to as C.SetType).element);
  }
  if (from.kind === "set" && to.kind === "array") {
    return checkCompatibility((from as C.SetType).element, (to as C.ArrayType).element);
  }

  // union member → target: check if any member is compatible
  if (from.kind === "union") {
    const members = (from as C.UnionType).members;
    const results = members.map(m => checkCompatibility(m, to));
    if (results.every(r => r.compat !== "incompatible")) {
      return { compat: "check", reason: "union member may not match target" };
    }
  }

  // target is a union → check if from matches any member
  if (to.kind === "union") {
    const members = (to as C.UnionType).members;
    for (const m of members) {
      const r = checkCompatibility(from, m);
      if (r.compat === "safe" || r.compat === "coerce") return r;
    }
    return incompatible("does not match any union member");
  }

  return incompatible(`cannot convert ${from.kind} to ${to.kind}`);
}

function checkStructural(from: C.StructType, to: C.StructType): CoercionResult {
  let result: CoercionResult = safe();
  for (const field of to.fields) {
    if (field.optional) continue;
    const fField = from.fields.find(f => f.name === field.name);
    if (!fField) return incompatible(`missing field: ${field.name}`);
    const compat = checkCompatibility(fField.type, field.type);
    if (compat.compat === "incompatible") return incompatible(`field ${field.name}: ${compat.reason}`);
    result = worst(result, compat);
  }
  // If from has extra fields, it's still structurally compatible (Go-style duck typing)
  // but we note it as coerce if cross-runtime
  if (from.origin !== to.origin && result.compat === "safe" && from.fields.length > to.fields.length) {
    result = coerce("source struct has extra fields (structural subtyping)");
  }
  return result;
}

function checkStructSatisfiesInterface(s: C.StructType, i: C.InterfaceType): CoercionResult {
  // If the struct has fields that match the interface method names, duck-type it
  if (s.fields.length > 0 && i.methods.length > 0) {
    for (const method of i.methods) {
      const hasField = s.fields.find(f => f.name === method.name);
      if (!hasField) {
        return { compat: "check", reason: `struct may not satisfy interface: missing '${method.name}'` };
      }
    }
    return coerce("struct satisfies interface structurally");
  }
  return coerce("struct-to-interface crossing uses structural duck typing");
}

/**
 * Enum → Union: map enum variants to union members by tag.
 * Rust `enum Shape { Circle(f64), Rect(f64, f64) }` →
 * TS `{ tag: "Circle", value: number } | { tag: "Rect", value: [number, number] }`
 */
function checkEnumToUnion(e: C.EnumType, u: C.UnionType): CoercionResult {
  const variants = e.variants.map(v => v.name);
  // Each variant should map to some union member — we can't deeply verify without
  // knowing the union member structure, so we emit a tag_dispatch bridge op
  return {
    compat: "coerce",
    reason: `enum '${e.name}' to union via tag dispatch`,
    bridgeOp: { op: "tag_dispatch", variants },
  };
}

/**
 * Union → Enum: reverse tag dispatch.
 */
function checkUnionToEnum(u: C.UnionType, e: C.EnumType): CoercionResult {
  const variants = e.variants.map(v => v.name);
  return {
    compat: "check",
    reason: `union to enum '${e.name}' — runtime must validate variant tags`,
    bridgeOp: { op: "tag_dispatch", variants },
    guard: {
      js: `["${variants.join('","')}"].includes(value.tag)`,
      python: `value["tag"] in {${variants.map(v => `"${v}"`).join(", ")}}`,
    },
  };
}

/**
 * Enum → Enum: variant name matching across runtimes.
 */
function checkEnumToEnum(from: C.EnumType, to: C.EnumType): CoercionResult {
  const fromNames = new Set(from.variants.map(v => v.name));
  const toNames = to.variants.map(v => v.name);
  const missing = toNames.filter(n => !fromNames.has(n));
  if (missing.length > 0) {
    return incompatible(`enum '${from.name}' missing variants: ${missing.join(", ")}`);
  }
  // All target variants exist in source
  if (from.variants.length === to.variants.length) {
    // Check payloads
    let result: CoercionResult = safe();
    for (const tv of to.variants) {
      const fv = from.variants.find(v => v.name === tv.name)!;
      if (fv.payload && tv.payload) {
        const compat = checkCompatibility(fv.payload, tv.payload);
        result = worst(result, compat);
      } else if (fv.payload !== tv.payload) {
        return incompatible(`variant '${tv.name}' payload mismatch`);
      }
    }
    return result;
  }
  // Source has extra variants — coerce (target is a subset)
  return coerce(`enum '${from.name}' has extra variants not in '${to.name}'`);
}

// ---- Helpers ----

function safe(): CoercionResult { return { compat: "safe" }; }
function coerce(reason: string): CoercionResult { return { compat: "coerce", reason }; }
function check(reason: string): CoercionResult { return { compat: "check", reason }; }
function incompatible(reason: string): CoercionResult { return { compat: "incompatible", reason }; }

function intMin(t: C.IntType): string {
  if (t.size === "big") return "-Infinity";
  return t.signed ? String(-(2 ** (t.size as number - 1))) : "0";
}
function intMax(t: C.IntType): string {
  if (t.size === "big") return "Infinity";
  return t.signed ? String(2 ** (t.size as number - 1) - 1) : String(2 ** (t.size as number) - 1);
}

function worst(a: CoercionResult, b: CoercionResult): CoercionResult {
  const order: Compatibility[] = ["safe", "coerce", "check", "incompatible"];
  return order.indexOf(a.compat) >= order.indexOf(b.compat) ? a : b;
}

/**
 * Compose an outer bridge op with an inner result's bridge op.
 * If inner has no bridge op, return just the outer op.
 * If both exist, create a compose chain.
 */
function composeOps(outerOp: BridgeOp, inner: CoercionResult): BridgeOp {
  if (!inner.bridgeOp) return outerOp;
  // Flatten: if inner is already a compose, append
  const innerSteps = inner.bridgeOp.op === "compose"
    ? (inner.bridgeOp as { op: "compose"; steps: BridgeOp[] }).steps
    : [inner.bridgeOp];
  return { op: "compose", steps: [outerOp, ...innerSteps] };
}
