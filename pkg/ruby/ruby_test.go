package ruby

import (
	"runtime"
	"testing"
)

func init() {
	runtime.LockOSThread()
}

func TestRubyInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if !r.initialized {
		t.Fatal("expected initialized=true")
	}
}

func TestRubyDoubleInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if err := r.Initialize(); err == nil {
		t.Fatal("expected error on double initialize")
	}
}

func TestRubyExecuteSimple(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("puts 'hello from ruby'")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "hello from ruby\n" {
		t.Fatalf("expected 'hello from ruby\\n', got %q", result.Output)
	}
}

func TestRubyExecuteExpression(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("puts 2 + 2")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "4\n" {
		t.Fatalf("expected '4\\n', got %q", result.Output)
	}
}

func TestRubyExecuteError(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("raise 'test error'")
	if result.Err == nil {
		t.Fatal("expected error from raise")
	}
}

func TestRubyRuntimeErrorStructuredEnvelope(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
payload = {
  runtime: "javascript",
  origin_runtime: "python",
  type: "ValueError",
  message: "bad value",
  traceback: "at parse (<anonymous>:1:2)",
  stack_frames: ["at parse (<anonymous>:1:2)"],
  cause_chain: [{
    type: "TypeError",
    message: "inner",
    traceback: "TypeError: inner\n    at cause (<anonymous>:2:4)",
    stack_frames: ["at cause (<anonymous>:2:4)"],
    details: {code: "E_INNER", path: ["user", "age"]}
  }],
  boundary_path: "call[javascript] > callback[python]",
  original_error_handle: "py-error-7",
  details: [{path: ["user", "age"], code: "too_small"}]
}
err = OmniVM::RuntimeError.new("ERR:" + JSON.generate(payload), runtime: "javascript", boundary_path: "call[javascript]")
raise "runtime #{err.runtime.inspect}" unless err.runtime == "javascript"
raise "origin #{err.origin_runtime.inspect}" unless err.origin_runtime == "python"
raise "type #{err.type.inspect}" unless err.type == "ValueError"
raise "message #{err.message.inspect}" unless err.message == "bad value"
raise "stack #{err.stack_frames.inspect}" unless err.stack_frames == ["at parse (<anonymous>:1:2)"]
expected_cause = [{
  type: "TypeError",
  message: "inner",
  traceback: "TypeError: inner\n    at cause (<anonymous>:2:4)",
  stack_frames: ["at cause (<anonymous>:2:4)"],
  details: {"code" => "E_INNER", "path" => ["user", "age"]}
}]
raise "cause #{err.cause_chain.inspect}" unless err.cause_chain == expected_cause
raise "boundary #{err.boundary_path.inspect}" unless err.boundary_path == "call[javascript] > callback[python]"
raise "handle #{err.original_error_handle.inspect}" unless err.original_error_handle == "py-error-7"
raise "details #{err.details.inspect}" unless err.details == [{"path" => ["user", "age"], "code" => "too_small"}]
copy = err.to_h
copy[:stack_frames][0] = "changed"
copy[:cause_chain][0][:message] = "changed"
copy[:cause_chain][0][:stack_frames][0] = "changed"
copy[:cause_chain][0][:details]["code"] = "changed"
copy[:details][0]["code"] = "changed"
raise "stack leaked" unless err.stack_frames == ["at parse (<anonymous>:1:2)"]
raise "cause leaked" unless err.cause_chain == expected_cause
raise "details leaked" unless err.details == [{"path" => ["user", "age"], "code" => "too_small"}]
json_hash = JSON.parse(err.to_json)
raise "json origin #{json_hash.inspect}" unless json_hash["origin_runtime"] == "python"
raise "json details #{json_hash.inspect}" unless json_hash["details"] == [{"path" => ["user", "age"], "code" => "too_small"}]
as_json = err.as_json
as_json[:details][0]["code"] = "changed"
raise "as_json leaked" unless err.details == [{"path" => ["user", "age"], "code" => "too_small"}]
wrapped_payload = payload.reject { |key, _| key == :boundary_path }
wrapped = OmniVM::RuntimeError.new("ERR:execute manifest: call [javascript]: " + JSON.generate(wrapped_payload), runtime: "ruby")
raise "wrapped origin #{wrapped.origin_runtime.inspect}" unless wrapped.origin_runtime == "python"
raise "wrapped details #{wrapped.details.inspect}" unless wrapped.details == [{"path" => ["user", "age"], "code" => "too_small"}]
raise "wrapped boundary #{wrapped.boundary_path.inspect}" unless wrapped.boundary_path == "execute manifest > call[javascript]"
direct = OmniVM::RuntimeError.new("ERR:javascript: " + JSON.generate(wrapped_payload), runtime: "ruby")
raise "direct origin #{direct.origin_runtime.inspect}" unless direct.origin_runtime == "python"
raise "direct boundary #{direct.boundary_path.inspect}" unless direct.boundary_path == "call[javascript]"
puts "ok"
`)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ok\n" {
		t.Fatalf("expected ok output, got %q", result.Output)
	}
}

func TestRubyExecuteMultiline(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	code := `
x = 10
y = 20
puts x + y
`
	result := r.Execute(code)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "30\n" {
		t.Fatalf("expected '30\\n', got %q", result.Output)
	}
}

func TestRubyNotInitialized(t *testing.T) {
	r := New()
	result := r.Execute("puts 'hi'")
	if result.Err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func TestRubyImportStdlib(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("require 'json'; puts JSON.generate({key: 'value'})")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	expected := "{\"key\":\"value\"}\n"
	if result.Output != expected {
		t.Fatalf("expected %q, got %q", expected, result.Output)
	}
}

func TestRubyEncodingDatabaseLoaded(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("puts Encoding.find('binary').name")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ASCII-8BIT\n" {
		t.Fatalf("expected ASCII-8BIT, got %q", result.Output)
	}
}

func TestRubyPump(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// Pump should not crash
	r.Pump()
}
