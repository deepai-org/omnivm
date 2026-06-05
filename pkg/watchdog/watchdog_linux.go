//go:build linux

// Package watchdog implements a C pthread-based watchdog timer with
// temporal signal routing for runtime-specific preemption.
//
// The watchdog runs as a dedicated OS thread (pthread), independent of
// the Go scheduler. When armed, it sleeps for a configurable timeout
// and then dispatches an interrupt to whichever runtime is currently
// active, as indicated by the active_runtime atomic.
//
// Interrupt mechanisms per runtime:
//   - Python: pipe write (safe from any thread, no GIL needed)
//   - JavaScript: v8::Isolate::TerminateExecution() (thread-safe atomic flag)
//   - Ruby: volatile flag → trace hook raises Interrupt at next line event
//   - JVM: Thread.interrupt() on the active Java thread
package watchdog

/*
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <unistd.h>
#include <errno.h>
#include <time.h>

static pthread_t watchdog_thread;
static pthread_mutex_t wd_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t wd_cond;
static int wd_armed = 0;
static int wd_timeout_ms = 0;
static int wd_running = 0;
static int wd_thread_started = 0;
static int wd_shutting_down = 0;
static int wd_cond_initialized = 0;
static int wd_generation = 0;
static pthread_t golden_tid;

// Temporal routing: which runtime is currently executing on Golden Thread
static atomic_int active_runtime = 0;
// 0=none, 1=python, 2=javascript, 3=ruby, 4=jvm

// Runtime interrupt function pointers (set during init)
static void (*py_interrupt_fn)(void) = NULL;
static void (*v8_terminate_fn)(void) = NULL;

// Ruby interrupt function pointer (pipe write, like Python)
static void (*rb_interrupt_fn)(void) = NULL;

// JVM interrupt function pointer (JNI Thread.interrupt on active thread)
static void (*jvm_interrupt_fn)(void) = NULL;

static void* watchdog_loop(void* arg) {
	(void)arg;
	pthread_mutex_lock(&wd_mutex);
	while (wd_running) {
		while (!wd_armed && wd_running)
			pthread_cond_wait(&wd_cond, &wd_mutex);
		if (!wd_running) break;

		int gen = wd_generation;
		struct timespec deadline;
		clock_gettime(CLOCK_MONOTONIC, &deadline);
		deadline.tv_sec += wd_timeout_ms / 1000;
		deadline.tv_nsec += (wd_timeout_ms % 1000) * 1000000L;
		if (deadline.tv_nsec >= 1000000000L) {
			deadline.tv_sec++;
			deadline.tv_nsec -= 1000000000L;
		}

		int rc = 0;
		while (wd_armed && rc != ETIMEDOUT && wd_running && wd_generation == gen)
			rc = pthread_cond_timedwait(&wd_cond, &wd_mutex, &deadline);

		// If generation changed, Arm() was called with a new timeout.
		// Loop back to recompute the deadline with the new value.
		if (wd_generation != gen) continue;
		if (!wd_armed || !wd_running) continue;

		// Timeout fired — interrupt the active runtime
		wd_armed = 0; // one-shot
		int rt = atomic_load(&active_runtime);
		pthread_mutex_unlock(&wd_mutex);

		switch (rt) {
		case 1: // Python — pipe write (safe from any thread)
			if (py_interrupt_fn) py_interrupt_fn();
			break;
		case 2: // JavaScript — V8 atomic flag (thread-safe)
			if (v8_terminate_fn) v8_terminate_fn();
			break;
		case 3: // Ruby — sets volatile flag checked by trace hook
			if (rb_interrupt_fn) rb_interrupt_fn();
			break;
		case 4: // JVM — JNI Thread.interrupt() on the active Java thread
			if (jvm_interrupt_fn) jvm_interrupt_fn();
			break;
		}

		pthread_mutex_lock(&wd_mutex);
	}
	pthread_mutex_unlock(&wd_mutex);
	return NULL;
}

static void omnivm_watchdog_init(pthread_t golden) {
	pthread_mutex_lock(&wd_mutex);
	if (wd_running || wd_shutting_down) {
		golden_tid = golden;
		pthread_mutex_unlock(&wd_mutex);
		return;
	}

	if (!wd_cond_initialized) {
		// Use CLOCK_MONOTONIC for the condvar so the watchdog is immune to
		// NTP syncs and wall-clock jumps.
		pthread_condattr_t attr;
		pthread_condattr_init(&attr);
		pthread_condattr_setclock(&attr, CLOCK_MONOTONIC);
		pthread_cond_init(&wd_cond, &attr);
		pthread_condattr_destroy(&attr);
		wd_cond_initialized = 1;
	}

	golden_tid = golden;
	wd_armed = 0;
	wd_running = 1;
	wd_generation++;

	if (pthread_create(&watchdog_thread, NULL, watchdog_loop, NULL) == 0) {
		wd_thread_started = 1;
	} else {
		wd_running = 0;
	}
	pthread_mutex_unlock(&wd_mutex);
}

static void omnivm_watchdog_arm(int timeout_ms) {
	pthread_mutex_lock(&wd_mutex);
	wd_timeout_ms = timeout_ms;
	wd_armed = 1;
	wd_generation++;
	pthread_cond_signal(&wd_cond);
	pthread_mutex_unlock(&wd_mutex);
}

static void omnivm_watchdog_disarm(void) {
	pthread_mutex_lock(&wd_mutex);
	wd_armed = 0;
	pthread_cond_signal(&wd_cond);
	pthread_mutex_unlock(&wd_mutex);
}

static void omnivm_watchdog_set_active_runtime(int rt) {
	atomic_store(&active_runtime, rt);
}

static int omnivm_watchdog_get_active_runtime(void) {
	return atomic_load(&active_runtime);
}

static void omnivm_watchdog_set_py_interrupt(void (*fn)(void)) {
	py_interrupt_fn = fn;
}

static void omnivm_watchdog_set_v8_terminate(void (*fn)(void)) {
	v8_terminate_fn = fn;
}

static void omnivm_watchdog_set_rb_interrupt(void (*fn)(void)) {
	rb_interrupt_fn = fn;
}

static void omnivm_watchdog_set_jvm_interrupt(void (*fn)(void)) {
	jvm_interrupt_fn = fn;
}

static void omnivm_watchdog_shutdown(void) {
	pthread_mutex_lock(&wd_mutex);
	if (!wd_thread_started) {
		wd_running = 0;
		wd_armed = 0;
		pthread_mutex_unlock(&wd_mutex);
		return;
	}
	wd_shutting_down = 1;
	wd_running = 0;
	wd_armed = 0;
	wd_generation++;
	pthread_cond_broadcast(&wd_cond);
	pthread_mutex_unlock(&wd_mutex);

	pthread_join(watchdog_thread, NULL);

	pthread_mutex_lock(&wd_mutex);
	wd_thread_started = 0;
	wd_shutting_down = 0;
	pthread_mutex_unlock(&wd_mutex);
}
*/
import "C"
import "unsafe"

// Runtime identity constants for temporal signal routing.
const (
	RuntimeNone       = 0
	RuntimePython     = 1
	RuntimeJavaScript = 2
	RuntimeRuby       = 3
	RuntimeJVM        = 4
	RuntimeGo         = 5
)

// Init starts the watchdog pthread. goldenTID is the pthread_t of the
// Golden Thread (main OS thread) used for SIGUSR1 delivery to Ruby.
func Init() {
	C.omnivm_watchdog_init(C.pthread_self())
}

// SetPythonInterrupt sets the function pointer called to interrupt Python.
// The function should write to the interrupt pipe (no GIL needed).
func SetPythonInterrupt(fn unsafe.Pointer) {
	C.omnivm_watchdog_set_py_interrupt((*[0]byte)(fn))
}

// SetV8Terminate sets the function pointer called to terminate V8 execution.
// v8::Isolate::TerminateExecution() is thread-safe.
func SetV8Terminate(fn unsafe.Pointer) {
	C.omnivm_watchdog_set_v8_terminate((*[0]byte)(fn))
}

// SetRubyInterrupt sets the function pointer called to interrupt Ruby execution.
// The function sets a volatile flag checked by a Ruby trace hook.
func SetRubyInterrupt(fn unsafe.Pointer) {
	C.omnivm_watchdog_set_rb_interrupt((*[0]byte)(fn))
}

// SetJVMInterrupt sets the function pointer called to interrupt Java execution.
// The function should attach to the JVM if needed and interrupt the active Java thread.
func SetJVMInterrupt(fn unsafe.Pointer) {
	C.omnivm_watchdog_set_jvm_interrupt((*[0]byte)(fn))
}

// Arm starts the watchdog timer. After timeoutMS milliseconds, the
// watchdog fires an interrupt to the currently active runtime (one-shot).
func Arm(timeoutMS int) {
	C.omnivm_watchdog_arm(C.int(timeoutMS))
}

// Disarm cancels any pending watchdog timeout.
func Disarm() {
	C.omnivm_watchdog_disarm()
}

// SetActiveRuntime sets which runtime is currently executing on the
// Golden Thread. The watchdog uses this to route interrupts.
func SetActiveRuntime(rt int) {
	C.omnivm_watchdog_set_active_runtime(C.int(rt))
}

// GetActiveRuntime returns the current active runtime constant.
func GetActiveRuntime() int {
	return int(C.omnivm_watchdog_get_active_runtime())
}

// Shutdown stops the watchdog pthread and waits for it to exit.
func Shutdown() {
	C.omnivm_watchdog_shutdown()
}
