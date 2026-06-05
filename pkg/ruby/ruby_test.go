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

func TestRubySizedQueueNonBlockingPush(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
q = Thread::SizedQueue.new(1)
q.push(:first)
begin
  q.push(:second, true)
  raise "missing nonblocking full diagnostic"
rescue ThreadError => e
  raise "bad nonblocking diagnostic: #{e.message}" unless e.message == "queue full"
end
begin
  q.push(:second)
  raise "missing blocking full diagnostic"
rescue ThreadError => e
  raise "bad blocking diagnostic: #{e.message}" unless e.message.include?("OmniVM embedded Ruby")
end
puts "ok"
`)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ok\n" {
		t.Fatalf("expected ok output, got %q", result.Output)
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
  name: "ValueError",
  message: "bad value",
  stack: "at parse (<anonymous>:1:2)",
  stack_frames: ["at parse (<anonymous>:1:2)"],
  cause_chain: [{
    name: "TypeError",
    message: "inner",
    stack: "TypeError: inner\n    at cause (<anonymous>:2:4)",
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
  runtime: "javascript",
  origin_runtime: "javascript",
  details: {"code" => "E_INNER", "path" => ["user", "age"]}
}]
raise "cause #{err.cause_chain.inspect}" unless err.cause_chain == expected_cause
raise "boundary #{err.boundary_path.inspect}" unless err.boundary_path == "call[javascript] > callback[python]"
raise "handle #{err.original_error_handle.inspect}" unless err.original_error_handle == "py-error-7"
raise "details #{err.details.inspect}" unless err.details == [{"path" => ["user", "age"], "code" => "too_small"}]
raise "camel origin #{err.originRuntime.inspect}" unless err.originRuntime == err.origin_runtime
raise "camel stack #{err.stackFrames.inspect}" unless err.stackFrames == err.stack_frames
raise "camel cause #{err.causeChain.inspect}" unless err.causeChain == err.cause_chain
raise "camel boundary #{err.boundaryPath.inspect}" unless err.boundaryPath == err.boundary_path
raise "camel handle #{err.originalErrorHandle.inspect}" unless err.originalErrorHandle == err.original_error_handle
raise "camel details json #{err.detailsJson.inspect}" unless err.detailsJson == err.details_json
stack_reader = err.stack_frames
stack_reader[0] = "changed"
camel_stack_reader = err.stackFrames
camel_stack_reader[0] = "changed"
cause_reader = err.cause_chain
cause_reader[0][:message] = "changed"
cause_reader[0][:details]["code"] = "changed"
camel_cause_reader = err.causeChain
camel_cause_reader[0][:message] = "changed"
camel_cause_reader[0][:details]["code"] = "changed"
details_reader = err.details
details_reader[0]["code"] = "changed"
raise "stack reader leaked" unless err.stack_frames == ["at parse (<anonymous>:1:2)"]
raise "cause reader leaked" unless err.cause_chain == expected_cause
raise "details reader leaked" unless err.details == [{"path" => ["user", "age"], "code" => "too_small"}]
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
override = OmniVM::RuntimeError.new("ERR:plain", runtime: "ruby", boundary_path: "custom", details: {"code" => "E_CUSTOM"})
raise "override details #{override.details.inspect}" unless override.details == {"code" => "E_CUSTOM"}
raise "override details_json #{override.details_json.inspect}" unless override.details_json == "{\"code\":\"E_CUSTOM\"}"
text_details = OmniVM::RuntimeError.new("ERR:javascript: AggregateError: invalid\n    at parse (<anonymous>:1:2)\ndetails_json: [{\"path\":[\"user\",\"age\"],\"code\":\"too_small\"}]", runtime: "javascript")
raise "text details #{text_details.details.inspect}" unless text_details.details == [{"path" => ["user", "age"], "code" => "too_small"}]
raise "text details_json #{text_details.details_json.inspect}" unless text_details.details_json == "[{\"path\":[\"user\",\"age\"],\"code\":\"too_small\"}]"
raise "text stack #{text_details.stack_frames.inspect}" unless text_details.stack_frames == ["at parse (<anonymous>:1:2)"]
raw_details = OmniVM::RuntimeError.new("ERR:javascript: AggregateError: invalid\ndetailsJson: not json", runtime: "javascript")
raise "raw details #{raw_details.details.inspect}" unless raw_details.details == "not json"
raise "raw details_json #{raw_details.details_json.inspect}" unless raw_details.details_json == "not json"
raise "raw stack #{raw_details.stack_frames.inspect}" unless raw_details.stack_frames == []
alias_payload = {
  runtime: "java",
  error_type: "IllegalArgumentException",
  message: "outer",
  cause_chain: [{
    runtime: "javascript",
    errorType: "TypeError",
    message: "inner"
  }]
}
alias_err = OmniVM::RuntimeError.new(JSON.generate(alias_payload), runtime: "go")
raise "alias type #{alias_err.type.inspect}" unless alias_err.type == "IllegalArgumentException"
raise "alias cause type #{alias_err.cause_chain.inspect}" unless alias_err.cause_chain[0][:type] == "TypeError"
puts "ok"
`)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ok\n" {
		t.Fatalf("expected ok output, got %q", result.Output)
	}
}

func TestRubyNativeThreadingGuardReportsStructuredDiagnostic(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
status = OmniVM.ruby_threading_status
raise "mode #{status.inspect}" unless status["mode"] == "single_vm_thread"
raise "native thread status #{status.inspect}" unless status["native_threads_supported"] == false
raise "Puma boundary #{status.inspect}" unless status["app_server_boundary"].include?("Puma")
status["mode"] = "mutated"
raise "status leaked mutation" unless OmniVM.ruby_threading_status["mode"] == "single_vm_thread"

begin
  OmniVM.assert_ruby_native_threads_supported("puma startup")
  raise "missing diagnostic"
rescue OmniVM::RuntimeError => e
  raise "message #{e.message.inspect}" unless e.message.include?("puma startup: native Ruby threads unsupported")
  raise "boundary #{e.boundary_path.inspect}" unless e.boundary_path == "ruby_threading"
  details = e.details
  raise "details #{details.inspect}" unless details["ruby_threading"]["native_threads_supported"] == false
  details["ruby_threading"]["mode"] = "mutated"
  raise "details leaked mutation" unless e.details["ruby_threading"]["mode"] == "single_vm_thread"
end
puts "ok"
`)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ok\n" {
		t.Fatalf("expected ok output, got %q", result.Output)
	}
}

func TestRubyOwnerDispatchGuardsReportStructuredDiagnostic(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
status = OmniVM.owner_dispatch_status
raise "mode #{status.inspect}" unless status["mode"] == "diagnostic_only"
raise "owner dispatch #{status.inspect}" unless status["owner_dispatch_supported"] == false
raise "missing JS target #{status.inspect}" unless status["owner_dispatch_targets"].key?("javascript_event_loop")
status["owner_dispatch_targets"]["javascript_event_loop"]["supported"] = true
raise "status leaked mutation" unless OmniVM.owner_dispatch_status["owner_dispatch_targets"]["javascript_event_loop"]["supported"] == false

target = OmniVM.owner_dispatch_target_status("js")
raise "target alias #{target.inspect}" unless target["target"] == "javascript_event_loop"
raise "requested target #{target.inspect}" unless target["requested_target"] == "js"
target["supported"] = true
raise "target leaked mutation" unless OmniVM.owner_dispatch_target_status("js")["supported"] == false

begin
  OmniVM.assert_owner_dispatch_supported("rack startup")
  raise "missing universal diagnostic"
rescue OmniVM::RuntimeError => e
  raise "message #{e.message.inspect}" unless e.message.include?("rack startup: owner dispatch unsupported")
  raise "boundary #{e.boundary_path.inspect}" unless e.boundary_path == "owner_dispatch"
  details = e.details
  raise "details #{details.inspect}" unless details["owner_dispatch"]["owner_dispatch_supported"] == false
  details["owner_dispatch"]["mode"] = "mutated"
  raise "details leaked mutation" unless e.details["owner_dispatch"]["mode"] == "diagnostic_only"
  e.details_json = '{"owner_dispatch":{"owner_dispatch_supported":true,"mode":"json-set"}}'
  raise "details_json setter details #{e.details.inspect}" unless e.details["owner_dispatch"]["mode"] == "json-set"
  e.detailsJson = {"owner_dispatch" => {"owner_dispatch_supported" => false, "mode" => "alias-object-set"}}
  raise "detailsJson setter json #{e.details_json.inspect}" unless JSON.parse(e.details_json)["owner_dispatch"]["mode"] == "alias-object-set"
  e.details = {"owner_dispatch" => {"owner_dispatch_supported" => false, "mode" => "details-set"}}
  raise "details setter json #{e.details_json.inspect}" unless JSON.parse(e.details_json)["owner_dispatch"]["mode"] == "details-set"
  e.originRuntime = "owner-ruby"
  e.boundaryPath = "owner_dispatch > normalized"
  e.originalErrorHandle = "rb-owner-1"
  e.stackFrames = ["normalized stack"]
  e.causeChain = [{"runtime" => "ruby", "message" => "inner"}]
  e.traceback = "normalized traceback"
  envelope = e.to_h
  raise "originRuntime setter #{envelope.inspect}" unless envelope[:origin_runtime] == "owner-ruby"
  raise "boundaryPath setter #{envelope.inspect}" unless envelope[:boundary_path] == "owner_dispatch > normalized"
  raise "originalErrorHandle setter #{envelope.inspect}" unless envelope[:original_error_handle] == "rb-owner-1"
  raise "stackFrames setter #{envelope.inspect}" unless envelope[:stack_frames] == ["normalized stack"]
  raise "causeChain setter #{envelope.inspect}" unless envelope[:cause_chain] == [{"runtime" => "ruby", "message" => "inner"}]
  raise "traceback setter #{envelope.inspect}" unless envelope[:traceback] == "normalized traceback"
end

begin
  OmniVM.assert_owner_dispatch_target_supported("ruby", "async bridge")
  raise "missing target diagnostic"
rescue OmniVM::RuntimeError => e
  raise "target message #{e.message.inspect}" unless e.message.include?("async bridge: owner dispatch target unsupported: ruby_fiber_thread")
  raise "target boundary #{e.boundary_path.inspect}" unless e.boundary_path == "owner_dispatch_target"
  raise "top-level target details #{e.details.inspect}" unless e.details["target"] == "ruby_fiber_thread"
  raise "top-level requested target details #{e.details.inspect}" unless e.details["requested_target"] == "ruby"
  raise "target details #{e.details.inspect}" unless e.details["owner_dispatch_target"]["target"] == "ruby_fiber_thread"
end
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

func TestRubyBufferOwnerScopesAndReleases(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
module OmniVM
  @events = []
  class << self
    attr_reader :events
    def set_buffer(name, data, dtype = 0)
      @events << [:set, name, data, dtype]
    end
    def release_buffer(name)
      @events << [:release, name]
    end
    def buffer_status_json(name)
      @events << [:status, name]
      JSON.generate({"name" => name, "lease_state" => "owned"})
    end
  end
end

owner = OmniVM.buffer_owner(:payload, "abc", dtype: 7)
raise "set event mismatch #{OmniVM.events.inspect}" unless OmniVM.events == [[:set, "payload", "abc", 7]]
begin
  owner.enter
rescue OmniVM::RuntimeError => err
  raise "active re-entry error mismatch #{err.message.inspect}" unless err.message.include?("is already active")
  raise "active re-entry boundary mismatch #{err.boundary_path.inspect}" unless err.boundary_path == "native_memory"
  raise "active re-entry details mismatch #{err.details.inspect}" unless err.details == {"buffer" => {"name" => "payload", "state" => "active", "lease_state" => "active", "active_owner" => true}}
else
  raise "active re-entry did not fail"
end
raise "active re-entry published events #{OmniVM.events.inspect}" unless OmniVM.events == [[:set, "payload", "abc", 7]]
owner_status = owner.status
raise "status mismatch #{owner_status.inspect}" unless owner_status["name"] == "payload" && owner_status["lease_state"] == "owned"
raise "release did not return true" unless owner.release == true
raise "second release was not idempotent" unless owner.release == false
raise "released? mismatch" unless owner.released? == true
begin
  owner.enter
rescue OmniVM::RuntimeError => err
  raise "released re-entry error mismatch #{err.message.inspect}" unless err.message.include?("cannot be re-entered after release")
  raise "released re-entry boundary mismatch #{err.boundary_path.inspect}" unless err.boundary_path == "native_memory"
  raise "released re-entry details mismatch #{err.details.inspect}" unless err.details == {"buffer" => {"name" => "payload", "state" => "released", "lease_state" => "released", "released" => true}}
else
  raise "released re-entry did not fail"
end
raise "released re-entry changed events #{OmniVM.events.inspect}" unless OmniVM.events == [[:set, "payload", "abc", 7], [:status, "payload"], [:release, "payload"]]

events_before_block = OmniVM.events.dup
block_result = OmniVM.buffer_owner("block") do |scoped|
  raise "block owner name mismatch" unless scoped.name == "block"
  :body_result
end
raise "block result mismatch #{block_result.inspect}" unless block_result == :body_result
raise "block release mismatch #{OmniVM.events.inspect}" unless OmniVM.events == events_before_block + [[:release, "block"]]

module OmniVM
  class << self
    def release_buffer(name)
      @events << [:release_tombstone, name]
      raise RuntimeError.new(
        "release failed for #{name}",
        runtime: "ruby",
        boundary_path: "native_memory",
        details: {"buffer" => {"name" => name, "state" => "released_detached", "released" => true, "release_error" => "producer release failed"}}
      )
    end
  end
end
tombstoned = OmniVM.buffer_owner("tombstoned")
begin
  tombstoned.release
rescue OmniVM::RuntimeError => err
  raise "tombstone boundary mismatch #{err.boundary_path.inspect}" unless err.boundary_path == "native_memory"
  raise "tombstone details mismatch #{err.details.inspect}" unless err.details.dig("buffer", "released") == true
  raise "tombstone release_error missing #{err.details.inspect}" unless err.details.dig("buffer", "release_error") == "producer release failed"
else
  raise "tombstone release failure was not raised"
end
raise "tombstoned owner did not mark released" unless tombstoned.released? == true
raise "tombstoned owner second release was not idempotent" unless tombstoned.release == false
raise "tombstone release event mismatch #{OmniVM.events.inspect}" unless OmniVM.events.last == [:release_tombstone, "tombstoned"]
puts "ok"
	`)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ok\n" {
		t.Fatalf("expected ok output, got %q", result.Output)
	}
}

func TestRubyBufferOwnerPreservesBodyExceptionWhenReleaseFails(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
module OmniVM
  @events = []
  class << self
    attr_reader :events
    def release_buffer(name)
      @events << [:release, name]
      raise "release failed"
    end
  end
end

begin
  OmniVM.buffer_owner("payload") do |_owner|
    raise "body failed"
  end
rescue => err
  raise "body exception was masked: #{err.message}" unless err.message == "body failed"
  cleanup_errors = OmniVM.cleanup_errors(err)
  raise "cleanup error was not retained" unless cleanup_errors&.first&.message == "release failed"
  cleanup_errors.clear
  raise "cleanup_errors returned internal storage" unless OmniVM.cleanup_errors(err)&.first&.message == "release failed"
else
  raise "body exception was not raised"
end
raise "release not attempted #{OmniVM.events.inspect}" unless OmniVM.events == [[:release, "payload"]]
puts "ok"
`)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ok\n" {
		t.Fatalf("expected ok output, got %q", result.Output)
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
