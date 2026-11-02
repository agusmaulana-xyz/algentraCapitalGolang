//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

func prepareChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func stopChildProcess(cmd *exec.Cmd, done <-chan error, timeout time.Duration) error {
	select {
	case err := <-done:
		return err
	default:
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid)); err != nil {
		_ = cmd.Process.Kill()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = cmd.Process.Kill()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			return os.ErrDeadlineExceeded
		}
	}
}

func killTerminalsIn(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	script := `$root = [System.IO.Path]::GetFullPath($env:MT5_TERMINAL_DIR).TrimEnd('\') + '\'
Get-Process -Name 'terminal64' -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith($root, [System.StringComparison]::OrdinalIgnoreCase) } | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }`
	units := utf16.Encode([]rune(script))
	raw := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		raw = append(raw, byte(unit), byte(unit>>8))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	cmd.Env = append(filteredTerminalEnvironment(os.Environ()), "MT5_TERMINAL_DIR="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
	time.Sleep(time.Second)
}
