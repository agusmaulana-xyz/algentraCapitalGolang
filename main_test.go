package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

type probeFunc func(context.Context, credentials) (probeOutcome, error)

const testAdminSessionToken = "test-admin-session-token"
const testAdminCSRFToken = "test-admin-csrf-token"

func (f probeFunc) Check(ctx context.Context, creds credentials) (probeOutcome, error) {
	return f(ctx, creds)
}

func newTestApp(t *testing.T, probe loginProbe) (*app, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mt5-login.db")
	db, err := openDatabase(dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	portalPath := filepath.Join(t.TempDir(), "portal.db")
	portalDB, err := openPortalDatabase(portalPath)
	if err != nil {
		t.Fatalf("open portal database: %v", err)
	}
	t.Cleanup(func() { _ = portalDB.Close() })
	auth, err := newPortalAuth(portalDB, "admin", "test-admin-password-12")
	if err != nil {
		t.Fatalf("initialize portal auth: %v", err)
	}
	tokenHash := sha256.Sum256([]byte(testAdminSessionToken))
	_, err = portalDB.Exec(`INSERT INTO portal_sessions(session_hash, user_id, role, email, display_name, csrf_token, expires_at, created_at) VALUES(?, NULL, 'admin', ?, 'Test Admin', ?, ?, ?)`, hex.EncodeToString(tokenHash[:]), "admin", testAdminCSRFToken, time.Now().Add(time.Hour).Unix(), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("create test admin session: %v", err)
	}
	return &app{db: db, auth: auth, probe: probe, config: config{TerminalPath: "terminal64.exe"}}, dbPath
}

func postConnect(handler http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54000"
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Origin", "http://127.0.0.1:8080")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", testAdminCSRFToken)
	req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: testAdminSessionToken})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func float64Pointer(value float64) *float64 { return &value }

func TestPortalClientRegistrationAndAdminIsolation(t *testing.T) {
	application, _ := newTestApp(t, probeFunc(func(context.Context, credentials) (probeOutcome, error) {
		return probeOutcome{}, nil
	}))
	handler := application.routes()

	request := httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(`{"name":"Client One","email":"client@example.com","password":"client-portal-password-12"}`))
	request.RemoteAddr = "127.0.0.1:54000"
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Origin", "http://127.0.0.1:8080")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("registration status = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "client-portal-password-12") {
		t.Fatal("registration response leaked the portal password")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != portalSessionCookie || !cookies[0].HttpOnly {
		t.Fatalf("registration cookie = %#v, want an HttpOnly portal session", cookies)
	}
	var storedHash string
	if err := application.auth.db.QueryRow(`SELECT password_hash FROM portal_users WHERE email=?`, "client@example.com").Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == "client-portal-password-12" || !verifyPortalPassword(storedHash, "client-portal-password-12") {
		t.Fatal("portal password was not stored as a verifiable password hash")
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/api/client/dashboard", nil)
	dashboardRequest.RemoteAddr = "127.0.0.1:54000"
	dashboardRequest.AddCookie(cookies[0])
	dashboardResponse := httptest.NewRecorder()
	handler.ServeHTTP(dashboardResponse, dashboardRequest)
	if dashboardResponse.Code != http.StatusOK || !strings.Contains(dashboardResponse.Body.String(), `"email":"client@example.com"`) {
		t.Fatalf("client dashboard response = %d: %s", dashboardResponse.Code, dashboardResponse.Body.String())
	}

	adminRequest := httptest.NewRequest(http.MethodGet, "/admin", nil)
	adminRequest.RemoteAddr = "127.0.0.1:54000"
	adminRequest.AddCookie(cookies[0])
	adminResponse := httptest.NewRecorder()
	handler.ServeHTTP(adminResponse, adminRequest)
	if adminResponse.Code != http.StatusSeeOther || adminResponse.Header().Get("Location") != "/dashboard" {
		t.Fatalf("client access to admin dashboard = %d, location %q", adminResponse.Code, adminResponse.Header().Get("Location"))
	}

	for _, assetPath := range []string{"/assets/index.html", "/assets/"} {
		adminAssetRequest := httptest.NewRequest(http.MethodGet, assetPath, nil)
		adminAssetResponse := httptest.NewRecorder()
		handler.ServeHTTP(adminAssetResponse, adminAssetRequest)
		if adminAssetResponse.Code != http.StatusNotFound {
			t.Fatalf("admin asset path %q status = %d, want 404", assetPath, adminAssetResponse.Code)
		}
	}
}

func TestPortalAdminLoginAndProtectedAdminAPI(t *testing.T) {
	application, _ := newTestApp(t, probeFunc(func(context.Context, credentials) (probeOutcome, error) {
		return probeOutcome{}, nil
	}))
	handler := application.routes()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/admin/login", strings.NewReader(`{"username":"admin","password":"test-admin-password-12"}`))
	request.RemoteAddr = "127.0.0.1:54000"
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Origin", "http://127.0.0.1:8080")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("admin login status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Role      string `json:"role"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Role != "admin" || result.CSRFToken == "" {
		t.Fatalf("admin login result = %#v", result)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("admin login cookies = %#v", cookies)
	}

	unauthorized := httptest.NewRequest(http.MethodGet, "/api/copy/status", nil)
	unauthorized.RemoteAddr = "127.0.0.1:54000"
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("copy status without session = %d, want 401", unauthorizedResponse.Code)
	}

	adminStatus := httptest.NewRequest(http.MethodGet, "/api/copy/status", nil)
	adminStatus.RemoteAddr = "127.0.0.1:54000"
	adminStatus.AddCookie(cookies[0])
	adminStatusResponse := httptest.NewRecorder()
	handler.ServeHTTP(adminStatusResponse, adminStatus)
	if adminStatusResponse.Code != http.StatusOK {
		t.Fatalf("admin copy status = %d: %s", adminStatusResponse.Code, adminStatusResponse.Body.String())
	}
}

func TestConnectionResultsAndPasswordHandling(t *testing.T) {
	cases := []struct {
		name         string
		login        string
		outcome      probeOutcome
		probeErr     error
		wantStatus   string
		wantValid    bool
		wantBalance  *float64
		wantCurrency string
	}{
		{name: "probe reports successful broker login and balance", login: "100001", outcome: probeOutcome{Valid: true, Message: "Login broker berhasil.", Balance: float64Pointer(1234.56), Currency: "USC"}, wantStatus: "connected", wantValid: true, wantBalance: float64Pointer(1234.56), wantCurrency: "USC"},
		{name: "broker rejects password", login: "100002", outcome: probeOutcome{Valid: false, Message: "Login ditolak broker."}, wantStatus: "failed", wantValid: false},
		{name: "terminal reports another account", login: "100003", outcome: probeOutcome{Valid: true, Message: "Terminal siap.", ObservedLogin: 900003}, wantStatus: "failed", wantValid: false},
		{name: "terminal reports another server", login: "100004", outcome: probeOutcome{Valid: true, Message: "Terminal siap.", ObservedLogin: 100004, ObservedServer: "Different-Server"}, wantStatus: "failed", wantValid: false},
		{name: "probe times out", login: "100005", outcome: probeOutcome{Message: "Pemeriksaan MT5 gagal."}, probeErr: errors.New("terminal unavailable"), wantStatus: "failed", wantValid: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			probe := probeFunc(func(_ context.Context, got credentials) (probeOutcome, error) {
				if got.LoginText() != testCase.login {
					t.Fatalf("probe received login %q, want %q", got.LoginText(), testCase.login)
				}
				if got.Company != "Broker One" {
					t.Fatalf("probe received broker %q, want Broker One", got.Company)
				}
				if got.Password != "temporary-secret-"+testCase.login {
					t.Fatal("probe did not receive the submitted transient password")
				}
				return testCase.outcome, testCase.probeErr
			})
			application, dbPath := newTestApp(t, probe)
			secret := "temporary-secret-" + testCase.login
			payload, err := json.Marshal(loginRequest{Login: testCase.login, Server: "Broker-Live", Company: "Broker One", Password: secret})
			if err != nil {
				t.Fatal(err)
			}
			response := postConnect(application.routes(), string(payload))
			if response.Code != http.StatusOK {
				t.Fatalf("connect status = %d, body = %s", response.Code, response.Body.String())
			}
			var got struct {
				Valid    bool     `json:"valid"`
				Status   string   `json:"status"`
				Balance  *float64 `json:"balance"`
				Currency string   `json:"currency"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Valid != testCase.wantValid || got.Status != testCase.wantStatus {
				t.Fatalf("result valid=%v status=%q, want valid=%v status=%q", got.Valid, got.Status, testCase.wantValid, testCase.wantStatus)
			}
			if testCase.wantBalance == nil {
				if got.Balance != nil || got.Currency != "" {
					t.Fatalf("failed result exposed balance=%v currency=%q", got.Balance, got.Currency)
				}
			} else if got.Balance == nil || *got.Balance != *testCase.wantBalance || got.Currency != testCase.wantCurrency {
				t.Fatalf("response balance=%v currency=%q, want %v %s", got.Balance, got.Currency, *testCase.wantBalance, testCase.wantCurrency)
			}
			if strings.Contains(response.Body.String(), secret) {
				t.Fatal("response leaked the submitted password")
			}
			rows, err := application.db.Query(`SELECT login, server, status, message, updated_at, balance, currency FROM mt5_accounts`)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for rows.Next() {
				var login, server, status, message, updated, currency string
				var balance sql.NullFloat64
				if err := rows.Scan(&login, &server, &status, &message, &updated, &balance, &currency); err != nil {
					t.Fatal(err)
				}
				count++
				if login != testCase.login || server != "Broker-Live" || status != testCase.wantStatus || updated == "" {
					t.Fatalf("unexpected stored account: %q %q %q %q", login, server, status, updated)
				}
				if testCase.wantBalance == nil {
					if balance.Valid || currency != "" {
						t.Fatalf("failed account retained balance=%v currency=%q", balance, currency)
					}
				} else if !balance.Valid || balance.Float64 != *testCase.wantBalance || currency != testCase.wantCurrency {
					t.Fatalf("stored balance=%v currency=%q, want %v %s", balance, currency, *testCase.wantBalance, testCase.wantCurrency)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			if count != 1 {
				t.Fatalf("stored rows = %d, want 1", count)
			}

			accountsReq := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
			accountsReq.RemoteAddr = "127.0.0.1:54000"
			accountsReq.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: testAdminSessionToken})
			accountsRes := httptest.NewRecorder()
			application.routes().ServeHTTP(accountsRes, accountsReq)
			if strings.Contains(accountsRes.Body.String(), secret) {
				t.Fatal("accounts API leaked a submitted password")
			}
			if testCase.wantBalance != nil && (!strings.Contains(accountsRes.Body.String(), `"balance":1234.56`) || !strings.Contains(accountsRes.Body.String(), `"currency":"USC"`)) {
				t.Fatalf("accounts API did not expose verified balance: %s", accountsRes.Body.String())
			}
			if err := application.db.Close(); err != nil {
				t.Fatal(err)
			}
			databaseBytes, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(databaseBytes), secret) {
				t.Fatal("SQLite file contains the submitted password")
			}
		})
	}
}

func (c credentials) LoginText() string { return strconv.FormatInt(c.Login, 10) }

func TestInvalidAndCrossOriginRequestsAreRejected(t *testing.T) {
	called := false
	application, _ := newTestApp(t, probeFunc(func(context.Context, credentials) (probeOutcome, error) {
		called = true
		return probeOutcome{Valid: true}, nil
	}))
	handler := application.routes()

	invalid := postConnect(handler, `{"login":"abc","server":"Broker-Live","password":"not-saved"}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid login status = %d, want 400", invalid.Code)
	}
	if called {
		t.Fatal("probe ran for invalid login data")
	}

	crossOriginReq := httptest.NewRequest(http.MethodPost, "/api/connect", strings.NewReader(`{"login":"1","server":"Broker-Live","password":"secret"}`))
	crossOriginReq.RemoteAddr = "127.0.0.1:54000"
	crossOriginReq.Host = "127.0.0.1:8080"
	crossOriginReq.Header.Set("Origin", "http://example.invalid")
	crossOriginReq.Header.Set("Content-Type", "application/json")
	crossOriginReq.Header.Set("X-CSRF-Token", testAdminCSRFToken)
	crossOriginReq.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: testAdminSessionToken})
	crossOriginRes := httptest.NewRecorder()
	handler.ServeHTTP(crossOriginRes, crossOriginReq)
	if crossOriginRes.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", crossOriginRes.Code)
	}
	if called {
		t.Fatal("probe ran for cross-origin request")
	}
	var count int
	if err := application.db.QueryRow(`SELECT COUNT(*) FROM mt5_accounts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stored rows = %d, want 0", count)
	}
}

func TestAccountUpsertAndSchemaDoNotIncludePassword(t *testing.T) {
	application, _ := newTestApp(t, probeFunc(func(_ context.Context, _ credentials) (probeOutcome, error) {
		return probeOutcome{Valid: true, Message: "ready"}, nil
	}))
	for i := 0; i < 2; i++ {
		response := postConnect(application.routes(), `{"login":"24000","server":"Broker-Demo","password":"ephemeral"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("connect status = %d: %s", response.Code, response.Body.String())
		}
	}
	var count int
	if err := application.db.QueryRow(`SELECT COUNT(*) FROM mt5_accounts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("upsert rows = %d, want 1", count)
	}

	rows, err := application.db.Query(`PRAGMA table_info(mt5_accounts)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(name), "password") {
			t.Fatalf("password column unexpectedly exists: %s", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedRecheckClearsPreviouslyVerifiedBalance(t *testing.T) {
	checks := 0
	application, _ := newTestApp(t, probeFunc(func(_ context.Context, _ credentials) (probeOutcome, error) {
		checks++
		if checks == 1 {
			return probeOutcome{Valid: true, Message: "ready", Balance: float64Pointer(10.25), Currency: "USC"}, nil
		}
		return probeOutcome{Valid: false, Message: "Invalid account"}, errors.New("broker rejected login")
	}))
	for i := 0; i < 2; i++ {
		response := postConnect(application.routes(), `{"login":"24001","server":"Broker-Live","password":"ephemeral"}`)
		if response.Code != http.StatusOK {
			t.Fatalf("connect status = %d: %s", response.Code, response.Body.String())
		}
	}
	var balance sql.NullFloat64
	var currency string
	if err := application.db.QueryRow(`SELECT balance, currency FROM mt5_accounts WHERE login = ?`, "24001").Scan(&balance, &currency); err != nil {
		t.Fatal(err)
	}
	if balance.Valid || currency != "" {
		t.Fatalf("failed recheck retained balance=%v currency=%q", balance, currency)
	}
}

func TestOpenDatabaseMigratesLegacyBalanceColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-mt5-login.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.Exec(`CREATE TABLE mt5_accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		login TEXT NOT NULL,
		server TEXT NOT NULL,
		status TEXT NOT NULL,
		message TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(login, server)
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO mt5_accounts (login, server, status, message, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"10001", "Broker-Live", "connected", "ready", "2026-10-24T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := openDatabase(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var balance sql.NullFloat64
	var currency string
	if err := db.QueryRow(`SELECT balance, currency FROM mt5_accounts WHERE login = ?`, "10001").Scan(&balance, &currency); err != nil {
		t.Fatal(err)
	}
	if balance.Valid || currency != "" {
		t.Fatalf("legacy row unexpectedly has balance=%v currency=%q", balance, currency)
	}
}

func TestLegacySimulationResultCannotAppearAsBrokerSuccess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mt5-login.db")
	db, err := openDatabase(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO mt5_accounts (login, server, status, message, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"123456", "Broker-Demo", "connected", "Simulasi berhasil. Broker MT5 belum dihubungi.", "2026-10-24T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDatabase(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status, message string
	if err := db.QueryRow(`SELECT status, message FROM mt5_accounts WHERE login = ?`, "123456").Scan(&status, &message); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(message, "belum diverifikasi") {
		t.Fatalf("legacy simulated result status=%q message=%q", status, message)
	}
}

func TestLoopbackOnlyBinding(t *testing.T) {
	for _, testCase := range []struct {
		address string
		wantErr bool
	}{{"127.0.0.1:8080", false}, {"[::1]:8080", false}, {"0.0.0.0:8080", true}, {"localhost:8080", true}} {
		err := requireLoopbackAddress(testCase.address)
		if (err != nil) != testCase.wantErr {
			t.Errorf("requireLoopbackAddress(%q) error = %v, wantErr %v", testCase.address, err, testCase.wantErr)
		}
	}
}

func TestStatusNeverAdvertisesSimulation(t *testing.T) {
	application, _ := newTestApp(t, probeFunc(func(context.Context, credentials) (probeOutcome, error) {
		return probeOutcome{}, nil
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.RemoteAddr = "127.0.0.1:54000"
	request.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: testAdminSessionToken})
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status code = %d", response.Code)
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "simulation") || strings.Contains(strings.ToLower(response.Body.String()), "simulasi") {
		t.Fatalf("status response mentions a simulation: %s", response.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["terminal_configured"] != true || status["probe_ready"] != true {
		t.Fatalf("unexpected real verification readiness: %#v", status)
	}
}

func TestPortableTerminalCopiesProbeAndServerDirectory(t *testing.T) {
	template := t.TempDir()
	terminalPath := filepath.Join(template, "terminal64.exe")
	if err := os.WriteFile(terminalPath, []byte("terminal template"), 0o600); err != nil {
		t.Fatal(err)
	}
	serverDirectory := filepath.Join(template, "Config")
	if err := os.MkdirAll(serverDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverDirectory, "servers.dat"), []byte("broker list"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := terminalProbe{terminalPath: terminalPath, runtimeRoot: filepath.Join(t.TempDir(), "terminals")}
	credentials := credentials{Login: 12345, Server: "Broker-Live", Password: "one-use-password"}
	accountDir, sourceDir, err := probe.runtimeDirectory(credentials)
	if err != nil {
		t.Fatal(err)
	}
	portablePath, err := probe.preparePortableTerminal(credentials, accountDir, sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	if portablePath != filepath.Join(accountDir, "terminal64.exe") {
		t.Fatalf("portable path = %q, want terminal inside account folder", portablePath)
	}
	for _, path := range []string{
		portablePath,
		filepath.Join(accountDir, "Config", "servers.dat"),
		filepath.Join(accountDir, "MQL5", "Experts", "Algentra", "MT5LoginProbe.ex5"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("portable dependency %q missing: %v", path, err)
		}
	}
	gotProbe, err := os.ReadFile(filepath.Join(accountDir, "MQL5", "Experts", "Algentra", "MT5LoginProbe.ex5"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotProbe) != string(loginProbeBinary) {
		t.Fatal("portable terminal did not receive the compiled login probe")
	}
	if strings.Contains(accountDir, "one-use-password") {
		t.Fatal("runtime path contains the broker password")
	}
}

func TestProbeAuthFailureMessages(t *testing.T) {
	cases := []struct {
		reason      string
		wantMessage string
		definitive  bool
	}{
		{"invalid account", "Invalid account", true},
		{"account disabled", "menonaktifkan akun", true},
		{"trading disabled on server", "Login broker berhasil", false},
		{"no connection", "No connection", false},
	}
	for _, testCase := range cases {
		message, definitive := authFailureMessage(testCase.reason)
		if definitive != testCase.definitive || !strings.Contains(strings.ToLower(message), strings.ToLower(testCase.wantMessage)) {
			t.Errorf("authFailureMessage(%q) = %q, %v", testCase.reason, message, definitive)
		}
	}
}

func TestScanAuthLogReadsOnlyNewMT5Lines(t *testing.T) {
	terminalDir := t.TempDir()
	logsDir := filepath.Join(terminalDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logsDir, "20261024.log")
	if err := os.WriteFile(logPath, encodeUTF16LE("old '875001' authorized on old server\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	offsets := logOffsets(terminalDir)
	newLine := encodeUTF16LE("authorization on Broker-Live '875001' failed (Invalid account)\r\n")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.Write(newLine); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	authorized, reason := scanAuthLog(terminalDir, offsets, 875001)
	if authorized || reason != "invalid account" {
		t.Fatalf("scan result authorized=%v reason=%q", authorized, reason)
	}
}

func TestScanAuthLogKeepsAuthorizationWhenTradingIsDisabled(t *testing.T) {
	terminalDir := t.TempDir()
	logsDir := filepath.Join(terminalDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logsDir, "20261024.log")
	lines := "Network '875001': authorized on Broker-Live\r\n" +
		"Network '875001': trading has been disabled - disabled on server\r\n"
	if err := os.WriteFile(logPath, encodeUTF16LE(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	authorized, reason := scanAuthLog(terminalDir, map[string]int64{}, 875001)
	if !authorized || reason != "trading disabled on server" {
		t.Fatalf("scan result authorized=%v reason=%q", authorized, reason)
	}
}

func TestScanNetworkFailureReadsOnlyNewMT5Lines(t *testing.T) {
	terminalDir := t.TempDir()
	logsDir := filepath.Join(terminalDir, "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logsDir, "20261024.log")
	if err := os.WriteFile(logPath, encodeUTF16LE("old no connection to stale endpoint\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	offsets := logOffsets(terminalDir)
	if scanNetworkFailure(terminalDir, offsets) {
		t.Fatal("scanner accepted a stale network failure from before login started")
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.Write(encodeUTF16LE("Network 'account' : no connection to broker-endpoint\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	if !scanNetworkFailure(terminalDir, offsets) {
		t.Fatal("scanner missed a new broker network failure")
	}
}

func TestReadProbeStatusFromExpertLog(t *testing.T) {
	terminalDir := t.TempDir()
	expertLogs := filepath.Join(terminalDir, "MQL5", "Logs")
	if err := os.MkdirAll(expertLogs, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(expertLogs, "20261024.log")
	if err := os.WriteFile(logPath, encodeUTF16LE("old expert output\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	offsets := logOffsets(terminalDir)
	payload := `{"ready":true,"valid":true,"message":"Login broker berhasil.","login":12345,"server":"Broker-Live","balance":987.65,"currency":"USC"}`
	line := "[MT5LoginProbe] STATUS " + payload + "\r\n"
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(encodeUTF16LE(line)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, ok := readProbeStatusFromLogs(terminalDir, offsets)
	if !ok || !got.Ready || !got.Valid || got.Login != 12345 || got.Server != "Broker-Live" || got.Balance == nil || *got.Balance != 987.65 || got.Currency != "USC" {
		t.Fatalf("probe status = %#v, found=%v", got, ok)
	}
}

func TestValidateProbeReportReturnsBalanceOnlyForVerifiedIdentity(t *testing.T) {
	expected := credentials{Login: 452002597, Server: "Broker-Live"}
	report := probeReport{
		Ready: true, Valid: true, Message: "Login broker berhasil.",
		Login: expected.Login, Server: expected.Server,
		Balance: float64Pointer(100000.25), Currency: "USC",
	}
	got := validateProbeReport(report, expected, "")
	if !got.Valid || got.Balance == nil || *got.Balance != 100000.25 || got.Currency != "USC" {
		t.Fatalf("verified probe outcome = %#v", got)
	}

	report.Login++
	got = validateProbeReport(report, expected, "")
	if got.Valid || got.Balance != nil || got.Currency != "" {
		t.Fatalf("mismatched identity exposed balance: %#v", got)
	}
}

func encodeUTF16LE(value string) []byte {
	units := utf16.Encode([]rune(value))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		data[i*2] = byte(unit)
		data[i*2+1] = byte(unit >> 8)
	}
	return data
}

func TestBrokerAccessPointValidation(t *testing.T) {
	for _, testCase := range []struct {
		value string
		valid bool
	}{
		{"broker.example:443", true},
		{"127.0.0.1:1950", true},
		{"broker.example:0", false},
		{"broker.example:70000", false},
		{"https://broker.example", false},
	} {
		if got := validBrokerAccessPoint(testCase.value); got != testCase.valid {
			t.Errorf("validBrokerAccessPoint(%q) = %v, want %v", testCase.value, got, testCase.valid)
		}
	}
	if !isPrivateBrokerAccessPoint("10.131.152.102:443") {
		t.Fatal("RFC1918 broker address was not identified as private")
	}
	if isPrivateBrokerAccessPoint("broker.example:443") {
		t.Fatal("public broker hostname was classified as private")
	}
}

func TestBrokerServerSearchRanksExactMatchesAndDeduplicates(t *testing.T) {
	companies := []brokerDirectoryCompany{
		{CompanyName: "Broker One", Results: []brokerDirectoryEntry{
			{Name: "Broker One-Cent"},
			{Name: "Broker One-Demo"},
			{Name: "Broker One-Cent"},
		}},
		{CompanyName: "Broker Two", Results: []brokerDirectoryEntry{{Name: "Broker One-Cent"}}},
	}
	got := rankBrokerServers("Broker One-Cent", companies)
	if len(got) != 3 {
		t.Fatalf("search results = %d, want same-broker duplicates removed while preserving different brokers", len(got))
	}
	if got[0].Name != "Broker One-Cent" || got[0].Company != "Broker One" {
		t.Fatalf("exact result was not ranked first: %#v", got[0])
	}
}

func TestBrokerAccessPointUsesSelectedCompany(t *testing.T) {
	companies := []brokerDirectoryCompany{
		{CompanyName: "Broker One", Results: []brokerDirectoryEntry{{Name: "Cent", Access: []string{"one.example:443"}}}},
		{CompanyName: "Broker Two", Results: []brokerDirectoryEntry{{Name: "Cent", Access: []string{"10.131.152.102:443", "two.example:443"}}}},
	}
	got, err := brokerAccessPointForServer("Cent", "Broker Two", companies)
	if err != nil {
		t.Fatal(err)
	}
	if got != "two.example:443" {
		t.Fatalf("access point = %q, want selected broker's endpoint", got)
	}
}

func TestBrokerAccessPointRejectsPrivateOnlyAddress(t *testing.T) {
	companies := []brokerDirectoryCompany{
		{CompanyName: "Broker One", Results: []brokerDirectoryEntry{{Name: "Cent", Access: []string{"10.131.152.102:443"}}}},
	}
	got, err := brokerAccessPointForServer("Cent", "Broker One", companies)
	if got != "" || !errors.Is(err, errPrivateBrokerAccessPoint) {
		t.Fatalf("private access point = %q, err = %v; want a private-address error", got, err)
	}
}

func TestBrokerSearchShortQueryDoesNotContactDirectory(t *testing.T) {
	application, _ := newTestApp(t, probeFunc(func(context.Context, credentials) (probeOutcome, error) {
		return probeOutcome{}, nil
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/mt5/servers?q=ce", nil)
	response := httptest.NewRecorder()
	application.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"servers":[]`) {
		t.Fatalf("short broker query response = %d %s", response.Code, response.Body.String())
	}
}

func TestSeedBrokerServersPrefersTheMatchingMT5Profile(t *testing.T) {
	root := t.TempDir()
	templateDir := filepath.Join(root, "template")
	terminalPath := filepath.Join(templateDir, "terminal64.exe")
	if err := os.MkdirAll(filepath.Join(templateDir, "Config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "Config", "servers.dat"), []byte("install default list"), 0o600); err != nil {
		t.Fatal(err)
	}
	appData := filepath.Join(root, "AppData")
	t.Setenv("APPDATA", appData)
	terminalDataRoot := filepath.Join(appData, "MetaQuotes", "Terminal")
	matchingProfile := filepath.Join(terminalDataRoot, "profile-match")
	latestProfile := filepath.Join(terminalDataRoot, "profile-latest")
	for _, profile := range []string{matchingProfile, latestProfile} {
		if err := os.MkdirAll(filepath.Join(profile, "config"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(matchingProfile, "origin.txt"), []byte(terminalPath), 0o600); err != nil {
		t.Fatal(err)
	}
	matchingData := []byte("broker profile server list")
	if err := os.WriteFile(filepath.Join(matchingProfile, "config", "servers.dat"), matchingData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(latestProfile, "config", "servers.dat"), []byte("different profile list"), 0o600); err != nil {
		t.Fatal(err)
	}
	accountDir := filepath.Join(root, "runtime", "account")
	if err := seedBrokerServers(accountDir, templateDir, terminalPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(accountDir, "Config", "servers.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(matchingData) {
		t.Fatalf("seeded broker list = %q, want matching MT5 profile list", string(got))
	}
}

func TestFilteredTerminalEnvironmentRemovesSecrets(t *testing.T) {
	got := filteredTerminalEnvironment([]string{
		"Path=C:\\Windows\\System32",
		"APP_SECRET_KEY=do-not-pass",
		"MT5_PASSWORD=do-not-pass",
		"WORKER_API_KEY=do-not-pass",
		"APPDATA=C:\\Users\\tester\\AppData\\Roaming",
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "Path=") || !strings.Contains(joined, "APPDATA=") {
		t.Fatalf("terminal environment lost required variables: %q", joined)
	}
	if strings.Contains(joined, "do-not-pass") || strings.Contains(joined, "SECRET") || strings.Contains(joined, "PASSWORD") || strings.Contains(joined, "API_KEY") {
		t.Fatalf("terminal environment contains secret keys: %q", joined)
	}
}

func TestEnableAutoTradingPreservesOtherConfiguration(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "Config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	commonINI := filepath.Join(configDir, "common.ini")
	original := "[Experts]\r\nEnabled=0\r\nAllowLiveTrading=0\r\n;keep this comment\r\nAccount=1\r\n\r\n[Other]\r\nValue=untouched\r\n"
	if err := os.WriteFile(commonINI, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enableAutoTrading(root); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(commonINI)
	if err != nil {
		t.Fatal(err)
	}
	value := string(updated)
	for _, expected := range []string{"Enabled=1", "AllowLiveTrading=1", "Account=0", "Profile=0", ";keep this comment", "[Other]", "Value=untouched"} {
		if !strings.Contains(value, expected) {
			t.Errorf("common.ini missing %q after preparation", expected)
		}
	}
}
