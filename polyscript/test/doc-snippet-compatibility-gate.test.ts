import { Lexer } from "../src/lexer";
import { Parser } from "../src/parser";
import { RuntimeResolver } from "../src/runtime-resolver";
import { ManifestCodeGenerator } from "../src/codegen-omnivm";
import { collectFreeIdentifiers } from "../src/codegen-omnivm/source-reconstruct";

function compileSnippet(code: string) {
  const tokens = new Lexer(code).tokenize();
  const ast = new Parser(tokens, code).parse();
  const resolver = new RuntimeResolver();
  const annotated = resolver.resolve(ast, code);
  const manifest = new ManifestCodeGenerator().generate(annotated);
  return { annotated, manifest };
}

function manifestText(manifest: any): string {
  return JSON.stringify(manifest);
}

function opCodes(manifest: any): string[] {
  return manifest.ops
    .map((op: any) => op.code ?? op.source ?? "")
    .filter((code: unknown): code is string => typeof code === "string" && code.length > 0);
}

function allOpCodes(manifest: any): string[] {
  const codes: string[] = [];
  const visit = (node: any) => {
    if (!node || typeof node !== "object") {
      return;
    }
    if (typeof node.code === "string" && node.code.length > 0) {
      codes.push(node.code);
    }
    if (typeof node.source === "string" && node.source.length > 0) {
      codes.push(node.source);
    }
    for (const value of Object.values(node)) {
      if (Array.isArray(value)) {
        value.forEach(visit);
      } else if (value && typeof value === "object") {
        visit(value);
      }
    }
  };
  visit(manifest);
  return codes;
}

function allOps(manifest: any): any[] {
  const ops: any[] = [];
  const visit = (node: any) => {
    if (!node || typeof node !== "object") {
      return;
    }
    if (typeof node.op === "string") {
      ops.push(node);
    }
    for (const value of Object.values(node)) {
      if (Array.isArray(value)) {
        value.forEach(visit);
      } else if (value && typeof value === "object") {
        visit(value);
      }
    }
  };
  visit(manifest);
  return ops;
}

describe("doc-snippet compatibility gate", () => {
  const bridgeHelperPattern = /(?:omnivm\.proxyGet|proxy_get|omnivm_get|OmniVM\.proxyGet)/;
  const nativeMemoryHelperPattern = /(?:to_arrow\(|to_buffer\(|release_buffer\(|buffer_owner\(|get_buffer\()/;

  test("keeps collision-heavy proxy access natural in mixed docs-shaped snippets", () => {
    const { manifest } = compileSnippet(`
import pandas as pd

def callable_then(value=None):
  return f"called:{value}"

def make_payload():
  return {
    "then": callable_then,
    "items": ["alpha", "beta"],
    "keys": ["id", "name"],
    "get": "field-get",
    "close": "field-close",
    "length": 2,
    "count": 7
  }

const frame = pd.DataFrame([{"then": "field-then", "items": "field-items", "keys": "field-keys"}])
const filtered_frame = frame[frame["items"] == "field-items"]
const selected_columns = frame.loc[frame["keys"] == "field-keys", ["items", "keys"]]
const payload = make_payload()
const natural = JSON.stringify({
  "then": payload.then("manual"),
  "items": payload.items.length,
  "keys": payload.keys.length,
  "get": payload.get,
  "close": payload.close,
  "length": payload.length,
  "count": payload.count,
  "rowThen": frame.to_dict("records")[0].then
})
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const pandasProducer = allOps(manifest).find(
      (op: any) => op.runtime === "python" && String(op.code ?? op.source ?? "").includes("pd.DataFrame")
    );
    expect(pandasProducer).toBeDefined();

    const jsOps = manifest.ops.filter((op: any) => op.runtime === "javascript");
    const jsText = jsOps.map((op: any) => op.code ?? op.source ?? "").join("\n");
    expect(jsText).toContain('payload.then("manual")');
    expect(jsText).toContain("payload.items.length");
    expect(jsText).toContain("payload.keys.length");
    expect(jsText).toContain("payload.get");
    expect(jsText).toContain("payload.close");
    expect(jsText).toContain("payload.length");
    expect(jsText).toContain("payload.count");
    expect(jsText).toContain('frame.to_dict("records")[0].then');

    const pyText = allOps(manifest)
      .filter((op: any) => op.runtime === "python")
      .map((op: any) => op.code ?? op.source ?? "")
      .join("\n");
    expect(pyText).toContain('frame[frame["items"] == "field-items"]');
    expect(pyText).toContain('frame.loc[frame["keys"] == "field-keys", ["items", "keys"]]');
  });

  test("keeps JavaScript destructuring and spread natural over cross-runtime proxies", () => {
    const { manifest } = compileSnippet(`
def make_payload():
  return {
    "items": ["alpha", "beta"],
    "keys": ["id"],
    "count": 2,
    "close": "field-close"
  }

const payload = make_payload()
const { items, keys: field_keys, count, close, missing = "fallback" } = payload
const spread_items = [...items]
const copy = { ...payload }
const summary = \`\${items.length}:\${field_keys.length}:\${count}:\${close}:\${missing}:\${spread_items[0]}:\${copy.items.length}:\${copy.keys.length}\`
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    expect(ops.some((op: any) =>
      op.runtime === "python" && String(op.code ?? "").includes("make_payload()")
    )).toBe(true);

    const jsCodes = ops
      .filter((op: any) => op.runtime === "javascript")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(jsCodes).toContain("payload.items");
    expect(jsCodes).toContain("payload.keys");
    expect(jsCodes).toContain("payload.count");
    expect(jsCodes).toContain("payload.close");
    expect(jsCodes).toContain('(typeof payload.missing === "undefined" ? "fallback" : payload.missing)');
    expect(jsCodes).toContain("[...items]");
    expect(jsCodes).toContain("{...payload}");
    expect(jsCodes).not.toContain("{...: ...payload}");

    for (const name of ["items", "field_keys", "count", "close", "missing", "copy"]) {
      expect(ops.some((op: any) => op.op === "eval" && op.runtime === "javascript" && op.bind === name)).toBe(true);
    }
    const destructured = ops.filter((op: any) =>
      op.op === "eval" &&
      op.runtime === "javascript" &&
      ["items", "field_keys", "count", "close", "missing"].includes(op.bind)
    );
    expect(destructured.every((op: any) => op.captures?.payload === "payload")).toBe(true);
  });

  test("keeps JavaScript array rest/default destructuring natural over cross-runtime proxies", () => {
    const { manifest } = compileSnippet(`
def make_rows():
  return [
    {"items": "alpha", "count": 1, "close": "row-close"},
    {"items": "beta", "count": 2, "close": "row-close"},
    {"items": "gamma", "count": 3, "close": "row-close"}
  ]

def empty_rows():
  return []

const [first, second, ...rest] = make_rows()
const [fallback = {"items": "fallback", "count": 0}] = empty_rows()
const { items: first_items, count: first_count } = first
const summary = \`\${first_items}:\${first_count}:\${second.items}:\${fallback.items}:\${rest.length}:\${rest[0].items}\`
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    expect(ops.some((op: any) =>
      op.runtime === "python" && String(op.code ?? "").includes("make_rows()")
    )).toBe(true);
    expect(ops.some((op: any) =>
      op.runtime === "python" && String(op.code ?? "").includes("empty_rows()")
    )).toBe(true);

    const jsCodes = ops
      .filter((op: any) => op.runtime === "javascript")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(jsCodes).toContain("[0]");
    expect(jsCodes).toContain("[1]");
    expect(jsCodes).toContain("Array.from(__destructure_");
    expect(jsCodes).toContain(".slice(2)");
    expect(jsCodes).toContain('(typeof __destructure_');
    expect(jsCodes).toContain(' === "undefined" ? {"items": "fallback", "count": 0} : ');
    expect(jsCodes).toContain("first.items");
    expect(jsCodes).toContain("first.count");
    expect(jsCodes).toContain("rest[0].items");

    for (const name of ["first", "second", "rest", "fallback", "first_items", "first_count"]) {
      expect(ops.some((op: any) => op.op === "eval" && op.runtime === "javascript" && op.bind === name)).toBe(true);
    }
  });

  test("keeps JavaScript optional call and nullish access natural over cross-runtime proxies", () => {
    const { manifest } = compileSnippet(`
def callable_then(value=None):
  return f"called:{value}"

def make_payload(include_then):
  payload = {
    "items": ["alpha", "beta"],
    "keys": ["id"],
    "count": 2,
    "close": "field-close"
  }
  if include_then:
    payload["then"] = callable_then
  return payload

const payload = make_payload(True)
const missing_payload = make_payload(False)
const called = payload.then?.("manual") ?? "missing"
const missing_called = missing_payload.then?.("manual") ?? "missing"
const item_count = missing_payload.items?.length ?? 0
const missing_count = missing_payload.missing?.length ?? 0
const summary = \`\${called}:\${missing_called}:\${item_count}:\${missing_count}:\${payload.close}\`
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    expect(ops.some((op: any) =>
      op.runtime === "python" && String(op.code ?? op.source ?? "").includes("make_payload(True)")
    )).toBe(true);
    expect(ops.some((op: any) =>
      op.runtime === "python" && String(op.code ?? op.source ?? "").includes("make_payload(False)")
    )).toBe(true);

    const jsCodes = ops
      .filter((op: any) => op.runtime === "javascript")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(jsCodes).toContain('payload.then?.("manual") ?? "missing"');
    expect(jsCodes).toContain('missing_payload.then?.("manual") ?? "missing"');
    expect(jsCodes).toContain("missing_payload.items?.length ?? 0");
    expect(jsCodes).toContain("missing_payload.missing?.length ?? 0");
    expect(jsCodes).toContain("payload.close");
  });

  test("keeps JavaScript object enumeration natural over Python mapping proxies", () => {
    const { manifest } = compileSnippet(`
def make_payload():
  return {
    "items": ["alpha", "beta"],
    "keys": ["id", "name"],
    "then": "field-then",
    "get": "field-get",
    "close": "field-close",
    "length": 2,
    "count": 7,
    "rows": [
      {"items": "first", "count": 1},
      {"items": "second", "count": 2}
    ]
  }

payload = make_payload()

const names = Object.keys(payload).sort()
const pairs = Object.entries(payload)
const selected = Object.fromEntries(
  pairs
    .filter(([key]) => ["items", "keys", "then", "get", "close", "length", "count"].includes(key))
    .map(([key, value]) => [key, Array.isArray(value) ? value.length : value])
)
const copied = {...payload}
const assigned = Object.assign({}, payload)
const values_count = Object.values(payload).length
const row_summary = payload.rows.map(row => \`\${row.items}:\${row.count}\`).join("|")
const has_items = Object.prototype.hasOwnProperty.call(payload, "items")
const loop_names = []
for (const key in payload) {
  loop_names.push(key)
}
loop_names.sort()
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    expect(ops.some((op: any) =>
      op.runtime === "python" && String(op.code ?? op.source ?? "").includes("make_payload()")
    )).toBe(true);

    const jsCodes = ops
      .filter((op: any) => op.runtime === "javascript")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(jsCodes).toContain("Object.keys(payload).sort()");
    expect(jsCodes).toContain("Object.entries(payload)");
    expect(jsCodes).toContain("Object.fromEntries(");
    expect(jsCodes).toContain("{...payload}");
    expect(jsCodes).toContain("Object.assign({}, payload)");
    expect(jsCodes).toContain("Object.values(payload).length");
    expect(jsCodes).toContain("payload.rows.map");
    expect(jsCodes).toContain('Object.prototype.hasOwnProperty.call(payload, "items")');
    expect(jsCodes).toContain("for (const key in payload)");
    expect(jsCodes).toContain("loop_names.push(key)");
  });

  test("keeps Python dict-style iteration natural over JavaScript object proxies", () => {
    const { manifest } = compileSnippet(`
const payload = Object.freeze({
  "items": ["alpha", "beta"],
  "keys": ["id", "name"],
  "then": "field-then",
  "get": "field-get",
  "close": "field-close",
  "length": 2,
  "count": 7,
  "rows": [
    {"items": "first", "count": 1},
    {"items": "second", "count": 2}
  ]
})

names = []
for key in payload:
  names.append(key)
names = sorted(names)

selected_parts = []
for key in ["then", "get", "close", "length", "count"]:
  selected_parts.append(f"{key}={payload[key]}")

rows = payload.rows
row_labels = []
for row in rows:
  row_labels.append(f"{row.items}:{row.count}")
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    const jsProducer = ops.find((op: any) =>
      op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("Object.freeze")
    );
    expect(jsProducer).toBeDefined();

    const loops = ops.filter((op: any) => op.op === "loop" && op.mode === "foreach");
    expect(loops).toEqual(expect.arrayContaining([
      expect.objectContaining({
        variable: "key",
        iterable: expect.objectContaining({ kind: "ref", name: "payload" }),
        iterationMode: "auto",
      }),
      expect.objectContaining({
        variable: "row",
        iterable: expect.objectContaining({ kind: "ref", name: "rows" }),
        iterationMode: "auto",
      }),
    ]));

    const pyCodes = ops
      .filter((op: any) => op.runtime === "python")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(pyCodes).toContain("names.append(key)");
    expect(pyCodes).toContain("sorted(names)");
    expect(pyCodes).toContain('selected_parts.append(f"{key}={payload[key]}")');
    expect(pyCodes).toContain('row_labels.append(f"{row.items}:{row.count}")');
  });

  test("keeps Python mapping methods natural over JavaScript object proxies", () => {
    const { manifest } = compileSnippet(`
const payload = Object.freeze({
  "alpha": "first",
  "beta": "second",
  "then": "field-then",
  "close": "field-close",
  "length": 2,
  "count": 7
})

keys = sorted(payload.keys())
pairs = sorted([f"{key}:{value}" for key, value in payload.items()])
values = sorted([str(value) for value in payload.values()])
selected = f"{payload.get('alpha')}:{payload.get('missing', 'fallback')}:{payload.close}:{payload.count}"
copied = dict(payload)
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    const jsProducer = ops.find((op: any) =>
      op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("Object.freeze")
    );
    expect(jsProducer).toBeDefined();

    const pyCodes = ops
      .filter((op: any) => op.runtime === "python")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(pyCodes).toContain("sorted(payload.keys())");
    expect(pyCodes).toContain('sorted([f"{key}:{value}" for key, value in payload.items()])');
    expect(pyCodes).toContain("sorted([str(value) for value in payload.values()])");
    expect(pyCodes).toContain("payload.get('alpha')");
    expect(pyCodes).toContain("payload.get('missing', 'fallback')");
    expect(pyCodes).toContain("payload.close");
    expect(pyCodes).toContain("payload.count");
    expect(pyCodes).toContain("dict(payload)");
  });

  test("keeps Python mapping methods natural over JavaScript Map proxies", () => {
    const { manifest } = compileSnippet(`
const payload = new Map([
  ["alpha", "first"],
  ["beta", "second"],
  ["close", "field-close"],
  ["count", 7]
])

keys = sorted(payload.keys())
pairs = sorted([f"{key}:{value}" for key, value in payload.items()])
values = sorted([str(value) for value in payload.values()])
selected = f"{payload.get('alpha')}:{payload.get('missing', 'fallback')}:{payload.get('close')}:{payload.get('count')}"
copied = dict(payload)
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    const jsProducer = ops.find((op: any) =>
      op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("new Map")
    );
    expect(jsProducer).toBeDefined();

    const pyCodes = ops
      .filter((op: any) => op.runtime === "python")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(pyCodes).toContain("sorted(payload.keys())");
    expect(pyCodes).toContain('sorted([f"{key}:{value}" for key, value in payload.items()])');
    expect(pyCodes).toContain("sorted([str(value) for value in payload.values()])");
    expect(pyCodes).toContain("payload.get('alpha')");
    expect(pyCodes).toContain("payload.get('missing', 'fallback')");
    expect(pyCodes).toContain("payload.get('close')");
    expect(pyCodes).toContain("payload.get('count')");
    expect(pyCodes).toContain("dict(payload)");
  });

  test("keeps Ruby mapping methods natural over JavaScript object proxies", () => {
    const { manifest } = compileSnippet(`
const payload = Object.freeze({
  "alpha": "first",
  "beta": "second",
  "then": "field-then",
  "close": "field-close",
  "length": 2,
  "count": 7
})

class PayloadSummary
  def self.summarize(payload)
    ruby_keys = payload.keys.sort
    ruby_pairs = payload.each.map { |key, value| "#{key}:#{value}" }.sort
    ruby_values = payload.values.map { |value| value.to_s }.sort
    ruby_selected = "#{payload.fetch('alpha')}:#{payload.fetch('missing', 'fallback')}:#{payload.close}:#{payload.count}"
    ruby_copied = payload.to_h
    [ruby_keys.join(","), ruby_pairs.join("|"), ruby_values.join("|"), ruby_selected, ruby_copied["beta"]].join(";")
  end
end

const summary = PayloadSummary.summarize(payload)
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    const jsProducer = ops.find((op: any) =>
      op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("Object.freeze")
    );
    expect(jsProducer).toBeDefined();

    const rubyCodes = ops
      .filter((op: any) => op.runtime === "ruby")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(ops.some((op: any) =>
      op.op === "native" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").includes("class PayloadSummary")
    )).toBe(true);
    expect(ops.some((op: any) =>
      op.op === "eval" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").includes("PayloadSummary.summarize(payload)")
    )).toBe(true);
    expect(rubyCodes).toContain("payload.keys.sort");
    expect(rubyCodes).toContain('payload.each.map { |key, value| "#{key}:#{value}" }.sort');
    expect(rubyCodes).toContain("payload.values.map { |value| value.to_s }.sort");
    expect(rubyCodes).toContain("payload.fetch('alpha')");
    expect(rubyCodes).toContain("payload.fetch('missing', 'fallback')");
    expect(rubyCodes).toContain("payload.close");
    expect(rubyCodes).toContain("payload.count");
    expect(rubyCodes).toContain("payload.to_h");
  });

  test("keeps Java mapping methods natural over JavaScript object proxies", () => {
    const { manifest } = compileSnippet(`
const payload = Object.freeze({
  "alpha": "first",
  "beta": "second",
  "then": "field-then",
  "close": "field-close",
  "length": 2,
  "count": 7
})

const java_payload = java.util.Map.class.cast(payload)
const java_keys = new java.util.TreeSet(java_payload.keySet()).toString()
const java_pairs = java_payload.entrySet().stream().map(entry -> entry.getKey() + ":" + entry.getValue()).sorted().collect(java.util.stream.Collectors.joining("|"))
const java_values = java_payload.values().stream().map(value -> value.toString()).sorted().collect(java.util.stream.Collectors.joining("|"))
const java_selected = java.util.List.of(java_payload.get("alpha"), java_payload.getOrDefault("missing", "fallback"), java_payload.get("close"), java_payload.get("count")).stream().map(value -> value.toString()).collect(java.util.stream.Collectors.joining(":"))
const java_copied = java_payload.get("beta")
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const ops = allOps(manifest);
    expect(ops.some((op: any) =>
      op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("Object.freeze")
    )).toBe(true);
    expect(ops.some((op: any) =>
      op.op === "eval" &&
      op.runtime === "java" &&
      String(op.code ?? "").includes("payload.keySet()")
    )).toBe(true);

    const javaCodes = ops
      .filter((op: any) => op.runtime === "java")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(javaCodes).toContain("java.util.Map.class.cast(payload)");
    expect(javaCodes).toContain("java_payload.keySet()");
    expect(javaCodes).toContain("java_payload.entrySet()");
    expect(javaCodes).toContain("java_payload.values()");
    expect(javaCodes).toContain('java_payload.get("alpha")');
    expect(javaCodes).toContain('java_payload.getOrDefault("missing", "fallback")');
    expect(javaCodes).toContain('java_payload.get("close")');
    expect(javaCodes).toContain('java_payload.get("count")');
  });

  test("keeps lazy iterable snippets lazy and helper-free across a runtime boundary", () => {
    const { manifest } = compileSnippet(`
import itertools

def row_stream():
  for value in range(4):
    yield {"items": value, "keys": str(value), "count": value + 1}

def recent_orders(Order):
  return Order.objects.filter(status="open").iterator()

const first_rows = Array.from(row_stream()).slice(0, 2)
const labels = first_rows.map(row => \`\${row.items}:\${row.keys}:\${row.count}\`)
const queryset_rows = Array.from(recent_orders(Order)).slice(0, 2)
const queryset_labels = queryset_rows.map(row => row.items + ":" + row.count)

const result_set = java.sql.DriverManager.getConnection(url).createStatement().executeQuery("select id, count from orders")
while (result_set.next()) {
  const row_id = result_set.getLong("id")
  const row_count = result_set.count
  const row_items = result_set.items
}
result_set.close()
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("Array.from(row_stream()).slice(0, 2)");
    expect(codes).toMatch(/first_rows\.map\(\(?row\)? =>/);
    expect(codes).toContain("row.items");
    expect(codes).toContain("row.keys");
    expect(codes).toContain("row.count");
    expect(codes).toContain('Order.objects.filter(status = "open").iterator()');
    expect(codes).toContain("Array.from(recent_orders(Order)).slice(0, 2)");
    expect(codes).toContain('queryset_rows.map((row) => ((row.items + ":") + row.count))');
    expect(codes).toContain('java.sql.DriverManager.getConnection(url).createStatement().executeQuery("select id, count from orders")');
    expect(codes).toContain("result_set.next()");
    expect(codes).toContain('result_set.getLong("id")');
    expect(codes).toContain("result_set.count");
    expect(codes).toContain("result_set.items");
    expect(codes).toContain("result_set.close()");

    const streamConsumer = manifest.ops.find(
      (op: any) => op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("Array.from(row_stream())")
    );
    expect(streamConsumer).toBeDefined();

    const querysetProducer = allOps(manifest).find(
      (op: any) => op.runtime === "python" && String(op.code ?? op.source ?? "").includes("Order.objects.filter")
    );
    expect(querysetProducer).toBeDefined();

    const querysetConsumer = manifest.ops.find(
      (op: any) => op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("Array.from(recent_orders(Order))")
    );
    expect(querysetConsumer).toBeDefined();

    const resultSetOps = allOps(manifest).filter((op: any) =>
      op.runtime === "java" && JSON.stringify(op).includes("result_set")
    );
    expect(resultSetOps.length).toBeGreaterThanOrEqual(2);
    expect(allOps(manifest).some((op: any) =>
      op.op === "loop" &&
      op.test?.runtime === "java" &&
      JSON.stringify(op.test).includes("result_set.next()")
    )).toBe(true);
  });

  test("keeps JavaScript for-of over Python generators native and cancellable", () => {
    const { manifest } = compileSnippet(`
closed = []

def row_stream():
  try:
    for value in range(4):
      yield {"items": value, "close": "row-close"}
  finally:
    closed.append("closed")

const seen = []
for (const row of row_stream()) {
  seen.push(\`\${row.items}:\${row.close}\`)
  if (seen.length === 2) {
    break
  }
}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const pyProducer = allOps(manifest).find(
      (op: any) => op.op === "func_def" &&
        op.bodyRuntime === "python" &&
        String(op.sourceArtifact?.functionSource ?? "").includes("def row_stream")
    );
    expect(pyProducer).toBeDefined();

    const closedInit = allOps(manifest).find(
      (op: any) => op.op === "eval" &&
        op.bind === "closed" &&
        String(op.code ?? "") === "[]"
    );
    expect(closedInit).toBeDefined();
    expect(closedInit.runtime).toBe("python");

    const jsLoop = allOps(manifest).find(
      (op: any) => op.op === "exec" &&
        op.runtime === "javascript" &&
        String(op.code ?? "").includes("for (const row of row_stream())")
    );
    expect(jsLoop).toBeDefined();
    expect(String(jsLoop.code)).toContain("seen.push(`${row.items}:${row.close}`)");
    expect(String(jsLoop.code)).toContain("break");
    expect(jsLoop.captures).toMatchObject({ row_stream: "row_stream" });

    expect(allOps(manifest).some((op: any) =>
      op.op === "loop" &&
      JSON.stringify(op.iterable ?? {}).includes("row_stream()")
    )).toBe(false);
  });

  test("keeps Python itertools partial consumption over JavaScript generators natural and cancellable", () => {
    const { manifest } = compileSnippet(`
import itertools

closed = []
produced = []

function* js_rows(label) {
  try {
    for (const value of [0, 1, 2, 3]) {
      produced.push(value)
      yield {"items": value, "close": label, "count": value + 1}
    }
  } finally {
    closed.push(label)
  }
}

first_rows = list(itertools.islice(js_rows("slice"), 2))
labels = [f"{row.items}:{row.close}:{row.count}" for row in first_rows]
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const jsProducer = allOps(manifest).find(
      (op: any) => op.op === "func_def" &&
        op.generator === true &&
        op.name === "js_rows" &&
        String(op.sourceArtifact?.functionSource ?? "").includes("function* js_rows")
    );
    expect(jsProducer).toBeDefined();
    expect(jsProducer?.bodyRuntime).toBe("javascript");

    const pyCodes = allOps(manifest)
      .filter((op: any) => op.runtime === "python")
      .map((op: any) => String(op.code ?? op.source ?? ""))
      .join("\n");
    expect(pyCodes).toContain("list(itertools.islice(js_rows(\"slice\"), 2))");
    expect(pyCodes).toContain('[f"{row.items}:{row.close}:{row.count}" for row in first_rows]');

    const partialConsume = allOps(manifest).find(
      (op: any) =>
        op.runtime === "python" &&
        String(op.code ?? "").includes("itertools.islice(js_rows")
    );
    expect(partialConsume).toBeDefined();
    expect(JSON.stringify(manifest.ops)).not.toContain("omnivm.proxyGet");
    expect(JSON.stringify(manifest.ops)).not.toContain("proxyGet");
  });

  test("keeps Python async for over JavaScript async generators natural and cancellable", () => {
    const { manifest } = compileSnippet(`
import asyncio

closed = []
produced = []

async function* js_async_rows(label) {
  try {
    for (const value of [0, 1, 2, 3]) {
      produced.push(value)
      await Promise.resolve()
      yield {"items": value, "close": label, "count": value + 1}
    }
  } finally {
    closed.push(label)
  }
}

async def collect_rows():
  labels = []
  async for row in js_async_rows("async"):
    labels.append(f"{row.items}:{row.close}:{row.count}")
    if len(labels) == 2:
      break
  return labels

labels = asyncio.run(collect_rows())
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const jsProducer = allOps(manifest).find(
      (op: any) => op.op === "func_def" &&
        op.async === true &&
        op.generator === true &&
        op.name === "js_async_rows" &&
        String(op.sourceArtifact?.functionSource ?? "").includes("async function* js_async_rows")
    );
    expect(jsProducer).toBeDefined();
    expect(jsProducer?.bodyRuntime).toBe("javascript");

    const pyConsumer = allOps(manifest).find(
      (op: any) => op.op === "func_def" &&
        op.async === true &&
        op.name === "collect_rows"
    );
    expect(pyConsumer).toBeDefined();
    expect(pyConsumer?.bodyRuntime).toBe("python");
    const asyncLoop = pyConsumer?.body.find((op: any) =>
      op.op === "loop" &&
      op.await === true &&
      op.iterable?.name === 'js_async_rows("async")'
    );
    expect(asyncLoop).toBeDefined();

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("asyncio.run(collect_rows())");
    expect(codes).toContain('labels.append(f"{row.items}:{row.close}:{row.count}")');
    expect(JSON.stringify(manifest.ops)).not.toContain("omnivm.proxyGet");
    expect(JSON.stringify(manifest.ops)).not.toContain("proxyGet");
  });

  test("keeps Java collection-style collision access natural from docs-shaped JavaScript", () => {
    const { manifest } = compileSnippet(`
const java_payload = new java.util.HashMap()
java_payload.put("items", ["alpha", "beta"])
java_payload.put("keys", ["id", "name"])
java_payload.put("get", "field-get")
java_payload.put("close", "field-close")
java_payload.put("length", 2)
java_payload.put("count", 7)

const summary = JSON.stringify({
  "items": java_payload.items.length,
  "keys": java_payload.keys.length,
  "get": java_payload.get,
  "close": java_payload.close,
  "length": java_payload.length,
  "count": java_payload.count
})
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const javaProducer = manifest.ops.find(
      (op: any) => op.runtime === "java" && String(op.code ?? op.source ?? "").includes("new java.util.HashMap()")
    );
    expect(javaProducer).toBeDefined();

    const javaText = manifest.ops
      .filter((op: any) => op.runtime === "java")
      .map((op: any) => op.code ?? op.source ?? "")
      .join("\n");
    expect(javaText).toContain('java_payload.put("items", java.util.Arrays.asList("alpha", "beta"))');
    expect(javaText).toContain('java_payload.put("keys", java.util.Arrays.asList("id", "name"))');
    expect(javaText).not.toContain('["alpha", "beta"]');

    const jsText = manifest.ops
      .filter((op: any) => op.runtime === "javascript")
      .map((op: any) => op.code ?? op.source ?? "")
      .join("\n");
    expect(jsText).toContain("java_payload.items.length");
    expect(jsText).toContain("java_payload.keys.length");
    expect(jsText).toContain("java_payload.get");
    expect(jsText).toContain("java_payload.close");
    expect(jsText).toContain("java_payload.length");
    expect(jsText).toContain("java_payload.count");
  });

  test("keeps Ruby Rack-style response access natural and helper-free", () => {
    const { manifest } = compileSnippet(`
require "rack"

const ruby_response = Rack::Response.new("hello", 200, {"items": "field-items", "keys": "field-keys"}).finish
const response_summary = JSON.stringify({
  "status": ruby_response[0],
  "headers": ruby_response[1].keys,
  "bodyCount": ruby_response[2].count,
  "length": ruby_response.length,
  "close": ruby_response.close
})
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    expect(manifest.ops.some((op: any) =>
      op.op === "import" && op.runtime === "ruby" && op.path === "rack"
    )).toBe(true);

    const rubyProducer = manifest.ops.find(
      (op: any) => op.runtime === "ruby" && String(op.code ?? op.source ?? "").includes("Rack::Response.new")
    );
    expect(rubyProducer).toBeDefined();

    const jsText = manifest.ops
      .filter((op: any) => op.runtime === "javascript")
      .map((op: any) => op.code ?? op.source ?? "")
      .join("\n");
    expect(jsText).toContain("ruby_response[0]");
    expect(jsText).toContain("ruby_response[1].keys");
    expect(jsText).toContain("ruby_response[2].count");
    expect(jsText).toContain("ruby_response.length");
    expect(jsText).toContain("ruby_response.close");
  });

  test("keeps JavaScript-produced collision objects natural in Python Ruby and Java consumers", () => {
    const { manifest } = compileSnippet(`
const js_payload = {
  "items": ["alpha", "beta"],
  "keys": ["id", "name"],
  "get": "field-get",
  "close": "field-close",
  "length": 2,
  "count": 7
}

const py_count = len(js_payload.items) + js_payload.count
const ruby_summary = Docs::Summary.build(js_payload.items, js_payload.keys, js_payload.count, js_payload.close)
const java_summary = java.util.List.of(js_payload.count, js_payload.length).stream().map(value -> value.toString()).collect(java.util.stream.Collectors.joining(":"))
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const pythonText = manifest.ops
      .filter((op: any) => op.runtime === "python")
      .map((op: any) => op.code ?? op.source ?? "")
      .join("\n");
    expect(pythonText).toContain("len(js_payload.items) + js_payload.count");

    const rubyText = manifest.ops
      .filter((op: any) => op.runtime === "ruby")
      .map((op: any) => op.code ?? op.source ?? "")
      .join("\n");
    expect(rubyText).toContain("Docs::Summary.build(js_payload.items, js_payload.keys, js_payload.count, js_payload.close)");

    const javaText = manifest.ops
      .filter((op: any) => op.runtime === "java")
      .map((op: any) => op.code ?? op.source ?? "")
      .join("\n");
    expect(javaText).toContain("java.util.List.of(js_payload.count, js_payload.length)");
    expect(javaText).toContain("java.util.stream.Collectors.joining");
  });

  test("keeps cross-runtime error fields natural and helper-free in docs-shaped catch blocks", () => {
    const { manifest } = compileSnippet(`
def validate_order(payload):
  raise ValueError("bad order")

try {
  validate_order({"id": None})
} catch (err) {
  const js_error_summary = JSON.stringify({
    "runtime": err.runtime,
    "origin": err.originRuntime,
    "type": err.type,
    "message": err.message,
    "traceback": err.traceback,
    "stack": err.stackFrames.length,
    "causes": err.causeChain.length,
    "boundary": err.boundaryPath,
    "details": err.details
  })
}

function raise_js_error() {
  const err = new Error("bad js")
  err.details = {"code": "E_JS"}
  throw err
}

begin
  raise_js_error()
rescue RuntimeError => rb_err
  const ruby_error_summary = Docs::Errors.capture(rb_err.runtime, rb_err.origin_runtime, rb_err.type, rb_err.message, rb_err.stack_frames, rb_err.cause_chain, rb_err.boundary_path, rb_err.details)
end

try:
  raise_js_error()
except RuntimeError as py_err:
  py_error_summary = f"{py_err.runtime}:{py_err.origin_runtime}:{py_err.type}:{py_err.boundary_path}:{len(py_err.stack_frames)}:{len(py_err.cause_chain)}:{py_err.details}"

try {
  failJava()
} catch (RuntimeException java_err) {
  const java_error_summary = java_err.getRuntime() + ":" + java_err.getOriginRuntime() + ":" + java_err.getType() + ":" + java_err.getMessage() + ":" + java_err.getBoundaryPath() + ":" + java_err.getDetails()
}

try {
  failJdbc()
} catch (IOException | SQLException db_err) {
  const java_multi_error_summary = db_err.getMessage() + ":" + db_err.getCause()
}

try {
  throw new Error("field-check")
} catch (js_err) {
  const js_native_error_summary = js_err.name + ":" + js_err.message + ":" + js_err.stack + ":" + js_err.boundary_path
}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("err.runtime");
    expect(codes).toContain("err.originRuntime");
    expect(codes).toContain("err.type");
    expect(codes).toContain("err.message");
    expect(codes).toContain("err.traceback");
    expect(codes).toContain("err.stackFrames.length");
    expect(codes).toContain("err.causeChain.length");
    expect(codes).toContain("err.boundaryPath");
    expect(codes).toContain("err.details");
    expect(codes).toContain("rb_err.runtime");
    expect(codes).toContain("rb_err.origin_runtime");
    expect(codes).toContain("rb_err.type");
    expect(codes).toContain("rb_err.message");
    expect(codes).toContain("rb_err.stack_frames");
    expect(codes).toContain("rb_err.cause_chain");
    expect(codes).toContain("rb_err.boundary_path");
    expect(codes).toContain("rb_err.details");
    expect(codes).toContain("py_err.runtime");
    expect(codes).toContain("py_err.origin_runtime");
    expect(codes).toContain("py_err.type");
    expect(codes).toContain("py_err.boundary_path");
    expect(codes).toContain("py_err.stack_frames");
    expect(codes).toContain("py_err.cause_chain");
    expect(codes).toContain("py_err.details");
    expect(codes).toContain("java_err.getRuntime()");
    expect(codes).toContain("java_err.getOriginRuntime()");
    expect(codes).toContain("java_err.getType()");
    expect(codes).toContain("java_err.getMessage()");
    expect(codes).toContain("java_err.getBoundaryPath()");
    expect(codes).toContain("java_err.getDetails()");
    expect(codes).toContain("db_err.getMessage()");
    expect(codes).toContain("db_err.getCause()");
    expect(codes).toContain('throw new Error("field-check")');
    expect(codes).toContain("js_err.name");
    expect(codes).toContain("js_err.message");
    expect(codes).toContain("js_err.stack");
    expect(codes).toContain("js_err.boundary_path");

    const tryOps = JSON.stringify(manifest.ops.filter((op: any) => op.op === "try"));
    expect(tryOps).toContain('"param":"err"');
    expect(tryOps).toContain('"param":"rb_err"');
    expect(tryOps).toContain('"param":"py_err"');
    expect(tryOps).toContain('"param":"java_err"');
    expect(tryOps).toContain('"param":"js_err"');
    expect(tryOps).toContain('"runtime":"ruby"');
    expect(tryOps).toContain('"runtime":"javascript"');
    expect(tryOps).toContain('"runtime":"java"');
    expect(tryOps).toContain('"errorType":"RuntimeException"');
    expect(tryOps).toContain('"errorType":"IOException | SQLException"');
    expect(tryOps).not.toContain('"value":{"kind":"literal","value":"new Error(\\"field-check\\")"}');
  });

  test("keeps lifecycle and cancellation snippets helper-free with generic cleanup lowering", () => {
    const { manifest } = compileSnippet(`
def open_session():
  return Session()

def open_transaction():
  return engine.begin()

with open_session() as session:
  const first_rows = Array.from(session.execute("select * from orders")).slice(0, 1)
  const session_close_field = session.close
  const session_count = session.count

with open_transaction() as tx:
  const tx_rows = Array.from(tx.execute("select * from orders")).slice(0, 1)
  const tx_count = tx.count
  tx.rollback()
  tx.commit()

const controller = new AbortController()
try {
  const first_chunk = Array.from(node_response.body).slice(0, 1)
  controller.abort()
} finally {
  node_response.body.cancel()
  node_response.close()
}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain('Array.from(session.execute("select * from orders")).slice(0, 1)');
    expect(codes).toContain("session.close");
    expect(codes).toContain("session.count");
    expect(codes).toContain("open_transaction()");
    expect(codes).toContain('Array.from(tx.execute("select * from orders")).slice(0, 1)');
    expect(codes).toContain("tx.count");
    expect(codes).toContain("tx.rollback()");
    expect(codes).toContain("tx.commit()");
    expect(codes).toContain("new AbortController()");
    expect(codes).toContain("Array.from(node_response.body).slice(0, 1)");
    expect(codes).toContain("controller.abort()");
    expect(codes).toContain("node_response.body.cancel()");
    expect(codes).toContain("node_response.close()");

    const tryOps = allOps(manifest).filter((op: any) => op.op === "try");
    expect(tryOps.length).toBeGreaterThanOrEqual(2);
    const usingTry = tryOps.find((op: any) =>
      JSON.stringify(op.body).includes("open_session") &&
      JSON.stringify(op.body).includes("__enter__") &&
      JSON.stringify(op.finallyBody ?? []).includes("__exit__(None, None, None)") &&
      JSON.stringify(op.finallyBody ?? []).includes('"action":"close"')
    );
    expect(usingTry).toBeDefined();

    const transactionTry = tryOps.find((op: any) =>
      JSON.stringify(op.body).includes("open_transaction") &&
      JSON.stringify(op.body).includes("__enter__") &&
      JSON.stringify(op.body).includes("tx.rollback") &&
      JSON.stringify(op.body).includes("tx.commit") &&
      JSON.stringify(op.finallyBody ?? []).includes("__exit__(None, None, None)") &&
      JSON.stringify(op.finallyBody ?? []).includes('"action":"close"')
    );
    expect(transactionTry).toBeDefined();

    const abortTry = tryOps.find((op: any) =>
      JSON.stringify(op.body).includes("node_response.body") &&
      JSON.stringify(op.finallyBody ?? []).includes("node_response.close")
    );
    expect(abortTry).toBeDefined();
  });

  test("keeps async context-manager snippets helper-free with generic cleanup lowering", () => {
    const { manifest } = compileSnippet(`
import aiohttp

async def fetch_docs(url):
  async with aiohttp.ClientSession() as session:
    async with session.get(url) as response:
      count = response.count
      close_field = response.close
      return await response.json()
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("aiohttp.ClientSession()");
    expect(codes).toContain("session.get(url)");
    expect(codes).toContain("response.count");
    expect(codes).toContain("response.close");
    expect(codes).toContain("await response.json()");

    const ops = allOps(manifest);
    const func = ops.find((op: any) => op.op === "func_def" && op.name === "fetch_docs");
    expect(func?.async).toBe(true);
    expect(func?.bodyRuntime).toBe("python");

    const tryOps = ops.filter((op: any) => op.op === "try");
    const sessionTry = tryOps.find((op: any) =>
      JSON.stringify(op.body).includes("aiohttp.ClientSession") &&
      JSON.stringify(op.body).includes("await __using_context_1.__aenter__()") &&
      JSON.stringify(op.finallyBody ?? []).includes("await __using_context_1.__aexit__(None, None, None)") &&
      JSON.stringify(op.finallyBody ?? []).includes('"action":"close"')
    );
    expect(sessionTry).toBeDefined();

    const responseTry = tryOps.find((op: any) =>
      JSON.stringify(op.body).includes("session.get") &&
      JSON.stringify(op.body).includes(".__aenter__()") &&
      JSON.stringify(op.finallyBody ?? []).includes(".__aexit__(None, None, None)") &&
      JSON.stringify(op.finallyBody ?? []).includes('"action":"close"')
    );
    expect(responseTry).toBeDefined();
    expect(responseTry?.body.every((op: any) => !op.runtime || op.runtime === "python")).toBe(true);
  });

  test("keeps SQLAlchemy and Node stream lifecycle snippets generic and helper-free", () => {
    const { manifest } = compileSnippet(`
import sqlalchemy as sa
from sqlalchemy import text
import { Readable } from "node:stream"
import { pipeline } from "node:stream/promises"
import fs from "node:fs"

engine = sa.create_engine(url)

def load_orders():
  with sa.orm.Session(engine) as session:
    with session.begin() as tx:
      rows = session.execute(text("select id, count from orders"))
      const first_rows = Array.from(rows).slice(0, 2)
      const count_field = rows.count
      const close_field = rows.close
      tx.commit()
      return first_rows

async function read_upload_chunks() {
  const upload = Readable.from([{"items": "alpha", "keys": ["id"], "count": 1}])
  const labels = []
  try {
    for await (const chunk of upload) {
      labels.push(chunk.items)
      if (labels.length === 1) break
    }
    return labels
  } finally {
    upload.destroy()
  }
}

await pipeline(fs.createReadStream("input.txt"), fs.createWriteStream("output.txt"))
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);
    expect(text).not.toMatch(nativeMemoryHelperPattern);

    expect(allOps(manifest).some((op: any) =>
      op.runtime === "python" && String(op.code ?? op.source ?? "").includes("sa.create_engine")
    )).toBe(true);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("sa.create_engine(url)");
    expect(codes).toContain("sa.orm.Session(engine)");
    expect(codes).toContain("session.begin()");
    expect(codes).toContain('session.execute(text("select id, count from orders"))');
    expect(codes).toContain("Array.from(rows).slice(0, 2)");
    expect(codes).toContain("rows.count");
    expect(codes).toContain("rows.close");
    expect(codes).toContain("tx.commit()");
    expect(codes).toContain('Readable.from([{"items": "alpha", "keys": ["id"], "count": 1}])');
    expect(codes).toContain("labels.push(chunk.items)");
    expect(codes).toContain("upload.destroy()");
    expect(codes).toContain('pipeline(fs.createReadStream("input.txt"), fs.createWriteStream("output.txt"))');

    const ops = allOps(manifest);
    const sqlFunc = ops.find((op: any) => op.op === "func_def" && op.name === "load_orders");
    expect(sqlFunc?.bodyRuntime).toBe("python");

    const sessionTry = ops.find((op: any) =>
      op.op === "try" &&
      JSON.stringify(op.body).includes("sa.orm.Session") &&
      JSON.stringify(op.body).includes("__enter__") &&
      JSON.stringify(op.finallyBody ?? []).includes("__exit__(None, None, None)") &&
      JSON.stringify(op.finallyBody ?? []).includes('"action":"close"')
    );
    expect(sessionTry).toBeDefined();

    const transactionTry = ops.find((op: any) =>
      op.op === "try" &&
      JSON.stringify(op.body).includes("session.begin") &&
      JSON.stringify(op.body).includes("tx.commit") &&
      JSON.stringify(op.body).includes("__enter__") &&
      JSON.stringify(op.finallyBody ?? []).includes("__exit__(None, None, None)") &&
      JSON.stringify(op.finallyBody ?? []).includes('"action":"close"')
    );
    expect(transactionTry).toBeDefined();

    const streamFunc = ops.find((op: any) => op.op === "func_def" && op.name === "read_upload_chunks");
    expect(streamFunc?.async).toBe(true);
    expect(streamFunc?.bodyRuntime).toBe("javascript");

    const streamLoop = ops.find((op: any) =>
      op.op === "loop" &&
      op.await === true &&
      JSON.stringify(op).includes("upload")
    );
    expect(streamLoop).toBeDefined();

    const streamCleanup = ops.find((op: any) =>
      op.op === "try" &&
      JSON.stringify(op.body).includes("labels.push") &&
      JSON.stringify(op.finallyBody ?? []).includes("upload.destroy")
    );
    expect(streamCleanup).toBeDefined();

    expect(ops.some((op: any) =>
      op.op === "await" &&
      op.runtime === "javascript" &&
      String(op.from?.code ?? "").includes('fs.createReadStream("input.txt")') &&
      String(op.from?.code ?? "").includes('fs.createWriteStream("output.txt")')
    )).toBe(true);
  });

  test("keeps timeout and worker teardown snippets natural and helper-free", () => {
    const { manifest } = compileSnippet(`
const controller = new AbortController()
const timeout_signal = AbortSignal.timeout(1000)
const runtime_worker = new Worker("worker.js")
const timer = setTimeout(() => {
  controller.abort()
}, 1000)

try {
  runtime_worker.reload()
  runtime_worker.terminate()
} finally {
  clearTimeout(timer)
  controller.abort()
  runtime_worker.close()
}

func teardown_worker(id) {
  return id
}

const worker = go teardown_worker(1)
const worker_result = wait(worker)
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("teardown_worker(1)");
    expect(codes).toContain("wait(worker)");
    expect(codes).toContain("new AbortController()");
    expect(codes).toContain("AbortSignal.timeout(1000)");
    expect(codes).toContain('new Worker("worker.js")');
    expect(codes).toContain("setTimeout(() => { controller.abort() }, 1000)");
    expect(codes).toContain("runtime_worker.reload()");
    expect(codes).toContain("runtime_worker.terminate()");
    expect(codes).toContain("clearTimeout(timer)");
    expect(codes).toContain("runtime_worker.close()");

    const ops = allOps(manifest);
    const spawnOp = ops.find((op: any) => op.op === "spawn" && op.bind === "worker");
    expect(spawnOp).toBeDefined();
    expect(spawnOp?.runtime).toBe("go");
    expect(spawnOp?.code).toBe("teardown_worker(1)");

    const waitOp = ops.find((op: any) =>
      String(op.code ?? op.source ?? "").includes("wait(worker)")
    );
    expect(waitOp).toBeDefined();

    const teardownTry = ops.find((op: any) =>
      op.op === "try" &&
      JSON.stringify(op.body).includes("runtime_worker.reload") &&
      JSON.stringify(op.finallyBody ?? []).includes("clearTimeout(timer)") &&
      JSON.stringify(op.finallyBody ?? []).includes("runtime_worker.close")
    );
    expect(teardownTry).toBeDefined();
    expect(teardownTry?.body.every((op: any) => op.runtime === "javascript")).toBe(true);
    expect(teardownTry?.finallyBody.every((op: any) => op.runtime === "javascript")).toBe(true);
  });

  test("keeps native-ish buffer snippets natural without manual Arrow or buffer helpers", () => {
    const { manifest } = compileSnippet(`
import numpy as np
from pyarrow import array

const np_payload = np.arange(6).reshape(2, 3)
const tensor_view = np_payload.reshape(3, 2)
const arrow_array = array([1, 2, 3])
const py_view = memoryview(bytearray(b"abcd"))
const js_bytes = new Uint8Array([1, 2, 3, 4])
const js_view = new DataView(js_bytes.buffer)
const java_buffer = java.nio.ByteBuffer.allocateDirect(4)
java_buffer.put(0, 7)

const native_summary = JSON.stringify({
  "rows": np_payload.shape[0],
  "cols": np_payload.shape[1],
  "tensorRows": tensor_view.shape[0],
  "tensorCols": tensor_view.shape[1],
  "tensorDtype": tensor_view.dtype,
  "tensorStride": tensor_view.strides[0],
  "arrowType": arrow_array.type,
  "arrowLength": arrow_array.length,
  "arrowNullCount": arrow_array.null_count,
  "arrowDataBuffer": arrow_array.buffers()[1],
  "pyLength": py_view.length,
  "jsLength": js_bytes.length,
  "viewByteLength": js_view.byteLength,
  "javaCapacity": java_buffer.capacity(),
  "javaFirst": java_buffer.get(0)
})
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);
    expect(text).not.toMatch(nativeMemoryHelperPattern);

    expect(allOps(manifest).some((op: any) =>
      op.runtime === "python" && String(op.code ?? op.source ?? "").includes("np.arange")
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.runtime === "python" && String(op.code ?? op.source ?? "").includes("array([1, 2, 3])")
    )).toBe(true);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("np.arange(6).reshape(2, 3)");
    expect(codes).toContain("np_payload.reshape(3, 2)");
    expect(codes).toContain("array([1, 2, 3])");
    expect(codes).toContain('memoryview(bytearray(b"abcd"))');
    expect(codes).toContain("new Uint8Array([1, 2, 3, 4])");
    expect(codes).toContain("new DataView(js_bytes.buffer)");
    expect(codes).toContain("java.nio.ByteBuffer.allocateDirect(4)");
    expect(codes).toContain("java_buffer.put(0, 7)");
    expect(codes).toContain("np_payload.shape[0]");
    expect(codes).toContain("np_payload.shape[1]");
    expect(codes).toContain("tensor_view.shape[0]");
    expect(codes).toContain("tensor_view.shape[1]");
    expect(codes).toContain("tensor_view.dtype");
    expect(codes).toContain("tensor_view.strides[0]");
    expect(codes).toContain("arrow_array.type");
    expect(codes).toContain("arrow_array.length");
    expect(codes).toContain("arrow_array.null_count");
    expect(codes).toContain("arrow_array.buffers()[1]");
    expect(codes).toContain("py_view.length");
    expect(codes).toContain("js_bytes.length");
    expect(codes).toContain("js_view.byteLength");
    expect(codes).toContain("java_buffer.capacity()");
    expect(codes).toContain("java_buffer.get(0)");
  });

  test("keeps async iterable and executor snippets helper-free with generic async lowering", () => {
    const { manifest } = compileSnippet(`
import asyncio
import concurrent.futures as futures
import java.util.concurrent.CompletableFuture
import static java.util.concurrent.TimeUnit.SECONDS

async function collect_async(async_rows) {
  const labels = []
  for await (const row of async_rows) {
    labels.push(row.items)
    if (labels.length === 2) break
  }
  return labels
}

const js_ready = await Promise.all([load_js(), load_more_js()])
const py_ready = await asyncio.gather(load_py(), load_more_py())
const java_ready = java.util.concurrent.CompletableFuture.allOf(java_future, other_future)
const java_timeout_value = java_future.get(1, SECONDS)

with futures.ThreadPoolExecutor(max_workers=2) as pool:
  const future = pool.submit(load_py)
  const result = future.result()
  const done = future.done()

const executor = java.util.concurrent.Executors.newSingleThreadExecutor()
const task = executor.submit(java_task)
const task_done = task.isDone
const task_result = task.get()
const task_cancelled = task.cancel(true)
const task_was_cancelled = task.isCancelled
executor.shutdown()

const ruby_fiber_id = Fiber.current.object_id
const ruby_thread_id = Thread.current.object_id
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("labels.push(row.items)");
    expect(codes).toContain("load_js()");
    expect(codes).toContain("load_more_js()");
    expect(codes).toContain("load_py()");
    expect(codes).toContain("load_more_py()");
    expect(codes).toContain("java.util.concurrent.CompletableFuture.allOf(java_future, other_future)");
    expect(codes).toContain("java_future.get(1, SECONDS)");
    expect(codes).toContain("futures.ThreadPoolExecutor(max_workers = 2)");
    expect(codes).toContain("pool.submit(load_py)");
    expect(codes).toContain("future.result()");
    expect(codes).toContain("future.done()");
    expect(codes).toContain("java.util.concurrent.Executors.newSingleThreadExecutor()");
    expect(codes).toContain("executor.submit(java_task)");
    expect(codes).toContain("task.isDone");
    expect(codes).toContain("task.get()");
    expect(codes).toContain("task.cancel(true)");
    expect(codes).toContain("task.isCancelled");
    expect(codes).toContain("executor.shutdown()");
    expect(codes).toContain("Fiber.current.object_id");
    expect(codes).toContain("Thread.current.object_id");

    const ops = allOps(manifest);
    expect(ops.some((op: any) => op.op === "loop" && op.await === true)).toBe(true);
    expect(ops.some((op: any) => op.op === "parallel")).toBe(true);
    expect(ops.some((op: any) => op.op === "try" && JSON.stringify(op.finallyBody ?? []).includes('"action":"close"'))).toBe(true);
    expect(ops.some((op: any) =>
      op.runtime === "ruby" && String(op.code ?? op.source ?? "").includes("Fiber.current.object_id")
    )).toBe(true);
    expect(ops.some((op: any) =>
      op.runtime === "ruby" && String(op.code ?? op.source ?? "").includes("Thread.current.object_id")
    )).toBe(true);
  });

  test("keeps framework-shaped imports driven by syntax evidence instead of package-name helpers", () => {
    const { manifest } = compileSnippet(`
import django
from fastapi import FastAPI
import React from "react"
require "rack"
require "action_dispatch"
require "active_record"
import io.reactivex.rxjava3.core.Flowable

app = FastAPI()

def django_view(request):
  return {"items": request.items, "close": request.close, "count": request.count}

async def fastapi_endpoint(request, background_tasks):
  background_tasks.add_task(close_job)
  return {"items": request.items, "keys": request.keys, "count": request.count, "close": request.close}

const react_element = React.createElement("button", {"data-count": props.count}, props.items)
const rack_response = Rack::Response.new("hello", 200, {"close": "field-close"}).finish
const rails_response = ActionDispatch::Response.new(202, {"items": "field-items", "close": "field-close"}, ["body"])
const table_name = ActiveRecord::Base.connection.quote_table_name("orders")
const rails_summary = JSON.stringify({"status": rails_response.status, "items": rails_response.headers.items, "close": rails_response.close, "table": table_name})
const flowable = Flowable.just("alpha")
const subscription = flowable.subscribe(on_next)
subscription.dispose()
const disposed = subscription.isDisposed
const stage = java.util.concurrent.CompletableFuture.completedFuture("ok")
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    expect(allOps(manifest).some((op: any) =>
      op.runtime === "python" && String(op.code ?? op.source ?? "").includes("FastAPI()")
    )).toBe(true);
    for (const path of ["rack", "action_dispatch", "active_record"]) {
      expect(manifest.ops.some((op: any) =>
        op.op === "import" && op.runtime === "ruby" && op.path === path
      )).toBe(true);
    }

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("request.items");
    expect(codes).toContain("request.close");
    expect(codes).toContain("request.count");
    expect(codes).toContain("FastAPI()");
    expect(codes).toContain("background_tasks.add_task(close_job)");
    expect(codes).toContain('"keys": request.keys');
    expect(codes).toContain("React.createElement");
    expect(codes).toContain("props.count");
    expect(codes).toContain("props.items");
    expect(codes).toContain("Rack::Response.new");
    expect(codes).toContain("ActionDispatch::Response.new");
    expect(codes).toContain("ActiveRecord::Base.connection.quote_table_name");
    expect(codes).toContain("rails_response.status");
    expect(codes).toContain("rails_response.headers.items");
    expect(codes).toContain("rails_response.close");
    expect(codes).toContain("Flowable.just");
    expect(codes).toContain("flowable.subscribe(on_next)");
    expect(codes).toContain("subscription.dispose()");
    expect(codes).toContain("subscription.isDisposed");
    expect(codes).toContain("java.util.concurrent.CompletableFuture.completedFuture");

    const reactOp = manifest.ops.find((op: any) =>
      op.runtime === "javascript" && String(op.code ?? op.source ?? "").includes("React.createElement")
    );
    expect(reactOp).toBeDefined();

    const fastapiFunc = allOps(manifest).find((op: any) => op.op === "func_def" && op.name === "fastapi_endpoint");
    expect(fastapiFunc?.async).toBe(true);
    expect(fastapiFunc?.bodyRuntime).toBe("python");

    const rubyOp = manifest.ops.find((op: any) =>
      op.runtime === "ruby" && String(op.code ?? op.source ?? "").includes("Rack::Response.new")
    );
    expect(rubyOp).toBeDefined();
    expect(manifest.ops.some((op: any) =>
      op.runtime === "ruby" && String(op.code ?? op.source ?? "").includes("ActionDispatch::Response.new")
    )).toBe(true);
    expect(manifest.ops.some((op: any) =>
      op.runtime === "ruby" && String(op.code ?? op.source ?? "").includes("ActiveRecord::Base.connection.quote_table_name")
    )).toBe(true);

    const javaOps = manifest.ops.filter((op: any) => op.runtime === "java");
    expect(javaOps.some((op: any) => String(op.code ?? op.source ?? "").includes("Flowable.just"))).toBe(true);
    expect(javaOps.some((op: any) => String(op.code ?? op.source ?? "").includes("subscription.dispose"))).toBe(true);
    expect(javaOps.some((op: any) => String(op.code ?? op.source ?? "").includes("subscription.isDisposed"))).toBe(true);
    expect(javaOps.some((op: any) => String(op.code ?? op.source ?? "").includes("java.util.concurrent.CompletableFuture.completedFuture"))).toBe(true);
  });

  test("keeps React JSX docs snippets parseable and lowered without swallowing closing tags", () => {
    const { manifest } = compileSnippet(`
import React from "react"
import { useState } from "react"

function MyButton() {
  return <button>I'm a button</button>;
}

function List() {
  return <>
    <h1>Articles</h1>
    <ul><li>First</li></ul>
  </>;
}

function Counter() {
  const [count, setCount] = useState(0);
  return <button onClick={() => setCount(count + 1)}>{count}</button>;
}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const myButton = allOps(manifest).find((op: any) => op.op === "func_def" && op.name === "MyButton");
    expect(myButton?.bodyRuntime).toBe("javascript");
    const myButtonReturnCode = myButton?.body?.find((op: any) => op.op === "return")?.from?.code;
    expect(myButtonReturnCode).toBe('React.createElement("button", null, "I\'m a button")');
    expect(myButtonReturnCode).not.toContain("</button>");
    expect(JSON.stringify(myButton)).not.toContain('"code":"\\"\\""');

    const list = allOps(manifest).find((op: any) => op.op === "func_def" && op.name === "List");
    const listReturnCode = list?.body?.find((op: any) => op.op === "return")?.from?.code;
    expect(listReturnCode).toContain("React.Fragment");
    expect(listReturnCode).toContain('React.createElement("h1"');
    expect(listReturnCode).toContain('React.createElement("li"');

    const counter = allOps(manifest).find((op: any) => op.op === "func_def" && op.name === "Counter");
    expect(counter?.bodyRuntime).toBe("javascript");
    const counterText = JSON.stringify(counter);
    const counterReturnCode = counter?.body?.find((op: any) => op.op === "return")?.from?.code;
    expect(counterText).toContain("useState(0)");
    expect(counterReturnCode).toContain("setCount((count + 1))");
    expect(counterReturnCode).toContain('React.createElement("button"');
  });

  test("keeps custom JSX factory snippets package-agnostic", () => {
    const { manifest } = compileSnippet(`
/** @jsx h */
/** @jsxFrag Fragment */
import { h, Fragment } from "preact"

function Badge(props) {
  return <>
    <span className="badge">{props.label}</span>
  </>;
}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const badge = allOps(manifest).find((op: any) => op.op === "func_def" && op.name === "Badge");
    expect(badge?.bodyRuntime).toBe("javascript");
    const returnCode = badge?.body?.find((op: any) => op.op === "return")?.from?.code;
    expect(returnCode).toContain('h(Fragment, null');
    expect(returnCode).toContain('h("span", {className: "badge"}, props.label)');
    expect(returnCode).not.toContain("React.createElement");
    expect(returnCode).not.toContain("React.Fragment");
  });

  test("keeps JavaScript callback block statement boundaries in Express-shaped snippets", () => {
    const { manifest } = compileSnippet(`
import express from "express"

const app = express()
app.use((req, res, next) => {
  req.items = []
  next()
})
app.get("/", (req, res) => {
  res.send("Hello World!")
})
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("app.use((req, res, next) => { req.items = []; next() })");
    expect(codes).not.toContain("[]next");
    expect(codes).toContain('app.get("/", (req, res) => { res.send("Hello World!") })');
  });

  test("preserves framework route decorators as source instead of dropping registration", () => {
    const { annotated, manifest } = compileSnippet(`
from fastapi import FastAPI
from django.views.decorators.http import require_GET

app = FastAPI()

@app.get("/items/{item_id}")
async def read_item(item_id):
  return {"item_id": item_id}

@require_GET
def django_view(request):
  return {"items": request.items, "count": request.count}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const fastapiFunc = allOps(manifest).find((op: any) => op.op === "func_def" && op.name === "read_item");
    expect(fastapiFunc?.async).toBe(true);
    expect(fastapiFunc?.bodyRuntime).toBe("python");
    expect(fastapiFunc?.sourceArtifact?.functionSource).toContain('@app.get("/items/{item_id}")');
    expect(fastapiFunc?.sourceArtifact?.functionSource).toContain("async def read_item(item_id):");

    const djangoFunc = allOps(manifest).find((op: any) => op.op === "func_def" && op.name === "django_view");
    expect(djangoFunc?.bodyRuntime).toBe("python");
    expect(djangoFunc?.sourceArtifact?.functionSource).toContain("@require_GET");
    expect(djangoFunc?.sourceArtifact?.functionSource).toContain("def django_view(request):");
    expect(djangoFunc?.sourceArtifact?.functionSource).toContain("request.items");

    const readItem = annotated.program.body.find((node: any) => node.kind === "FuncDecl" && node.name.name === "read_item") as any;
    expect(readItem?.decorators?.[0]?.expression).toBeDefined();
    const freeIds = Array.from(collectFreeIdentifiers(readItem));
    expect(freeIds).toContain("app");
    expect(freeIds).not.toContain("item_id");

    const decoratorAffinity = annotated.affinityMap.get(readItem.decorators[0].expression);
    expect(decoratorAffinity?.runtime).toBe("python");
  });

  test("keeps decorated class methods attached to their class with generic decorator affinity", () => {
    const { annotated, manifest } = compileSnippet(`
from fastapi import FastAPI

app = FastAPI()

class Api:
  @app.get("/items")
  async def list_items(self, request):
    return {"items": request.items, "count": request.count, "close": request.close}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const classes = annotated.program.body.filter((node: any) => node.kind === "ClassDecl") as any[];
    expect(classes).toHaveLength(1);
    const cls = classes[0];
    expect(annotated.program.body.some((node: any) => node.kind === "FuncDecl" && node.name.name === "list_items")).toBe(false);
    expect(cls.members).toHaveLength(1);
    expect(cls.members[0].kind).toBe("FuncDecl");
    expect(cls.members[0].decorators?.[0]?.expression).toBeDefined();

    const freeIds = Array.from(collectFreeIdentifiers(cls));
    expect(freeIds).toContain("app");
    expect(freeIds).not.toContain("self");
    expect(freeIds).not.toContain("request");

    expect(annotated.affinityMap.get(cls)?.runtime).toBe("python");
    expect(annotated.affinityMap.get(cls.members[0].decorators[0].expression)?.runtime).toBe("python");

    const classOp = allOps(manifest).find((op: any) =>
      op.op === "native" && op.runtime === "python" && String(op.code ?? "").includes("class Api:")
    );
    expect(classOp).toBeDefined();
    expect(classOp?.code).toContain('@app.get("/items")');
    expect(classOp?.code).toContain("async def list_items(self, request):");
    expect(classOp?.code).toContain("request.items");
    expect(classOp?.code).toContain("request.count");
    expect(classOp?.code).toContain("request.close");
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" && op.runtime === "javascript" && String(op.code ?? "").includes('get("/items")')
    )).toBe(false);
  });

  test("keeps Java annotations with declaration modifiers attached to native classes", () => {
    const { annotated, manifest } = compileSnippet(`
import org.springframework.web.bind.annotation.RestController
import org.springframework.web.bind.annotation.GetMapping
import org.junit.jupiter.api.Test

@RestController
public class GreetingController {
  @GetMapping("/greeting")
  public String greeting() {
    return "ok";
  }
}

class ParserTest {
  @Test
  public void parses_docs_case() {
    assertEquals(1, 1);
  }
}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const classes = annotated.program.body.filter((node: any) => node.kind === "ClassDecl") as any[];
    expect(classes).toHaveLength(2);
    for (const cls of classes) {
      expect(annotated.affinityMap.get(cls)?.runtime).toBe("java");
    }

    const controller = classes.find(cls => cls.name.name === "GreetingController");
    expect(controller?.decorators?.[0]?.expression).toBeDefined();
    expect(controller?.members[0]?.decorators?.[0]?.expression).toBeDefined();

    const parserTest = classes.find(cls => cls.name.name === "ParserTest");
    expect(parserTest?.members[0]?.decorators?.[0]?.expression).toBeDefined();
    expect(parserTest?.members[0]?.name.name).toBe("parses_docs_case");

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain("@RestController");
    expect(codes).toContain("public class GreetingController");
    expect(codes).toContain('@GetMapping("/greeting")');
    expect(codes).toContain("public String greeting()");
    expect(codes).toContain("@Test");
    expect(codes).toContain("public void parses_docs_case()");
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" && op.runtime === "javascript" && String(op.code ?? "").includes("GetMapping")
    )).toBe(false);
  });

  test("keeps Python relative from-import docs snippets as imports", () => {
    const { manifest } = compileSnippet(`
from django.urls import path
from . import views
from .views import index
from ..models import User

urlpatterns = [
  path("articles/<int:year>/", views.year_archive),
  path("index/", index),
]

visible_patterns = urlpatterns[:limit]
first_pattern = urlpatterns[:1]
const model_name = User.__name__
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    for (const [path, source] of [
      ["django.urls", "from django.urls import path"],
      [".", "from . import views"],
      [".views", "from .views import index"],
      ["..models", "from ..models import User"],
    ]) {
      expect(manifest.ops.some((op: any) =>
        op.op === "import" &&
        op.runtime === "python" &&
        op.path === path &&
        op.sourceArtifact === source
      )).toBe(true);
    }

    const codes = allOpCodes(manifest).join("\n");
    expect(codes).toContain('path("articles/<int:year>/", views.year_archive)');
    expect(codes).toContain('path("index/", index)');
    expect(codes).toContain("urlpatterns[:limit]");
    expect(codes).toContain("urlpatterns[:1]");
    expect(codes).not.toContain("__slice__");
    expect(codes).toContain("User.__name__");
    expect(codes).not.toContain("from.import");
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" && String(op.code ?? "").includes("from.import")
    )).toBe(false);
  });

  test("keeps Ruby superclass classes as end-delimited native source", () => {
    const { annotated, manifest } = compileSnippet(`
require "active_record"
require "action_controller"

class Author < ActiveRecord::Base
  has_many :books

  def display_name
    name
  end
end

class ArticlesController < ApplicationController
  def show
    @article = Article.find(params[:id])
  end
end
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    expect(manifest.ops.some((op: any) =>
      op.op === "import" && op.runtime === "ruby" && op.path === "active_record"
    )).toBe(true);
    expect(manifest.ops.some((op: any) =>
      op.op === "import" && op.runtime === "ruby" && op.path === "action_controller"
    )).toBe(true);

    const classes = annotated.program.body.filter((node: any) => node.kind === "ClassDecl") as any[];
    expect(classes.map(cls => cls.name.name)).toEqual(["Author", "ArticlesController"]);
    for (const cls of classes) {
      expect(annotated.affinityMap.get(cls)?.runtime).toBe("ruby");
    }

    const rubyCodes = allOps(manifest)
      .filter((op: any) => op.op === "native" && op.runtime === "ruby")
      .map((op: any) => String(op.code ?? op.source ?? ""));
    expect(rubyCodes.some(code =>
      code.includes("class Author < ActiveRecord::Base") &&
      code.includes("has_many :books") &&
      code.includes("def display_name") &&
      code.includes("end")
    )).toBe(true);
    expect(rubyCodes.some(code =>
      code.includes("class ArticlesController < ApplicationController") &&
      code.includes("Article.find(params[:id])")
    )).toBe(true);

    expect(allOps(manifest).some((op: any) =>
      op.op === "func_def" && (op.name === "display_name" || op.name === "show")
    )).toBe(false);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" && String(op.code ?? "").trim() === "end"
    )).toBe(false);
  });

  test("keeps Ruby do-end DSL blocks as single native source statements", () => {
    const { manifest } = compileSnippet(`
require "active_record"
require "rack"

Article.where(published: true).find_each do |article|
  puts article.title
end

app = Rack::Builder.new do
  use Rack::ContentLength
  run ->(env) { [200, {"items" => "field-items"}, ["ok"]] }
end
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    const rubyCodes = allOps(manifest)
      .filter((op: any) => op.runtime === "ruby")
      .map((op: any) => String(op.code ?? op.source ?? ""));

    expect(rubyCodes.some(code =>
      code.includes("Article.where(published: true).find_each do |article|") &&
      code.includes("puts article.title") &&
      code.trim().endsWith("end")
    )).toBe(true);
    expect(rubyCodes.some(code =>
      code.includes("app = Rack::Builder.new do") &&
      code.includes("use Rack::ContentLength") &&
      code.includes('{"items" => "field-items"}') &&
      code.trim().endsWith("end")
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" && String(op.code ?? "").trim() === "end"
    )).toBe(false);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" && String(op.code ?? "").trim() === ": true"
    )).toBe(false);
  });

  test("keeps Ruby keyword-label arguments and command calls as single Ruby snippets", () => {
    const { manifest } = compileSnippet(`
require "active_record"
require "action_controller"

Article.where(published: true)
user = User.create(name: "Ada", active: true)
redirect_to action: "show", id: @post
Article.find(params[:id])
validates :name, presence: true
User.select(:id, :name)
resources :articles
only :index, :show
headers = {"items" => "field-items"}
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === "Article.where(published: true)"
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "eval" &&
      op.runtime === "ruby" &&
      op.bind === "user" &&
      String(op.code ?? "").trim() === 'User.create(name: "Ada", active: true)'
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === 'redirect_to action: "show", id: @post'
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === "Article.find(params[:id])"
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === "validates :name, presence: true"
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === "User.select(:id, :name)"
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === "resources :articles"
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === "only :index, :show"
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "eval" &&
      op.runtime === "ruby" &&
      op.bind === "headers" &&
      String(op.code ?? "").trim() === '{"items" => "field-items"}'
    )).toBe(true);

    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" && String(op.code ?? "").trim() === ": true"
    )).toBe(false);

    const { manifest: jsManifest } = compileSnippet(`
const config = {enabled: true, count: 2}
foo({enabled: true, count: 2})
`);
    expect(allOps(jsManifest).some((op: any) =>
      op.runtime === "javascript" && String(op.code ?? "").trim() === "foo({enabled: true, count: 2})"
    )).toBe(true);
  });

  test("keeps Ruby stabby lambda Rack snippets as single Ruby source spans", () => {
    const { manifest } = compileSnippet(`
require "rack"

app = ->(env) { [200, {"Content-Type" => "text/plain"}, ["OK"]] }
run ->(env) { [200, {"items" => "field-items"}, ["ok"]] }
Rack::Handler::WEBrick.run ->(env) { [200, {}, ["ok"]] }
`);

    const text = manifestText(manifest);
    expect(text).not.toMatch(bridgeHelperPattern);

    expect(allOps(manifest).some((op: any) =>
      op.op === "eval" &&
      op.runtime === "ruby" &&
      op.bind === "app" &&
      String(op.code ?? "").trim() === '->(env) { [200, {"Content-Type" => "text/plain"}, ["OK"]] }'
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === 'run ->(env) { [200, {"items" => "field-items"}, ["ok"]] }'
    )).toBe(true);
    expect(allOps(manifest).some((op: any) =>
      op.op === "exec" &&
      op.runtime === "ruby" &&
      String(op.code ?? "").trim() === 'Rack::Handler::WEBrick.run ->(env) { [200, {}, ["ok"]] }'
    )).toBe(true);

    const { manifest: jsManifest } = compileSnippet(`
const app = (env) => [200, {items: "field-items"}, ["ok"]]
const label = "ok"
`);
    expect(allOps(jsManifest).some((op: any) =>
      op.runtime === "javascript" &&
      String(op.code ?? "").trim() === '(env) => [200, {items: "field-items"}, ["ok"]]'
    )).toBe(true);
    expect(allOps(jsManifest).some((op: any) =>
      op.op === "declare" &&
      op.bind === "label" &&
      op.value?.kind === "literal" &&
      op.value?.value === "ok"
    )).toBe(true);
  });
});
