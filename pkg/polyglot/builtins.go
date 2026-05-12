package polyglot

import (
	"math"
	"strings"
)

// RegisterBuiltins registers Go-native typed functions in the global registry.
// These functions run in pure Go — no interpreter overhead, no GIL, no eval.
// They are registered under the "go" runtime and can be called from any runtime
// via callTyped("go", "funcName", args...).
func RegisterBuiltins() {
	r := GlobalRegistry

	// Math functions
	r.Register("go", "math.abs", func(args []Value) Value {
		if len(args) < 1 {
			return Error("math.abs requires 1 argument")
		}
		switch args[0].Tag {
		case TagI64:
			v := args[0].Int
			if v < 0 {
				v = -v
			}
			return I64(v)
		case TagF64:
			return F64(math.Abs(args[0].Float))
		default:
			return Error("math.abs: expected number")
		}
	})

	r.Register("go", "math.sqrt", func(args []Value) Value {
		if len(args) < 1 {
			return Error("math.sqrt requires 1 argument")
		}
		return F64(math.Sqrt(toFloat(args[0])))
	})

	r.Register("go", "math.pow", func(args []Value) Value {
		if len(args) < 2 {
			return Error("math.pow requires 2 arguments")
		}
		return F64(math.Pow(toFloat(args[0]), toFloat(args[1])))
	})

	r.Register("go", "math.floor", func(args []Value) Value {
		if len(args) < 1 {
			return Error("math.floor requires 1 argument")
		}
		return I64(int64(math.Floor(toFloat(args[0]))))
	})

	r.Register("go", "math.ceil", func(args []Value) Value {
		if len(args) < 1 {
			return Error("math.ceil requires 1 argument")
		}
		return I64(int64(math.Ceil(toFloat(args[0]))))
	})

	r.Register("go", "math.min", func(args []Value) Value {
		if len(args) < 2 {
			return Error("math.min requires at least 2 arguments")
		}
		min := toFloat(args[0])
		for _, a := range args[1:] {
			v := toFloat(a)
			if v < min {
				min = v
			}
		}
		// Return I64 if all args are integers
		if allInts(args) {
			return I64(int64(min))
		}
		return F64(min)
	})

	r.Register("go", "math.max", func(args []Value) Value {
		if len(args) < 2 {
			return Error("math.max requires at least 2 arguments")
		}
		max := toFloat(args[0])
		for _, a := range args[1:] {
			v := toFloat(a)
			if v > max {
				max = v
			}
		}
		if allInts(args) {
			return I64(int64(max))
		}
		return F64(max)
	})

	// String functions
	r.Register("go", "str.len", func(args []Value) Value {
		if len(args) < 1 {
			return Error("str.len requires 1 argument")
		}
		return I64(int64(len(args[0].Str)))
	})

	r.Register("go", "str.upper", func(args []Value) Value {
		if len(args) < 1 {
			return Error("str.upper requires 1 argument")
		}
		return String(strings.ToUpper(args[0].Str))
	})

	r.Register("go", "str.lower", func(args []Value) Value {
		if len(args) < 1 {
			return Error("str.lower requires 1 argument")
		}
		return String(strings.ToLower(args[0].Str))
	})

	r.Register("go", "str.contains", func(args []Value) Value {
		if len(args) < 2 {
			return Error("str.contains requires 2 arguments")
		}
		return Bool(strings.Contains(args[0].Str, args[1].Str))
	})

	r.Register("go", "str.replace", func(args []Value) Value {
		if len(args) < 3 {
			return Error("str.replace requires 3 arguments (str, old, new)")
		}
		return String(strings.ReplaceAll(args[0].Str, args[1].Str, args[2].Str))
	})

	// Type conversion
	r.Register("go", "int", func(args []Value) Value {
		if len(args) < 1 {
			return Error("int requires 1 argument")
		}
		switch args[0].Tag {
		case TagI64:
			return args[0]
		case TagF64:
			return I64(int64(args[0].Float))
		case TagBool:
			return I64(args[0].Int)
		default:
			return Error("int: cannot convert to int")
		}
	})

	r.Register("go", "float", func(args []Value) Value {
		if len(args) < 1 {
			return Error("float requires 1 argument")
		}
		return F64(toFloat(args[0]))
	})

	// Identity / echo (useful for testing)
	r.Register("go", "echo", func(args []Value) Value {
		if len(args) < 1 {
			return Null()
		}
		return args[0]
	})
}

func toFloat(v Value) float64 {
	switch v.Tag {
	case TagI64:
		return float64(v.Int)
	case TagF64:
		return v.Float
	case TagBool:
		return float64(v.Int)
	default:
		return 0
	}
}

func allInts(args []Value) bool {
	for _, a := range args {
		if a.Tag != TagI64 && a.Tag != TagBool {
			return false
		}
	}
	return true
}
