//go:build !linux

package dispatcher

import "context"

// RunEpoll falls back to the ticker-based Run() on non-Linux platforms.
func (d *Dispatcher) RunEpoll(ctx context.Context, uvBackendFD int) {
	d.Run(ctx)
}
