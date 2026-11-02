package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDeleteSavedAccountRemovesDatabaseRecordAndExactRuntimeFolder(t *testing.T) {
	root := t.TempDir()
	application, _ := newTestApp(t, &terminalProbe{runtimeRoot: root})
	now := "2026-11-02T00:00:00Z"
	result, err := application.db.Exec(`INSERT INTO mt5_accounts(login,server,status,message,updated_at) VALUES('12345','Broker-Demo','connected','ok',?)`, now)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("get account id: %v", err)
	}
	target, err := terminalAccountRuntimePath(root, 12345, "Broker-Demo")
	if err != nil {
		t.Fatalf("runtime target: %v", err)
	}
	sibling, err := terminalAccountRuntimePath(root, 54321, "Broker-Demo")
	if err != nil {
		t.Fatalf("sibling target: %v", err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	siblingFile := filepath.Join(sibling, "keep.dat")
	if err := os.WriteFile(siblingFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/accounts/"+strconv.FormatInt(id, 10)+"/delete", strings.NewReader("{}"))
	request.RemoteAddr = "127.0.0.1:54000"
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Origin", "http://127.0.0.1:8080")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", testAdminCSRFToken)
	request.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: testAdminSessionToken})
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", response.Code, response.Body.String())
	}

	var count int
	if err := application.db.QueryRow(`SELECT COUNT(*) FROM mt5_accounts WHERE id=?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("account record remains after deletion")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("account terminal directory remains or stat failed: %v", err)
	}
	if _, err := os.Stat(siblingFile); err != nil {
		t.Fatalf("another account's terminal folder was affected: %v", err)
	}
}
