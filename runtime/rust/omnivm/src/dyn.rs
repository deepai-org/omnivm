//! `omnivm::Dyn` — the gradually-typed boundary value.
//!
//! PolyScript lets a Rust `fn` omit parameter types (`fn score(review)`).
//! The compiler completes such signatures deterministically: every untyped
//! parameter becomes `omnivm::Dyn` (unless call-site evidence stamps a
//! concrete type), and an omitted return type whose body ends in an
//! expression becomes `-> impl omnivm::serde::Serialize`.
//!
//! `Dyn` is a transparent newtype over [`serde_json::Value`] with dynamic,
//! Python-flavored semantics:
//!
//! - `review["score"]`, `items[0]` indexing (chainable — returns `&Dyn`),
//! - arithmetic (`+ - * /`) between `Dyn` / `&Dyn` / `i64` / `f64` with
//!   numeric coercion (int op int stays int, anything float goes float) and
//!   `+` as string concatenation,
//! - comparisons against `i64` / `f64` / `&str` / `bool`,
//! - panicking accessors (`as_i64`, `as_f64`, `as_str`, `as_bool`) plus
//!   non-panicking `try_` variants, `len()`, `iter()`, `is_null()`.
//!
//! Type errors panic with Python-style messages (`TypeError: ...`,
//! `KeyError: ...`); every export entry point catches panics and turns them
//! into structured, catchable runtime errors — proven by the error-handling
//! corpus. Annotate the parameter with a concrete type to leave the dynamic
//! regime and use native methods.
//!
//! # Tier-3 boundary generics: the bound vocabulary
//!
//! `Dyn` is the always-works instantiation target for boundary generics
//! (`docs/rust-boundary-generics.md`, Tier 3): a generic `f<T: Bounds>`
//! whose bounds are a subset of the vocabulary below instantiates as
//! `f::<Dyn>`. Trait bounds become gradual contracts — checked at the
//! moment of use, blame at the call site, failures as the same catchable
//! Python-style panics the rest of the dynamic surface throws.
//!
//! The vocabulary: `Clone`, `Debug`, `Default` (null), `Display`,
//! `Serialize`/`Deserialize`, `From`/`Into` against `i64`/`f64`/`bool`/
//! `String` in both directions (coerce-or-panic outbound), `FromStr`
//! (JSON-or-string, infallible), `PartialEq`/`Eq`/`PartialOrd`/`Ord`/
//! `Hash` (one canonical semantics — see the float caveats at the
//! Eq/Ord/Hash section), the arithmetic operators `+ - * / %` (+ the
//! `*Assign` forms), unary `-`/`!`, and `Sum`/`Product`.
//!
//! Honest caveats, decided (the design doc's language):
//!
//! - **Parametricity is erased, not preserved.** A `fn id<T>(x: T) -> T`
//!   instantiated at `Dyn` can observe its argument. Boundary generics are
//!   documented as erased; we do not attempt polymorphic blame.
//! - **Float caveats** for `Eq`/`Ord`/`Hash`: floats compare bitwise, so
//!   `0.0 != -0.0` and `Dyn(1) != Dyn(1.0)` Dyn-vs-Dyn; `Ord` (and the
//!   consistent Dyn-vs-Dyn `PartialOrd`) is a canonical total order over
//!   the JSON value space. The Python-flavored coercing comparisons are
//!   the heterogeneous-scalar impls (`dyn == 1i64`, `dyn > 4.5f64`). Full
//!   discussion at the Eq/Ord/Hash section.

use std::cmp::Ordering;
use std::fmt;
use std::ops::Index;

use serde_json::Value;

/// A dynamically typed value crossing the runtime boundary.
///
/// `Default` is null (`Dyn(Value::Null)`), matching what a missing key or
/// out-of-range index produces. Equality (`PartialEq`, manual, below) is
/// deep structural equality with bitwise float comparison.
#[repr(transparent)]
#[derive(Clone, Debug, Default)]
pub struct Dyn(pub Value);

/// Serialize any value into the manifest value model (`serde_json::Value`).
/// The export shims already serialize returns through serde; this helper is
/// the explicit form for user code (`omnivm::to_value(anything)`).
pub fn to_value<T: serde::Serialize>(v: T) -> Value {
    match serde_json::to_value(v) {
        Ok(value) => value,
        Err(e) => panic!("TypeError: value is not serializable: {e}"),
    }
}

/// Internal numeric view used by arithmetic/comparison coercion.
#[derive(Clone, Copy)]
enum Num {
    I(i64),
    F(f64),
}

impl Num {
    fn as_f64(self) -> f64 {
        match self {
            Num::I(x) => x as f64,
            Num::F(x) => x,
        }
    }
}

impl Dyn {
    /// Borrow a `serde_json::Value` as `&Dyn`.
    // SAFETY of the cast: Dyn is #[repr(transparent)] over Value, so the
    // pointer cast is layout-correct by construction (the standard ref-cast
    // newtype idiom). This is what makes `d["a"]["b"]` chaining possible —
    // Index must return a reference.
    pub fn from_ref(value: &Value) -> &Dyn {
        unsafe { &*(value as *const Value as *const Dyn) }
    }

    /// Python-style type name, used in error messages.
    pub fn type_name(&self) -> &'static str {
        match &self.0 {
            Value::Null => "NoneType",
            Value::Bool(_) => "bool",
            Value::Number(n) => {
                if n.is_f64() {
                    "float"
                } else {
                    "int"
                }
            }
            Value::String(_) => "str",
            Value::Array(_) => "list",
            Value::Object(_) => "dict",
        }
    }

    pub fn is_null(&self) -> bool {
        self.0.is_null()
    }

    /// Owned lookup (`d.get("key")`) — clones the element; missing keys and
    /// non-dict receivers return `Dyn(Null)` instead of panicking.
    pub fn get(&self, key: &str) -> Dyn {
        match &self.0 {
            Value::Object(map) => Dyn(map.get(key).cloned().unwrap_or(Value::Null)),
            _ => Dyn(Value::Null),
        }
    }

    /// Owned positional lookup (`d.at(0)`) — clone; out of range is `Null`.
    pub fn at(&self, index: usize) -> Dyn {
        match &self.0 {
            Value::Array(items) => Dyn(items.get(index).cloned().unwrap_or(Value::Null)),
            _ => Dyn(Value::Null),
        }
    }

    pub fn len(&self) -> usize {
        match &self.0 {
            Value::Array(items) => items.len(),
            Value::Object(map) => map.len(),
            Value::String(s) => s.chars().count(),
            _ => panic!("TypeError: object of type '{}' has no len()", self.type_name()),
        }
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Python truthiness: `None`, `False`, `0`, `0.0`, `""`, `[]`, `{}` are
    /// falsy; everything else is truthy. Never panics. This is also the
    /// semantics of unary `!` on `Dyn` (Python's `not`, NOT Rust's bitwise
    /// integer `!`).
    pub fn truthy(&self) -> bool {
        match &self.0 {
            Value::Null => false,
            Value::Bool(b) => *b,
            Value::Number(n) => n.as_f64().is_some_and(|f| f != 0.0),
            Value::String(s) => !s.is_empty(),
            Value::Array(items) => !items.is_empty(),
            Value::Object(map) => !map.is_empty(),
        }
    }

    /// Iterate the elements of a list. Panics on non-lists.
    pub fn iter(&self) -> impl Iterator<Item = &Dyn> + '_ {
        match &self.0 {
            Value::Array(items) => items.iter().map(Dyn::from_ref),
            _ => panic!("TypeError: '{}' object is not iterable", self.type_name()),
        }
    }

    /// Iterate a dict's keys (Python's `.keys()`). Panics on non-dicts with
    /// Python's attribute-error dialect.
    pub fn keys(&self) -> impl Iterator<Item = &str> + '_ {
        match &self.0 {
            Value::Object(map) => map.keys().map(String::as_str),
            _ => panic!("AttributeError: '{}' object has no attribute 'keys'", self.type_name()),
        }
    }

    /// Iterate a dict's values (Python's `.values()`).
    pub fn values(&self) -> impl Iterator<Item = &Dyn> + '_ {
        match &self.0 {
            Value::Object(map) => map.values().map(Dyn::from_ref),
            _ => panic!("AttributeError: '{}' object has no attribute 'values'", self.type_name()),
        }
    }

    /// Iterate a dict's `(key, value)` pairs (Python's `.items()`).
    pub fn items(&self) -> impl Iterator<Item = (&str, &Dyn)> + '_ {
        match &self.0 {
            Value::Object(map) => map.iter().map(|(k, v)| (k.as_str(), Dyn::from_ref(v))),
            _ => panic!("AttributeError: '{}' object has no attribute 'items'", self.type_name()),
        }
    }

    pub fn try_as_i64(&self) -> Option<i64> {
        match &self.0 {
            Value::Number(n) => n.as_i64().or_else(|| n.as_f64().map(|f| f as i64)),
            Value::Bool(b) => Some(*b as i64),
            _ => None,
        }
    }

    pub fn as_i64(&self) -> i64 {
        self.try_as_i64()
            .unwrap_or_else(|| panic!("TypeError: expected int, got '{}'", self.type_name()))
    }

    pub fn try_as_f64(&self) -> Option<f64> {
        match &self.0 {
            Value::Number(n) => n.as_f64(),
            Value::Bool(b) => Some(*b as i64 as f64),
            _ => None,
        }
    }

    pub fn as_f64(&self) -> f64 {
        self.try_as_f64()
            .unwrap_or_else(|| panic!("TypeError: expected float, got '{}'", self.type_name()))
    }

    pub fn try_as_str(&self) -> Option<&str> {
        self.0.as_str()
    }

    pub fn as_str(&self) -> &str {
        self.try_as_str()
            .unwrap_or_else(|| panic!("TypeError: expected str, got '{}'", self.type_name()))
    }

    pub fn try_as_bool(&self) -> Option<bool> {
        self.0.as_bool()
    }

    pub fn as_bool(&self) -> bool {
        self.try_as_bool()
            .unwrap_or_else(|| panic!("TypeError: expected bool, got '{}'", self.type_name()))
    }

    fn num(&self) -> Option<Num> {
        match &self.0 {
            Value::Number(n) => match n.as_i64() {
                Some(x) => Some(Num::I(x)),
                None => n.as_f64().map(Num::F),
            },
            _ => None,
        }
    }
}

// ── serde (transparent) ─────────────────────────────────────────────

impl serde::Serialize for Dyn {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        self.0.serialize(serializer)
    }
}

impl<'de> serde::Deserialize<'de> for Dyn {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        Value::deserialize(deserializer).map(Dyn)
    }
}

// ── Display (Python-flavored, matching the error-message dialect) ───

impl fmt::Display for Dyn {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.0 {
            Value::String(s) => f.write_str(s),
            Value::Null => f.write_str("None"),
            Value::Bool(true) => f.write_str("True"),
            Value::Bool(false) => f.write_str("False"),
            other => write!(f, "{other}"),
        }
    }
}

// ── conversions ─────────────────────────────────────────────────────

impl From<Value> for Dyn {
    fn from(value: Value) -> Dyn {
        Dyn(value)
    }
}

impl From<i64> for Dyn {
    fn from(value: i64) -> Dyn {
        Dyn(Value::from(value))
    }
}

impl From<f64> for Dyn {
    fn from(value: f64) -> Dyn {
        Dyn(Value::from(value))
    }
}

impl From<bool> for Dyn {
    fn from(value: bool) -> Dyn {
        Dyn(Value::from(value))
    }
}

impl From<&str> for Dyn {
    fn from(value: &str) -> Dyn {
        Dyn(Value::from(value))
    }
}

impl From<String> for Dyn {
    fn from(value: String) -> Dyn {
        Dyn(Value::from(value))
    }
}

// ── coercing scalar extraction (`T: Into<f64>`-style bounds) ────────
//
// The Tier-3 counterparts of the panicking accessors: these make bounds
// like `T: Into<f64>` satisfiable at `T = Dyn`. Semantics and panic dialect
// are EXACTLY the matching accessor's (`as_f64` / `as_i64` / `as_bool` /
// `as_str` — Python `TypeError: expected ..., got '<type_name>'`); reach
// for the `try_as_*` accessors when failure should not panic.

impl From<Dyn> for f64 {
    /// Coerce-or-panic, same as [`Dyn::as_f64`]: int, float and bool all
    /// coerce; anything else panics `TypeError: expected float, got '...'`.
    fn from(value: Dyn) -> f64 {
        value.as_f64()
    }
}

impl From<Dyn> for i64 {
    /// Coerce-or-panic, same as [`Dyn::as_i64`]: ints pass through, floats
    /// truncate, bools widen (`True` → 1); anything else panics
    /// `TypeError: expected int, got '...'`.
    fn from(value: Dyn) -> i64 {
        value.as_i64()
    }
}

impl From<Dyn> for bool {
    /// Strict, same as [`Dyn::as_bool`]: only a JSON bool converts (this is
    /// NOT truthiness — see [`Dyn::truthy`] for that); anything else panics
    /// `TypeError: expected bool, got '...'`.
    fn from(value: Dyn) -> bool {
        value.as_bool()
    }
}

impl From<Dyn> for String {
    /// Strict, same as [`Dyn::as_str`]: only a JSON string converts (moves
    /// the owned string out); anything else panics
    /// `TypeError: expected str, got '...'`. Use `to_string()` (via
    /// `Display`) to stringify arbitrary values instead.
    fn from(value: Dyn) -> String {
        match value.0 {
            Value::String(s) => s,
            other => panic!("TypeError: expected str, got '{}'", Dyn(other).type_name()),
        }
    }
}

// ── FromStr (`"...".parse::<Dyn>()`) ────────────────────────────────

impl std::str::FromStr for Dyn {
    type Err = std::convert::Infallible;

    /// Parses JSON first (`"42"` → int, `"2.5"` → float, `"true"` → bool,
    /// `"[1,2]"` → list, `"\"x\""` → str `x`); anything that is not valid
    /// JSON falls back to a plain string `Dyn`, so parsing NEVER fails
    /// (`Err = Infallible`). This makes `T: FromStr` bounds satisfiable at
    /// `T = Dyn`.
    fn from_str(s: &str) -> Result<Dyn, Self::Err> {
        Ok(serde_json::from_str::<Value>(s).map(Dyn).unwrap_or_else(|_| Dyn::from(s)))
    }
}

// ── iteration (`for x in dyn` / `for x in &dyn`) ────────────────────
//
// Owned iteration is ARRAYS-only and yields owned `Dyn` elements; borrowed
// iteration yields `&Dyn` via the ref-cast idiom. Anything non-array panics
// with the Python dialect (`TypeError: 'dict' object is not iterable` — for
// dicts, reach for `.keys()` / `.values()` / `.items()` explicitly).

impl IntoIterator for Dyn {
    type Item = Dyn;
    type IntoIter = std::vec::IntoIter<Dyn>;

    fn into_iter(self) -> Self::IntoIter {
        match self.0 {
            Value::Array(items) => items.into_iter().map(Dyn).collect::<Vec<Dyn>>().into_iter(),
            other => panic!("TypeError: '{}' object is not iterable", Dyn(other).type_name()),
        }
    }
}

impl<'a> IntoIterator for &'a Dyn {
    type Item = &'a Dyn;
    type IntoIter = std::iter::Map<std::slice::Iter<'a, Value>, fn(&Value) -> &Dyn>;

    fn into_iter(self) -> Self::IntoIter {
        match &self.0 {
            Value::Array(items) => items.iter().map(Dyn::from_ref as fn(&Value) -> &Dyn),
            _ => panic!("TypeError: '{}' object is not iterable", self.type_name()),
        }
    }
}

// ── indexing (chainable: returns &Dyn) ──────────────────────────────

impl Index<&str> for Dyn {
    type Output = Dyn;
    fn index(&self, key: &str) -> &Dyn {
        match &self.0 {
            Value::Object(map) => match map.get(key) {
                Some(v) => Dyn::from_ref(v),
                None => panic!("KeyError: '{key}'"),
            },
            _ => panic!(
                "TypeError: '{}' object is not subscriptable (key '{key}')",
                self.type_name()
            ),
        }
    }
}

impl Index<usize> for Dyn {
    type Output = Dyn;
    fn index(&self, index: usize) -> &Dyn {
        match &self.0 {
            Value::Array(items) => match items.get(index) {
                Some(v) => Dyn::from_ref(v),
                None => panic!(
                    "IndexError: list index out of range (index {index}, len {})",
                    items.len()
                ),
            },
            _ => panic!(
                "TypeError: '{}' object is not subscriptable (index {index})",
                self.type_name()
            ),
        }
    }
}

// ── arithmetic ──────────────────────────────────────────────────────

/// One coercing binary numeric op (Python-style error messages; `+` also
/// concatenates strings). `int op int` stays int; any float operand makes
/// the result float; integer division (or modulo) by zero panics like
/// Python. `%` is remainder with Rust sign semantics, matching the existing
/// truncating integer `/`.
fn arith(lhs: &Dyn, rhs: &Dyn, op: char) -> Dyn {
    if op == '+' {
        if let (Value::String(a), Value::String(b)) = (&lhs.0, &rhs.0) {
            return Dyn(Value::String(format!("{a}{b}")));
        }
    }
    let (Some(a), Some(b)) = (lhs.num(), rhs.num()) else {
        panic!(
            "TypeError: unsupported operand type(s) for {op}: '{}' and '{}'",
            lhs.type_name(),
            rhs.type_name()
        );
    };
    if let (Num::I(x), Num::I(y)) = (a, b) {
        return Dyn::from(match op {
            '+' => x.wrapping_add(y),
            '-' => x.wrapping_sub(y),
            '*' => x.wrapping_mul(y),
            '%' => {
                if y == 0 {
                    panic!("ZeroDivisionError: integer modulo by zero");
                }
                x.wrapping_rem(y)
            }
            _ => {
                if y == 0 {
                    panic!("ZeroDivisionError: division by zero");
                }
                x / y
            }
        });
    }
    let (x, y) = (a.as_f64(), b.as_f64());
    Dyn::from(match op {
        '+' => x + y,
        '-' => x - y,
        '*' => x * y,
        '%' => x % y,
        _ => x / y,
    })
}

macro_rules! dyn_binop {
    ($trait:ident, $method:ident, $opch:literal) => {
        impl std::ops::$trait<Dyn> for Dyn {
            type Output = Dyn;
            fn $method(self, rhs: Dyn) -> Dyn {
                arith(&self, &rhs, $opch)
            }
        }
        impl std::ops::$trait<&Dyn> for Dyn {
            type Output = Dyn;
            fn $method(self, rhs: &Dyn) -> Dyn {
                arith(&self, rhs, $opch)
            }
        }
        impl std::ops::$trait<Dyn> for &Dyn {
            type Output = Dyn;
            fn $method(self, rhs: Dyn) -> Dyn {
                arith(self, &rhs, $opch)
            }
        }
        impl std::ops::$trait<&Dyn> for &Dyn {
            type Output = Dyn;
            fn $method(self, rhs: &Dyn) -> Dyn {
                arith(self, rhs, $opch)
            }
        }
        impl std::ops::$trait<i64> for Dyn {
            type Output = Dyn;
            fn $method(self, rhs: i64) -> Dyn {
                arith(&self, &Dyn::from(rhs), $opch)
            }
        }
        impl std::ops::$trait<i64> for &Dyn {
            type Output = Dyn;
            fn $method(self, rhs: i64) -> Dyn {
                arith(self, &Dyn::from(rhs), $opch)
            }
        }
        impl std::ops::$trait<f64> for Dyn {
            type Output = Dyn;
            fn $method(self, rhs: f64) -> Dyn {
                arith(&self, &Dyn::from(rhs), $opch)
            }
        }
        impl std::ops::$trait<f64> for &Dyn {
            type Output = Dyn;
            fn $method(self, rhs: f64) -> Dyn {
                arith(self, &Dyn::from(rhs), $opch)
            }
        }
        impl std::ops::$trait<Dyn> for i64 {
            type Output = Dyn;
            fn $method(self, rhs: Dyn) -> Dyn {
                arith(&Dyn::from(self), &rhs, $opch)
            }
        }
        impl std::ops::$trait<&Dyn> for i64 {
            type Output = Dyn;
            fn $method(self, rhs: &Dyn) -> Dyn {
                arith(&Dyn::from(self), rhs, $opch)
            }
        }
        impl std::ops::$trait<Dyn> for f64 {
            type Output = Dyn;
            fn $method(self, rhs: Dyn) -> Dyn {
                arith(&Dyn::from(self), &rhs, $opch)
            }
        }
        impl std::ops::$trait<&Dyn> for f64 {
            type Output = Dyn;
            fn $method(self, rhs: &Dyn) -> Dyn {
                arith(&Dyn::from(self), rhs, $opch)
            }
        }
    };
}

dyn_binop!(Add, add, '+');
dyn_binop!(Sub, sub, '-');
dyn_binop!(Mul, mul, '*');
dyn_binop!(Div, div, '/');
dyn_binop!(Rem, rem, '%');

// ── compound assignment (`d += x` etc.) ─────────────────────────────
//
// Same coercion and panic dialect as the binary forms (they ARE the binary
// forms: `d += x` is `d = d + x`). Provided for Dyn / &Dyn / i64 / f64
// right-hand sides so `T: AddAssign`-family bounds are satisfiable at Dyn.

macro_rules! dyn_assign_op {
    ($trait:ident, $method:ident, $opch:literal) => {
        impl std::ops::$trait<Dyn> for Dyn {
            fn $method(&mut self, rhs: Dyn) {
                *self = arith(self, &rhs, $opch);
            }
        }
        impl std::ops::$trait<&Dyn> for Dyn {
            fn $method(&mut self, rhs: &Dyn) {
                *self = arith(self, rhs, $opch);
            }
        }
        impl std::ops::$trait<i64> for Dyn {
            fn $method(&mut self, rhs: i64) {
                *self = arith(self, &Dyn::from(rhs), $opch);
            }
        }
        impl std::ops::$trait<f64> for Dyn {
            fn $method(&mut self, rhs: f64) {
                *self = arith(self, &Dyn::from(rhs), $opch);
            }
        }
    };
}

dyn_assign_op!(AddAssign, add_assign, '+');
dyn_assign_op!(SubAssign, sub_assign, '-');
dyn_assign_op!(MulAssign, mul_assign, '*');
dyn_assign_op!(DivAssign, div_assign, '/');
dyn_assign_op!(RemAssign, rem_assign, '%');

// ── unary operators ─────────────────────────────────────────────────

impl std::ops::Neg for &Dyn {
    type Output = Dyn;
    /// Numeric negation (wrapping for ints, matching the binary int ops).
    /// Non-numbers panic Python-style:
    /// `TypeError: bad operand type for unary -: 'str'`.
    fn neg(self) -> Dyn {
        match self.num() {
            Some(Num::I(x)) => Dyn::from(x.wrapping_neg()),
            Some(Num::F(x)) => Dyn::from(-x),
            None => panic!("TypeError: bad operand type for unary -: '{}'", self.type_name()),
        }
    }
}

impl std::ops::Neg for Dyn {
    type Output = Dyn;
    fn neg(self) -> Dyn {
        -&self
    }
}

impl std::ops::Not for &Dyn {
    type Output = Dyn;
    /// `!d` is Python's `not`: truthiness negation (see [`Dyn::truthy`]),
    /// yields a bool `Dyn`, never panics. It is NOT Rust's bitwise integer
    /// `!` — `!Dyn(0)` is `Dyn(true)`, not `Dyn(-1)`.
    fn not(self) -> Dyn {
        Dyn(Value::Bool(!self.truthy()))
    }
}

impl std::ops::Not for Dyn {
    type Output = Dyn;
    fn not(self) -> Dyn {
        !&self
    }
}

// ── Sum / Product (numeric, coercing) ───────────────────────────────
//
// `iter.sum::<Dyn>()` folds `+` from an int `0` identity; `product` folds
// `*` from int `1`. int-only input stays int, any float coerces the result
// to float — the same coercion as the binary ops, with the same TypeError
// panics for non-numbers (the `0` identity does NOT concatenate strings;
// collect and join instead).

impl std::iter::Sum for Dyn {
    fn sum<I: Iterator<Item = Dyn>>(iter: I) -> Dyn {
        iter.fold(Dyn::from(0i64), |acc, x| arith(&acc, &x, '+'))
    }
}

impl<'a> std::iter::Sum<&'a Dyn> for Dyn {
    fn sum<I: Iterator<Item = &'a Dyn>>(iter: I) -> Dyn {
        iter.fold(Dyn::from(0i64), |acc, x| arith(&acc, x, '+'))
    }
}

impl std::iter::Product for Dyn {
    fn product<I: Iterator<Item = Dyn>>(iter: I) -> Dyn {
        iter.fold(Dyn::from(1i64), |acc, x| arith(&acc, &x, '*'))
    }
}

impl<'a> std::iter::Product<&'a Dyn> for Dyn {
    fn product<I: Iterator<Item = &'a Dyn>>(iter: I) -> Dyn {
        iter.fold(Dyn::from(1i64), |acc, x| arith(&acc, x, '*'))
    }
}

// ── comparisons against native scalars ──────────────────────────────

impl PartialEq<i64> for Dyn {
    fn eq(&self, other: &i64) -> bool {
        match self.num() {
            Some(Num::I(x)) => x == *other,
            Some(Num::F(x)) => x == *other as f64,
            None => false,
        }
    }
}

impl PartialEq<f64> for Dyn {
    fn eq(&self, other: &f64) -> bool {
        match self.num() {
            Some(n) => n.as_f64() == *other,
            None => false,
        }
    }
}

impl PartialEq<bool> for Dyn {
    fn eq(&self, other: &bool) -> bool {
        self.0.as_bool() == Some(*other)
    }
}

impl PartialEq<&str> for Dyn {
    fn eq(&self, other: &&str) -> bool {
        self.0.as_str() == Some(*other)
    }
}

impl PartialEq<str> for Dyn {
    fn eq(&self, other: &str) -> bool {
        self.0.as_str() == Some(other)
    }
}

impl PartialEq<String> for Dyn {
    fn eq(&self, other: &String) -> bool {
        self.0.as_str() == Some(other.as_str())
    }
}

impl PartialEq<Dyn> for i64 {
    fn eq(&self, other: &Dyn) -> bool {
        other == self
    }
}

impl PartialEq<Dyn> for f64 {
    fn eq(&self, other: &Dyn) -> bool {
        other == self
    }
}

impl PartialEq<Dyn> for bool {
    fn eq(&self, other: &Dyn) -> bool {
        other == self
    }
}

impl PartialEq<Dyn> for &str {
    fn eq(&self, other: &Dyn) -> bool {
        other == self
    }
}

impl PartialOrd<i64> for Dyn {
    fn partial_cmp(&self, other: &i64) -> Option<Ordering> {
        match self.num()? {
            Num::I(x) => x.partial_cmp(other),
            Num::F(x) => x.partial_cmp(&(*other as f64)),
        }
    }
}

impl PartialOrd<f64> for Dyn {
    fn partial_cmp(&self, other: &f64) -> Option<Ordering> {
        self.num()?.as_f64().partial_cmp(other)
    }
}

impl PartialOrd<&str> for Dyn {
    fn partial_cmp(&self, other: &&str) -> Option<Ordering> {
        self.0.as_str()?.partial_cmp(*other)
    }
}

impl PartialOrd<Dyn> for i64 {
    fn partial_cmp(&self, other: &Dyn) -> Option<Ordering> {
        other.partial_cmp(self).map(Ordering::reverse)
    }
}

impl PartialOrd<Dyn> for f64 {
    fn partial_cmp(&self, other: &Dyn) -> Option<Ordering> {
        other.partial_cmp(self).map(Ordering::reverse)
    }
}

/// Dyn-vs-Dyn ordering IS the canonical total order of [`Ord`] below
/// (`partial_cmp` is always `Some`): numbers order numerically (an exact
/// int/float tie orders the int first), strings lexicographically, and
/// cross-type pairs order by type rank (null < bool < number < string <
/// list < dict) instead of being unordered. The Python-flavored coercing
/// comparisons live on the heterogeneous impls above (`dyn > 4`,
/// `dyn > 4.5`, `dyn > "a"`), which is what gradual user code writes.
///
/// WHY canonical and not Python-flavored: `PartialEq` is canonical
/// (bitwise floats, `Dyn(1) != Dyn(1.0)`), and the trait contract requires
/// `partial_cmp == Some(Equal)` exactly when `==` — a coercing
/// `partial_cmp` here would violate that (the pre-Tier-3 impl did).
/// Moreover std's `slice::sort` family and even `BTreeSet`/`BTreeMap`
/// bulk-build (`collect`) compare with `lt`, so any divergence from `Ord`
/// silently mis-sorts the ordered collections the `T: Ord` vocabulary
/// exists to serve. See the Eq/Ord/Hash section for the full contract.
impl PartialOrd for Dyn {
    fn partial_cmp(&self, other: &Dyn) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

// ── canonical equality, total order, hashing (Eq / Ord / Hash) ──────
//
// The Tier-3 bound vocabulary needs `Dyn` to satisfy `Eq + Ord + Hash` so
// generic code can use `BTreeSet`/`BTreeMap`/`HashSet`/`sort`. Floats
// forbid an honest derived `Eq`, so these impls share ONE canonical
// semantics and are mutually consistent
// (`a == b` ⇔ `a.cmp(&b) == Equal` ⇒ equal hashes):
//
// - `PartialEq`/`Eq`: deep structural equality; floats compare BITWISE
//   (`f64::to_bits`). The caveats, loudly:
//   * `Dyn(0.0) != Dyn(-0.0)` — positive and negative zero are distinct
//     values (and distinct map/set keys), unlike IEEE `==`.
//   * int and float NEVER compare equal Dyn-vs-Dyn: `Dyn(1) != Dyn(1.0)`
//     (the heterogeneous `dyn == 1i64` / `dyn == 1.0f64` impls above still
//     coerce — only the homogeneous form is canonical).
//   * NaN: `serde_json` cannot represent NaN/±inf (`Number::from_f64`
//     rejects them; they decay to null on construction), so NaN never
//     reaches these impls. If one ever did, bitwise equality keeps `Eq`
//     honest (NaN == NaN, reflexivity holds).
// - `Ord`: a total order over the whole JSON value space — type rank first
//   (null < bool < number < string < list < dict), then value. Numbers
//   order numerically: exact for int-vs-int (i64/u64 widened to i128),
//   `f64::total_cmp` for floats (so `-0.0 < 0.0`); a numerically-tied
//   int/float pair orders the int FIRST (`Dyn(1) < Dyn(1.0)`), which keeps
//   `cmp == Equal` exactly aligned with `Eq`. This stays a total order
//   because integer→f64 conversion is monotone. Lists compare
//   lexicographically; dicts compare as sorted-(key, value) sequences.
// - `Hash`: type-rank tag + canonical content; floats hash `to_bits`, dict
//   entries hash in sorted-key order. Equal values hash equal.
//
// CONSISTENCY, loudly: Dyn-vs-Dyn `PartialOrd` (above) is defined as
// `Some(cmp)`, so the whole homogeneous lattice — `PartialEq`, `Eq`,
// `PartialOrd`, `Ord`, `Hash` — shares the one canonical semantics and
// every trait contract holds. This matters in practice because std's
// `slice::sort` family, `BinaryHeap`, and even the `BTreeSet`/`BTreeMap`
// bulk-build (`collect`) compare with the `<` operator family
// (`PartialOrd::lt`) under a `T: Ord` bound; a Python-flavored coercing
// `partial_cmp` here would silently mis-sort exactly the ordered
// collections this vocabulary exists to serve (and already violated the
// `PartialEq`/`PartialOrd` consistency contract before Tier 3, since
// `Dyn(1) != Dyn(1.0)` while `partial_cmp` said `Some(Equal)`).
//
// The Python-flavored coercing comparisons remain on the heterogeneous
// impls (`dyn > 4`, `dyn == 4.5`, `dyn > "a"`) — the form gradual user
// code writes. The Dyn-vs-Dyn deltas from the pre-Tier-3 behavior are
// edge-only: exact int/float ties are now ordered int-first instead of
// `Equal`, and cross-type pairs are now rank-ordered instead of unordered
// (`None` → every comparison false). Numerically distinct number
// comparisons and string comparisons are unchanged.

/// Exact integer view of a serde number (i64 and u64 both widen to i128);
/// `None` means the number is a float.
fn int_val(n: &serde_json::Number) -> Option<i128> {
    n.as_i64().map(i128::from).or_else(|| n.as_u64().map(i128::from))
}

/// Bit pattern of a float-kind serde number (callers check `int_val` is
/// `None` first; finite by construction — serde_json rejects NaN/±inf).
fn float_bits(n: &serde_json::Number) -> u64 {
    n.as_f64().unwrap_or_default().to_bits()
}

fn num_eq(x: &serde_json::Number, y: &serde_json::Number) -> bool {
    match (int_val(x), int_val(y)) {
        (Some(a), Some(b)) => a == b,
        (None, None) => float_bits(x) == float_bits(y),
        _ => false, // int and float are canonically distinct
    }
}

fn num_cmp(x: &serde_json::Number, y: &serde_json::Number) -> Ordering {
    match (int_val(x), int_val(y)) {
        (Some(a), Some(b)) => a.cmp(&b),
        (None, None) => {
            x.as_f64().unwrap_or_default().total_cmp(&y.as_f64().unwrap_or_default())
        }
        // Numerically tied int/float pairs order the int first. i128→f64
        // is monotone, so refining its ties this way stays a total order.
        (Some(a), None) => {
            (a as f64).total_cmp(&y.as_f64().unwrap_or_default()).then(Ordering::Less)
        }
        (None, Some(b)) => {
            x.as_f64().unwrap_or_default().total_cmp(&(b as f64)).then(Ordering::Greater)
        }
    }
}

/// Canonical type rank for the total order: null < bool < number < string
/// < list < dict (also the `Hash` discriminant tag).
fn type_rank(v: &Value) -> u8 {
    match v {
        Value::Null => 0,
        Value::Bool(_) => 1,
        Value::Number(_) => 2,
        Value::String(_) => 3,
        Value::Array(_) => 4,
        Value::Object(_) => 5,
    }
}

/// Dict entries in sorted-key order. serde_json's default map is a
/// `BTreeMap` (already sorted — the sort is a cheap no-op), but sorting
/// explicitly keeps Ord/Hash canonical even if feature unification ever
/// switches the workspace to `preserve_order`.
fn sorted_entries(map: &serde_json::Map<String, Value>) -> Vec<(&String, &Value)> {
    let mut entries: Vec<(&String, &Value)> = map.iter().collect();
    entries.sort_by_key(|(k, _)| *k);
    entries
}

fn deep_eq(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Null, Value::Null) => true,
        (Value::Bool(x), Value::Bool(y)) => x == y,
        (Value::Number(x), Value::Number(y)) => num_eq(x, y),
        (Value::String(x), Value::String(y)) => x == y,
        (Value::Array(x), Value::Array(y)) => {
            x.len() == y.len() && x.iter().zip(y).all(|(v, w)| deep_eq(v, w))
        }
        (Value::Object(x), Value::Object(y)) => {
            x.len() == y.len() && x.iter().all(|(k, v)| y.get(k).is_some_and(|w| deep_eq(v, w)))
        }
        _ => false,
    }
}

fn deep_cmp(a: &Value, b: &Value) -> Ordering {
    match (a, b) {
        (Value::Bool(x), Value::Bool(y)) => x.cmp(y),
        (Value::Number(x), Value::Number(y)) => num_cmp(x, y),
        (Value::String(x), Value::String(y)) => x.cmp(y),
        (Value::Array(x), Value::Array(y)) => {
            for (v, w) in x.iter().zip(y) {
                let ord = deep_cmp(v, w);
                if ord != Ordering::Equal {
                    return ord;
                }
            }
            x.len().cmp(&y.len())
        }
        (Value::Object(x), Value::Object(y)) => {
            for ((ka, va), (kb, vb)) in sorted_entries(x).into_iter().zip(sorted_entries(y)) {
                let ord = ka.cmp(kb).then_with(|| deep_cmp(va, vb));
                if ord != Ordering::Equal {
                    return ord;
                }
            }
            x.len().cmp(&y.len())
        }
        // Null-vs-Null lands here too: rank 0 vs rank 0 is Equal.
        _ => type_rank(a).cmp(&type_rank(b)),
    }
}

fn hash_value<H: std::hash::Hasher>(v: &Value, state: &mut H) {
    use std::hash::Hash;
    state.write_u8(type_rank(v));
    match v {
        Value::Null => {}
        Value::Bool(b) => b.hash(state),
        Value::Number(n) => match int_val(n) {
            Some(i) => {
                state.write_u8(0);
                i.hash(state);
            }
            None => {
                state.write_u8(1);
                float_bits(n).hash(state);
            }
        },
        Value::String(s) => s.hash(state),
        Value::Array(items) => {
            state.write_usize(items.len());
            for item in items {
                hash_value(item, state);
            }
        }
        Value::Object(map) => {
            state.write_usize(map.len());
            for (k, v) in sorted_entries(map) {
                k.hash(state);
                hash_value(v, state);
            }
        }
    }
}

/// Deep structural equality, floats bitwise (see the section comment for
/// the `-0.0`, int-vs-float and NaN caveats).
impl PartialEq for Dyn {
    fn eq(&self, other: &Dyn) -> bool {
        deep_eq(&self.0, &other.0)
    }
}

/// Honest because floats compare bitwise and serde_json cannot hold NaN:
/// reflexivity holds for every representable value.
impl Eq for Dyn {}

/// Canonical total order over the JSON value space (type rank, then
/// value). Diverges from the coercing `PartialOrd` — see the section
/// comment.
impl Ord for Dyn {
    fn cmp(&self, other: &Dyn) -> Ordering {
        deep_cmp(&self.0, &other.0)
    }
}

/// Consistent with `Eq`: equal values hash equal (floats via `to_bits`,
/// dict entries in sorted-key order).
impl std::hash::Hash for Dyn {
    fn hash<H: std::hash::Hasher>(&self, state: &mut H) {
        hash_value(&self.0, state)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn index_and_chain() {
        let d = Dyn(json!({"a": {"b": [1, 2.5, "x"]}}));
        assert_eq!(d["a"]["b"][0], 1);
        assert_eq!(d["a"]["b"][1], 2.5);
        assert_eq!(d["a"]["b"][2], "x");
    }

    #[test]
    fn arithmetic_coerces() {
        let i = Dyn::from(4i64);
        let f = Dyn::from(0.5f64);
        assert_eq!(&i + 1, 5);
        assert_eq!(&i + &f, 4.5);
        assert_eq!(&i * 2, 8);
        assert_eq!(7 - &i, 3);
        assert_eq!(&i / 2, 2); // int / int stays int
        assert_eq!(Dyn::from("ab") + Dyn::from("cd"), "abcd");
    }

    #[test]
    fn python_style_errors() {
        let err = std::panic::catch_unwind(|| Dyn(json!({"a": 1}))["b"].clone())
            .err()
            .map(crate::error::panic_message)
            .unwrap_or_default();
        assert!(err.contains("KeyError"), "got: {err}");
        let err = std::panic::catch_unwind(|| Dyn::from("x") + 1)
            .err()
            .map(crate::error::panic_message)
            .unwrap_or_default();
        assert!(err.contains("TypeError"), "got: {err}");
    }

    #[test]
    fn accessors_and_len() {
        let d = Dyn(json!([1, 2, 3]));
        assert_eq!(d.len(), 3);
        let total: i64 = d.iter().map(|v| v.as_i64()).sum();
        assert_eq!(total, 6);
        assert!(Dyn(Value::Null).is_null());
        assert_eq!(Dyn::from(true).as_bool(), true);
        assert_eq!(Dyn::from(2.5).as_f64(), 2.5);
        assert_eq!(Dyn::from("s").as_str(), "s");
        assert_eq!(Dyn::from(3i64).try_as_str(), None);
    }

    #[test]
    fn to_value_serializes() {
        assert_eq!(to_value(vec![1, 2]), json!([1, 2]));
        assert_eq!(to_value("x"), json!("x"));
    }

    #[test]
    fn for_loop_owned_and_borrowed() {
        let d = Dyn(json!([1, 2, 3]));
        let mut borrowed = 0i64;
        for item in &d {
            borrowed += item.as_i64();
        }
        assert_eq!(borrowed, 6);
        let mut owned = 0i64;
        for item in d {
            owned += item.as_i64(); // item: Dyn (owned)
        }
        assert_eq!(owned, 6);
    }

    #[test]
    fn non_array_iteration_is_a_type_error() {
        let owned = std::panic::catch_unwind(|| {
            for _ in Dyn(json!({"a": 1})) {}
        })
        .err()
        .map(crate::error::panic_message)
        .unwrap_or_default();
        assert!(
            owned.contains("TypeError: 'dict' object is not iterable"),
            "got: {owned}"
        );
        let borrowed = std::panic::catch_unwind(|| {
            for _ in &Dyn(json!(42)) {}
        })
        .err()
        .map(crate::error::panic_message)
        .unwrap_or_default();
        assert!(
            borrowed.contains("TypeError: 'int' object is not iterable"),
            "got: {borrowed}"
        );
    }

    #[test]
    fn dict_keys_values_items() {
        let d = Dyn(json!({"a": 1, "b": 2}));
        let keys: Vec<&str> = d.keys().collect();
        assert_eq!(keys, vec!["a", "b"]);
        let total: i64 = d.values().map(|v| v.as_i64()).sum();
        assert_eq!(total, 3);
        let items: Vec<(String, i64)> =
            d.items().map(|(k, v)| (k.to_string(), v.as_i64())).collect();
        assert_eq!(items, vec![("a".to_string(), 1), ("b".to_string(), 2)]);
        let err = std::panic::catch_unwind(|| Dyn(json!([1])).keys().count())
            .err()
            .map(crate::error::panic_message)
            .unwrap_or_default();
        assert!(err.contains("AttributeError"), "got: {err}");
    }

    /// Run `f`, expecting a panic; return the Python-style message.
    fn panics(f: impl FnOnce() + std::panic::UnwindSafe) -> String {
        std::panic::catch_unwind(f)
            .err()
            .map(crate::error::panic_message)
            .unwrap_or_default()
    }

    #[test]
    fn from_dyn_scalar_coercions() {
        // Happy paths mirror the as_* accessors exactly.
        assert_eq!(f64::from(Dyn::from(2i64)), 2.0); // int coerces
        assert_eq!(f64::from(Dyn::from(true)), 1.0); // bool widens
        assert_eq!(i64::from(Dyn::from(2.9)), 2); // float truncates
        assert_eq!(i64::from(Dyn::from(false)), 0);
        assert!(bool::from(Dyn::from(true)));
        assert_eq!(String::from(Dyn::from("hi")), "hi");
        // Into<...> bounds are the point of these impls.
        fn sum_into_f64<T: Into<f64>>(items: Vec<T>) -> f64 {
            items.into_iter().map(Into::into).sum()
        }
        assert_eq!(sum_into_f64(vec![Dyn::from(1i64), Dyn::from(0.5)]), 1.5);
        // Panic dialect matches the accessors.
        let err = panics(|| {
            f64::from(Dyn::from("x"));
        });
        assert!(err.contains("TypeError: expected float, got 'str'"), "got: {err}");
        let err = panics(|| {
            i64::from(Dyn(json!([1])));
        });
        assert!(err.contains("TypeError: expected int, got 'list'"), "got: {err}");
        let err = panics(|| {
            bool::from(Dyn::from(1i64));
        });
        assert!(err.contains("TypeError: expected bool, got 'int'"), "got: {err}");
        let err = panics(|| {
            String::from(Dyn::from(1.5));
        });
        assert!(err.contains("TypeError: expected str, got 'float'"), "got: {err}");
    }

    #[test]
    fn default_is_null() {
        assert!(Dyn::default().is_null());
        fn make<T: Default>() -> T {
            T::default()
        }
        assert!(make::<Dyn>().is_null());
    }

    #[test]
    fn from_str_json_or_string() {
        let parse = |s: &str| s.parse::<Dyn>().unwrap();
        assert_eq!(parse("42"), Dyn(json!(42)));
        assert_eq!(parse("2.5"), Dyn(json!(2.5)));
        assert_eq!(parse("true"), Dyn(json!(true)));
        assert_eq!(parse("null"), Dyn(Value::Null));
        assert_eq!(parse("[1, 2]"), Dyn(json!([1, 2])));
        assert_eq!(parse("{\"a\": 1}"), Dyn(json!({"a": 1})));
        assert_eq!(parse("\"quoted\""), Dyn(json!("quoted")));
        // Non-JSON falls back to a plain string — parsing never fails.
        assert_eq!(parse("hello world"), Dyn(json!("hello world")));
        assert_eq!(parse("1 2"), Dyn(json!("1 2"))); // trailing junk -> string
        fn generic_parse<T: std::str::FromStr>(s: &str) -> Option<T> {
            s.parse().ok()
        }
        assert_eq!(generic_parse::<Dyn>("7"), Some(Dyn(json!(7))));
    }

    #[test]
    fn truthiness_and_not() {
        for falsy in [json!(null), json!(false), json!(0), json!(0.0), json!(""), json!([]), json!({})]
        {
            assert!(!Dyn(falsy.clone()).truthy(), "expected falsy: {falsy}");
            assert_eq!(!Dyn(falsy.clone()), Dyn(json!(true)), "not {falsy}");
        }
        for truthy in [json!(true), json!(1), json!(-0.5), json!("x"), json!([0]), json!({"a": 0})]
        {
            assert!(Dyn(truthy.clone()).truthy(), "expected truthy: {truthy}");
            assert_eq!(!&Dyn(truthy.clone()), Dyn(json!(false)), "not {truthy}");
        }
    }

    #[test]
    fn unary_neg() {
        assert_eq!(-Dyn::from(5i64), -5);
        assert_eq!(-&Dyn::from(-2.5), 2.5);
        let err = panics(|| {
            let _ = -Dyn::from("x");
        });
        assert!(
            err.contains("TypeError: bad operand type for unary -: 'str'"),
            "got: {err}"
        );
    }

    #[test]
    fn rem_and_assign_ops() {
        assert_eq!(Dyn::from(7i64) % 3, 1);
        assert_eq!(&Dyn::from(7.5) % 2, 1.5);
        assert_eq!(7 % Dyn::from(4i64), 3);
        let err = panics(|| {
            let _ = Dyn::from(1i64) % 0;
        });
        assert!(err.contains("ZeroDivisionError: integer modulo by zero"), "got: {err}");
        let err = panics(|| {
            let _ = Dyn::from("x") % 2;
        });
        assert!(err.contains("TypeError: unsupported operand type(s) for %"), "got: {err}");

        let mut d = Dyn::from(10i64);
        d += 5; // i64 rhs
        d -= Dyn::from(3i64); // Dyn rhs
        d *= &Dyn::from(2i64); // &Dyn rhs
        d /= 4;
        d %= 4;
        assert_eq!(d, 2);
        let mut f = Dyn::from(1i64);
        f += 0.5; // float coerces, same as binary +
        assert_eq!(f, 1.5);
        let err = panics(|| {
            let mut s = Dyn::from("x");
            s -= 1;
        });
        assert!(err.contains("TypeError: unsupported operand type(s) for -"), "got: {err}");
    }

    #[test]
    fn sum_and_product() {
        let d = Dyn(json!([1, 2, 3]));
        let owned: Dyn = d.clone().into_iter().sum();
        assert_eq!(owned, 6); // all ints stays int
        let borrowed: Dyn = d.iter().sum();
        assert_eq!(borrowed, 6);
        let mixed: Dyn = Dyn(json!([1, 2.5])).into_iter().sum();
        assert_eq!(mixed, 3.5); // any float coerces
        let product: Dyn = Dyn(json!([2, 3, 4])).into_iter().product();
        assert_eq!(product, 24);
        let empty_sum: Dyn = Vec::<Dyn>::new().into_iter().sum();
        assert_eq!(empty_sum, 0);
        let empty_product: Dyn = Vec::<Dyn>::new().into_iter().product();
        assert_eq!(empty_product, 1);
        // Strings do not sum (the int 0 identity does not concatenate).
        let err = panics(|| {
            let _: Dyn = Dyn(json!(["a", "b"])).into_iter().sum();
        });
        assert!(err.contains("TypeError: unsupported operand type(s) for +"), "got: {err}");
    }

    #[test]
    fn canonical_eq_is_deep_and_bitwise_for_floats() {
        assert_eq!(Dyn(json!({"a": [1, {"b": "x"}]})), Dyn(json!({"a": [1, {"b": "x"}]})));
        assert_ne!(Dyn(json!({"a": 1})), Dyn(json!({"a": 2})));
        assert_ne!(Dyn(json!({"a": 1})), Dyn(json!({"a": 1, "b": 2})));
        // Documented caveats: int != float, 0.0 != -0.0 (bitwise floats).
        assert_ne!(Dyn(json!(1)), Dyn(json!(1.0)));
        assert_ne!(Dyn(json!(0.0)), Dyn(json!(-0.0)));
        assert_eq!(Dyn(json!(-0.0)), Dyn(json!(-0.0)));
        // The heterogeneous scalar comparisons still coerce.
        assert_eq!(Dyn(json!(1)), 1.0f64);
        // NaN decays to null at construction — it never reaches Eq.
        assert!(Dyn::from(f64::NAN).is_null());
    }

    #[test]
    fn canonical_ord_total_order() {
        use std::cmp::Ordering;
        // Type rank: null < bool < number < string < list < dict. Plain
        // `.sort()` works because PartialOrd is consistent with Ord (std's
        // sorts compare with `lt`).
        let mut ranked = vec![
            Dyn(json!({"k": 1})),
            Dyn(json!([1])),
            Dyn(json!("s")),
            Dyn(json!(7)),
            Dyn(json!(false)),
            Dyn(Value::Null),
        ];
        ranked.sort();
        let ranks: Vec<&str> = ranked.iter().map(|d| d.type_name()).collect();
        assert_eq!(ranks, vec!["NoneType", "bool", "int", "str", "list", "dict"]);
        let mut nums = vec![Dyn(json!(2.5)), Dyn(json!(1)), Dyn(json!(2))];
        nums.sort();
        assert_eq!(nums, vec![Dyn(json!(1)), Dyn(json!(2)), Dyn(json!(2.5))]);
        // Iterator::max/min consult Ord::cmp (canonical).
        let max = vec![Dyn(json!(1)), Dyn(json!("s")), Dyn(json!(2))].into_iter().max();
        assert_eq!(max, Some(Dyn(json!("s")))); // strings outrank numbers
        // Numbers order numerically; tied int/float orders the int first.
        assert_eq!(Dyn(json!(1)).cmp(&Dyn(json!(2.5))), Ordering::Less);
        assert_eq!(Dyn(json!(3.5)).cmp(&Dyn(json!(2))), Ordering::Greater);
        assert_eq!(Dyn(json!(1)).cmp(&Dyn(json!(1.0))), Ordering::Less);
        assert_eq!(Dyn(json!(-0.0)).cmp(&Dyn(json!(0.0))), Ordering::Less); // total_cmp
        // Lists lexicographic, dicts as sorted entry sequences. (`<` is the
        // coercing PartialOrd and stays None for these — use cmp.)
        assert_eq!(Dyn(json!([1, 2])).cmp(&Dyn(json!([1, 3]))), Ordering::Less);
        assert_eq!(Dyn(json!([1, 2])).cmp(&Dyn(json!([1, 2, 0]))), Ordering::Less);
        assert_eq!(Dyn(json!({"a": 1, "b": 2})).cmp(&Dyn(json!({"b": 2, "a": 1}))), Ordering::Equal);
        assert_eq!(Dyn(json!({"a": 1})).cmp(&Dyn(json!({"a": 2}))), Ordering::Less);
        // cmp == Equal exactly when ==.
        assert_eq!(Dyn(json!(1.5)).cmp(&Dyn(json!(1.5))), Ordering::Equal);
        // Ord bounds work: BTreeSet dedups + sorts.
        let set: std::collections::BTreeSet<Dyn> =
            [json!(3), json!(1), json!(1), json!("a"), json!(1.0)]
                .into_iter()
                .map(Dyn)
                .collect();
        let sorted: Vec<Dyn> = set.into_iter().collect();
        assert_eq!(
            sorted,
            vec![Dyn(json!(1)), Dyn(json!(1.0)), Dyn(json!(3)), Dyn(json!("a"))]
        );
    }

    #[test]
    fn partial_ord_consistent_with_ord_and_eq() {
        use std::cmp::Ordering;
        // The trait-contract triangle holds Dyn-vs-Dyn: partial_cmp is
        // always Some(cmp), and Equal exactly when ==. Mixed int/float
        // ties and cross-type pairs are the edge cases that used to lie.
        for (a, b) in [
            (json!(1), json!(1.0)),
            (json!(1), json!("a")),
            (json!(2.5), json!(2.5)),
            (json!(null), json!(false)),
            (json!([1]), json!({"a": 1})),
        ] {
            let (a, b) = (Dyn(a), Dyn(b));
            assert_eq!(a.partial_cmp(&b), Some(a.cmp(&b)), "{a} vs {b}");
            assert_eq!(a == b, a.cmp(&b) == Ordering::Equal, "{a} vs {b}");
        }
        // Dyn-vs-Dyn comparisons with distinct numeric values are
        // unchanged from the pre-Tier-3 coercing behavior...
        assert!(Dyn(json!(4.5)) > Dyn(json!(4)));
        assert!(Dyn(json!(3.0)) < Dyn(json!(4)));
        // ...and the heterogeneous scalar comparisons still coerce
        // Python-style (ties included).
        assert!(Dyn(json!(1)) >= 1.0f64);
        assert!(Dyn(json!(1.0)) == 1i64);
        assert!(Dyn(json!(4.5)) > 4i64);
    }

    #[test]
    fn hash_consistent_with_eq() {
        use std::collections::HashSet;
        fn hash_of(d: &Dyn) -> u64 {
            use std::hash::{Hash, Hasher};
            let mut h = std::collections::hash_map::DefaultHasher::new();
            d.hash(&mut h);
            h.finish()
        }
        // Equal values hash equal (dict key order does not matter).
        assert_eq!(
            hash_of(&Dyn(json!({"a": 1, "b": [2.5, "x"]}))),
            hash_of(&Dyn(json!({"b": [2.5, "x"], "a": 1})))
        );
        // Unequal-by-canon values get distinct treatment in sets: int 1,
        // float 1.0 and bool true are three distinct keys.
        let set: HashSet<Dyn> =
            [json!(1), json!(1), json!(1.0), json!(true), json!("a"), json!("a")]
                .into_iter()
                .map(Dyn)
                .collect();
        assert_eq!(set.len(), 4);
        assert!(set.contains(&Dyn(json!(1))));
        assert!(set.contains(&Dyn(json!(1.0))));
        // Hash + Eq bounds work generically.
        fn distinct<T: std::hash::Hash + Eq>(items: Vec<T>) -> usize {
            items.into_iter().collect::<HashSet<T>>().len()
        }
        assert_eq!(distinct(vec![Dyn(json!(0.0)), Dyn(json!(-0.0))]), 2); // bitwise
    }

    #[test]
    fn bounded_generics_instantiate_at_dyn() {
        // The Tier-3 contract in miniature: typical bounded generics accept
        // T = Dyn with runtime-checked semantics.
        fn smallest<T: PartialOrd + Clone>(items: &[T]) -> T {
            let mut best = items[0].clone();
            for x in &items[1..] {
                if *x < best {
                    best = x.clone();
                }
            }
            best
        }
        let items = vec![Dyn(json!(4.5)), Dyn(json!(2)), Dyn(json!(3.25))];
        assert_eq!(smallest(&items), 2);

        fn describe<T: std::fmt::Display + Default>(x: T) -> String {
            format!("{x}/{}", T::default())
        }
        assert_eq!(describe(Dyn(json!("v"))), "v/None");
    }
}
