//go:build linux

package rust

import (
	"encoding/json"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func makeEventFD(t *testing.T) int {
	t.Helper()
	fd, _, errno := syscall.Syscall(syscall.SYS_EVENTFD2, 0, syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errno != 0 {
		t.Fatalf("eventfd2: %v", errno)
	}
	return int(fd)
}

func eventFDWrite(t *testing.T, fd int) {
	t.Helper()
	one := uint64(1)
	if _, _, errno := syscall.Syscall(syscall.SYS_WRITE, uintptr(fd), uintptr(unsafe.Pointer(&one)), 8); errno != 0 {
		t.Fatalf("eventfd write: %v", errno)
	}
}

func eventFDDrain(t *testing.T, fd int) uint64 {
	t.Helper()
	var buf uint64
	n, _, errno := syscall.Syscall(syscall.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(&buf)), 8)
	if errno == syscall.EAGAIN {
		return 0
	}
	if errno != 0 || n != 8 {
		t.Fatalf("eventfd read: n=%d errno=%v", n, errno)
	}
	return buf
}

// TestEventfdTwoConsumerPair is the design-required edge-triggered/two-consumer
// test: the tokio arm observes readiness without reading (the dispatcher
// drains), and the level-check protocol catches writes landing in the
// drained-but-not-yet-re-parked window.
func TestEventfdTwoConsumerPair(t *testing.T) {
	r := requireToolchain(t)
	tc := r.Toolchain()
	source := `
async fn forever_park() -> i64 {
    omnivm::tokio::time::sleep(std::time::Duration::from_secs(3600)).await;
    0
}
omnivm::export_async_fn!(OmniVMCall_forever_park, forever_park, 0);
omnivm::unit_abi_marker!();
`
	soPath, err := tc.BuildUnit(source, []string{"forever_park"})
	if err != nil {
		t.Fatalf("BuildUnit: %v", err)
	}
	unit, err := LoadUnit(soPath)
	if err != nil {
		t.Fatalf("LoadUnit: %v", err)
	}
	raw, _ := unit.Call("OmniVMCall_forever_park", "[]")
	env, err := decodeEnvelope(raw)
	if err != nil || env.Boundary != "rust_future" {
		t.Fatalf("future envelope: %+v (%v)", env, err)
	}
	var handle uint64
	if _, scanErr := json.Number(env.HandleID).Int64(); scanErr == nil {
		n, _ := json.Number(env.HandleID).Int64()
		handle = uint64(n)
	}
	defer r.Support().ReleaseFuture(handle)

	fd := makeEventFD(t)
	defer syscall.Close(fd)

	drive := func(sliceMs uint64) (string, time.Duration) {
		start := time.Now()
		out := r.Support().Drive(handle, sliceMs, fd)
		var resp DriveResponse
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatalf("drive response: %v (%s)", err, out)
		}
		if resp.Done {
			t.Fatalf("future completed unexpectedly: %s", out)
		}
		return resp.Reason, time.Since(start)
	}

	// 1. Quiet park: exits on heartbeat after the full slice.
	reason, took := drive(60)
	if reason != "heartbeat" || took < 40*time.Millisecond {
		t.Fatalf("quiet park: reason=%s took=%v, want heartbeat after ~60ms", reason, took)
	}

	// 2. Flood the fd, then park: must exit on taskfd immediately, without
	// consuming the counter (the dispatcher drains).
	eventFDWrite(t, fd)
	eventFDWrite(t, fd)
	eventFDWrite(t, fd)
	reason, took = drive(5000)
	if reason != "taskfd" || took > time.Second {
		t.Fatalf("flooded park: reason=%s took=%v, want immediate taskfd", reason, took)
	}
	if got := eventFDDrain(t, fd); got != 3 {
		t.Fatalf("tokio arm consumed the eventfd: counter=%d, want 3", got)
	}

	// 3. Re-park after the drain, then write mid-park: a fresh edge must
	// wake the park.
	wrote := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		eventFDWrite(t, fd)
		close(wrote)
	}()
	reason, took = drive(5000)
	<-wrote
	if reason != "taskfd" || took > time.Second {
		t.Fatalf("mid-park write: reason=%s took=%v, want taskfd wakeup", reason, took)
	}

	// 4. The race window: drain, then write while NOT parked, then re-park —
	// the wakeup must still arrive (level check beats the stale edge cache).
	eventFDDrain(t, fd)
	eventFDWrite(t, fd)
	reason, took = drive(5000)
	if reason != "taskfd" || took > time.Second {
		t.Fatalf("drained-window write: reason=%s took=%v, want immediate taskfd", reason, took)
	}
	eventFDDrain(t, fd)
}
