package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAccountTerminalDirectoryOnlyRemovesTarget(t *testing.T) {
	root := t.TempDir()
	target, err := terminalAccountRuntimePath(root, 10001, "Broker-Demo")
	if err != nil {
		t.Fatalf("terminalAccountRuntimePath() error = %v", err)
	}
	sibling, err := terminalAccountRuntimePath(root, 10002, "Broker-Demo")
	if err != nil {
		t.Fatalf("terminalAccountRuntimePath() sibling error = %v", err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "session.dat"), []byte("account"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(sibling, "keep.dat")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	probe := &terminalProbe{runtimeRoot: root}
	if err := removeAccountTerminalDirectory(probe, 10001, "Broker-Demo"); err != nil {
		t.Fatalf("removeAccountTerminalDirectory() error = %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target directory still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sibling account was changed: %v", err)
	}
	if err := removeAccountTerminalDirectory(probe, 10001, "Broker-Demo"); err != nil {
		t.Fatalf("deleting an already removed terminal folder should be safe: %v", err)
	}
}

func TestTerminalAccountRuntimePathRejectsInvalidIdentity(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		login  int64
		server string
	}{
		{0, "Broker"}, {-2, "Broker"}, {10001, ""},
	} {
		if _, err := terminalAccountRuntimePath(root, test.login, test.server); err == nil {
			t.Errorf("expected invalid identity to be rejected: login=%d server=%q", test.login, test.server)
		}
	}
}
