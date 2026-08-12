//go:build windows

package sandbox

import (
	"os"
	"os/exec"
)

func prepareProcess(*exec.Cmd) {}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return process.Kill()
}
