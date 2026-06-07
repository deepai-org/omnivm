import { OmniRuntime } from './types';

/**
 * Method name → runtime affinity mapping.
 * When a method call like `obj.upper()` is encountered,
 * the method name can provide evidence for which runtime the expression belongs to.
 */
export const METHOD_AFFINITY: Record<string, OmniRuntime> = {
  // Python string methods
  upper: OmniRuntime.Python,
  lower: OmniRuntime.Python,
  strip: OmniRuntime.Python,
  lstrip: OmniRuntime.Python,
  rstrip: OmniRuntime.Python,
  startswith: OmniRuntime.Python,
  endswith: OmniRuntime.Python,
  isdigit: OmniRuntime.Python,
  isalpha: OmniRuntime.Python,
  isalnum: OmniRuntime.Python,
  isupper: OmniRuntime.Python,
  islower: OmniRuntime.Python,
  zfill: OmniRuntime.Python,
  encode: OmniRuntime.Python,
  decode: OmniRuntime.Python,
  splitlines: OmniRuntime.Python,
  capitalize: OmniRuntime.Python,
  title: OmniRuntime.Python,
  swapcase: OmniRuntime.Python,
  center: OmniRuntime.Python,
  ljust: OmniRuntime.Python,
  rjust: OmniRuntime.Python,

  // Python list/dict methods
  append: OmniRuntime.Python,
  extend: OmniRuntime.Python,
  items: OmniRuntime.Python,
  keys: OmniRuntime.Python,
  values: OmniRuntime.Python,
  update: OmniRuntime.Python,
  setdefault: OmniRuntime.Python,
  popitem: OmniRuntime.Python,

  // JavaScript array methods
  map: OmniRuntime.JavaScript,
  filter: OmniRuntime.JavaScript,
  reduce: OmniRuntime.JavaScript,
  forEach: OmniRuntime.JavaScript,
  find: OmniRuntime.JavaScript,
  findIndex: OmniRuntime.JavaScript,
  some: OmniRuntime.JavaScript,
  every: OmniRuntime.JavaScript,
  flatMap: OmniRuntime.JavaScript,
  flat: OmniRuntime.JavaScript,
  fill: OmniRuntime.JavaScript,
  copyWithin: OmniRuntime.JavaScript,
  entries: OmniRuntime.JavaScript,
  from: OmniRuntime.JavaScript,
  of: OmniRuntime.JavaScript,

  // JavaScript string methods
  padStart: OmniRuntime.JavaScript,
  padEnd: OmniRuntime.JavaScript,
  trimStart: OmniRuntime.JavaScript,
  trimEnd: OmniRuntime.JavaScript,
  charAt: OmniRuntime.JavaScript,
  charCodeAt: OmniRuntime.JavaScript,
  codePointAt: OmniRuntime.JavaScript,
  normalize: OmniRuntime.JavaScript,
  matchAll: OmniRuntime.JavaScript,
  replaceAll: OmniRuntime.JavaScript,
  localeCompare: OmniRuntime.JavaScript,
  substring: OmniRuntime.JavaScript,

  // JavaScript Promise methods
  then: OmniRuntime.JavaScript,
  catch: OmniRuntime.JavaScript,
  finally: OmniRuntime.JavaScript,

  // JavaScript object methods
  hasOwnProperty: OmniRuntime.JavaScript,
  toString: OmniRuntime.JavaScript,
  valueOf: OmniRuntime.JavaScript,
  toJSON: OmniRuntime.JavaScript,
  stringify: OmniRuntime.JavaScript,

  // Ruby methods
  each: OmniRuntime.Ruby,
  each_with_index: OmniRuntime.Ruby,
  each_with_object: OmniRuntime.Ruby,
  collect: OmniRuntime.Ruby,
  select: OmniRuntime.Ruby,
  reject: OmniRuntime.Ruby,
  inject: OmniRuntime.Ruby,
  detect: OmniRuntime.Ruby,
  map_with_index: OmniRuntime.Ruby,
  flat_map: OmniRuntime.Ruby,
  each_slice: OmniRuntime.Ruby,
  each_cons: OmniRuntime.Ruby,
  take_while: OmniRuntime.Ruby,
  drop_while: OmniRuntime.Ruby,
  group_by: OmniRuntime.Ruby,
  sort_by: OmniRuntime.Ruby,
  min_by: OmniRuntime.Ruby,
  max_by: OmniRuntime.Ruby,
  count: OmniRuntime.Ruby,
  freeze: OmniRuntime.Ruby,
  frozen: OmniRuntime.Ruby,
  dup: OmniRuntime.Ruby,
  clone: OmniRuntime.Ruby,
  respond_to: OmniRuntime.Ruby,
  send: OmniRuntime.Ruby,
  is_a: OmniRuntime.Ruby,
  kind_of: OmniRuntime.Ruby,
  instance_of: OmniRuntime.Ruby,
  nil: OmniRuntime.Ruby,
  puts: OmniRuntime.Ruby,
  chomp: OmniRuntime.Ruby,
  chop: OmniRuntime.Ruby,
  gsub: OmniRuntime.Ruby,
  sub: OmniRuntime.Ruby,
  scan: OmniRuntime.Ruby,
  to_s: OmniRuntime.Ruby,
  to_i: OmniRuntime.Ruby,
  to_f: OmniRuntime.Ruby,
  to_a: OmniRuntime.Ruby,
  to_h: OmniRuntime.Ruby,
  to_sym: OmniRuntime.Ruby,

  // Java methods
  println: OmniRuntime.Java,
  charAt_: OmniRuntime.Java, // disambiguated from JS
  compareTo: OmniRuntime.Java,
  equals: OmniRuntime.Java,
  hashCode: OmniRuntime.Java,
  getClass: OmniRuntime.Java,
  keySet: OmniRuntime.Java,
  entrySet: OmniRuntime.Java,
  getOrDefault: OmniRuntime.Java,
  instanceof_: OmniRuntime.Java,
  toArray: OmniRuntime.Java,
  iterator: OmniRuntime.Java,
  stream: OmniRuntime.Java,
  parallelStream: OmniRuntime.Java,
  collect_: OmniRuntime.Java, // disambiguated from Ruby
  synchronized_: OmniRuntime.Java,

  // Go methods (less common as method names, but some standard patterns)
  Println: OmniRuntime.Go,
  Printf: OmniRuntime.Go,
  Sprintf: OmniRuntime.Go,
  Fprintf: OmniRuntime.Go,
  Errorf: OmniRuntime.Go,
  Fatal: OmniRuntime.Go,
  Fatalf: OmniRuntime.Go,
};

/**
 * Builtin function name → runtime affinity mapping.
 */
export const BUILTIN_AFFINITY: Record<string, OmniRuntime> = {
  // Python builtins
  print: OmniRuntime.Python,
  len: OmniRuntime.Python,
  range: OmniRuntime.Python,
  enumerate: OmniRuntime.Python,
  zip: OmniRuntime.Python,
  sorted: OmniRuntime.Python,
  reversed: OmniRuntime.Python,
  isinstance: OmniRuntime.Python,
  issubclass: OmniRuntime.Python,
  type: OmniRuntime.Python,
  str: OmniRuntime.Python,
  int: OmniRuntime.Python,
  float: OmniRuntime.Python,
  bool: OmniRuntime.Python,
  list: OmniRuntime.Python,
  dict: OmniRuntime.Python,
  tuple: OmniRuntime.Python,
  set: OmniRuntime.Python,
  frozenset: OmniRuntime.Python,
  bytes: OmniRuntime.Python,
  bytearray: OmniRuntime.Python,
  memoryview: OmniRuntime.Python,
  abs: OmniRuntime.Python,
  all: OmniRuntime.Python,
  any: OmniRuntime.Python,
  bin: OmniRuntime.Python,
  hex: OmniRuntime.Python,
  oct: OmniRuntime.Python,
  ord: OmniRuntime.Python,
  chr: OmniRuntime.Python,
  dir: OmniRuntime.Python,
  getattr: OmniRuntime.Python,
  setattr: OmniRuntime.Python,
  hasattr: OmniRuntime.Python,
  delattr: OmniRuntime.Python,
  globals: OmniRuntime.Python,
  locals: OmniRuntime.Python,
  vars: OmniRuntime.Python,
  open: OmniRuntime.Python,
  input: OmniRuntime.Python,
  eval: OmniRuntime.Python,
  exec: OmniRuntime.Python,
  compile: OmniRuntime.Python,
  super: OmniRuntime.Python,
  property: OmniRuntime.Python,
  staticmethod: OmniRuntime.Python,
  classmethod: OmniRuntime.Python,
  repr: OmniRuntime.Python,
  hash: OmniRuntime.Python,
  id: OmniRuntime.Python,
  map_: OmniRuntime.Python, // disambiguated from JS

  // JavaScript builtins
  console: OmniRuntime.JavaScript,
  setTimeout: OmniRuntime.JavaScript,
  setInterval: OmniRuntime.JavaScript,
  clearTimeout: OmniRuntime.JavaScript,
  clearInterval: OmniRuntime.JavaScript,
  fetch: OmniRuntime.JavaScript,
  require: OmniRuntime.JavaScript,
  Promise: OmniRuntime.JavaScript,
  JSON: OmniRuntime.JavaScript,
  Math: OmniRuntime.JavaScript,
  Date: OmniRuntime.JavaScript,
  RegExp: OmniRuntime.JavaScript,
  Error: OmniRuntime.JavaScript,
  Symbol: OmniRuntime.JavaScript,
  AbortController: OmniRuntime.JavaScript,
  AbortSignal: OmniRuntime.JavaScript,
  Worker: OmniRuntime.JavaScript,
  ReadableStream: OmniRuntime.JavaScript,
  Map: OmniRuntime.JavaScript,
  Set: OmniRuntime.JavaScript,
  WeakMap: OmniRuntime.JavaScript,
  WeakSet: OmniRuntime.JavaScript,
  Array: OmniRuntime.JavaScript,
  Object: OmniRuntime.JavaScript,
  Number: OmniRuntime.JavaScript,
  String: OmniRuntime.JavaScript,
  Boolean: OmniRuntime.JavaScript,
  parseInt: OmniRuntime.JavaScript,
  parseFloat: OmniRuntime.JavaScript,
  isNaN: OmniRuntime.JavaScript,
  isFinite: OmniRuntime.JavaScript,
  encodeURIComponent: OmniRuntime.JavaScript,
  decodeURIComponent: OmniRuntime.JavaScript,
  encodeURI: OmniRuntime.JavaScript,
  decodeURI: OmniRuntime.JavaScript,
  atob: OmniRuntime.JavaScript,
  btoa: OmniRuntime.JavaScript,
  document: OmniRuntime.JavaScript,
  window: OmniRuntime.JavaScript,
  globalThis: OmniRuntime.JavaScript,
  process: OmniRuntime.JavaScript,
  Buffer: OmniRuntime.JavaScript,
  __dirname: OmniRuntime.JavaScript,
  __filename: OmniRuntime.JavaScript,
  module: OmniRuntime.JavaScript,
  exports: OmniRuntime.JavaScript,

  // Go builtins
  make: OmniRuntime.Go,
  append: OmniRuntime.Go,
  cap: OmniRuntime.Go,
  close: OmniRuntime.Go,
  complex: OmniRuntime.Go,
  copy: OmniRuntime.Go,
  delete: OmniRuntime.Go,
  imag: OmniRuntime.Go,
  new: OmniRuntime.Go,
  panic: OmniRuntime.Go,
  real: OmniRuntime.Go,
  recover: OmniRuntime.Go,
  recv: OmniRuntime.Go,
  send: OmniRuntime.Go,
  wait: OmniRuntime.Go,
  fmt: OmniRuntime.Go,

  // Ruby builtins
  puts: OmniRuntime.Ruby,
  gets: OmniRuntime.Ruby,
  p: OmniRuntime.Ruby,
  pp: OmniRuntime.Ruby,
  raise: OmniRuntime.Ruby,
  Fiber: OmniRuntime.Ruby,
  attr_reader: OmniRuntime.Ruby,
  attr_writer: OmniRuntime.Ruby,
  attr_accessor: OmniRuntime.Ruby,
  require_relative: OmniRuntime.Ruby,
  include: OmniRuntime.Ruby,
  extend: OmniRuntime.Ruby,
  prepend: OmniRuntime.Ruby,
  yield: OmniRuntime.Ruby,
  block_given: OmniRuntime.Ruby,
  lambda: OmniRuntime.Ruby,
  proc: OmniRuntime.Ruby,

  // Java builtins (typically accessed via class names)
  System: OmniRuntime.Java,
  Thread: OmniRuntime.Java,
  Runnable: OmniRuntime.Java,
  StringBuilder: OmniRuntime.Java,
  ArrayList: OmniRuntime.Java,
  HashMap: OmniRuntime.Java,
  LinkedList: OmniRuntime.Java,
  HashSet: OmniRuntime.Java,
  TreeMap: OmniRuntime.Java,
  Collections: OmniRuntime.Java,
  Arrays: OmniRuntime.Java,
  Optional: OmniRuntime.Java,
  Stream: OmniRuntime.Java,
  Collectors: OmniRuntime.Java,
  Integer: OmniRuntime.Java,
  Long: OmniRuntime.Java,
  Double: OmniRuntime.Java,
  Float: OmniRuntime.Java,
  Byte: OmniRuntime.Java,
  Character: OmniRuntime.Java,
  Short: OmniRuntime.Java,
};

/**
 * Package/global roots that are meaningful even when they are not imported.
 *
 * Java code often uses fully-qualified class names such as `java.*`, `org.*`,
 * or `com.*`. Those roots should carry Java affinity before member propagation
 * has a chance to fall back to the default runtime.
 */
export const GLOBAL_AFFINITY: Record<string, OmniRuntime> = {
  // JavaScript constructor/object roots and host globals.
  console: OmniRuntime.JavaScript,
  Promise: OmniRuntime.JavaScript,
  JSON: OmniRuntime.JavaScript,
  Math: OmniRuntime.JavaScript,
  Date: OmniRuntime.JavaScript,
  RegExp: OmniRuntime.JavaScript,
  Error: OmniRuntime.JavaScript,
  Symbol: OmniRuntime.JavaScript,
  AbortController: OmniRuntime.JavaScript,
  AbortSignal: OmniRuntime.JavaScript,
  Worker: OmniRuntime.JavaScript,
  ReadableStream: OmniRuntime.JavaScript,
  Map: OmniRuntime.JavaScript,
  Set: OmniRuntime.JavaScript,
  WeakMap: OmniRuntime.JavaScript,
  WeakSet: OmniRuntime.JavaScript,
  Array: OmniRuntime.JavaScript,
  Object: OmniRuntime.JavaScript,
  Number: OmniRuntime.JavaScript,
  String: OmniRuntime.JavaScript,
  Boolean: OmniRuntime.JavaScript,
  document: OmniRuntime.JavaScript,
  window: OmniRuntime.JavaScript,
  globalThis: OmniRuntime.JavaScript,
  process: OmniRuntime.JavaScript,
  Buffer: OmniRuntime.JavaScript,
  module: OmniRuntime.JavaScript,
  exports: OmniRuntime.JavaScript,

  // Go package-like roots commonly used without an import in examples.
  fmt: OmniRuntime.Go,

  // Ruby core class roots.
  Fiber: OmniRuntime.Ruby,

  // Java package roots.
  java: OmniRuntime.Java,
  javax: OmniRuntime.Java,
  jakarta: OmniRuntime.Java,
  org: OmniRuntime.Java,
  com: OmniRuntime.Java,

  // Java classes commonly used as member roots.
  System: OmniRuntime.Java,
  Thread: OmniRuntime.Java,
  Runnable: OmniRuntime.Java,
  StringBuilder: OmniRuntime.Java,
  ArrayList: OmniRuntime.Java,
  HashMap: OmniRuntime.Java,
  LinkedList: OmniRuntime.Java,
  HashSet: OmniRuntime.Java,
  TreeMap: OmniRuntime.Java,
  Collections: OmniRuntime.Java,
  Arrays: OmniRuntime.Java,
  Optional: OmniRuntime.Java,
  Stream: OmniRuntime.Java,
  Collectors: OmniRuntime.Java,
  Integer: OmniRuntime.Java,
  Long: OmniRuntime.Java,
  Double: OmniRuntime.Java,
  Float: OmniRuntime.Java,
  Byte: OmniRuntime.Java,
  Character: OmniRuntime.Java,
  Short: OmniRuntime.Java,
};

const QUALIFIED_GLOBAL_AFFINITY: Array<[string[], OmniRuntime]> = [
  [["Thread", "current"], OmniRuntime.Ruby],
  [["io", "reactivex"], OmniRuntime.Java],
  [["io", "grpc"], OmniRuntime.Java],
  [["io", "netty"], OmniRuntime.Java],
];

/**
 * Method names that are ambiguous across runtimes.
 * These should not be used as strong evidence alone.
 */
export const AMBIGUOUS_METHODS = new Set([
  // Common ecosystem model/row field names. These are valid methods in some
  // runtimes, but should not create runtime affinity without object provenance.
  "then",
  "items",
  "keys",
  "values",
  "entries",
  "count",
  "split",
  "join",
  "replace",
  "indexOf",
  "includes",
  "slice",
  "splice",
  "push",
  "pop",
  "shift",
  "sort",
  "reverse",
  "length",
  "size",
  "concat",
  "trim",
  "match",
  "search",
  "index",
  "insert",
  "remove",
  "clear",
  "get",
  "set",
  "close",
  "add",
  "contains",
  "isEmpty",
  "toUpperCase",
  "toLowerCase",
]);

/**
 * Look up a method name's runtime affinity.
 * Returns undefined for ambiguous or unknown methods.
 */
export function lookupMethodAffinity(methodName: string): OmniRuntime | undefined {
  if (AMBIGUOUS_METHODS.has(methodName)) {
    return undefined;
  }
  return METHOD_AFFINITY[methodName];
}

/**
 * Look up a builtin function's runtime affinity.
 */
export function lookupBuiltinAffinity(name: string): OmniRuntime | undefined {
  return BUILTIN_AFFINITY[name];
}

/**
 * Look up a globally available runtime root.
 */
export function lookupGlobalAffinity(name: string): OmniRuntime | undefined {
  return GLOBAL_AFFINITY[name];
}

/**
 * Look up qualified roots whose first segment is too ambiguous to expose as a
 * bare global, such as Java's io.reactivex versus Python/Go io.
 */
export function lookupQualifiedGlobalAffinity(parts: string[]): OmniRuntime | undefined {
  for (const [prefix, runtime] of QUALIFIED_GLOBAL_AFFINITY) {
    if (parts.length >= prefix.length && prefix.every((part, idx) => parts[idx] === part)) {
      return runtime;
    }
  }
  return undefined;
}
