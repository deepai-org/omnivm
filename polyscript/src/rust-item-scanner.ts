/**
 * Rust item raw scanner — Regime 1 of the two-regime Rust parsing
 * architecture.
 *
 * PolyScript is a syntactic union language; full Rust cannot be expressed in
 * the union grammar (lifetimes, where clauses, struct-payload enum variants,
 * nested use groups, ...). Instead of teaching the union lexer Rust, the
 * parser hands off to this scanner whenever a definite-Rust item anchor is
 * hit at module level. The scanner consumes exactly ONE item VERBATIM —
 * it never tokenizes Rust. It only knows how to skip Rust trivia (comments,
 * strings, char literals vs lifetimes) and count balanced delimiters, which
 * is sufficient because Rust requires balanced delimiters everywhere,
 * including inside macro token trees.
 *
 * The same scanner is used by the round-trip oracle test
 * (test/rust-roundtrip.test.ts) so a mis-slice shows up as a byte mismatch
 * against real crate sources.
 */

export type RustItemKind =
  | "fn"
  | "struct"
  | "enum"
  | "union"
  | "trait"
  | "impl"
  | "mod"
  | "use"
  | "static"
  | "const"
  | "type"
  | "macro"
  | "macro-call"
  | "extern";

export interface RustFnSig {
  name: string;
  async: boolean;
  paramCount: number;
  /** Best-effort parameter names (argN when a pattern can't be named). */
  params: string[];
  /** Raw parameter type texts, parallel to `params` ("" when unknown). */
  paramTypes: string[];
  /**
   * Indices of parameters written as a bare identifier with NO `: Type`
   * (`fn score(review)`). This is the gradual-typing superset: plain rustc
   * rejects type-less params, so recording them never changes how valid
   * Rust slices. Pattern params (`(a, b): (i64, i64)`) are NOT untyped —
   * they carry a type and simply have no extractable name.
   */
  untypedParams: number[];
  /** Raw return type text after `->`, before `where`/`{` ("" when none). */
  returnType: string;
  /**
   * True when the fn declares non-lifetime generic parameters (`<T>`,
   * `<const N: usize>`). Lifetime-only lists (`<'a>`) do NOT count — they
   * are erased at the export boundary and never block monomorphization.
   */
  typeGenerics: boolean;
}

export interface ScannedRustItem {
  itemKind: RustItemKind;
  /** Start offset including any directly preceding #[...] / /// lines. */
  start: number;
  /** Offset of the anchor keyword (pub/fn/struct/...). */
  anchorStart: number;
  /** End offset (exclusive) — one past the closing `}` or `;`. */
  end: number;
  /** Verbatim source slice [start, end). */
  text: string;
  /** Primary declared name (struct/enum/static/... name, first fn name). */
  name?: string;
  /** Signatures of top-level `fn` items (empty unless itemKind === "fn"). */
  fns: RustFnSig[];
  /** Names this item binds at module scope (use leaves + roots, decl name). */
  bindings: string[];
}

const IDENT_START = /[A-Za-z_]/;
const IDENT_CHAR = /[A-Za-z0-9_]/;

/** Items that terminate only at a depth-0 `;`. */
const SEMI_ONLY: ReadonlySet<RustItemKind> = new Set([
  "use", "static", "const", "type", "extern",
]);

/** Rust primitive type names — definite-Rust signal in type position. */
const RUST_PRIMITIVES = new Set([
  "i8", "i16", "i32", "i64", "i128", "isize",
  "u8", "u16", "u32", "u64", "u128", "usize",
  "f32", "f64", "str", "bool", "char",
]);

/** `pub`, `pub(crate)`, `pub(in some::path)` prefix. */
const PUB_RE = /^pub(?:\s*\((?:[^()]|\([^()]*\))*\))?\s+/;

/**
 * Module-level macro invocation head: `ident!` / `path::ident!` immediately
 * followed by an opening delimiter (`{`, `(`, `[`). `macro_rules!` is
 * classified separately (and first), so it never reaches this pattern.
 */
const MACRO_CALL_RE = /^[A-Za-z_]\w*(?:\s*::\s*[A-Za-z_]\w*)*\s*!\s*[({[]/;

/**
 * Classify the item anchored at `pos` (which must point at the first
 * character of the anchor keyword run, after any attributes). Returns null
 * when the text at pos does not start a recognizable Rust item.
 */
export function classifyRustAnchor(source: string, pos: number): RustItemKind | null {
  let rest = source.slice(pos, pos + 256);
  const pub = rest.match(PUB_RE);
  if (pub) rest = rest.slice(pub[0].length);

  if (/^use\b/.test(rest)) return "use";
  if (/^extern\s+crate\b/.test(rest)) return "extern";
  if (/^macro_rules\s*!/.test(rest)) return "macro";
  if (/^(?:(?:async|unsafe|const)\s+)*(?:extern\s*"[^"\n]*"\s*)?fn\s+[A-Za-z_]/.test(rest)) return "fn";
  if (/^struct\b/.test(rest)) return "struct";
  if (/^enum\b/.test(rest)) return "enum";
  if (/^union\s+[A-Za-z_]/.test(rest)) return "union";
  if (/^trait\b/.test(rest)) return "trait";
  if (/^impl\b/.test(rest)) return "impl";
  if (/^mod\s+[A-Za-z_]\w*\s*[{;]/.test(rest)) return "mod";
  if (/^static\s+(?:mut\s+)?[A-Za-z_]\w*\s*:/.test(rest)) return "static";
  if (/^const\s+(?:mut\s+)?[A-Za-z_]\w*\s*:/.test(rest)) return "const";
  if (/^type\s+[A-Za-z_]\w*\s*(?:<[^\n]*>\s*)?=/.test(rest)) return "type";
  if (/^unsafe\s+impl\b/.test(rest)) return "impl";
  if (/^unsafe\s+trait\b/.test(rest)) return "trait";
  if (MACRO_CALL_RE.test(rest)) return "macro-call";
  return null;
}

/**
 * Skip the balanced `[...]` group of an attribute whose `#` is at `hashPos`
 * (optionally followed by `!`). Trivia-aware (strings, chars, comments).
 * Returns the offset one past the closing `]`, or -1.
 */
function skipAttrGroup(source: string, hashPos: number): number {
  let p = hashPos + 1;
  if (source[p] === "!") p++;
  if (source[p] !== "[") return -1;
  let depth = 0;
  while (p < source.length) {
    const step = stepToken(source, p);
    if (step.next !== -1) { p = step.next; continue; }
    if (source[p] === "[") depth++;
    else if (source[p] === "]") { depth--; p++; if (depth === 0) return p; continue; }
    p++;
  }
  return -1;
}

/**
 * Walk backwards from the line-start offset `lineStart` over the contiguous
 * run of OUTER attribute lines (`#[...]`, including multi-line balanced
 * groups produced by rustfmt) and doc-comment lines (`///`). Returns the
 * start offset of the run (== `lineStart` when there is none). Module-level
 * inner attributes (`#![...]`) and inner docs (`//!`) never attach.
 */
export function attributePrefixStart(source: string, lineStart: number): number {
  let start = lineStart;
  outer: while (start > 0) {
    const prevEnd = start - 1; // the `\n` before the current start
    const prevStart = source.lastIndexOf("\n", prevEnd - 1) + 1;
    const prevLine = source.slice(prevStart, prevEnd);
    if (/^\s*#\[.*\]\s*$/.test(prevLine) || /^\s*\/\/\/(?:[^\n]*)?$/.test(prevLine)) {
      start = prevStart;
      continue;
    }
    // Multi-line attribute: the previous line closes a `#[ ... ]` group that
    // opened on an earlier line. Find a candidate opener walking up, then
    // validate by skipping the balanced group FORWARD (string-aware) and
    // requiring it to end on the previous line with only whitespace after.
    if (/\]\s*$/.test(prevLine) && !/^\s*#\[/.test(prevLine)) {
      let candEnd = prevStart; // exclusive end of candidate line (at its \n... actually start of the line below)
      for (let lines = 0; lines < 200 && candEnd > 0; lines++) {
        const cEnd = candEnd - 1;
        const cStart = source.lastIndexOf("\n", cEnd - 1) + 1;
        const cLine = source.slice(cStart, cEnd);
        const m = /^(\s*)#\[/.exec(cLine);
        if (m) {
          const hashPos = cStart + m[1].length;
          const groupEnd = skipAttrGroup(source, hashPos);
          if (groupEnd !== -1 && groupEnd > prevStart && groupEnd <= prevEnd &&
              /^\s*$/.test(source.slice(groupEnd, prevEnd))) {
            start = cStart;
            continue outer;
          }
          break; // opener found but it doesn't close on prevLine — stop
        }
        if (/^\s*#!\[/.test(cLine)) break; // inner attribute — never attach
        candEnd = cStart;
      }
      break;
    }
    break;
  }
  return start;
}

/**
 * Walk backwards from the anchor over directly preceding attribute lines
 * (`#[...]`, including multi-line groups) and doc-comment lines (`///`).
 * Those belong to the item. Module-level inner attributes (`#![...]`) and
 * inner docs (`//!`) do NOT.
 */
function itemStartWithPrefix(source: string, anchorPos: number): number {
  const lineStart = source.lastIndexOf("\n", anchorPos - 1) + 1;
  // Only attach previous lines when the anchor starts its own line.
  if (!/^\s*$/.test(source.slice(lineStart, anchorPos))) return anchorPos;
  const start = attributePrefixStart(source, lineStart);
  return start === lineStart ? anchorPos : start;
}

/** Skip a (possibly nested) block comment starting at `/*`. */
function skipBlockComment(source: string, pos: number): number {
  let depth = 0;
  while (pos < source.length) {
    if (source[pos] === "/" && source[pos + 1] === "*") { depth++; pos += 2; continue; }
    if (source[pos] === "*" && source[pos + 1] === "/") { depth--; pos += 2; if (depth === 0) return pos; continue; }
    pos++;
  }
  return pos;
}

/** Skip a normal escaped string starting at the opening `"`. */
function skipEscapedString(source: string, pos: number): number {
  pos++; // opening quote
  while (pos < source.length) {
    if (source[pos] === "\\") { pos += 2; continue; }
    if (source[pos] === '"') return pos + 1;
    pos++;
  }
  return pos;
}

/** Skip a raw string `r"…"`, `r#"…"#`, … starting at the first `#` or `"`. */
function skipRawString(source: string, pos: number): number {
  let hashes = 0;
  while (source[pos] === "#") { hashes++; pos++; }
  if (source[pos] !== '"') return -1; // raw identifier (r#ident) — not a string
  pos++;
  const closer = '"' + "#".repeat(hashes);
  const idx = source.indexOf(closer, pos);
  return idx === -1 ? source.length : idx + closer.length;
}

/**
 * Skip a char literal or lifetime starting at `'`.
 * Same rule as rustc's lexer: `'` followed by ident chars WITHOUT a closing
 * `'` immediately after one char is a lifetime — consume `'ident`.
 */
function skipCharOrLifetime(source: string, pos: number): number {
  const next = source[pos + 1];
  if (next === "\\") {
    // Escaped char literal: '\n', '\u{...}', '\\', ... Start at the
    // backslash so the escape pair is consumed as a unit — starting past it
    // would misread the second char of '\\' (or '\'') as a new escape and
    // swallow the closing quote, causing a runaway scan.
    let p = pos + 1;
    while (p < source.length) {
      if (source[p] === "\\") { p += 2; continue; }
      if (source[p] === "'") return p + 1;
      p++;
    }
    return p;
  }
  if (next !== undefined && next !== "'" && source[pos + 2] === "'") {
    return pos + 3; // 'x'
  }
  if (next !== undefined && IDENT_START.test(next)) {
    let p = pos + 1;
    while (p < source.length && IDENT_CHAR.test(source[p])) p++;
    return p; // lifetime or loop label: 'a, 'static, 'outer
  }
  return pos + 1; // stray quote — be permissive
}

interface TriviaStep {
  /** New position after skipping one trivia element, or -1 if not trivia. */
  next: number;
}

/**
 * If `pos` is at a trivia element (comment, string, char/lifetime, raw
 * string via prefix identifier), return the position after it; else -1.
 * When the element is an identifier (possibly a raw-string prefix), the
 * identifier text is reported via `outWord`.
 */
function stepToken(source: string, pos: number, outWord?: { word: string }): TriviaStep {
  const c = source[pos];
  if (c === "/" && source[pos + 1] === "/") {
    const nl = source.indexOf("\n", pos);
    return { next: nl === -1 ? source.length : nl + 1 };
  }
  if (c === "/" && source[pos + 1] === "*") {
    return { next: skipBlockComment(source, pos) };
  }
  if (c === '"') {
    return { next: skipEscapedString(source, pos) };
  }
  if (c === "'") {
    return { next: skipCharOrLifetime(source, pos) };
  }
  if (IDENT_START.test(c)) {
    let p = pos + 1;
    while (p < source.length && IDENT_CHAR.test(source[p])) p++;
    const word = source.slice(pos, p);
    if (outWord) outWord.word = word;
    // String prefixes: r"", r#""#, b"", br"", c"", cr"", rb is invalid.
    if ((word === "r" || word === "br" || word === "cr") && (source[p] === '"' || source[p] === "#")) {
      const after = skipRawString(source, p);
      if (after !== -1) return { next: after };
      // r#ident raw identifier: consume the hash and the identifier.
      let q = p + 1;
      while (q < source.length && IDENT_CHAR.test(source[q])) q++;
      if (outWord) outWord.word = source.slice(p + 1, q);
      return { next: q };
    }
    if ((word === "b" || word === "c") && source[p] === '"') {
      return { next: skipEscapedString(source, p) };
    }
    if (word === "b" && source[p] === "'") {
      return { next: skipCharOrLifetime(source, p) };
    }
    return { next: p };
  }
  return { next: -1 };
}

/**
 * Find the end (exclusive) of one module-level macro invocation item
 * (`ident! { ... }`, `path::ident!(...);`, `ident![...];`) whose macro path
 * starts at `anchorPos`. Brace-delimited invocations end at the matching `}`
 * (no semicolon); paren/bracket-delimited ones REQUIRE the trailing `;`
 * directly after the balanced group (only trivia between) — exactly Rust's
 * item-position macro invocation grammar. Returns -1 otherwise.
 */
function scanMacroCallEnd(source: string, anchorPos: number): number {
  // Walk the macro path up to `!`.
  let pos = anchorPos;
  let bangSeen = false;
  while (pos < source.length) {
    const c = source[pos];
    if (c === "!") { bangSeen = true; pos++; break; }
    if (/\s/.test(c) || c === ":") { pos++; continue; }
    const step = stepToken(source, pos);
    if (step.next === -1) return -1; // not a macro path
    pos = step.next;
  }
  if (!bangSeen) return -1;

  // Opening delimiter (whitespace/comments allowed, nothing else).
  while (pos < source.length) {
    const c = source[pos];
    if (/\s/.test(c)) { pos++; continue; }
    if (c === "/" && (source[pos + 1] === "/" || source[pos + 1] === "*")) {
      pos = stepToken(source, pos).next;
      continue;
    }
    break;
  }
  const open = source[pos];
  if (open !== "{" && open !== "(" && open !== "[") return -1;
  const close = open === "{" ? "}" : open === "(" ? ")" : "]";

  // Balanced-delimiter scan of the token tree (Rust guarantees balance).
  let depth = 0;
  while (pos < source.length) {
    const step = stepToken(source, pos);
    if (step.next !== -1) { pos = step.next; continue; }
    const c = source[pos];
    if (c === open) depth++;
    else if (c === close) {
      depth--;
      pos++;
      if (depth === 0) {
        if (open === "{") return pos; // `}`-delimited: no semicolon
        // `(`/`[`-delimited: require `;` right after the group (trivia ok).
        while (pos < source.length) {
          const t = source[pos];
          if (/\s/.test(t)) { pos++; continue; }
          if (t === "/" && (source[pos + 1] === "/" || source[pos + 1] === "*")) {
            pos = stepToken(source, pos).next;
            continue;
          }
          return t === ";" ? pos + 1 : -1;
        }
        return -1;
      }
      continue;
    }
    pos++;
  }
  return -1;
}

/**
 * Find the end (exclusive) of one Rust item whose anchor keyword starts at
 * `anchorPos`. Returns -1 when no well-formed end is found.
 */
function scanItemEnd(source: string, anchorPos: number, kind: RustItemKind): number {
  if (kind === "macro-call") return scanMacroCallEnd(source, anchorPos);
  const semiOnly = SEMI_ONLY.has(kind);
  let pos = anchorPos;
  let brace = 0, paren = 0, bracket = 0;

  while (pos < source.length) {
    const step = stepToken(source, pos);
    if (step.next !== -1) { pos = step.next; continue; }

    const c = source[pos];
    switch (c) {
      case "{": brace++; pos++; continue;
      case "}":
        brace--;
        pos++;
        if (brace === 0 && !semiOnly) return pos; // closing brace of the item body
        if (brace < 0) return -1;
        continue;
      case "(": paren++; pos++; continue;
      case ")": paren--; pos++; continue;
      case "[": bracket++; pos++; continue;
      case "]": bracket--; pos++; continue;
      case ";":
        pos++;
        if (brace === 0 && paren === 0 && bracket === 0) return pos;
        continue;
      default:
        pos++;
        continue;
    }
  }
  return -1;
}

/** Extract bound names from a verbatim `use ...;` item. */
export function extractUseBindings(text: string): string[] {
  const names = new Set<string>();
  let body = text
    .replace(/\/\/[^\n]*/g, " ")
    .replace(/^\s*#\[[^\n]*\]\s*$/gm, " ")
    .replace(/\s+/g, " ")
    .trim()
    .replace(/^pub(?:\s*\([^)]*\))?\s+/, "")
    .replace(/^use\s+/, "")
    .replace(/;\s*$/, "");

  const root = body.match(/^([A-Za-z_]\w*)/);
  if (root && !["crate", "super", "self"].includes(root[1])) names.add(root[1]);

  // Leaves: identifiers that end a path segment (before `,`, `}`, `;`, end,
  // or introduced by `as`).
  for (const m of body.matchAll(/([A-Za-z_]\w*)\s*(?:,|\}|$)/g)) {
    if (m[1] !== "as" && m[1] !== "self") names.add(m[1]);
  }
  for (const m of body.matchAll(/\bas\s+([A-Za-z_]\w*)/g)) names.add(m[1]);
  // Simple path `use a::b::c` (no group): the last segment binds.
  if (!body.includes("{")) {
    const last = body.match(/::\s*([A-Za-z_]\w*)\s*$/);
    if (last) names.add(last[1]);
  }
  return [...names];
}

/**
 * Extract top-level `fn` signatures (name, async flag, parameter count and
 * best-effort names) from a verbatim item slice. Operates with the same
 * trivia skipping as the scanner so doc comments mentioning `fn` are inert.
 * Only depth-0 fns are reported (an `fn` item has exactly one; impl/trait
 * methods live at depth > 0 and are NOT exported).
 */
export function extractRustFnSignatures(text: string): RustFnSig[] {
  const sigs: RustFnSig[] = [];
  let pos = 0;
  let brace = 0, paren = 0, bracket = 0;
  let lastWords: string[] = [];

  while (pos < text.length) {
    const out = { word: "" };
    const step = stepToken(text, pos, out);
    if (step.next !== -1) {
      if (out.word) {
        if (out.word === "fn" && brace === 0 && paren === 0 && bracket === 0) {
          const isAsync = lastWords.includes("async");
          // Name follows `fn`.
          const nameMatch = /^\s*([A-Za-z_]\w*)/.exec(text.slice(step.next));
          if (nameMatch) {
            const name = nameMatch[1];
            const nameEnd = step.next + nameMatch[0].length;
            const parenIdx = findParamListStart(text, nameEnd);
            const { segments, end: paramsEnd } =
              parenIdx === -1 ? { segments: [], end: -1 } : scanParamSegments(text, parenIdx);
            const params = segments.map((seg, i) => paramSegmentName(seg, i));
            const paramTypes = segments.map(seg => paramSegmentType(seg));
            const untypedParams = segments
              .map((seg, i) => (paramSegmentIsBare(seg) ? i : -1))
              .filter(i => i !== -1);
            const returnType = paramsEnd === -1 ? "" : extractReturnTypeText(text, paramsEnd);
            const typeGenerics =
              parenIdx !== -1 && hasNonLifetimeGenerics(text.slice(nameEnd, parenIdx));
            sigs.push({
              name, async: isAsync, paramCount: params.length,
              params, paramTypes, untypedParams, returnType, typeGenerics,
            });
          }
          lastWords = [];
        } else {
          lastWords.push(out.word);
          if (lastWords.length > 4) lastWords.shift();
        }
      }
      pos = step.next;
      continue;
    }
    const c = text[pos];
    if (c === "{") { brace++; lastWords = []; }
    else if (c === "}") brace--;
    else if (c === "(") paren++;
    else if (c === ")") paren--;
    else if (c === "[") bracket++;
    else if (c === "]") bracket--;
    else if (c === ";" || c === "#") lastWords = [];
    pos++;
  }
  return sigs;
}

/**
 * Find the opening `(` of the parameter list, skipping a generic parameter
 * list `<...>` after the fn name (which may itself contain parens, e.g.
 * `fn f<F: Fn(i64) -> i64>(g: F)`).
 */
function findParamListStart(text: string, pos: number): number {
  let angle = 0;
  while (pos < text.length) {
    const step = stepToken(text, pos);
    if (step.next !== -1) { pos = step.next; continue; }
    const c = text[pos];
    if (c === "(" && angle === 0) return pos;
    if (c === "<") angle++;
    else if (c === "-" && text[pos + 1] === ">") { pos += 2; continue; }
    else if (c === ">") { if (angle > 0) angle--; }
    else if (c === "{" || c === ";") return -1;
    pos++;
  }
  return -1;
}

/**
 * Split the parameter list whose opening `(` is at `parenIdx` into raw
 * parameter segments (`name: Type` texts). Tracks `()[]{}<>` so commas
 * inside `HashMap<String, i64>` or tuple types don't split. `end` is the
 * position one past the closing `)` (-1 when the list never closes).
 */
function scanParamSegments(
  text: string,
  parenIdx: number,
): { segments: string[]; spans: Array<{ start: number; end: number }>; end: number } {
  let pos = parenIdx + 1;
  let paren = 1, bracket = 0, brace = 0, angle = 0;
  let segStart = pos;
  let end = -1;
  const segments: string[] = [];
  const spans: Array<{ start: number; end: number }> = [];

  const pushSeg = (segEnd: number) => {
    const raw = text.slice(segStart, segEnd);
    const seg = raw.trim();
    if (seg.length > 0) {
      segments.push(seg);
      const lead = raw.length - raw.trimStart().length;
      spans.push({ start: segStart + lead, end: segStart + lead + seg.length });
    }
  };

  while (pos < text.length && paren > 0) {
    const step = stepToken(text, pos);
    if (step.next !== -1) { pos = step.next; continue; }
    const c = text[pos];
    if (c === "(") paren++;
    else if (c === ")") { paren--; if (paren === 0) { pushSeg(pos); pos++; end = pos; break; } }
    else if (c === "[") bracket++;
    else if (c === "]") bracket--;
    else if (c === "{") brace++;
    else if (c === "}") brace--;
    else if (c === "-" && text[pos + 1] === ">") { pos += 2; continue; } // Fn(..) -> Ret
    else if (c === "=" && text[pos + 1] === ">") { pos += 2; continue; }
    else if (c === "<") angle++;
    else if (c === ">") { if (angle > 0) angle--; }
    else if (c === "," && paren === 1 && bracket === 0 && brace === 0 && angle === 0) {
      pushSeg(pos);
      segStart = pos + 1;
    }
    pos++;
  }

  return { segments, spans, end };
}

/** Best-effort parameter name from a raw `name: Type` segment. */
function paramSegmentName(seg: string, index: number): string {
  const named = /^(?:mut\s+)?([A-Za-z_]\w*)\s*:/.exec(seg);
  if (named) return named[1];
  const bare = /^(?:mut\s+)?([A-Za-z_]\w*)\s*$/.exec(seg);
  if (bare) return bare[1];
  return `arg${index}`;
}

/** Raw type text from a `name: Type` segment ("" for patterns/untyped). */
function paramSegmentType(seg: string): string {
  const named = /^(?:mut\s+)?[A-Za-z_]\w*\s*:\s*([\s\S]+)$/.exec(seg);
  return named ? named[1].trim() : "";
}

/** A bare-identifier parameter with no `: Type` — the gradual-typing slot. */
function paramSegmentIsBare(seg: string): boolean {
  return /^(?:mut\s+)?[A-Za-z_]\w*$/.test(seg);
}

/**
 * Byte-offset facts the gradual-typing signature completion needs about the
 * depth-0 `fn <fnName>` inside a verbatim item slice. All offsets are into
 * `text`. The completion (manifest-generator) only ever INSERTS text at
 * these offsets — never a newline — so item line counts (and therefore the
 * unit source map) are preserved by construction.
 */
export interface RustFnCompletionScan {
  /** Trimmed [start, end) offsets of each parameter segment. */
  paramSpans: Array<{ start: number; end: number }>;
  /** Indices of bare-identifier (type-less) parameters. */
  untypedParams: number[];
  /** Offset one past the closing `)` of the parameter list. */
  paramsEnd: number;
  /** True when the signature already declares a `->` return type. */
  hasReturnType: boolean;
  /**
   * True when the fn has a `{...}` body whose last significant (non-trivia)
   * character is not `;` — i.e. the body ends in a tail expression and the
   * fn returns its value. False for statement-only and empty bodies.
   */
  bodyTailIsExpression: boolean;
}

/**
 * Locate the depth-0 `fn <fnName>` in a verbatim item slice and report the
 * spans the signature completion rewrites. Returns null when the fn can't
 * be found or its parameter list never closes.
 */
export function scanRustFnCompletion(text: string, fnName: string): RustFnCompletionScan | null {
  let pos = 0;
  let brace = 0, paren = 0, bracket = 0;

  while (pos < text.length) {
    const out = { word: "" };
    const step = stepToken(text, pos, out);
    if (step.next !== -1) {
      if (out.word === "fn" && brace === 0 && paren === 0 && bracket === 0) {
        const nameMatch = /^\s*([A-Za-z_]\w*)/.exec(text.slice(step.next));
        if (nameMatch && nameMatch[1] === fnName) {
          const nameEnd = step.next + nameMatch[0].length;
          const parenIdx = findParamListStart(text, nameEnd);
          if (parenIdx === -1) return null;
          const { segments, spans, end: paramsEnd } = scanParamSegments(text, parenIdx);
          if (paramsEnd === -1) return null;
          const untypedParams = segments
            .map((seg, i) => (paramSegmentIsBare(seg) ? i : -1))
            .filter(i => i !== -1);
          const hasReturnType = extractReturnTypeText(text, paramsEnd) !== "";
          return {
            paramSpans: spans,
            untypedParams,
            paramsEnd,
            hasReturnType,
            bodyTailIsExpression: fnBodyTailIsExpression(text, paramsEnd),
          };
        }
      }
      pos = step.next;
      continue;
    }
    const c = text[pos];
    if (c === "{") brace++;
    else if (c === "}") brace--;
    else if (c === "(") paren++;
    else if (c === ")") paren--;
    else if (c === "[") bracket++;
    else if (c === "]") bracket--;
    pos++;
  }
  return null;
}

/**
 * From one past the parameter list's `)`, find the fn body `{...}` and
 * decide whether it ends in a tail expression: the last significant
 * (non-trivia, non-whitespace) character before the closing `}` is not `;`.
 * Empty and `;`-terminated (declaration-only) bodies report false.
 */
function fnBodyTailIsExpression(text: string, paramsEnd: number): boolean {
  // Skip the return type / where clause to the depth-0 body `{`.
  let pos = paramsEnd;
  let angle = 0, paren = 0, bracket = 0;
  while (pos < text.length) {
    const step = stepToken(text, pos);
    if (step.next !== -1) { pos = step.next; continue; }
    const c = text[pos];
    if (c === "{" && angle === 0 && paren === 0 && bracket === 0) break;
    if (c === ";" && angle === 0 && paren === 0 && bracket === 0) return false; // bodyless fn
    if (c === "-" && text[pos + 1] === ">") { pos += 2; continue; }
    if (c === "<") angle++;
    else if (c === ">") { if (angle > 0) angle--; }
    else if (c === "(") paren++;
    else if (c === ")") paren--;
    else if (c === "[") bracket++;
    else if (c === "]") bracket--;
    pos++;
  }
  if (pos >= text.length) return false;

  // Balanced scan of the body, remembering the last significant character.
  const bodyOpen = pos;
  let depth = 0;
  let lastSig = -1;
  while (pos < text.length) {
    const step = stepToken(text, pos);
    if (step.next !== -1) {
      // Comments are trivia; strings/identifiers are significant content.
      const c = text[pos];
      const isComment = c === "/" && (text[pos + 1] === "/" || text[pos + 1] === "*");
      if (!isComment) lastSig = step.next - 1;
      pos = step.next;
      continue;
    }
    const c = text[pos];
    if (c === "{") { depth++; if (pos !== bodyOpen) lastSig = pos; pos++; continue; }
    if (c === "}") {
      depth--;
      if (depth === 0) break; // body close — do not count it
      lastSig = pos;
      pos++;
      continue;
    }
    if (!/\s/.test(c)) lastSig = pos;
    pos++;
  }
  if (lastSig <= bodyOpen) return false; // empty body
  return text[lastSig] !== ";";
}

/**
 * Raw return type text starting at `pos` (one past the closing `)` of the
 * parameter list). Returns "" when there is no `->`. Capture ends at the
 * depth-0 body `{`, a depth-0 `;`, or a depth-0 `where` keyword.
 */
function extractReturnTypeText(text: string, pos: number): string {
  while (pos < text.length && /\s/.test(text[pos])) pos++;
  if (text[pos] !== "-" || text[pos + 1] !== ">") return "";
  pos += 2;
  const start = pos;
  let angle = 0, paren = 0, bracket = 0;

  while (pos < text.length) {
    const out = { word: "" };
    const step = stepToken(text, pos, out);
    if (step.next !== -1) {
      if (out.word === "where" && angle === 0 && paren === 0 && bracket === 0) {
        return text.slice(start, pos).trim();
      }
      pos = step.next;
      continue;
    }
    const c = text[pos];
    if ((c === "{" || c === ";") && angle === 0 && paren === 0 && bracket === 0) {
      return text.slice(start, pos).trim();
    }
    if (c === "-" && text[pos + 1] === ">") { pos += 2; continue; } // nested fn types
    if (c === "<") angle++;
    else if (c === ">") { if (angle > 0) angle--; }
    else if (c === "(") paren++;
    else if (c === ")") paren--;
    else if (c === "[") bracket++;
    else if (c === "]") bracket--;
    pos++;
  }
  return text.slice(start).trim();
}

/**
 * Does a generic parameter list slice (the text between the fn name and the
 * opening `(`, e.g. `<T, U: Bound>` or `<'a>`) declare any NON-lifetime
 * parameter? Lifetime-only lists are erasable at the export boundary.
 */
export function hasNonLifetimeGenerics(genericsText: string): boolean {
  const open = genericsText.indexOf("<");
  if (open === -1) return false;
  const close = genericsText.lastIndexOf(">");
  const inner = genericsText.slice(open + 1, close === -1 ? undefined : close);

  let depth = 0;
  let segStart = 0;
  const segs: string[] = [];
  for (let i = 0; i < inner.length; i++) {
    const c = inner[i];
    if (c === "<" || c === "(" || c === "[") depth++;
    else if (c === ">" || c === ")" || c === "]") depth--;
    else if (c === "," && depth === 0) { segs.push(inner.slice(segStart, i)); segStart = i + 1; }
  }
  segs.push(inner.slice(segStart));

  return segs.some(seg => {
    const t = seg.trim();
    return t.length > 0 && !t.startsWith("'");
  });
}

/** Primary declared name of a non-fn item, when extractable. */
function itemDeclaredName(text: string, kind: RustItemKind): string | undefined {
  const stripped = text
    .replace(/^\s*(?:#\[[^\n]*\]|\/\/\/[^\n]*)\s*$/gm, "")
    .replace(/^\s+/, "")
    .replace(PUB_RE, "");
  let m: RegExpExecArray | null = null;
  switch (kind) {
    case "struct": case "enum": case "union": case "trait": case "mod":
      m = new RegExp(`^${kind}\\s+([A-Za-z_]\\w*)`).exec(stripped);
      break;
    case "static": case "const":
      m = /^(?:static|const)\s+(?:mut\s+)?([A-Za-z_]\w*)/.exec(stripped);
      break;
    case "type":
      m = /^type\s+([A-Za-z_]\w*)/.exec(stripped);
      break;
    case "macro":
      m = /^macro_rules\s*!\s*([A-Za-z_]\w*)/.exec(stripped);
      break;
    case "extern":
      m = /^extern\s+crate\s+([A-Za-z_]\w*)/.exec(stripped);
      break;
    default:
      break;
  }
  return m ? m[1] : undefined;
}

/**
 * Scan exactly one Rust item whose anchor keyword begins at `anchorPos`.
 * Returns null when the anchor doesn't classify or the item never closes.
 */
export function scanRustItemAt(source: string, anchorPos: number): ScannedRustItem | null {
  const itemKind = classifyRustAnchor(source, anchorPos);
  if (!itemKind) return null;

  const end = scanItemEnd(source, anchorPos, itemKind);
  if (end === -1) return null;

  // Run-away guard for semicolon-terminated items: a Rust use/static/const/
  // type item never contains a blank line. If the scan crossed one, the `;`
  // it found belongs to something else — reject.
  if (SEMI_ONLY.has(itemKind) && /\n[ \t]*\n/.test(source.slice(anchorPos, end))) {
    return null;
  }

  const start = itemStartWithPrefix(source, anchorPos);
  const text = source.slice(start, end);

  const fns = itemKind === "fn" ? extractRustFnSignatures(text) : [];
  let name: string | undefined;
  const bindings: string[] = [];
  if (itemKind === "fn") {
    name = fns[0]?.name;
    for (const sig of fns) bindings.push(sig.name);
  } else if (itemKind === "use") {
    bindings.push(...extractUseBindings(text));
  } else {
    name = itemDeclaredName(text, itemKind);
    if (name) bindings.push(name);
  }

  return { itemKind, start, anchorStart: anchorPos, end, text, name, fns, bindings };
}

/**
 * Reference extraction of all top-level items from a pure Rust source file.
 * Used by the round-trip oracle; reuses the exact production scanner.
 * Module-level inner attributes (`#![...]`) and inner doc comments (`//!`)
 * are module trivia, not items.
 */
export function scanTopLevelRustItems(source: string): ScannedRustItem[] {
  const items: ScannedRustItem[] = [];
  let pos = 0;

  while (pos < source.length) {
    const c = source[pos];
    if (/\s/.test(c)) { pos++; continue; }
    if (c === "/" && (source[pos + 1] === "/" || source[pos + 1] === "*")) {
      const step = stepToken(source, pos);
      pos = step.next;
      continue;
    }
    if (c === "#") {
      // Attribute line. Outer attributes (#[...]) belong to the next item and
      // are picked up by the walk-back; inner attributes (#![...]) are module
      // trivia. Either way, skip the balanced bracket group.
      let p = pos + 1;
      if (source[p] === "!") p++;
      if (source[p] === "[") {
        let depth = 0;
        while (p < source.length) {
          const step = stepToken(source, p);
          if (step.next !== -1) { p = step.next; continue; }
          if (source[p] === "[") depth++;
          else if (source[p] === "]") { depth--; if (depth === 0) { p++; break; } }
          p++;
        }
        pos = p;
        continue;
      }
      pos++;
      continue;
    }
    const item = scanRustItemAt(source, pos);
    if (item) {
      items.push(item);
      pos = item.end;
      continue;
    }
    // Not an item anchor — skip one token to make progress.
    const step = stepToken(source, pos);
    pos = step.next !== -1 ? step.next : pos + 1;
  }

  return items;
}

/**
 * Definite-Rust gate for module-level `const NAME: Type = ...;` items: the
 * declared type must carry a Rust-only signal (primitive ints/floats, `::`
 * paths, references/lifetimes). TS `const x: number = 5` must NOT match.
 */
export function rustConstTypeSignal(typeText: string): boolean {
  // `::` paths, lifetime refs (&'a str), &str/&[..]/&mut — Rust-only.
  // A LEADING `&` is a Rust reference type: TS only uses `&` BETWEEN types
  // (intersection `A & B`), never first. Raw pointers (`*mut T`/`*const T`)
  // are likewise Rust-only.
  if (/::/.test(typeText) || /&\s*(?:'\w+|mut\b|str\b|\[)/.test(typeText)) return true;
  if (/^\s*&/.test(typeText) || /^\s*\*\s*(?:mut|const)\b/.test(typeText)) return true;
  for (const word of typeText.split(/[^A-Za-z0-9_]+/)) {
    if (RUST_PRIMITIVES.has(word)) return true;
  }
  return /\b(?:Vec|Option|Result|Box|Arc|Rc|LazyLock|OnceLock|Mutex|RwLock|HashMap|HashSet|BTreeMap)\s*</.test(typeText);
}
