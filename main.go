package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	_ "modernc.org/sqlite"
)

//go:embed web/*
var webFiles embed.FS

//go:embed mt5/experts/Algentra/MT5LoginProbe.ex5
var loginProbeBinary []byte

//go:embed mt5/experts/Algentra/TelegramSignalEA.ex5
var telegramSignalBinary []byte

//go:embed mt5/experts/Algentra/MT5FollowerCopyEA.ex5
var followerCopyBinary []byte

const (
	maxRequestBytes = 8 << 10
	probeTimeout    = 150 * time.Second
)

var errPrivateBrokerAccessPoint = errors.New("direktori broker hanya mengembalikan alamat IP privat")

type config struct {
	Address      string
	DatabasePath string
	DataDir      string
	TerminalPath string
}

type loginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	Server   string `json:"server"`
	Company  string `json:"company"`
}

type credentials struct {
	Login    int64
	Password string
	Server   string
	Company  string
}

type probeOutcome struct {
	Valid          bool     `json:"valid"`
	Message        string   `json:"message"`
	ObservedLogin  int64    `json:"-"`
	ObservedServer string   `json:"-"`
	Symbol         string   `json:"symbol,omitempty"`
	Balance        *float64 `json:"balance,omitempty"`
	Currency       string   `json:"currency,omitempty"`
}

type loginProbe interface {
	Check(context.Context, credentials) (probeOutcome, error)
}

type terminalProbe struct {
	terminalPath string
	runtimeRoot  string
	mu           sync.Mutex
}

type probeReport struct {
	Ready    bool     `json:"ready"`
	Valid    bool     `json:"valid"`
	Message  string   `json:"message"`
	Login    int64    `json:"login"`
	Server   string   `json:"server"`
	Symbol   string   `json:"symbol"`
	Chart    string   `json:"chart_symbol"`
	Balance  *float64 `json:"balance"`
	Currency string   `json:"currency"`
}

type account struct {
	ID        int64    `json:"id"`
	Login     string   `json:"login"`
	Server    string   `json:"server"`
	Status    string   `json:"status"`
	Message   string   `json:"message"`
	UpdatedAt string   `json:"updated_at"`
	Symbol    string   `json:"symbol,omitempty"`
	Balance   *float64 `json:"balance,omitempty"`
	Currency  string   `json:"currency,omitempty"`
}

type app struct {
	db          *sql.DB
	auth        *portalAuth
	probe       loginProbe
	config      config
	copier      *copyManager
	telegram    *telegramManager
	signals     *telegramSignalService
	signalEAKey string
	geminiModel string
}

func main() {
	if err := loadDotEnv(".env"); err != nil {
		log.Fatal("File .env tidak dapat dibaca:", err)
	}
	cfg := config{
		Address:      envOr("MT5_LOGIN_ADDR", "127.0.0.1:8080"),
		DatabasePath: envOr("MT5_LOGIN_DB", filepath.Join("data", "mt5-login.db")),
		DataDir:      envOr("MT5_LOGIN_DATA_DIR", "data"),
		TerminalPath: strings.TrimSpace(os.Getenv("MT5_TERMINAL_PATH")),
	}
	if err := requireLoopbackAddress(cfg.Address); err != nil {
		log.Fatal(err)
	}
	db, err := openDatabase(cfg.DatabasePath)
	if err != nil {
		log.Fatal("Database SQLite tidak dapat dibuka:", err)
	}
	defer db.Close()
	portalDB, err := openPortalDatabase(envOr("ALGENTRA_PORTAL_DB", filepath.Join(cfg.DataDir, "algentra-portal.db")))
	if err != nil {
		log.Fatal("Database portal Algentra tidak dapat dibuka:", err)
	}
	defer portalDB.Close()
	auth, err := newPortalAuth(portalDB, os.Getenv("ALGENTRA_ADMIN_USERNAME"), os.Getenv("ALGENTRA_ADMIN_PASSWORD"))
	if err != nil {
		log.Fatal("Autentikasi portal tidak dapat disiapkan:", err)
	}
	if auth.adminSetupError != "" {
		log.Printf("Admin login belum aktif: %s", auth.adminSetupError)
	}
	_, _ = db.Exec(`UPDATE telegram_signal_settings SET live_enabled=0, updated_at=? WHERE id=1`, time.Now().UTC().Format(time.RFC3339Nano))
	_, _ = db.Exec(`UPDATE telegram_signals SET status='DRY_RUN', reason='Aplikasi dimulai ulang; signal lama tidak otomatis dikirim.' WHERE status IN ('PENDING','CLAIMED')`)
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		log.Fatal("Alamat server tidak dapat dibuka:", err)
	}
	defer listener.Close()

	probe := &terminalProbe{terminalPath: cfg.TerminalPath, runtimeRoot: filepath.Join(cfg.DataDir, "terminals")}
	copier, err := newCopyManager(db, probe)
	if err != nil {
		_ = listener.Close()
		log.Fatal("Copy trading tidak dapat disiapkan:", err)
	}
	addressHost, addressPort, _ := net.SplitHostPort(cfg.Address)
	copier.serverURL = "http://" + net.JoinHostPort(addressHost, addressPort)
	telegramAPIID, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("TELEGRAM_API_ID")))
	telegram := newTelegramManager(telegramAPIID, os.Getenv("TELEGRAM_API_HASH"), filepath.Join(cfg.DataDir, "telegram-user.gotd.session"))
	parser := newGeminiSignalParser(os.Getenv("GEMINI_API_KEY"), os.Getenv("GEMINI_MODEL"))
	signals := newTelegramSignalService(db, parser, telegram)
	telegram.setMessageHandler(signals.processMessage)
	application := &app{db: db, auth: auth, probe: probe, config: cfg, copier: copier, telegram: telegram, signals: signals, signalEAKey: copier.signalEAKey, geminiModel: parser.model}
	managerCtx, stopManager := context.WithCancel(context.Background())
	defer func() {
		stopManager()
		copier.shutdown()
	}()
	go copier.run(managerCtx)
	if err := telegram.start(managerCtx); err != nil {
		log.Printf("Telegram user listener belum aktif: %v", err)
	}
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           application.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      4 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("Algentra Capital berjalan lokal di http://%s", cfg.Address)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("Server berhenti: %v", err)
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func filteredTerminalEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, pair := range environment {
		key, _, _ := strings.Cut(pair, "=")
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "PASS_") ||
			strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") ||
			strings.Contains(upper, "API_KEY") || strings.Contains(upper, "APIKEY") {
			continue
		}
		filtered = append(filtered, pair)
	}
	return filtered
}

func requireLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("MT5_LOGIN_ADDR tidak valid: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("MT5_Login hanya boleh di-bind ke alamat loopback, misalnya 127.0.0.1:8080")
	}
	return nil
}

func openDatabase(path string) (*sql.DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, err
	}
	const schema = `
CREATE TABLE IF NOT EXISTS mt5_accounts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	login TEXT NOT NULL,
	server TEXT NOT NULL,
	status TEXT NOT NULL,
	message TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	symbol TEXT NOT NULL DEFAULT '',
	balance REAL,
	currency TEXT NOT NULL DEFAULT '',
	UNIQUE(login, server)
);
CREATE INDEX IF NOT EXISTS idx_mt5_accounts_updated_at ON mt5_accounts(updated_at DESC);
CREATE TABLE IF NOT EXISTS copy_master (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	login TEXT NOT NULL,
	server TEXT NOT NULL,
	status TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	symbol TEXT NOT NULL DEFAULT '',
	connected_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS copy_clients (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	login TEXT NOT NULL,
	server TEXT NOT NULL,
	status TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	account_token TEXT NOT NULL,
	lot_mode TEXT NOT NULL DEFAULT 'rasio',
	ratio_lot REAL NOT NULL DEFAULT 1,
	lot_tetap REAL NOT NULL DEFAULT 0.01,
	max_lot_per_order REAL NOT NULL DEFAULT 0.1,
	max_lot_total REAL NOT NULL DEFAULT 0.5,
	max_open_positions INTEGER NOT NULL DEFAULT 10,
	copy_enabled INTEGER NOT NULL DEFAULT 0,
	dry_run INTEGER NOT NULL DEFAULT 1,
	copy_after INTEGER NOT NULL DEFAULT 0,
	last_seen_at TEXT NOT NULL DEFAULT '',
	balance REAL,
	currency TEXT NOT NULL DEFAULT '',
	history_cursor INTEGER NOT NULL DEFAULT 0,
	history_cursor_msc INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL,
	UNIQUE(login, server)
);
CREATE INDEX IF NOT EXISTS idx_copy_clients_updated_at ON copy_clients(updated_at DESC, id DESC);
CREATE TABLE IF NOT EXISTS telegram_signal_sources (
	chat_id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	username TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 0,
	alias TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS telegram_signal_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	live_enabled INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
INSERT OR IGNORE INTO telegram_signal_settings(id, live_enabled, updated_at) VALUES(1, 0, '');
CREATE TABLE IF NOT EXISTS telegram_signals (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	chat_id TEXT NOT NULL,
	group_name TEXT NOT NULL,
	message_id TEXT NOT NULL,
	sender_name TEXT NOT NULL DEFAULT '',
	raw_text TEXT NOT NULL,
	parsed_json TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	symbol TEXT NOT NULL DEFAULT '',
	entry_low REAL,
	entry_high REAL,
	sl REAL,
	tp_json TEXT NOT NULL DEFAULT '[]',
	confidence REAL,
	status TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	telegram_created_at TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	claimed_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE(chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_telegram_signals_status_created ON telegram_signals(status, created_at, id);
CREATE TABLE IF NOT EXISTS telegram_signal_orders (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	signal_id INTEGER NOT NULL REFERENCES telegram_signals(id) ON DELETE CASCADE,
	leg INTEGER NOT NULL,
	status TEXT NOT NULL,
	ticket TEXT NOT NULL DEFAULT '',
	symbol TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	lots REAL,
	entry_price REAL,
	reason TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	UNIQUE(signal_id, leg)
);
CREATE INDEX IF NOT EXISTS idx_telegram_signal_orders_ticket ON telegram_signal_orders(ticket);
CREATE TABLE IF NOT EXISTS telegram_signal_results (
	ticket TEXT PRIMARY KEY,
	signal_id INTEGER NOT NULL REFERENCES telegram_signals(id) ON DELETE CASCADE,
	leg INTEGER NOT NULL,
	profit REAL NOT NULL,
	result TEXT NOT NULL,
	closed_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS telegram_ea_heartbeat (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	received_at TEXT NOT NULL,
	terminal TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	symbol TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO telegram_ea_heartbeat(id, received_at) VALUES(1, '');
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := ensureAccountBalanceColumns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Older prototype builds saved successful simulations as "connected".
	// Reclassify those exact legacy rows so they cannot be mistaken for a
	// successful broker authentication after switching to real MT5 checks.
	if _, err := db.Exec(`
UPDATE mt5_accounts
SET status = 'failed', message = 'Hasil lama dari mode simulasi. Login broker belum diverifikasi; jalankan pemeriksaan ulang.'
WHERE status = 'connected' AND message = 'Simulasi berhasil. Broker MT5 belum dihubungi.'
`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ensureAccountBalanceColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(mt5_accounts)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		columns[strings.ToLower(name)] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !columns["symbol"] {
		if _, err := db.Exec(`ALTER TABLE mt5_accounts ADD COLUMN symbol TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !columns["balance"] {
		if _, err := db.Exec(`ALTER TABLE mt5_accounts ADD COLUMN balance REAL`); err != nil {
			return err
		}
	}
	if !columns["currency"] {
		if _, err := db.Exec(`ALTER TABLE mt5_accounts ADD COLUMN currency TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) routes() http.Handler {
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.handleIndex)
	mux.HandleFunc("GET /copy-trading", a.handleStrategiesPage)
	mux.HandleFunc("GET /login", a.handleClientLoginPage)
	mux.HandleFunc("GET /register", a.handleClientRegisterPage)
	mux.HandleFunc("GET /dashboard", a.handleClientDashboardPage)
	mux.HandleFunc("GET /admin/login", a.handleAdminLoginPage)
	mux.HandleFunc("GET /admin", a.handleAdminDashboardPage)
	mux.HandleFunc("GET /admin/signals", a.handleAdminSignalsPage)
	assetServer := http.FileServer(http.FS(assets))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extension := strings.ToLower(filepath.Ext(r.URL.Path))
		if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "/") || (extension != ".css" && extension != ".js") {
			http.NotFound(w, r)
			return
		}
		assetServer.ServeHTTP(w, r)
	})))
	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("GET /api/auth/session", a.handlePortalSession)
	mux.HandleFunc("GET /api/auth/admin-status", a.handleAdminSetupStatus)
	mux.HandleFunc("POST /api/auth/register", a.handlePortalRegister)
	mux.HandleFunc("POST /api/auth/client/login", a.handlePortalClientLogin)
	mux.HandleFunc("POST /api/auth/admin/login", a.handlePortalAdminLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handlePortalLogout)
	mux.HandleFunc("GET /api/public/strategies", a.handlePublicStrategies)
	mux.HandleFunc("GET /api/client/dashboard", a.handleClientDashboard)
	mux.HandleFunc("POST /api/client/mt5", a.handleClientMT5Login)
	mux.HandleFunc("POST /api/client/mode", a.handleClientMode)
	mux.HandleFunc("POST /api/client/pause", a.handleClientPause)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("GET /api/accounts", a.handleAccounts)
	mux.HandleFunc("POST /api/accounts/{id}/delete", a.handleDeleteSavedAccount)
	mux.HandleFunc("GET /api/mt5/servers", a.handleBrokerServerSearch)
	mux.HandleFunc("POST /api/connect", a.handleConnect)
	mux.HandleFunc("GET /api/copy/status", a.handleCopyStatus)
	mux.HandleFunc("POST /api/copy/master", a.handleCopyMaster)
	mux.HandleFunc("POST /api/copy/clients", a.handleCopyClient)
	mux.HandleFunc("POST /api/copy/clients/{id}/mode", a.handleCopyClientMode)
	mux.HandleFunc("POST /api/copy/clients/{id}/pause", a.handlePauseCopyClient)
	mux.HandleFunc("POST /api/copy/clients/{id}/delete", a.handleDeleteCopyClient)
	mux.HandleFunc("POST /api/copy/master/pause", a.handlePauseCopyMaster)
	mux.HandleFunc("POST /api/copy/master/delete", a.handleDeleteCopyMaster)
	mux.HandleFunc("GET /api/admin/signals/dashboard", a.handleSignalDashboard)
	mux.HandleFunc("POST /api/admin/signals/telegram/send-code", a.handleTelegramSendCode)
	mux.HandleFunc("POST /api/admin/signals/telegram/verify-code", a.handleTelegramVerifyCode)
	mux.HandleFunc("POST /api/admin/signals/telegram/verify-2fa", a.handleTelegramVerify2FA)
	mux.HandleFunc("POST /api/admin/signals/telegram/logout", a.handleTelegramLogout)
	mux.HandleFunc("GET /api/admin/signals/telegram/groups", a.handleTelegramGroups)
	mux.HandleFunc("POST /api/admin/signals/telegram/groups", a.handleSaveTelegramGroups)
	mux.HandleFunc("POST /api/admin/signals/live", a.handleSignalLive)
	mux.HandleFunc("POST /api/admin/signals/parse-test", a.handleSignalParseTest)
	mux.HandleFunc("POST /api/ea/master/snapshot", a.handleSignalMasterSnapshot)
	mux.HandleFunc("GET /api/ea/pending", a.handleSignalPending)
	mux.HandleFunc("POST /api/ea/heartbeat", a.handleSignalHeartbeat)
	mux.HandleFunc("POST /api/ea/report", a.handleSignalExecutionReport)
	mux.HandleFunc("POST /api/ea/result", a.handleSignalResult)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	a.servePortalPage(w, "public.html")
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", false); !ok {
		return
	}
	terminalConfigured := a.config.TerminalPath != ""
	terminalExists := false
	if terminalConfigured {
		terminal, err := filepath.Abs(a.config.TerminalPath)
		if err == nil {
			info, statErr := os.Stat(terminal)
			terminalExists = statErr == nil && !info.IsDir() && strings.EqualFold(filepath.Ext(terminal), ".exe")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"terminal_configured": terminalConfigured,
		"terminal_exists":     terminalExists,
		"probe_ready":         len(loginProbeBinary) > 0,
	})
}

func (a *app) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", false); !ok {
		return
	}
	rows, err := a.db.Query(`SELECT id, login, server, status, message, updated_at, symbol, balance, currency FROM mt5_accounts ORDER BY updated_at DESC, id DESC LIMIT 50`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Daftar akun tidak dapat dibaca."})
		return
	}
	defer rows.Close()
	accounts := make([]account, 0)
	for rows.Next() {
		var item account
		var balance sql.NullFloat64
		if err := rows.Scan(&item.ID, &item.Login, &item.Server, &item.Status, &item.Message, &item.UpdatedAt, &item.Symbol, &balance, &item.Currency); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Daftar akun tidak dapat dibaca."})
			return
		}
		if balance.Valid {
			value := balance.Float64
			item.Balance = &value
		}
		accounts = append(accounts, item)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Daftar akun tidak dapat dibaca."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

func (a *app) handleBrokerServerSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) < 3 || len(query) > 96 {
		writeJSON(w, http.StatusOK, map[string]any{"servers": []brokerServerOption{}})
		return
	}
	servers, err := searchBrokerServers(query)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Pencarian server broker gagal. Periksa internet lalu coba lagi."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (a *app) handleConnect(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	if !requestIsLocal(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses hanya diizinkan dari browser lokal."})
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Origin permintaan tidak diizinkan."})
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Gunakan content type application/json."})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input loginRequest
	if err := decoder.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Data akun tidak valid."})
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Kirim satu object JSON saja."})
		return
	}
	login, err := strconv.ParseInt(strings.TrimSpace(input.Login), 10, 64)
	server := strings.TrimSpace(input.Server)
	company := strings.TrimSpace(input.Company)
	if err != nil || login <= 0 || server == "" || len(server) > 120 || len(company) > 160 || len(input.Password) == 0 || len(input.Password) > 128 || strings.ContainsAny(server+company+input.Password, "\r\n\x00") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Nomor login, server, atau password tidak valid."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout+45*time.Second)
	defer cancel()
	outcome, probeErr := a.probe.Check(ctx, credentials{Login: login, Password: input.Password, Server: server, Company: company})
	if probeErr != nil {
		outcome.Valid = false
		outcome.Symbol = ""
		outcome.Balance = nil
		outcome.Currency = ""
		if outcome.Message == "" {
			outcome.Message = "Pemeriksaan MT5 gagal. Periksa terminal dan konfigurasi login."
		}
	}
	if outcome.ObservedLogin != 0 && outcome.ObservedLogin != login {
		outcome.Valid = false
		outcome.Symbol = ""
		outcome.Balance = nil
		outcome.Currency = ""
		outcome.Message = "Terminal terhubung ke akun yang berbeda dari nomor login yang dimasukkan."
	}
	if outcome.ObservedServer != "" && !strings.EqualFold(strings.TrimSpace(outcome.ObservedServer), server) {
		outcome.Valid = false
		outcome.Symbol = ""
		outcome.Balance = nil
		outcome.Currency = ""
		outcome.Message = "Server terminal berbeda dari server yang dimasukkan. Periksa nama server broker."
	}
	outcome.Symbol = strings.TrimSpace(outcome.Symbol)
	if !outcome.Valid {
		outcome.Symbol = ""
	}
	if !outcome.Valid || outcome.Balance == nil || !isValidBalance(*outcome.Balance) {
		outcome.Balance = nil
		outcome.Currency = ""
	} else {
		outcome.Currency = strings.TrimSpace(outcome.Currency)
		if outcome.Currency == "" || len(outcome.Currency) > 12 {
			outcome.Currency = ""
			outcome.Balance = nil
		}
	}
	outcome.Message = limitText(outcome.Message, 240)
	status := "failed"
	if outcome.Valid {
		status = "connected"
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	var balanceValue any
	currency := ""
	if outcome.Valid && outcome.Balance != nil && outcome.Currency != "" {
		balanceValue = *outcome.Balance
		currency = outcome.Currency
	}
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer saveCancel()
	_, err = a.db.ExecContext(saveCtx, `
INSERT INTO mt5_accounts (login, server, status, message, updated_at, symbol, balance, currency)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(login, server) DO UPDATE SET status=excluded.status, message=excluded.message, updated_at=excluded.updated_at, symbol=excluded.symbol, balance=excluded.balance, currency=excluded.currency
`, strconv.FormatInt(login, 10), server, status, outcome.Message, updatedAt, outcome.Symbol, balanceValue, currency)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Hasil pemeriksaan tidak dapat disimpan."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":      outcome.Valid,
		"message":    outcome.Message,
		"login":      strconv.FormatInt(login, 10),
		"server":     server,
		"status":     status,
		"updated_at": updatedAt,
		"symbol":     outcome.Symbol,
		"balance":    outcome.Balance,
		"currency":   outcome.Currency,
	})
}

func isValidBalance(balance float64) bool {
	return !math.IsNaN(balance) && !math.IsInf(balance, 0) && balance >= 0
}

func requestIsLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(origin, requestHost string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, requestHost)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func limitText(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return string(runes)
}

func marketWatchStartupSymbol(terminalDir string, login int64) string {
	pattern := filepath.Join(terminalDir, "bases", "*", "symbols", fmt.Sprintf("selected-%d.dat", login))
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return ""
	}
	sort.Strings(paths)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fields := strings.FieldsFunc(decodeUTF16LE(data), func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-')
		})
		for i := 0; i+1 < len(fields); i++ {
			if !strings.EqualFold(fields[i], "Selected") {
				continue
			}
			candidate := strings.TrimSpace(fields[i+1])
			if len(candidate) >= 3 && len(candidate) <= 32 {
				return candidate
			}
		}
	}
	return ""
}

func suffixFromMarketWatchSymbol(symbol string) (string, bool) {
	symbol = strings.TrimSpace(symbol)
	if len(symbol) <= 6 {
		return "", false
	}
	for i := 0; i < 6; i++ {
		character := symbol[i]
		if character < 'A' || character > 'Z' {
			return "", false
		}
	}
	return symbol[6:], true
}

func marketWatchBrokerSuffix(terminalDir string, login int64, startupSymbol string) string {
	pattern := filepath.Join(terminalDir, "bases", "*", "symbols", fmt.Sprintf("selected-%d.dat", login))
	paths, err := filepath.Glob(pattern)
	if err != nil {
		if suffix, ok := suffixFromMarketWatchSymbol(startupSymbol); ok {
			return suffix
		}
		return ""
	}
	sort.Strings(paths)
	suffixes := make([]string, 0)
	counts := make([]int, 0)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fields := strings.FieldsFunc(decodeUTF16LE(data), func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-')
		})
		selected := -1
		for i, field := range fields {
			if strings.EqualFold(field, "Selected") {
				selected = i + 1
				break
			}
		}
		if selected < 0 {
			continue
		}
		for _, candidate := range fields[selected:] {
			suffix, ok := suffixFromMarketWatchSymbol(candidate)
			if !ok {
				continue
			}
			found := -1
			for i := range suffixes {
				if strings.EqualFold(suffixes[i], suffix) {
					found = i
					break
				}
			}
			if found < 0 {
				suffixes = append(suffixes, suffix)
				counts = append(counts, 1)
			} else {
				counts[found]++
			}
		}
	}
	best := -1
	for i, count := range counts {
		if best < 0 || count > counts[best] {
			best = i
		}
	}
	if best >= 0 {
		return suffixes[best]
	}
	if suffix, ok := suffixFromMarketWatchSymbol(startupSymbol); ok {
		return suffix
	}
	return ""
}

func (p *terminalProbe) Check(ctx context.Context, creds credentials) (probeOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(p.terminalPath) == "" {
		return probeOutcome{Message: "Path MT5_TERMINAL_PATH belum diatur. Pilih terminal64.exe yang terpasang."}, errors.New("MT5_TERMINAL_PATH belum diatur")
	}
	terminalDir, sourceDir, err := p.runtimeDirectory(creds)
	if err != nil {
		return probeOutcome{Message: limitText(err.Error(), 240)}, err
	}
	killTerminalsIn(terminalDir)
	keepTerminalOpen := false
	defer func() {
		if !keepTerminalOpen {
			killTerminalsIn(terminalDir)
		}
	}()
	terminal, err := p.preparePortableTerminal(creds, terminalDir, sourceDir)
	if err != nil {
		return probeOutcome{Message: limitText(err.Error(), 240)}, err
	}
	if err := enableAutoTrading(terminalDir); err != nil {
		return probeOutcome{Message: "Izin Expert terminal MT5 tidak dapat disiapkan."}, err
	}
	filesDir := filepath.Join(terminalDir, "MQL5", "Files")
	presetsDir := filepath.Join(terminalDir, "MQL5", "Presets")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return probeOutcome{Message: "Folder data terminal MT5 tidak dapat ditulis."}, err
	}
	if err := os.MkdirAll(presetsDir, 0o700); err != nil {
		return probeOutcome{Message: "Folder preset terminal MT5 tidak dapat ditulis."}, err
	}
	stamp := strconv.FormatInt(time.Now().UnixNano(), 10)
	reportName := "mt5-login-probe-" + stamp + ".json"
	presetName := "mt5-login-probe-" + stamp + ".set"
	reportPath := filepath.Join(filesDir, reportName)
	legacyReportPath := filepath.Join(filesDir, "algentra-login-probe.json")
	presetPath := filepath.Join(presetsDir, presetName)
	commonFilesDir, err := mt5CommonFilesDirectory()
	if err != nil {
		return probeOutcome{Message: "Folder laporan bersama MT5 tidak dapat ditemukan."}, err
	}
	if err := os.MkdirAll(commonFilesDir, 0o700); err != nil {
		return probeOutcome{Message: "Folder laporan bersama MT5 tidak dapat disiapkan."}, err
	}
	commonReportPath := filepath.Join(commonFilesDir, reportName)
	if err := os.Remove(legacyReportPath); err != nil && !os.IsNotExist(err) {
		return probeOutcome{Message: "Laporan MT5 sebelumnya tidak dapat dibersihkan."}, err
	}
	defer os.Remove(presetPath)
	defer os.Remove(reportPath)
	defer os.Remove(commonReportPath)
	defer os.Remove(legacyReportPath)

	accessPoint := creds.Server
	serverSeedErr := seedBrokerServers(terminalDir, sourceDir, p.terminalPath)
	resolvedAccessPoint, lookupErr := findBrokerAccessPoint(creds.Server, creds.Company)
	privateDirectoryAddress := errors.Is(lookupErr, errPrivateBrokerAccessPoint)
	if lookupErr == nil {
		accessPoint = resolvedAccessPoint
	} else if serverSeedErr != nil {
		if privateDirectoryAddress {
			return probeOutcome{Message: privateBrokerDirectoryMessage()}, errors.Join(serverSeedErr, lookupErr)
		}
		return probeOutcome{Message: "Server broker tidak ditemukan pada direktori atau profil MT5 lokal. Cari server dari daftar, lalu coba lagi."}, errors.Join(serverSeedErr, lookupErr)
	}
	probeDir, err := os.MkdirTemp("", "mt5-login-")
	if err != nil {
		return probeOutcome{Message: "Folder sementara pemeriksaan login tidak dapat dibuat."}, err
	}
	defer os.RemoveAll(probeDir)
	_ = os.Chmod(probeDir, 0o700)
	offsets := logOffsets(terminalDir)
	startupSymbol := marketWatchStartupSymbol(terminalDir, creds.Login)
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	suffix := marketWatchBrokerSuffix(terminalDir, creds.Login, startupSymbol)
	expectedGoldSymbol := "XAUUSD" + suffix
	log.Printf("MT5 login probe: suffix Market Watch %q, chart target %q", suffix, expectedGoldSymbol)
	presetContents := "ProbeStatusFileName=" + reportName + "\r\nExpectedGoldSymbol=" + expectedGoldSymbol + "\r\n"
	if err := os.WriteFile(presetPath, []byte(presetContents), 0o600); err != nil {
		return probeOutcome{Message: "Preset Expert login probe tidak dapat disiapkan."}, err
	}

	iniPath := filepath.Join(probeDir, "login-probe.ini")
	ini := strings.Join([]string{
		"[Common]",
		"Login=" + strconv.FormatInt(creds.Login, 10),
		"Password=" + creds.Password,
		"Server=" + accessPoint,
		"KeepPrivate=1",
		"NewsEnable=0",
		"ProxyEnable=0",
		"",
		"[Experts]",
		"AllowLiveTrading=1",
		"AllowDllImport=0",
		"Enabled=1",
		"Account=0",
		"Profile=0",
		"",
		// Attach the Expert directly to the broker's XAUUSD symbol. The suffix is
		// derived from the symbols saved in this account's Market Watch.
		"[StartUp]",
		`Expert=Algentra\MT5LoginProbe`,
		"ExpertParameters=" + presetName,
		"Symbol=" + expectedGoldSymbol,
		"Period=M1",
		"",
	}, "\r\n")
	if err := os.WriteFile(iniPath, []byte(ini), 0o600); err != nil {
		return probeOutcome{Message: "Konfigurasi sementara terminal MT5 tidak dapat ditulis."}, err
	}
	defer os.Remove(iniPath)

	cmd := exec.Command(terminal, "/portable", "/config:"+iniPath)
	cmd.Dir = terminalDir
	cmd.Env = filteredTerminalEnvironment(os.Environ())
	prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		return probeOutcome{Message: "Terminal MT5 tidak dapat dijalankan."}, err
	}
	terminalStartedAt := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	processDone := (<-chan error)(done)
	processExited := false
	defer func() {
		if !processExited && !keepTerminalOpen {
			_ = stopChildProcess(cmd, done, 10*time.Second)
		}
	}()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	var lastReport probeReport
	seenReport := false
	authorized := false
	for {
		select {
		case <-probeCtx.Done():
			loggedIn, reason := scanAuthLog(terminalDir, offsets, creds.Login)
			if loggedIn {
				authorized = true
			}
			if reason != "" {
				message, _ := authFailureMessage(reason)
				return probeOutcome{Message: message}, probeCtx.Err()
			}
			if scanNetworkFailure(terminalDir, offsets) {
				if privateDirectoryAddress {
					return probeOutcome{Message: privateBrokerDirectoryMessage()}, probeCtx.Err()
				}
				return probeOutcome{Message: networkFailureMessage()}, probeCtx.Err()
			}
			if authorized {
				chart, _ := readProbeChartFromLogs(terminalDir, offsets)
				result := outcomeFromAuthorizedChart(creds, chart, expectedGoldSymbol)
				keepTerminalOpen = shouldKeepTerminalOpen(result)
				return result, nil
			}
			if seenReport && lastReport.Message != "" && !strings.Contains(strings.ToLower(lastReport.Message), "menunggu koneksi broker") {
				return probeOutcome{Message: limitText(lastReport.Message, 240)}, probeCtx.Err()
			}
			if privateDirectoryAddress {
				return probeOutcome{Message: privateBrokerDirectoryMessage()}, probeCtx.Err()
			}
			return probeOutcome{Message: "MT5 tidak menyelesaikan koneksi dalam 150 detik. Periksa server broker dan koneksi internet."}, probeCtx.Err()
		case waitErr := <-processDone:
			processExited = true
			processDone = nil
			report, ok := readAnyProbeStatus(reportPath, commonReportPath, legacyReportPath)
			if !ok {
				report, ok = readProbeStatusFromLogs(terminalDir, offsets)
			}
			if ok && report.Ready {
				result := validateProbeReport(report, creds, expectedGoldSymbol)
				keepTerminalOpen = shouldKeepTerminalOpen(result)
				return result, nil
			}
			if ok {
				seenReport, lastReport = true, report
			}
			loggedIn, reason := scanAuthLog(terminalDir, offsets, creds.Login)
			if reason != "" {
				if message, definitive := authFailureMessage(reason); definitive {
					return probeOutcome{Message: message}, waitErr
				}
			}
			if scanNetworkFailure(terminalDir, offsets) {
				if privateDirectoryAddress {
					return probeOutcome{Message: privateBrokerDirectoryMessage()}, waitErr
				}
				return probeOutcome{Message: networkFailureMessage()}, waitErr
			}
			if loggedIn {
				authorized = true
				if !seenReport && time.Since(terminalStartedAt) >= 5*time.Second {
					chart, ok := readProbeChartFromLogs(terminalDir, offsets)
					if ok {
						result := outcomeFromAuthorizedChart(creds, chart, expectedGoldSymbol)
						keepTerminalOpen = shouldKeepTerminalOpen(result)
						return result, nil
					}
				}
			}
			// MT5 may exit this process to hand off to LiveUpdate, which then
			// restarts terminal64.exe with the same portable data and config.
			// Keep polling for the report until the normal probe deadline.
		case <-ticker.C:
			report, ok := readAnyProbeStatus(reportPath, commonReportPath, legacyReportPath)
			if !ok {
				report, ok = readProbeStatusFromLogs(terminalDir, offsets)
			}
			if ok {
				seenReport, lastReport = true, report
				if report.Ready {
					result := validateProbeReport(report, creds, expectedGoldSymbol)
					keepTerminalOpen = shouldKeepTerminalOpen(result)
					if !processExited && !keepTerminalOpen {
						_ = stopChildProcess(cmd, done, 10*time.Second)
						processExited = true
					}
					return result, nil
				}
			}
			loggedIn, reason := scanAuthLog(terminalDir, offsets, creds.Login)
			if reason != "" {
				if message, definitive := authFailureMessage(reason); definitive {
					return probeOutcome{Message: message}, errors.New("broker rejected the login")
				}
			}
			authorized = authorized || loggedIn
			if authorized && !seenReport && time.Since(terminalStartedAt) >= 5*time.Second {
				if chart, ok := readProbeChartFromLogs(terminalDir, offsets); ok {
					result := outcomeFromAuthorizedChart(creds, chart, expectedGoldSymbol)
					keepTerminalOpen = shouldKeepTerminalOpen(result)
					return result, nil
				}
			}
		}
	}
}

func (p *terminalProbe) runtimeDirectory(creds credentials) (string, string, error) {
	sourceTerminal, err := filepath.Abs(p.terminalPath)
	if err != nil {
		return "", "", errors.New("Path terminal64.exe tidak valid.")
	}
	info, err := os.Stat(sourceTerminal)
	if err != nil || info.IsDir() || !strings.EqualFold(filepath.Ext(sourceTerminal), ".exe") {
		return "", "", errors.New("terminal64.exe tidak ditemukan pada path yang dipilih.")
	}
	if strings.ContainsAny(creds.Server+creds.Company+creds.Password, "\r\n\x00") {
		return "", "", errors.New("Server, broker, atau password mengandung karakter yang tidak valid.")
	}
	if len(loginProbeBinary) == 0 {
		return "", "", errors.New("MT5LoginProbe.ex5 belum tersedia di aplikasi.")
	}
	accountDir, err := terminalAccountRuntimePath(p.runtimeRoot, creds.Login, creds.Server)
	if err != nil {
		return "", "", err
	}
	sourceDir := filepath.Dir(sourceTerminal)
	if pathWithin(sourceDir, accountDir) || pathWithin(accountDir, sourceDir) {
		return "", "", errors.New("Folder terminal template tidak boleh bertumpang tindih dengan data/terminals.")
	}
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return "", "", errors.New("Folder terminal runtime tidak dapat dibuat.")
	}
	return accountDir, sourceDir, nil
}

func terminalAccountRuntimePath(runtimeRoot string, login int64, server string) (string, error) {
	server = strings.TrimSpace(server)
	if runtimeRoot == "" || login <= 0 || server == "" || len(server) > 120 || strings.ContainsAny(server, "\r\n\x00") {
		return "", errors.New("identitas akun terminal tidak valid")
	}
	root, err := filepath.Abs(runtimeRoot)
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(strconv.FormatInt(login, 10) + "\x00" + strings.ToLower(server)))
	accountDir, err := filepath.Abs(filepath.Join(root, hex.EncodeToString(key[:8])))
	if err != nil {
		return "", err
	}
	if accountDir == root || !pathWithin(root, accountDir) {
		return "", errors.New("folder terminal akun berada di luar direktori runtime")
	}
	return accountDir, nil
}

func (p *terminalProbe) preparePortableTerminal(creds credentials, accountDir, sourceDir string) (string, error) {
	sourceTerminal, err := filepath.Abs(p.terminalPath)
	if err != nil {
		return "", errors.New("Path terminal64.exe tidak valid.")
	}
	portableTerminal := filepath.Join(accountDir, "terminal64.exe")
	if err := copyFileIfMissing(sourceTerminal, portableTerminal); err != nil {
		return "", errors.New("Terminal MT5 tidak dapat disiapkan ke folder portable lokal.")
	}
	configSource := filepath.Join(sourceDir, "Config")
	if info, statErr := os.Stat(configSource); statErr == nil && info.IsDir() {
		if err := copyTreeIfMissing(configSource, filepath.Join(accountDir, "Config")); err != nil {
			return "", errors.New("Direktori server broker MT5 tidak dapat disalin.")
		}
	}
	expertPath := filepath.Join(accountDir, "MQL5", "Experts", "Algentra", "MT5LoginProbe.ex5")
	if err := os.MkdirAll(filepath.Dir(expertPath), 0o700); err != nil {
		return "", errors.New("Folder Expert terminal MT5 tidak dapat dibuat.")
	}
	if err := os.WriteFile(expertPath, loginProbeBinary, 0o600); err != nil {
		return "", errors.New("MT5LoginProbe.ex5 tidak dapat disiapkan.")
	}
	return portableTerminal, nil
}

func pathWithin(parent, candidate string) bool {
	parent = strings.ToLower(filepath.Clean(parent))
	candidate = strings.ToLower(filepath.Clean(candidate))
	relative, err := filepath.Rel(parent, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func copyFileIfMissing(source, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".mt5-copy-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		if _, statErr := os.Stat(destination); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func copyTreeIfMissing(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symbolic link tidak didukung di folder Config MT5")
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyFileIfMissing(path, target)
	})
}

type brokerDirectoryCompany struct {
	CompanyName string                 `json:"companyName"`
	Results     []brokerDirectoryEntry `json:"results"`
}

type brokerDirectoryEntry struct {
	Name   string   `json:"name"`
	Access []string `json:"access"`
}

type brokerServerOption struct {
	Name    string `json:"name"`
	Company string `json:"company"`
}

func mt5CommonFilesDirectory() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		var err error
		appData, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(appData) == "" {
		return "", errors.New("folder data bersama MT5 tidak dapat ditemukan")
	}
	return filepath.Join(appData, "MetaQuotes", "Terminal", "Common", "Files"), nil
}

func seedBrokerServers(accountDir, templateDir, terminalPath string) error {
	target := filepath.Join(accountDir, "Config", "servers.dat")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	var candidates []string
	terminalDir := filepath.Dir(terminalPath)
	if appData := os.Getenv("APPDATA"); appData != "" {
		terminalDataRoot := filepath.Join(appData, "MetaQuotes", "Terminal")
		entries, _ := os.ReadDir(terminalDataRoot)
		want := normalizedWindowsPath(terminalPath)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			dataDir := filepath.Join(terminalDataRoot, entry.Name())
			origin, _ := os.ReadFile(filepath.Join(dataDir, "origin.txt"))
			if strings.Contains(normalizedWindowsPath(string(origin)), want) {
				candidates = append(candidates, filepath.Join(dataDir, "config", "servers.dat"))
			}
		}

		// When this install path has no matching profile, reuse the newest
		// broker list from another MT5 profile for the same Windows user.
		var newest string
		var newestAt time.Time
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			candidate := filepath.Join(terminalDataRoot, entry.Name(), "config", "servers.dat")
			info, err := os.Stat(candidate)
			if err == nil && info.Size() > 0 && info.ModTime().After(newestAt) {
				newest, newestAt = candidate, info.ModTime()
			}
		}
		if newest != "" {
			candidates = append(candidates, newest)
		}
	}
	candidates = append(candidates,
		filepath.Join(terminalDir, "Config", "servers.dat"),
		filepath.Join(terminalDir, "config", "servers.dat"),
		filepath.Join(templateDir, "Config", "servers.dat"),
		filepath.Join(templateDir, "config", "servers.dat"),
	)

	for _, source := range candidates {
		info, err := os.Stat(source)
		if err != nil || info.IsDir() || info.Size() == 0 {
			continue
		}
		if filepath.Clean(source) == filepath.Clean(target) {
			return nil
		}
		return copyFileReplace(source, target)
	}
	return os.ErrNotExist
}

func copyFileReplace(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0o600)
}

func normalizedWindowsPath(value string) string {
	value = strings.TrimSpace(strings.Trim(value, "\x00"))
	value = strings.ReplaceAll(value, "/", "\\")
	return strings.ToLower(filepath.Clean(value))
}

func findBrokerAccessPoint(server, companyName string) (string, error) {
	companies, err := fetchBrokerDirectory(server)
	if err != nil {
		return "", err
	}
	return brokerAccessPointForServer(server, companyName, companies)
}

func brokerAccessPointForServer(server, companyName string, companies []brokerDirectoryCompany) (string, error) {
	privateAddressSeen := false
	for _, company := range companies {
		if companyName != "" && !strings.EqualFold(strings.TrimSpace(company.CompanyName), strings.TrimSpace(companyName)) {
			continue
		}
		for _, candidate := range company.Results {
			if !strings.EqualFold(strings.TrimSpace(candidate.Name), strings.TrimSpace(server)) {
				continue
			}
			for _, access := range candidate.Access {
				if validBrokerAccessPoint(access) {
					if isPrivateBrokerAccessPoint(access) {
						privateAddressSeen = true
						continue
					}
					return strings.TrimSpace(access), nil
				}
			}
		}
	}
	if privateAddressSeen {
		return "", fmt.Errorf("%w: %q", errPrivateBrokerAccessPoint, server)
	}
	return "", fmt.Errorf("server broker %q tidak ditemukan", server)
}

func searchBrokerServers(query string) ([]brokerServerOption, error) {
	companies, err := fetchBrokerDirectory(query)
	if err != nil {
		return nil, err
	}
	return rankBrokerServers(query, companies), nil
}

func rankBrokerServers(query string, companies []brokerDirectoryCompany) []brokerServerOption {
	needle := strings.ToLower(strings.TrimSpace(query))
	seen := make(map[string]struct{})
	servers := make([]brokerServerOption, 0)
	for _, company := range companies {
		companyName := strings.TrimSpace(company.CompanyName)
		if companyName == "" {
			continue
		}
		for _, result := range company.Results {
			name := strings.TrimSpace(result.Name)
			if name == "" {
				continue
			}
			key := strings.ToLower(companyName) + "|" + strings.ToLower(name)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			servers = append(servers, brokerServerOption{Name: name, Company: companyName})
		}
	}
	sort.Slice(servers, func(i, j int) bool {
		iExact := !strings.EqualFold(servers[i].Name, needle)
		jExact := !strings.EqualFold(servers[j].Name, needle)
		if iExact != jExact {
			return !iExact
		}
		if !strings.EqualFold(servers[i].Name, servers[j].Name) {
			return strings.ToLower(servers[i].Name) < strings.ToLower(servers[j].Name)
		}
		return strings.ToLower(servers[i].Company) < strings.ToLower(servers[j].Company)
	})
	if len(servers) > 200 {
		servers = servers[:200]
	}
	return servers
}

func fetchBrokerDirectory(query string) ([]brokerDirectoryCompany, error) {
	endpoint := strings.TrimSpace(os.Getenv("MT5_BROKER_DIRECTORY_URL"))
	if endpoint == "" {
		endpoint = "https://mt5.mtapi.io/Search"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("direktori broker harus menggunakan endpoint HTTPS")
	}
	values := parsed.Query()
	values.Set("company", strings.TrimSpace(query))
	parsed.RawQuery = values.Encode()
	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("direktori broker menjawab %s", response.Status)
	}
	var companies []brokerDirectoryCompany
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&companies); err != nil {
		return nil, err
	}
	return companies, nil
}

func validBrokerAccessPoint(value string) bool {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil || host == "" || strings.ContainsAny(host, " \t/\\") {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port > 0 && port <= 65535
}

func isPrivateBrokerAccessPoint(value string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast())
}

func logOffsets(dir string) map[string]int64 {
	offsets := map[string]int64{}
	for _, pattern := range []string{
		filepath.Join(dir, "logs", "*.log"),
		filepath.Join(dir, "MQL5", "Logs", "*.log"),
	} {
		files, _ := filepath.Glob(pattern)
		for _, path := range files {
			if info, err := os.Stat(path); err == nil {
				offsets[path] = info.Size()
			}
		}
	}
	return offsets
}

func scanAuthLog(dir string, offsets map[string]int64, login int64) (authorized bool, failure string) {
	files, _ := filepath.Glob(filepath.Join(dir, "logs", "*.log"))
	needle := "'" + strconv.FormatInt(login, 10) + "'"
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		start := offsets[path]
		if start < 0 || start > int64(len(data)) {
			start = 0
		}
		data = data[start&^1:]
		units := make([]uint16, 0, len(data)/2)
		for i := 0; i+1 < len(data); i += 2 {
			units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
		}
		for _, line := range strings.Split(string(utf16.Decode(units)), "\n") {
			if !strings.Contains(line, needle) {
				continue
			}
			lower := strings.ToLower(line)
			if strings.Contains(lower, "authorized on") {
				authorized, failure = true, ""
			} else if strings.Contains(lower, "trading has been disabled") && strings.Contains(lower, "disabled on server") {
				failure = "trading disabled on server"
			} else if i := strings.LastIndex(lower, "authorization on"); i >= 0 {
				if j := strings.LastIndex(lower, "failed ("); j > i {
					authorized = false
					failure = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(lower[j+len("failed ("):]), ")"))
				}
			}
		}
	}
	return authorized, failure
}

func scanNetworkFailure(dir string, offsets map[string]int64) bool {
	for _, pattern := range []string{
		filepath.Join(dir, "logs", "*.log"),
		filepath.Join(dir, "MQL5", "Logs", "*.log"),
	} {
		files, _ := filepath.Glob(pattern)
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			start := offsets[path]
			if start < 0 || start > int64(len(data)) {
				start = 0
			}
			data = data[start&^1:]
			for _, line := range strings.Split(strings.ToLower(decodeUTF16LE(data)), "\n") {
				if strings.Contains(line, "no connection to") ||
					strings.Contains(line, "network is unreachable") ||
					strings.Contains(line, "connection failed") ||
					strings.Contains(line, "failed to connect") {
					return true
				}
			}
		}
	}
	return false
}

func networkFailureMessage() string {
	return "Terminal MT5 tidak dapat menjangkau server broker (No connection). Periksa internet komputer ini, firewall/antivirus atau VPN, lalu pastikan nama server cocok dengan broker. Kredensial belum sempat diverifikasi."
}

func privateBrokerDirectoryMessage() string {
	return "Server memakai IP privat yang tidak terjangkau. Segarkan daftar MT5 via File > Open an Account, lalu pilih server dari detail akun. Jika perlu jaringan privat, gunakan VPN resmi broker. Login belum diverifikasi."
}

func authFailureMessage(reason string) (string, bool) {
	r := strings.ToLower(reason)
	switch {
	case strings.Contains(r, "invalid account"):
		return "Broker menolak login (Invalid account). Periksa nomor login, password, dan server.", true
	case strings.Contains(r, "account disabled"):
		return "Broker menonaktifkan akun ini.", true
	case strings.Contains(r, "trading disabled on server"):
		return "Login broker berhasil, tetapi server menonaktifkan izin trading akun ini.", false
	case strings.Contains(r, "no connection"):
		return "Terminal tidak dapat terhubung ke server broker (No connection). Periksa nama server dan internet.", false
	default:
		return "Broker menolak login: " + reason + ".", false
	}
}

func readProbeReport(path string) (probeReport, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return probeReport{}, false
	}
	var report probeReport
	if json.Unmarshal(data, &report) != nil {
		return probeReport{}, false
	}
	return report, true
}

func readAnyProbeStatus(paths ...string) (probeReport, bool) {
	for _, path := range paths {
		if report, ok := readProbeReport(path); ok {
			return report, true
		}
	}
	return probeReport{}, false
}

func readProbeStatusFromLogs(dir string, offsets map[string]int64) (probeReport, bool) {
	patterns := []string{
		filepath.Join(dir, "logs", "*.log"),
		filepath.Join(dir, "MQL5", "Logs", "*.log"),
	}
	var latest probeReport
	found := false
	const marker = "[MT5LoginProbe] STATUS "
	for _, pattern := range patterns {
		files, _ := filepath.Glob(pattern)
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			start := offsets[path]
			if start < 0 || start > int64(len(data)) {
				start = 0
			}
			data = data[start&^1:]
			for _, line := range strings.Split(decodeUTF16LE(data), "\n") {
				index := strings.Index(line, marker)
				if index < 0 {
					continue
				}
				var report probeReport
				if json.Unmarshal([]byte(strings.TrimSpace(line[index+len(marker):])), &report) == nil {
					latest, found = report, true
				}
			}
		}
	}
	return latest, found
}

func decodeUTF16LE(data []byte) string {
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		units = append(units, uint16(data[i])|uint16(data[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

func readProbeChartFromLogs(dir string, offsets map[string]int64) (string, bool) {
	files := make([]string, 0)
	for _, pattern := range []string{
		filepath.Join(dir, "logs", "*.log"),
		filepath.Join(dir, "MQL5", "Logs", "*.log"),
	} {
		matches, _ := filepath.Glob(pattern)
		files = append(files, matches...)
	}
	sort.Strings(files)
	chart := ""
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		start := offsets[path]
		if start < 0 || start > int64(len(data)) {
			start = 0
		}
		data = data[start&^1:]
		for _, line := range strings.Split(decodeUTF16LE(data), "\n") {
			lower := strings.ToLower(line)
			const loadedMarker = "expert mt5loginprobe ("
			if index := strings.Index(lower, loadedMarker); index >= 0 {
				start := index + len(loadedMarker)
				if end := strings.Index(line[start:], ","); end > 0 {
					candidate := strings.TrimSpace(line[start : start+end])
					if candidate != "" {
						chart = candidate
					}
				}
			}
		}
	}
	return chart, chart != ""
}

func outcomeFromAuthorizedChart(creds credentials, chart, expectedSymbol string) probeOutcome {
	result := probeOutcome{
		Valid:          true,
		ObservedLogin:  creds.Login,
		ObservedServer: creds.Server,
	}
	result.Symbol = strings.TrimSpace(chart)
	if !probeSymbolMatches(expectedSymbol, result.Symbol) {
		result.Valid = false
		result.Message = "Coba login sekali lagi. Jika masih belum terhubung, coba periksa server dan kata sandi Anda."
		return result
	}
	result.Message = "Login broker berhasil. Expert aktif pada chart " + result.Symbol + "."
	return result
}

func probeSymbolMatches(expected, actual string) bool {
	expected = strings.ToUpper(strings.TrimSpace(expected))
	actual = strings.ToUpper(strings.TrimSpace(actual))
	if expected == "" {
		return true
	}
	return strings.HasPrefix(expected, "XAUUSD") && strings.HasPrefix(actual, "XAUUSD")
}

func validateProbeReport(report probeReport, expected credentials, expectedSymbol string) probeOutcome {
	result := probeOutcome{
		Valid:          report.Valid,
		Message:        limitText(report.Message, 240),
		ObservedLogin:  report.Login,
		ObservedServer: strings.TrimSpace(report.Server),
		Symbol:         strings.TrimSpace(report.Symbol),
	}
	if report.Login != expected.Login {
		result.Valid = false
		result.Message = "Terminal terhubung ke akun yang berbeda dari nomor login yang dimasukkan."
	}
	if !strings.EqualFold(strings.TrimSpace(report.Server), strings.TrimSpace(expected.Server)) {
		result.Valid = false
		result.Message = "Server terminal berbeda dari server yang dimasukkan. Periksa nama server broker."
	}
	if result.Valid {
		chart := strings.TrimSpace(report.Chart)
		if !probeSymbolMatches(expectedSymbol, chart) || !probeSymbolMatches(expectedSymbol, report.Symbol) {
			result.Valid = false
			result.Message = "Coba login sekali lagi. Jika masih belum terhubung, coba periksa server dan kata sandi Anda."
		} else {
			result.Symbol = chart
		}
	}
	if result.Valid && report.Balance != nil && isValidBalance(*report.Balance) {
		currency := strings.TrimSpace(report.Currency)
		if currency != "" && len(currency) <= 12 {
			balance := *report.Balance
			result.Balance = &balance
			result.Currency = currency
		}
	}
	return result
}

func shouldKeepTerminalOpen(result probeOutcome) bool {
	// Keep an authenticated terminal open even when chart selection failed so
	// the actual MT5 state remains available for diagnosis.
	return result.Valid
}
