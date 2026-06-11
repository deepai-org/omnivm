package manifest

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// TestRustStreamBatchedPull: a 200-value rust-produced stream pulled with
// stream_next max_n moves up to 64 values per OBJECT call (one envelope
// crossing) instead of one per value — the rust `next` method honors
// {"n": K} and answers in the plural {"done","values"} shape, with "done"
// riding WITH the final values.
func TestRustStreamBatchedPull(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{
			OpType: "func_def", Name: "big_stream", BodyRuntime: "rust",
			Exports: []string{"big_stream"},
			Source: `
fn big_stream() -> omnivm::objects::ObjectHandleRef {
    omnivm::objects::export_stream(0..200i64)
}
`,
		},
		{OpType: "eval", Runtime: "rust", Code: "big_stream()", Bind: "s"},
	}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	binding, _ := e.getBinding("s")
	descriptor, ok := binding.(map[string]interface{})
	if !ok || descriptor["__omnivm_stream__"] != true {
		t.Fatalf("s = %#v, want stream descriptor", binding)
	}
	id, isInt := descriptor["id"].(int64)
	if !isInt {
		t.Fatalf("descriptor id type %T", descriptor["id"])
	}

	before := rustStreamObjectCalls.Load()
	var got []interface{}
	for pulls := 0; pulls < 50; pulls++ {
		raw, err := e.HandleCall(fmt.Sprintf(`{"op":"stream_next","id":%d,"max_n":64}`, id))
		if err != nil {
			t.Fatalf("stream_next batch: %v", err)
		}
		var out struct {
			Value struct {
				Done   bool          `json:"done"`
				Values []interface{} `json:"values"`
			} `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode batch %q: %v", raw, err)
		}
		got = append(got, out.Value.Values...)
		if out.Value.Done {
			break
		}
	}
	if len(got) != 200 || !numEquals(got[0], 0) || !numEquals(got[199], 199) {
		t.Fatalf("batched stream: %d values (first/last %v/%v), want 200 (0..199)",
			len(got), got[0], got[len(got)-1])
	}
	calls := rustStreamObjectCalls.Load() - before
	if calls < 1 || calls >= 20 {
		t.Fatalf("object calls for 200 values = %d, want batched (<20; unbatched would be 200)", calls)
	}
	t.Logf("object calls for 200 values: %d (unbatched would be 200)", calls)
}

// TestRustAsyncStreamBatchedConsumption: TestRustAsyncStreamExport's shape at
// scale — a 200-value rust async stream consumed by a rust peer through
// Channel::recv_async (which pulls with max_n) costs a handful of object
// calls, not 200, with strict ordering preserved end to end.
func TestRustAsyncStreamBatchedConsumption(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{
			OpType: "func_def", Name: "make_counter", BodyRuntime: "rust",
			Exports: []string{"make_counter"},
			Source: `
fn make_counter() -> omnivm::objects::ObjectHandleRef {
    struct Counter(i64);
    impl futures_core::Stream for Counter {
        type Item = i64;
        fn poll_next(
            mut self: std::pin::Pin<&mut Self>,
            _cx: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Option<i64>> {
            if self.0 >= 200 {
                return std::task::Poll::Ready(None);
            }
            self.0 += 1;
            std::task::Poll::Ready(Some(self.0))
        }
    }
    omnivm::objects::export_async_stream(Counter(0))
}
`,
		},
		{
			OpType: "func_def", Name: "drain_counter", BodyRuntime: "rust", Async: true,
			Params:  []*Param{{Name: "input"}},
			Exports: []string{"drain_counter"},
			Source: `
async fn drain_counter(input: omnivm::Channel) -> i64 {
    let mut sum = 0i64;
    let mut expect = 0i64;
    while let Some(value) = input.recv_async().await.unwrap() {
        expect += 1;
        let n = value.as_i64().unwrap_or(-1);
        if n != expect {
            return -expect; // order/loss sentinel: which receive went wrong
        }
        sum += n;
    }
    sum
}
`,
		},
		{OpType: "eval", Runtime: "rust", Code: "make_counter()", Bind: "ticks"},
	}}); err != nil {
		t.Fatalf("compile: %v", err)
	}

	before := rustStreamObjectCalls.Load()
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "eval", Runtime: "rust", Code: "drain_counter(ticks)", Bind: "sum"},
	}}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	sum, _ := e.getBinding("sum")
	if !numEquals(sum, 20100) {
		t.Fatalf("drain_counter = %v, want 20100 (values lost or reordered)", sum)
	}
	calls := rustStreamObjectCalls.Load() - before
	if calls < 1 || calls >= 20 {
		t.Fatalf("object calls for 200 async-stream values = %d, want batched (<20)", calls)
	}
	t.Logf("object calls for 200 async-stream values: %d (unbatched would be 200)", calls)
}
