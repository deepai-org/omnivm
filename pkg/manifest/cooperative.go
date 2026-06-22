package manifest

import (
	"sync/atomic"
	"time"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/dispatcher"
	"github.com/omnivm/omnivm/pkg/polyglot"
)

// Cooperative (host-driven) manifest execution.
//
// In c-shared/libomnivm mode the Golden Thread is owned by the embedding host
// (a Python asyncio loop or gevent hub). A normal manifest run is one opaque,
// blocking call that freezes that host scheduler for its whole duration. The
// cooperative path instead runs the manifest on an orchestration goroutine and
// marshals every guest call back to the Golden Thread one at a time, so the host
// can interleave its own work between guest calls — while every guest eval still
// runs on the single Golden Thread. No new OS threads are created: the
// orchestration goroutine never touches a guest runtime itself; it only blocks
// on the dispatcher waiting for the host to service each marshaled call.

// Pump step status codes (returned by CoopJob.Step).
const (
	PumpStatusDone    = 0 // the run finished; fetch the result
	PumpStatusMore    = 1 // guest has more synchronous work; pump again
	PumpStatusWaiting = 2 // (reserved) guest is waiting on I/O; host may park
)

// submitContext is shared by every marshaling runtime wrapper in one cooperative
// run. onHost is true while a marshaled call is running on the Golden Thread, so
// a nested bridge callback (guest code that re-enters the manifest) runs its
// guest calls directly instead of re-marshaling — re-marshaling would deadlock
// because RunOnMain from the Golden Thread has no servicer.
type submitContext struct {
	disp   *dispatcher.Dispatcher
	onHost atomic.Bool
}

// hostRun runs fn on the Golden Thread. If already on the Golden Thread (a nested
// call within a marshaled op), it runs fn inline.
func (s *submitContext) hostRun(fn func()) {
	if s.onHost.Load() {
		fn()
		return
	}
	_ = s.disp.RunOnMain(func() error {
		s.onHost.Store(true)
		defer s.onHost.Store(false)
		fn()
		return nil
	})
}

// marshalRT wraps a guest runtime so its guest-touching methods are marshaled to
// the Golden Thread. Name/Initialize/SetBridgeCallback/Shutdown are pass-through
// (no live guest interaction during a run).
type marshalRT struct {
	inner pkg.Runtime
	ctx   *submitContext
}

func (m *marshalRT) Name() string                      { return m.inner.Name() }
func (m *marshalRT) Initialize() error                 { return m.inner.Initialize() }
func (m *marshalRT) SetBridgeCallback(c, f uintptr)    { m.inner.SetBridgeCallback(c, f) }
func (m *marshalRT) Shutdown() error                   { return m.inner.Shutdown() }

func (m *marshalRT) Execute(code string) pkg.Result {
	var r pkg.Result
	m.ctx.hostRun(func() { r = m.inner.Execute(code) })
	return r
}

func (m *marshalRT) Eval(code string) pkg.Result {
	var r pkg.Result
	m.ctx.hostRun(func() { r = m.inner.Eval(code) })
	return r
}

func (m *marshalRT) Pump() {
	m.ctx.hostRun(func() { m.inner.Pump() })
}

func (m *marshalRT) exportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	var eb pkg.ExportedBuffer
	var ok bool
	var err error
	m.ctx.hostRun(func() { eb, ok, err = m.inner.(pkg.BufferExporter).ExportBuffer(name, expr) })
	return eb, ok, err
}

// marshalRTTypedBuf adds the optional TypedEvaler + BufferExporter surface
// (Python, Ruby).
type marshalRTTypedBuf struct{ *marshalRT }

func (m *marshalRTTypedBuf) EvalTyped(code string) polyglot.Value {
	var v polyglot.Value
	m.ctx.hostRun(func() {
		v = m.inner.(interface {
			EvalTyped(string) polyglot.Value
		}).EvalTyped(code)
	})
	return v
}

func (m *marshalRTTypedBuf) ExportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	return m.marshalRT.exportBuffer(name, expr)
}

// marshalRTBufUV adds the optional BufferExporter + libuv backend-timeout surface
// (JavaScript).
type marshalRTBufUV struct{ *marshalRT }

func (m *marshalRTBufUV) ExportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	return m.marshalRT.exportBuffer(name, expr)
}

func (m *marshalRTBufUV) GetUVBackendTimeout() int {
	var n int
	m.ctx.hostRun(func() {
		n = m.inner.(interface{ GetUVBackendTimeout() int }).GetUVBackendTimeout()
	})
	return n
}

// wrapMarshalRuntime selects the wrapper variant that mirrors the inner runtime's
// optional interfaces, so type assertions elsewhere keep behaving correctly.
// Only python/javascript/ruby/java are wrapped for cooperative runs; rust/go
// (which carry concrete-type assertions and FileExecutor) are left to the
// blocking path by the host driver.
func wrapMarshalRuntime(rt pkg.Runtime, ctx *submitContext) pkg.Runtime {
	base := &marshalRT{inner: rt, ctx: ctx}
	switch rt.Name() {
	case "python", "ruby":
		return &marshalRTTypedBuf{marshalRT: base}
	case "javascript":
		return &marshalRTBufUV{marshalRT: base}
	default:
		return base
	}
}

// CoopJob is a running cooperative manifest execution.
type CoopJob struct {
	disp   *dispatcher.Dispatcher
	doneCh chan struct{}
	result string
	err    error
}

// CooperativeRuntimesSupported reports whether every runtime named is safe to run
// cooperatively (marshaled). rust/go are not wrapped, so manifests using them must
// use the blocking path.
func CooperativeRuntimesSupported(names []string) bool {
	for _, n := range names {
		switch n {
		case "python", "javascript", "java", "ruby":
		default:
			return false
		}
	}
	return true
}

// ExecuteCooperative starts running manifest m on an orchestration goroutine with
// all guest calls marshaled to the Golden Thread via disp. The caller drives it by
// calling Step on the Golden Thread until it returns PumpStatusDone, then Result.
//
// The executor's runtimes are replaced with marshaling wrappers; the executor must
// be a fresh per-run instance (as the libomnivm run path creates).
func (e *Executor) ExecuteCooperative(m *Manifest, disp *dispatcher.Dispatcher) *CoopJob {
	ctx := &submitContext{disp: disp}
	wrapped := make(map[string]pkg.Runtime, len(e.runtimes))
	for name, rt := range e.runtimes {
		wrapped[name] = wrapMarshalRuntime(rt, ctx)
	}
	e.runtimes = wrapped
	e.submitCtx = ctx

	job := &CoopJob{disp: disp, doneCh: make(chan struct{})}
	go func() {
		defer close(job.doneCh)
		if err := e.Execute(m); err != nil {
			job.err = err
			return
		}
		job.result = "OK"
	}()
	return job
}

// Step services at most one marshaled guest call on the Golden Thread, blocking up
// to timeout for one to arrive. MUST be called on the Golden Thread. Returns:
//   - PumpStatusDone:    the run finished; call Result.
//   - PumpStatusMore:    a guest call was serviced; pump again promptly.
//   - PumpStatusWaiting: no guest work was ready (the orchestration goroutine is
//     between ops or waiting on something); the host should back off briefly
//     rather than spin. (A future enhancement parks on a wake fd instead.)
func (j *CoopJob) Step(timeout time.Duration) int {
	select {
	case <-j.doneCh:
		return PumpStatusDone
	default:
	}
	serviced := j.disp.PumpOnce(timeout)
	select {
	case <-j.doneCh:
		return PumpStatusDone
	default:
	}
	if serviced {
		return PumpStatusMore
	}
	return PumpStatusWaiting
}

// Result blocks until the run is done and returns its result/err.
func (j *CoopJob) Result() (string, error) {
	<-j.doneCh
	return j.result, j.err
}
