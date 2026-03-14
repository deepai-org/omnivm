//go:build linux

package dispatcher

/*
#include <sys/eventfd.h>
#include <sys/timerfd.h>
#include <sys/epoll.h>
#include <unistd.h>
#include <stdint.h>
#include <time.h>

static int omnivm_create_eventfd(void) {
	return eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC);
}

static int omnivm_create_timerfd_ms(int interval_ms) {
	int fd = timerfd_create(CLOCK_MONOTONIC, TFD_NONBLOCK | TFD_CLOEXEC);
	if (fd < 0) return -1;
	struct itimerspec its = {
		.it_interval = {interval_ms / 1000, (interval_ms % 1000) * 1000000L},
		.it_value    = {interval_ms / 1000, (interval_ms % 1000) * 1000000L}
	};
	timerfd_settime(fd, 0, &its, NULL);
	return fd;
}

static int omnivm_create_epoll(void) {
	return epoll_create1(EPOLL_CLOEXEC);
}

static void omnivm_epoll_add(int epfd, int fd) {
	struct epoll_event ev = { .events = EPOLLIN, .data.fd = fd };
	epoll_ctl(epfd, EPOLL_CTL_ADD, fd, &ev);
}

// Returns number of ready fds, populates ready_fds array with fd values
static int omnivm_epoll_wait_fds(int epfd, int* ready_fds, int max_fds, int timeout_ms) {
	struct epoll_event events[8];
	int n = max_fds < 8 ? max_fds : 8;
	int count = epoll_wait(epfd, events, n, timeout_ms);
	for (int i = 0; i < count; i++) ready_fds[i] = events[i].data.fd;
	return count;
}

// eventfd write: MUST be uint64_t (8 bytes) or kernel returns EINVAL
static void omnivm_eventfd_write(int fd) {
	uint64_t val = 1;
	(void)write(fd, &val, sizeof(val));
}

// Drain fd: read uint64_t values until EAGAIN
static void omnivm_drain_fd(int fd) {
	uint64_t val;
	while (read(fd, &val, sizeof(val)) == (ssize_t)sizeof(val)) {}
}
*/
import "C"
import (
	"context"
	"syscall"
)

// RunEpoll starts the dispatcher loop using Linux epoll with eventfd for
// task wakeup and timerfd for heartbeat pumping. uvBackendFD is libuv's
// backend fd for V8 I/O events (-1 to disable).
func (d *Dispatcher) RunEpoll(ctx context.Context, uvBackendFD int) {
	defer close(d.stopped)

	taskFD := int(C.omnivm_create_eventfd())
	shutdownFD := int(C.omnivm_create_eventfd())
	heartbeatFD := int(C.omnivm_create_timerfd_ms(10)) // 10ms heartbeat

	defer syscall.Close(taskFD)
	defer syscall.Close(shutdownFD)
	defer syscall.Close(heartbeatFD)

	epollFD := int(C.omnivm_create_epoll())
	defer syscall.Close(epollFD)

	C.omnivm_epoll_add(C.int(epollFD), C.int(taskFD))
	C.omnivm_epoll_add(C.int(epollFD), C.int(shutdownFD))
	C.omnivm_epoll_add(C.int(epollFD), C.int(heartbeatFD))
	if uvBackendFD >= 0 {
		C.omnivm_epoll_add(C.int(epollFD), C.int(uvBackendFD))
	}

	// Install wakeup function so RunOnMain/RunAsync can signal new tasks
	d.wakeupFunc = func() {
		C.omnivm_eventfd_write(C.int(taskFD))
	}

	go func() {
		<-ctx.Done()
		C.omnivm_eventfd_write(C.int(shutdownFD))
	}()

	var readyFDs [8]C.int
	for {
		n := int(C.omnivm_epoll_wait_fds(C.int(epollFD), &readyFDs[0], 8, -1))

		// Classify which fds triggered
		gotTask, gotHeartbeat, gotUV, gotShutdown := false, false, false, false
		for i := 0; i < n; i++ {
			fd := int(readyFDs[i])
			switch fd {
			case taskFD:
				gotTask = true
			case heartbeatFD:
				gotHeartbeat = true
			case shutdownFD:
				gotShutdown = true
			default:
				if fd == uvBackendFD {
					gotUV = true
				}
			}
		}

		// Drain eventfds/timerfds to re-arm
		if gotTask {
			C.omnivm_drain_fd(C.int(taskFD))
		}
		if gotHeartbeat {
			C.omnivm_drain_fd(C.int(heartbeatFD))
		}

		if gotShutdown {
			d.drain()
			return
		}

		// Task wakeup — drain ALL pending tasks (eventfd coalesces writes).
		// Pump between tasks so V8 timers/microtasks aren't starved.
		if gotTask {
			for {
				select {
				case t := <-d.taskChan:
					d.executeTask(t)
					d.pumpAll()
				default:
					goto doneTaskDrain
				}
			}
		doneTaskDrain:
		}

		// UV wakeup — only pump V8
		if gotUV {
			d.pumpNamed("javascript")
		}

		// Heartbeat — pump everything
		if gotHeartbeat {
			d.pumpAll()
		}
	}
}
