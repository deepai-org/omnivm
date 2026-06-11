# Boundary Generics: Dispatch, Not Instantiation

Status: design of record (2026-06-11). Builds on gradual typing
(`docs/rust-compatibility.md`) and the lane architecture. Not yet
implemented; staging at the end.

## The realization

Monomorphization fails at the cross-language boundary because `T` is open.
But nothing crosses our boundary except the serde data model — a closed set
of shapes the wire format itself tags. At the boundary, `T` is not open;
it is a closed sum with runtime type information we already paid for (the
codec tag). Boundary generics are therefore a **dispatch** problem, and we
solve it in tiers that degrade gracefully:

> Resolution order is static → Dyn contract → closed-set dispatch →
> diagnostic, and each tier is observationally equivalent to the one below
> it — same semantics, different cost. Users never need to know which tier
> fired.

This is already the house contract: the omni_value_t typed lane vs the JSON
envelope is selected invisibly per call under exactly this rule.

## Tier 1 — per-call-site stamping (static, zero cost)

The compiler sees every foreign call site. For each call to a
boundary-reachable generic fn with inferable evidence (literals, gradual
typing flow, DataFrame provenance), synthesize one concrete wrapper per
instantiation (`__omnivm_smooth_f64`) and rewrite that call op to target
it. The generic declaration stays byte-verbatim.

This CORRECTS the current stamping design: today stamping rewrites the
declaration and therefore requires all call sites to agree. Per-site
stamping needs no agreement — each site gets its own stamp. The synthesis
machinery is the existing owned-param adapter generator pointed at a new
target.

Future: a statically typed guest (TypeScript) can receive generics AS
generics — emit a generic signature in its declarations and monomorphize
from the TS-side usage. The boundary is only as dynamic as the caller.

## Tier 2 — exhaustive instantiation over the serde lattice

When call sites are not statically evident: enumerate candidate
instantiations over the serde-model leaves that satisfy the bounds, compile
all of them, export ONE dispatcher that switches on the incoming codec tag.
The wire format is the RTTI; we read the tag the codec already wrote.

**The implementation unlock (we have no trait solver, and don't need one):
let rustc prune the lattice via autoref specialization inside a single
compile.** For each candidate `C`, the generated dispatcher arm calls
through a probe token whose by-value impl exists only `where C: Bounds`;
the autoref fallback arm resolves to "no instantiation for this tag".
Satisfying candidates become real calls; non-satisfying ones quietly
collapse — no probe builds, no TS-side bound evaluation. (Same pattern as
the error/outcome tokens.)

Caps: candidate-count × generic-arity product is bounded (bounds keep real
products tiny); exceeding the cap falls to Tier 3 or the diagnostic, naming
the product.

## Tier 3 — the Dyn instantiation (always works, runtime-checked)

`omnivm::Dyn` is a valid instantiation target. Give it runtime-dispatched
impls of the common bound vocabulary (`Into<f64>` coerce-or-error,
`PartialOrd`, `Clone`, `Display`, `Hash` with documented float caveats);
auto-export `f::<Dyn>` whenever a generic's bounds are a subset of the
vocabulary. Trait bounds become gradual contracts: checked at the moment of
use, blame at the call site, failures as the catchable python-style panics
Dyn already throws.

Honest caveats, decided:
- **Parametricity is erased, not preserved.** A `fn id<T>(x: T) -> T`
  instantiated at Dyn can observe its argument. We document boundary
  generics as erased; we do not attempt polymorphic blame.
- **Coherence is safe by ownership**: Dyn is our type; the orphan rule
  prevents user impls of foreign traits on it. The vocabulary impl set
  lives with Dyn.

SHIP ORDER NOTE: Tier 3 ships BEFORE Tier 2. It is the always-works floor;
Tier 2 is then purely a performance upgrade over a working baseline.

## Tier 4 — deliberately not built

JIT monomorphization on cache miss (cranelift over shipped MIR) is the only
tier that makes never-before-seen foreign type shapes "just work", and we
are not building it. We are already a runtime-rustc system at the LOAD
boundary (units compile on first use); mid-execution stamping is the part
that does not fit this host. A tier-miss produces the diagnostic instead:
"no boundary instantiation exists — here is why each candidate failed",
with .poly coordinates and the annotate-to-fix hint.

## Sharp edges, decided early

- **Const generics**: Tier-1 evidence or an explicit small-domain dispatch
  table (`N in 1..=8`); otherwise diagnostic. No cleverness.
- **Multi-param generics**: product capped; the cap diagnostic names the
  product. Prefer Tier 3 when capped.
- **`dyn Trait` params**: Tier 3 fixes the current footnote — a foreign
  object crossing as `dyn Trait` is a Dyn wearing a vtable synthesized from
  its ObjectExport methods (the handle-proxy substrate already exists).
- **Return-position `-> T`**: the chosen instantiation determines the
  codec, same dispatch. `-> impl Trait` is already solved (gradual
  returns).

## One sentence

Stop treating the boundary as where monomorphization is impossible; treat
it as where the type universe is finally small enough to monomorphize
exhaustively — stamping when we can prove more, Dyn contracts when we can
prove less.
