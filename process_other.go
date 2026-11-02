//go:build !windows

package main

import (
	"os/exec"
	"time"
)

func prepareChild(_ *exec.Cmd) {}

func stopChildProcess(cmd *exec.Cmd, done <-chan error, _ time.Duration) error {
	select {
	case err := <-done:
		return err
	default:
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	return <-done
}

func killTerminalsIn(_ string) {}
