package rust

import (
	"io"
	"os"
	"syscall"
)

// captureFD1 redirects file descriptor 1 around fn and returns what was
// written. Rust's println! writes straight to fd 1 (it does not share Go's
// os.Stdout), so capture must happen at the descriptor level.
func captureFD1(fn func()) string {
	pr, pw, err := os.Pipe()
	if err != nil {
		fn()
		return ""
	}

	saved, err := syscall.Dup(1)
	if err != nil {
		pr.Close()
		pw.Close()
		fn()
		return ""
	}
	os.Stdout.Sync()
	if err := dup2(int(pw.Fd()), 1); err != nil {
		syscall.Close(saved)
		pr.Close()
		pw.Close()
		fn()
		return ""
	}

	done := make(chan string, 1)
	go func() {
		buf, _ := io.ReadAll(pr)
		done <- string(buf)
	}()

	fn()

	// Restore fd 1 before closing the writer so the reader sees EOF.
	dup2(saved, 1)
	syscall.Close(saved)
	pw.Close()
	out := <-done
	pr.Close()
	return out
}
