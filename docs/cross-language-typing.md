# Cross-Language Typing: Your Types Reach Further, Not Away

For teams with an existing typed Python (or TypeScript, or typed Go)
codebase who worry that mixing a second language into a `.poly` file throws
that type work away. It does the opposite. This document is both the honest
audit and the contract.

## The one-sentence answer

Each language keeps its own type checker for its own code; OmniVM adds a
**boundary checker** that verifies agreement *across* the language seam —
something neither mypy nor rustc nor tsc can do alone — and hands your
existing checker the declarations it needs to cover the boundary too.

## What happens to your Python types (audited, with the gaps named)

1. **They are preserved, verbatim.** PolyScript parses Python
   structurally, annotations included (`x: int`, `-> str`, `list[int]`,
   `int | None`, `Optional`, `@dataclass`). When Python executes it is
   emitted by span extraction — the original source byte-for-byte. So
   `mypy` still runs on your `.py` files unchanged, and the type-bearing
   source is exactly what runs.

2. **They are checked across the boundary.** The compiler lowers your
   annotations into a canonical type model and compares them against the
   *other language's* signature on a `safe | coerce | check | incompatible`
   lattice. A `list[int]` flowing into a Rust `fn need_int(x: i64)` is now a
   **compile error** that names both runtimes and the `.poly` line and stops
   the build — a cross-language bug class your single-language checker
   cannot see. A lossy crossing (`float` → `i64`) is a **warning**.

3. **They earn the fast, safe lanes.** A typed argument (`n: int`,
   `frame: DataFrame`) is now used as evidence: it rides the typed
   `omni_value_t` lane or the zero-copy Arrow lane *and* gets statically
   checked, instead of degrading to the dynamic `Dyn` value. Annotating is a
   local upgrade that buys speed and safety — never a rewrite.

4. **mypy covers the boundary too.** `polyc-stubs` generates `.pyi` type
   stubs for every function defined in another runtime but callable from
   Python (and `.d.ts` for the TS guest). Your existing `mypy` run now
   type-checks your calls *into* Rust/Go/JS. We extend your tooling's reach
   rather than escaping it. (Demo: `docs/cross-language-stubs-example/` —
   real mypy flags a wrong-typed call into a Rust function against the
   generated stub.)

## The boundary type model (the contract)

Everything crosses through one closed value model (the serde data model:
null, bool, int, float, string, array, object, plus the Arrow plane for
tabular data). Each language's types project onto it:

| canonical | Python | Rust | TypeScript |
|---|---|---|---|
| int | `int` | `i64`/`u*` | `number` |
| float | `float` | `f64` | `number` |
| string | `str` | `String`/`&str` | `string` |
| bool | `bool` | `bool` | `boolean` |
| array<T> | `list[T]` | `Vec<T>`/`&[T]` | `T[]` |
| option<T> | `T \| None` | `Option<T>` | `T \| null` |
| map<K,V> | `dict[K,V]` | `HashMap<K,V>` | `Record<K,V>` |
| table | pandas/polars | `DataFrame` (zero-copy) | — |
| dynamic | (untyped) | `omnivm::Dyn` | `unknown` |

**Soundness guarantee: no silent type confusion.** Lossless coercions are
automatic (`int`→`float`). Lossy or cross-kind ones (`2.7`→`i64`, an int
where a `bool` is expected) are **structured, catchable errors**, never
silent truncation and never undefined behavior — checked statically where
both sides are typed, enforced at runtime otherwise. This is *stronger* than
ordinary dynamic FFI.

**Gradual, not all-or-nothing.** Untyped boundary slots become `Dyn` and are
checked at the moment of use (catchable, Python-flavored errors). Add a type
and that slot upgrades to static checking + a faster lane. You choose how
much typing discipline to spend, per parameter, and you never lose what you
already wrote.

## Honest limits

- Boundary checking is **gradual**: where one side is genuinely untyped, the
  agreement is enforced at runtime (a catchable error), not proven at
  compile time. Type more of the boundary to push more checks earlier.
- Generic functions crossing the boundary are **erased, not parametric**
  (see `docs/rust-boundary-generics.md`); their `.pyi` params surface as
  `Any`.
- `DataFrame` maps to `Any` in `.pyi` today (with a comment) rather than a
  typed protocol.

## Try it

```bash
# generate .pyi/.d.ts so mypy/tsc cover your cross-language calls
polyc-stubs your_app.poly --pyi --out your_app
mypy your_app.py            # now sees the Rust/Go/JS signatures

# a real boundary mismatch fails the build, naming both sides
polyc your_app.poly         # error: boundary-type-mismatch: 'arg0:need_int'
                            #   (Array<int> in python) cannot flow to rust as i64
```

See also: `docs/rust-compatibility.md` (the boundary contract rules),
`docs/lane-parity.md` (which crossings are typed/zero-copy per language),
`docs/rust-boundary-generics.md` (generics at the seam).
