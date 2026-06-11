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

use std::cmp::Ordering;
use std::fmt;
use std::ops::Index;

use serde_json::Value;

/// A dynamically typed value crossing the runtime boundary.
#[repr(transparent)]
#[derive(Clone, Debug, Default, PartialEq)]
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

    /// Iterate the elements of a list. Panics on non-lists.
    pub fn iter(&self) -> impl Iterator<Item = &Dyn> + '_ {
        match &self.0 {
            Value::Array(items) => items.iter().map(Dyn::from_ref),
            _ => panic!("TypeError: '{}' object is not iterable", self.type_name()),
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
/// the result float; integer division by zero panics like Python.
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

/// Dyn-vs-Dyn ordering: numbers coerce, strings compare lexicographically,
/// anything else is unordered (`None` — comparisons evaluate false).
impl PartialOrd for Dyn {
    fn partial_cmp(&self, other: &Dyn) -> Option<Ordering> {
        if let (Some(a), Some(b)) = (self.num(), other.num()) {
            if let (Num::I(x), Num::I(y)) = (a, b) {
                return x.partial_cmp(&y);
            }
            return a.as_f64().partial_cmp(&b.as_f64());
        }
        if let (Value::String(a), Value::String(b)) = (&self.0, &other.0) {
            return a.partial_cmp(b);
        }
        None
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
}
