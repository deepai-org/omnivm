// Package golang provides a Go runtime that compiles and executes Go files via "go run".
package golang

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"syscall"

	"github.com/omnivm/omnivm/pkg"
)

var exitStatusRe = regexp.MustCompile(`exit status (\d+)`)

type Runtime struct{}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Name() string { return "go" }

func (r *Runtime) ExecuteFile(path string, args []string, stdin io.Reader) pkg.Result {
	cmdArgs := append([]string{"run", path}, args...)
	cmd := exec.Command("go", cmdArgs...)
	cmd.Stdin = stdin

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		exitCode := 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()
			}
		}
		errMsg := stderr.String()
		// go run wraps non-zero exits as "exit status N" on stderr
		// and always returns exit code 1. Parse the real code.
		if m := exitStatusRe.FindStringSubmatch(errMsg); m != nil {
			if code, e := strconv.Atoi(m[1]); e == nil {
				exitCode = code
			}
		}
		if errMsg == "" {
			errMsg = err.Error()
		}
		return pkg.Result{
			Output:   stdout.String(),
			Err:      fmt.Errorf("%s", errMsg),
			ExitCode: exitCode,
		}
	}

	return pkg.Result{Output: stdout.String()}
}
