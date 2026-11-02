package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	copyMasterSnapshotName = "algentra-copy-master.json"
	copyProbeTimeout       = 45 * time.Second
	copyMasterFreshness    = 5 * time.Second
	copyClientFreshness    = 15 * time.Second
)

type copyMasterRecord struct {
	Login       string
	Server      string
	Status      string
	Message     string
	Symbol      string
	ConnectedAt string
	UpdatedAt   string
}

type copyClientRecord struct {
	ID               int64
	Login            string
	Server           string
	Status           string
	Message          string
	AccountToken     string
	LotMode          string
	RatioLot         float64
	FixedLot         float64
	MaxLotPerOrder   float64
	MaxLotTotal      float64
	MaxOpenPositions int
	CopyEnabled      bool
	DryRun           bool
	CopyAfter        int64
	LastSeenAt       string
	Balance          sql.NullFloat64
	Currency         string
	HistoryCursor    int64
	HistoryCursorMSC int64
	UpdatedAt        string
}

type masterPosition struct {
	Ticket     string  `json:"ticket"`
	Symbol     string  `json:"symbol"`
	Action     string  `json:"action"`
	Lots       float64 `json:"lots"`
	EntryPrice float64 `json:"entry_price"`
	StopLoss   float64 `json:"sl"`
	TakeProfit float64 `json:"tp"`
	OpenedAt   int64   `json:"opened_at"`
}

type masterSnapshot struct {
	Active       bool             `json:"active"`
	AccountLogin string           `json:"account_login"`
	Server       string           `json:"server"`
	UpdatedAt    int64            `json:"updated_at"`
	Positions    []masterPosition `json:"positions"`
}

type managedBridgeRequest struct {
	ID     uint64 `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Token  string `json:"token"`
	Login  string `json:"login"`
	Server string `json:"server"`
	Body   string `json:"body"`
}

type followerStateReport struct {
	Balance              float64              `json:"balance"`
	Equity               float64              `json:"equity"`
	Currency             string               `json:"currency"`
	TerminalTradeAllowed bool                 `json:"terminal_trade_allowed"`
	ExpertTradeAllowed   bool                 `json:"expert_trade_allowed"`
	Deals                []followerDealCursor `json:"deals"`
}

type followerDealCursor struct {
	Ticket  string `json:"deal_ticket"`
	TimeMSC int64  `json:"time_msc"`
}

type managedTerminal struct {
	key        string
	accountDir string
	cmd        *exec.Cmd
	done       chan error
	exited     chan struct{}
	logFile    *os.File
}

type copyManager struct {
	db          *sql.DB
	probe       *terminalProbe
	root        string
	commonDir   string
	signalEAKey string
	serverURL   string

	mu        sync.Mutex
	connectMu sync.Mutex
	clients   map[int64]copyClientRecord
	sessions  map[string]*managedTerminal
}

func newCopyManager(db *sql.DB, sourceProbe *terminalProbe) (*copyManager, error) {
	dataDir := filepath.Dir(sourceProbe.runtimeRoot)
	root := filepath.Join(dataDir, "copy-trading")
	commonDir, err := mt5CommonFilesDirectory()
	if err != nil {
		return nil, fmt.Errorf("folder file bersama MT5 tidak ditemukan: %w", err)
	}
	if err := os.MkdirAll(commonDir, 0o700); err != nil {
		return nil, fmt.Errorf("folder file bersama MT5 tidak dapat dibuat: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o700); err != nil {
		return nil, fmt.Errorf("folder log copy trading tidak dapat dibuat: %w", err)
	}
	signalEAKey, err := randomCopyToken()
	if err != nil {
		return nil, fmt.Errorf("kunci EA signal tidak dapat dibuat: %w", err)
	}
	manager := &copyManager{
		db: db,
		probe: &terminalProbe{
			terminalPath: sourceProbe.terminalPath,
			runtimeRoot:  filepath.Join(root, "terminals"),
		},
		root: root, commonDir: commonDir, signalEAKey: signalEAKey,
		clients: map[int64]copyClientRecord{}, sessions: map[string]*managedTerminal{},
	}
	// Copy permission is per process session. A restarted local server begins
	// paused so a stale terminal cannot resume live orders without a fresh click.
	_, err = db.Exec(`UPDATE copy_clients SET copy_enabled=0, status='paused', message='Aplikasi dimulai ulang; periksa akun lalu aktifkan kembali copy.', updated_at=?`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	if err := manager.refreshClients(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *copyManager) run(ctx context.Context) {
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			clients := make([]copyClientRecord, 0, len(m.clients))
			for _, client := range m.clients {
				clients = append(clients, client)
			}
			m.mu.Unlock()
			for _, client := range clients {
				m.serveBridgeRequest(client)
			}
		}
	}
}

func (m *copyManager) shutdown() {
	_, _ = m.db.Exec(`UPDATE copy_clients SET copy_enabled=0, status='paused', message='Aplikasi dihentikan; copy trading dijeda.', updated_at=?`, time.Now().UTC().Format(time.RFC3339Nano))
	_, _ = m.db.Exec(`UPDATE copy_master SET status='stopped', message='Aplikasi dihentikan.', updated_at=? WHERE id=1`, time.Now().UTC().Format(time.RFC3339Nano))
	m.mu.Lock()
	sessions := make([]*managedTerminal, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = map[string]*managedTerminal{}
	m.mu.Unlock()
	for _, session := range sessions {
		m.stopManagedTerminal(session)
	}
	killTerminalsIn(filepath.Join(m.probe.runtimeRoot))
}

func (m *copyManager) refreshClients() error {
	rows, err := m.db.Query(`SELECT id, login, server, status, message, account_token, lot_mode, ratio_lot, lot_tetap, max_lot_per_order, max_lot_total, max_open_positions, copy_enabled, dry_run, copy_after, last_seen_at, balance, currency, history_cursor, history_cursor_msc, updated_at FROM copy_clients`)
	if err != nil {
		return err
	}
	defer rows.Close()
	clients := map[int64]copyClientRecord{}
	for rows.Next() {
		var client copyClientRecord
		var enabled, dryRun int
		if err := rows.Scan(&client.ID, &client.Login, &client.Server, &client.Status, &client.Message, &client.AccountToken, &client.LotMode, &client.RatioLot, &client.FixedLot, &client.MaxLotPerOrder, &client.MaxLotTotal, &client.MaxOpenPositions, &enabled, &dryRun, &client.CopyAfter, &client.LastSeenAt, &client.Balance, &client.Currency, &client.HistoryCursor, &client.HistoryCursorMSC, &client.UpdatedAt); err != nil {
			return err
		}
		client.CopyEnabled, client.DryRun = enabled != 0, dryRun != 0
		clients[client.ID] = client
	}
	if err := rows.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.clients = clients
	m.mu.Unlock()
	return nil
}

func (m *copyManager) client(id int64) (copyClientRecord, error) {
	var client copyClientRecord
	var enabled, dryRun int
	err := m.db.QueryRow(`SELECT id, login, server, status, message, account_token, lot_mode, ratio_lot, lot_tetap, max_lot_per_order, max_lot_total, max_open_positions, copy_enabled, dry_run, copy_after, last_seen_at, balance, currency, history_cursor, history_cursor_msc, updated_at FROM copy_clients WHERE id=?`, id).Scan(
		&client.ID, &client.Login, &client.Server, &client.Status, &client.Message, &client.AccountToken, &client.LotMode, &client.RatioLot, &client.FixedLot, &client.MaxLotPerOrder, &client.MaxLotTotal, &client.MaxOpenPositions, &enabled, &dryRun, &client.CopyAfter, &client.LastSeenAt, &client.Balance, &client.Currency, &client.HistoryCursor, &client.HistoryCursorMSC, &client.UpdatedAt,
	)
	if err != nil {
		return client, err
	}
	client.CopyEnabled, client.DryRun = enabled != 0, dryRun != 0
	return client, nil
}

func (m *copyManager) saveClientStatus(id int64, status, message string) error {
	_, err := m.db.Exec(`UPDATE copy_clients SET status=?, message=?, updated_at=? WHERE id=?`, status, message, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (m *copyManager) connectMaster(ctx context.Context, creds credentials) (probeOutcome, error) {
	m.connectMu.Lock()
	defer m.connectMu.Unlock()
	if runtime.GOOS != "windows" {
		return probeOutcome{}, errors.New("copy trading MT5 hanya dapat dijalankan pada Windows")
	}
	if err := m.rejectDuplicateMasterClient(creds, true); err != nil {
		return probeOutcome{}, err
	}
	outcome, err := m.probe.Check(ctx, creds)
	if err != nil || !outcome.Valid {
		return outcome, err
	}
	terminalDir, sourceDir, err := m.probe.runtimeDirectory(creds)
	if err != nil {
		return outcome, err
	}
	changeAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = m.db.Exec(`UPDATE telegram_signal_settings SET live_enabled=0,updated_at=? WHERE id=1`, changeAt)
	_, _ = m.db.Exec(`UPDATE telegram_signals SET status='DRY_RUN',reason='Akun utama MT5 sedang disambungkan; signal lama tidak otomatis dikirim.' WHERE status IN ('PENDING','CLAIMED')`)
	m.stopSession("master")
	var oldLogin, oldServer string
	if queryErr := m.db.QueryRow(`SELECT login, server FROM copy_master WHERE id=1`).Scan(&oldLogin, &oldServer); queryErr == nil &&
		(oldLogin != strconv.FormatInt(creds.Login, 10) || !strings.EqualFold(oldServer, creds.Server)) {
		if oldCredsLogin, parseErr := strconv.ParseInt(oldLogin, 10, 64); parseErr == nil && oldCredsLogin > 0 {
			oldDir, _, dirErr := m.probe.runtimeDirectory(credentials{Login: oldCredsLogin, Server: oldServer})
			if dirErr == nil {
				killTerminalsIn(oldDir)
			}
		}
	}
	killTerminalsIn(terminalDir)
	connectedAt := time.Now().UTC()
	if _, err := m.db.Exec(`INSERT INTO copy_master(id, login, server, status, message, symbol, connected_at, updated_at) VALUES(1, ?, ?, 'starting', '', ?, ?, ?) ON CONFLICT(id) DO UPDATE SET login=excluded.login, server=excluded.server, status='starting', message='', symbol=excluded.symbol, connected_at=excluded.connected_at, updated_at=excluded.updated_at`, strconv.FormatInt(creds.Login, 10), creds.Server, outcome.Symbol, connectedAt.Format(time.RFC3339Nano), connectedAt.Format(time.RFC3339Nano)); err != nil {
		return outcome, err
	}
	// A different master must never inherit a follower's live-copy permission.
	_, _ = m.db.Exec(`UPDATE copy_clients SET copy_enabled=0, copy_after=?, status='paused', message='Akun utama berubah; aktifkan kembali client setelah memeriksa sumber sinyal.', updated_at=?`, connectedAt.Unix(), connectedAt.Format(time.RFC3339Nano))
	_ = m.refreshClients()
	if err := m.startMasterTerminal(creds, terminalDir, sourceDir, outcome.Symbol); err != nil {
		_ = m.saveMasterStatus("error", err.Error())
		return outcome, err
	}
	if err := m.waitForMaster(ctx, creds, connectedAt); err != nil {
		// Keep a launched terminal open after a readiness timeout so the admin
		// can configure MT5's manual WebRequest allow-list. A canceled request
		// still tears it down to avoid leaving an unmanaged terminal behind.
		if ctx.Err() != nil {
			m.stopSession("master")
		}
		_ = m.saveMasterStatus("error", err.Error())
		return outcome, err
	}
	_ = m.saveMasterStatus("connected", "Akun utama siap menerbitkan posisi MT5.")
	return outcome, nil
}

func (m *copyManager) connectClient(ctx context.Context, creds credentials, settings copyClientSettings) (copyClientRecord, probeOutcome, error) {
	m.connectMu.Lock()
	defer m.connectMu.Unlock()
	if runtime.GOOS != "windows" {
		return copyClientRecord{}, probeOutcome{}, errors.New("copy trading MT5 hanya dapat dijalankan pada Windows")
	}
	settings, err := normalizeCopyClientSettings(settings)
	if err != nil {
		return copyClientRecord{}, probeOutcome{}, err
	}
	if err := m.rejectDuplicateMasterClient(creds, false); err != nil {
		return copyClientRecord{}, probeOutcome{}, err
	}
	outcome, err := m.probe.Check(ctx, creds)
	if err != nil || !outcome.Valid {
		return copyClientRecord{}, outcome, err
	}
	var client copyClientRecord
	err = m.db.QueryRow(`SELECT id, account_token FROM copy_clients WHERE login=? AND lower(server)=lower(?)`, strconv.FormatInt(creds.Login, 10), creds.Server).Scan(&client.ID, &client.AccountToken)
	if errors.Is(err, sql.ErrNoRows) {
		client.AccountToken, err = randomCopyToken()
		if err == nil {
			err = m.db.QueryRow(`INSERT INTO copy_clients(login, server, status, message, account_token, lot_mode, ratio_lot, lot_tetap, max_lot_per_order, max_lot_total, max_open_positions, copy_enabled, dry_run, copy_after, last_seen_at, updated_at) VALUES(?, ?, 'starting', '', ?, ?, ?, ?, ?, ?, ?, 0, 1, ?, '', ?) RETURNING id`, strconv.FormatInt(creds.Login, 10), creds.Server, client.AccountToken, settings.LotMode, settings.RatioLot, settings.FixedLot, settings.MaxLotPerOrder, settings.MaxLotTotal, settings.MaxOpenPositions, time.Now().Unix(), time.Now().UTC().Format(time.RFC3339Nano)).Scan(&client.ID)
		}
	} else if err == nil {
		_, err = m.db.Exec(`UPDATE copy_clients SET status='starting', message='', lot_mode=?, ratio_lot=?, lot_tetap=?, max_lot_per_order=?, max_lot_total=?, max_open_positions=?, copy_enabled=0, dry_run=1, copy_after=?, last_seen_at='', updated_at=? WHERE id=?`, settings.LotMode, settings.RatioLot, settings.FixedLot, settings.MaxLotPerOrder, settings.MaxLotTotal, settings.MaxOpenPositions, time.Now().Unix(), time.Now().UTC().Format(time.RFC3339Nano), client.ID)
	}
	if err != nil {
		return client, outcome, err
	}
	client, err = m.client(client.ID)
	if err != nil {
		return client, outcome, err
	}
	_ = m.refreshClients()
	terminalDir, sourceDir, err := m.probe.runtimeDirectory(creds)
	if err != nil {
		return client, outcome, err
	}
	m.stopSession(clientSessionKey(client.ID))
	killTerminalsIn(terminalDir)
	startedAt := time.Now().UTC()
	if err := m.startClientTerminal(creds, client, terminalDir, sourceDir, outcome.Symbol); err != nil {
		_ = m.saveClientStatus(client.ID, "error", err.Error())
		return client, outcome, err
	}
	if err := m.waitForClient(ctx, client.ID, startedAt); err != nil {
		m.stopSession(clientSessionKey(client.ID))
		_ = m.saveClientStatus(client.ID, "error", err.Error())
		return client, outcome, err
	}
	_ = m.saveClientStatus(client.ID, "standby", "Akun client tersambung dan menunggu izin copy.")
	client, _ = m.client(client.ID)
	return client, outcome, nil
}

func (m *copyManager) rejectDuplicateMasterClient(creds credentials, asMaster bool) error {
	login := strconv.FormatInt(creds.Login, 10)
	if !asMaster {
		var masterLogin, masterServer string
		err := m.db.QueryRow(`SELECT login, server FROM copy_master WHERE id=1`).Scan(&masterLogin, &masterServer)
		if err == nil && masterLogin == login && strings.EqualFold(masterServer, creds.Server) {
			return errors.New("akun yang sama tidak dapat digunakan sebagai akun utama dan akun client")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	var clientID int64
	err := m.db.QueryRow(`SELECT id FROM copy_clients WHERE login=? AND lower(server)=lower(?)`, login, creds.Server).Scan(&clientID)
	if err == nil {
		if asMaster {
			return errors.New("akun yang sama tidak dapat digunakan sebagai akun utama dan akun client")
		}
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !asMaster {
		var clientCount int
		if err := m.db.QueryRow(`SELECT COUNT(*) FROM copy_clients`).Scan(&clientCount); err != nil {
			return err
		}
		if clientCount >= 50 {
			return errors.New("batas 50 akun client sudah tercapai")
		}
	}
	return nil
}

func (m *copyManager) startMasterTerminal(creds credentials, terminalDir, sourceDir, symbol string) error {
	return m.startManagedTerminal("master", 0, creds, terminalDir, sourceDir, symbol, telegramSignalBinary)
}

func (m *copyManager) startClientTerminal(creds credentials, client copyClientRecord, terminalDir, sourceDir, symbol string) error {
	return m.startManagedTerminal(clientSessionKey(client.ID), client.ID, creds, terminalDir, sourceDir, symbol, followerCopyBinary)
}

func (m *copyManager) startManagedTerminal(key string, clientID int64, creds credentials, terminalDir, sourceDir, symbol string, binary []byte) error {
	if len(binary) == 0 {
		return errors.New("binary Expert copy trading belum tersedia; compile Expert MT5 terlebih dahulu")
	}
	terminal, err := m.probe.preparePortableTerminal(creds, terminalDir, sourceDir)
	if err != nil {
		return err
	}
	if err := enableAutoTrading(terminalDir); err != nil {
		return errors.New("izin Expert MT5 tidak dapat disiapkan")
	}
	name := "TelegramSignalEA"
	if clientID > 0 {
		name = "MT5FollowerCopyEA"
	}
	expertPath := filepath.Join(terminalDir, "MQL5", "Experts", "Algentra", name+".ex5")
	if err := os.MkdirAll(filepath.Dir(expertPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(expertPath, binary, 0o600); err != nil {
		return errors.New("binary Expert copy trading tidak dapat dipasang ke terminal")
	}
	presetName := strings.ToLower(name) + "-managed.set"
	presetPath := filepath.Join(terminalDir, "MQL5", "Presets", presetName)
	if err := os.MkdirAll(filepath.Dir(presetPath), 0o700); err != nil {
		return err
	}
	var preset string
	if clientID == 0 {
		suffix := marketWatchBrokerSuffix(terminalDir, creds.Login, symbol)
		serverURL := strings.TrimSpace(m.serverURL)
		if serverURL == "" {
			serverURL = "http://127.0.0.1:8080"
		}
		preset = "ServerURL=" + serverURL + "\r\n" +
			"ApiKey=" + m.signalEAKey + "\r\n" +
			"PollIntervalMs=1000||1000||250||5000||N\r\n" +
			"MagicNumber=26093001\r\n" +
			"LotMode=0\r\n" +
			"FixedLot=0.01\r\n" +
			"RiskPercent=1.0\r\n" +
			"MaxSpreadPoints=80\r\n" +
			"AllowBuy=true\r\n" +
			"AllowSell=true\r\n" +
			"UseDefaultSLTP=false\r\n" +
			"SymbolSuffix=" + suffix + "\r\n" +
			"MarketWatchlistCsv=XAUUSD,EURUSD,USDJPY,GBPUSD\r\n" +
			"MaxMarketDeviationPct=5.0\r\n" +
			"MaxRetries=3\r\n"
	} else {
		client, err := m.client(clientID)
		if err != nil {
			return err
		}
		preset = clientPreset(client)
	}
	if err := os.WriteFile(presetPath, []byte(preset), 0o600); err != nil {
		return errors.New("preset Expert copy trading tidak dapat ditulis")
	}
	if err := os.Chmod(presetPath, 0o600); err != nil {
		return errors.New("izin file preset Expert copy trading tidak dapat dibatasi")
	}
	accessPoint, err := m.startupAccessPoint(creds, terminalDir, sourceDir)
	if err != nil {
		return err
	}
	configDir, err := os.MkdirTemp("", "mt5-copy-start-")
	if err != nil {
		return errors.New("folder sementara startup MT5 tidak dapat dibuat")
	}
	_ = os.Chmod(configDir, 0o700)
	startupPath := filepath.Join(configDir, "copy-startup.ini")
	started := false
	defer func() {
		if !started {
			_ = os.RemoveAll(configDir)
		}
	}()
	if err := writeCopyStartup(startupPath, creds, accessPoint, name, presetName, symbol); err != nil {
		return err
	}
	cmd := exec.Command(terminal, "/portable", "/config:"+startupPath)
	cmd.Dir = terminalDir
	cmd.Env = filteredTerminalEnvironment(os.Environ())
	prepareChild(cmd)
	logName := "telegram-signal-ea.log"
	if clientID > 0 {
		logName = fmt.Sprintf("client-%d.log", clientID)
	}
	logFile, err := os.OpenFile(filepath.Join(m.root, "logs", logName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("file log MT5 copy trading tidak dapat dibuka")
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return errors.New("terminal MT5 copy trading tidak dapat dijalankan")
	}
	started = true
	session := &managedTerminal{key: key, accountDir: terminalDir, cmd: cmd, done: make(chan error, 1), exited: make(chan struct{}), logFile: logFile}
	m.mu.Lock()
	m.sessions[key] = session
	m.mu.Unlock()
	go func() {
		waitErr := cmd.Wait()
		session.done <- waitErr
		close(session.exited)
		m.mu.Lock()
		if m.sessions[key] == session {
			delete(m.sessions, key)
		}
		m.mu.Unlock()
		if clientID == 0 {
			_ = m.saveMasterStatus("stopped", "Terminal akun utama berhenti.")
		} else {
			_ = m.saveClientStatus(clientID, "offline", "Terminal akun client berhenti; sambungkan ulang akun.")
		}
		if logFile != nil {
			_ = logFile.Close()
		}
	}()
	// MT5 consumes its startup INI during launch. It contains the broker password,
	// so remove it as soon as the terminal has read the configuration.
	time.AfterFunc(5*time.Second, func() { _ = os.RemoveAll(configDir) })
	return nil
}

func clientPreset(client copyClientRecord) string {
	name := func(key, value string) string { return key + "=" + value + "\r\n" }
	setting := func(key string, value float64) string {
		formatted := strconv.FormatFloat(value, 'f', 8, 64)
		return key + "=" + formatted + "||" + formatted + "||0.01||100.0||N\r\n"
	}
	return name("ServerURL", "http://127.0.0.1:8080") +
		name("AccountToken", client.AccountToken) +
		"PollIntervalMs=500||500||100||5000||N\r\n" +
		"MagicNumber=880001||880001||1||880001||N\r\n" +
		"VolumeMultiplier=1.0||1.0||0.1||10.0||N\r\n" +
		"MaxEntryDeviationPercent=0.0||0.0||0.1||10.0||N\r\n" +
		"MaxSignalAgeSeconds=3.0||3.0||0.1||60.0||N\r\n" +
		"MaxSlippagePoints=20||20||1||100||N\r\n" +
		"SymbolSuffix=\r\nSymbolMapCsv=\r\n" +
		"ManagedMode=true||true||false||true||N\r\n" +
		"ManagedFileBridge=true||true||false||true||N\r\n" +
		name("BridgeChannel", strconv.FormatInt(client.ID, 10)) +
		"DryRun=" + strconv.FormatBool(client.DryRun) + "||" + strconv.FormatBool(client.DryRun) + "||false||true||N\r\n" +
		name("LotMode", client.LotMode) +
		setting("FixedLot", client.FixedLot) +
		setting("RatioLot", client.RatioLot) +
		"MasterBalance=10000.0||10000.0||100.0||1000000.0||N\r\n" +
		setting("MaxLotPerOrder", client.MaxLotPerOrder) +
		setting("MaxLotTotal", client.MaxLotTotal) +
		fmt.Sprintf("MaxOpenPositions=%d||%d||1||1000||N\r\n", client.MaxOpenPositions, client.MaxOpenPositions) +
		"ReverseDirection=false||false||false||true||N\r\n"
}

func writeCopyStartup(path string, creds credentials, server, expert, preset, symbol string) error {
	symbol = strings.TrimSpace(symbol)
	for _, value := range []string{server, creds.Password, symbol} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("server, simbol, atau password mengandung karakter yang tidak valid")
		}
	}
	if symbol == "" {
		return errors.New("simbol broker belum terdeteksi; terminal copy trading tidak dijalankan")
	}
	lines := []string{
		"[Common]",
		"Login=" + strconv.FormatInt(creds.Login, 10),
		"Server=" + server,
	}
	if creds.Password != "" {
		lines = append(lines, "Password="+creds.Password)
	}
	lines = append(lines,
		"KeepPrivate=1", "NewsEnable=0", "ProxyEnable=0", "",
		"[Experts]", "AllowLiveTrading=1", "AllowDllImport=0", "Enabled=1", "Account=0", "Profile=0", "",
		"[StartUp]", `Expert=Algentra\`+expert, "ExpertParameters="+preset, "Symbol="+symbol, "Period=M1", "",
	)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\r\n")), 0o600); err != nil {
		return errors.New("konfigurasi startup terminal copy trading tidak dapat ditulis")
	}
	return nil
}

func (m *copyManager) startupAccessPoint(creds credentials, terminalDir, sourceDir string) (string, error) {
	if accessPoint, err := findBrokerAccessPoint(creds.Server, creds.Company); err == nil {
		return accessPoint, nil
	}
	if err := seedBrokerServers(terminalDir, sourceDir, m.probe.terminalPath); err != nil {
		return "", errors.New("server broker tidak tersedia di direktori atau profil MT5 lokal")
	}
	return creds.Server, nil
}

func (m *copyManager) waitForMaster(ctx context.Context, creds credentials, connectedAt time.Time) error {
	timer := time.NewTimer(copyProbeTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if snapshot, active := m.readActiveMasterSnapshot(creds.Login, creds.Server, connectedAt); active {
			_ = snapshot
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("akun utama sudah login tetapi EA Telegram belum mengirim snapshot. Pastikan AutoTrading aktif dan tambahkan http://127.0.0.1:8080 ke Tools > Options > Expert Advisors > Allow WebRequest")
		case <-ticker.C:
		}
	}
}

func (m *copyManager) waitForClient(ctx context.Context, id int64, startedAt time.Time) error {
	timer := time.NewTimer(copyProbeTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		client, err := m.client(id)
		if err == nil {
			seen, parseErr := time.Parse(time.RFC3339Nano, client.LastSeenAt)
			if parseErr == nil && !seen.Before(startedAt) && time.Since(seen) <= copyClientFreshness {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("Login Gagal. Coba sekali lagi. Jika masih belum terhubung, silakan periksa server dan kata sandi.")
		case <-ticker.C:
		}
	}
}

func (m *copyManager) readActiveMasterSnapshot(login int64, server string, connectedAt time.Time) (masterSnapshot, bool) {
	var status string
	if err := m.db.QueryRow(`SELECT status FROM copy_master WHERE id=1`).Scan(&status); err != nil || status == "paused" || status == "stopped" || status == "error" {
		return masterSnapshot{}, false
	}
	snapshot, err := m.readMasterSnapshot()
	if err != nil || !snapshot.Active || snapshot.AccountLogin != strconv.FormatInt(login, 10) || !strings.EqualFold(strings.TrimSpace(snapshot.Server), strings.TrimSpace(server)) {
		return snapshot, false
	}
	updated := time.Unix(snapshot.UpdatedAt, 0)
	now := time.Now()
	if snapshot.UpdatedAt <= 0 || snapshot.UpdatedAt < connectedAt.Unix()+1 || updated.After(now.Add(5*time.Second)) || now.Sub(updated) > copyMasterFreshness {
		return snapshot, false
	}
	return snapshot, true
}

func (m *copyManager) readMasterSnapshot() (masterSnapshot, error) {
	var snapshot masterSnapshot
	path := filepath.Join(m.commonDir, copyMasterSnapshotName)
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshot, err
	}
	if len(data) > 8<<20 {
		return snapshot, errors.New("snapshot akun utama terlalu besar")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return snapshot, errors.New("snapshot akun utama memiliki data tambahan")
	}
	if len(snapshot.Positions) > 5000 {
		return snapshot, errors.New("snapshot akun utama berisi terlalu banyak posisi")
	}
	for _, position := range snapshot.Positions {
		if position.Ticket == "" || position.Symbol == "" || (position.Action != "BUY" && position.Action != "SELL") ||
			!finiteNumber(position.Lots) || position.Lots <= 0 || !finiteNumber(position.EntryPrice) ||
			!finiteNumber(position.StopLoss) || !finiteNumber(position.TakeProfit) || position.OpenedAt <= 0 {
			return snapshot, errors.New("snapshot akun utama berisi posisi yang tidak valid")
		}
	}
	return snapshot, nil
}

func (m *copyManager) stopSession(key string) {
	m.mu.Lock()
	session := m.sessions[key]
	delete(m.sessions, key)
	m.mu.Unlock()
	if session != nil {
		m.stopManagedTerminal(session)
	}
}

func (m *copyManager) stopManagedTerminal(session *managedTerminal) {
	select {
	case <-session.exited:
	default:
		_ = stopChildProcess(session.cmd, session.done, 8*time.Second)
	}
	killTerminalsIn(session.accountDir)
	if session.logFile != nil {
		_ = session.logFile.Close()
	}
}

func clientSessionKey(id int64) string { return fmt.Sprintf("client:%d", id) }

func randomCopyToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func finiteNumber(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (m *copyManager) saveMasterStatus(status, message string) error {
	_, err := m.db.Exec(`UPDATE copy_master SET status=?, message=?, updated_at=? WHERE id=1`, status, message, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (m *copyManager) serveBridgeRequest(client copyClientRecord) {
	name := "algentra-managed-" + strconv.FormatInt(client.ID, 10)
	requestPath := filepath.Join(m.commonDir, name+".req")
	claimedPath := filepath.Join(m.commonDir, name+".processing")
	data, err := os.ReadFile(requestPath)
	if err != nil {
		return
	}
	if err := os.Rename(requestPath, claimedPath); err != nil {
		return
	}
	data, err = os.ReadFile(claimedPath)
	_ = os.Remove(claimedPath)
	if err != nil || len(data) > 2<<20 {
		return
	}
	var request managedBridgeRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return
	}
	status, body := m.processBridgeRequest(client.ID, request)
	responsePath := filepath.Join(m.commonDir, name+".res")
	if err := writeCopyBridgeResponse(responsePath, request.ID, status, body); err != nil {
		log.Printf("file bridge client %d gagal menulis response: %v", client.ID, err)
	}
}

func (m *copyManager) processBridgeRequest(id int64, request managedBridgeRequest) (int, []byte) {
	if request.ID == 0 || (request.Method != http.MethodGet && request.Method != http.MethodPost) ||
		(request.Path != "/api/mt5/follower/positions" && request.Path != "/api/mt5/follower/report") {
		return http.StatusBadRequest, []byte(`{"error":"Permintaan bridge tidak valid."}`)
	}
	client, err := m.client(id)
	if err != nil {
		return http.StatusUnauthorized, []byte(`{"error":"Akun client tidak ditemukan."}`)
	}
	provided, expected := []byte(request.Token), []byte(client.AccountToken)
	if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 ||
		request.Login != client.Login || !strings.EqualFold(strings.TrimSpace(request.Server), strings.TrimSpace(client.Server)) {
		return http.StatusUnauthorized, []byte(`{"error":"Akun MT5 tidak terotorisasi."}`)
	}
	now := time.Now().UTC()
	if request.Method == http.MethodGet && request.Path == "/api/mt5/follower/positions" {
		payload := m.positionsForClient(client, now)
		encoded, err := json.Marshal(payload)
		if err != nil {
			return http.StatusInternalServerError, []byte(`{"error":"Snapshot tidak dapat dibaca."}`)
		}
		_, _ = m.db.Exec(`UPDATE copy_clients SET last_seen_at=?, updated_at=? WHERE id=?`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), id)
		return http.StatusOK, encoded
	}
	if request.Method == http.MethodPost && request.Path == "/api/mt5/follower/report" {
		if len(request.Body) > 1<<20 || !json.Valid([]byte(request.Body)) {
			return http.StatusBadRequest, []byte(`{"error":"Laporan akun tidak valid."}`)
		}
		var report followerStateReport
		if err := json.Unmarshal([]byte(request.Body), &report); err != nil || !finiteNumber(report.Balance) || !finiteNumber(report.Equity) || report.Balance < 0 || report.Equity < 0 || len(report.Currency) > 12 {
			return http.StatusBadRequest, []byte(`{"error":"Laporan akun tidak valid."}`)
		}
		status := "standby"
		if client.CopyEnabled {
			status = "simulasi"
			if !client.DryRun {
				status = "copying"
			}
		}
		cursor, cursorMSC := client.HistoryCursor, client.HistoryCursorMSC
		for _, deal := range report.Deals {
			if ticket, parseErr := strconv.ParseInt(strings.TrimSpace(deal.Ticket), 10, 64); parseErr == nil && ticket > cursor {
				cursor = ticket
			}
			if deal.TimeMSC > cursorMSC {
				cursorMSC = deal.TimeMSC
			}
		}
		_, _ = m.db.Exec(`UPDATE copy_clients SET status=?, message='', last_seen_at=?, balance=?, currency=?, history_cursor=?, history_cursor_msc=?, updated_at=? WHERE id=?`, status, now.Format(time.RFC3339Nano), report.Balance, strings.TrimSpace(report.Currency), cursor, cursorMSC, now.Format(time.RFC3339Nano), id)
		response, _ := json.Marshal(map[string]int64{"history_cursor": cursor, "history_cursor_msc": cursorMSC})
		return http.StatusOK, response
	}
	return http.StatusMethodNotAllowed, []byte(`{"error":"Metode bridge tidak didukung."}`)
}

func (m *copyManager) positionsForClient(client copyClientRecord, now time.Time) map[string]any {
	positions := []masterPosition{}
	active := false
	var master copyMasterRecord
	err := m.db.QueryRow(`SELECT login, server, status, message, symbol, connected_at, updated_at FROM copy_master WHERE id=1`).Scan(&master.Login, &master.Server, &master.Status, &master.Message, &master.Symbol, &master.ConnectedAt, &master.UpdatedAt)
	if err == nil {
		connectedAt, parseErr := time.Parse(time.RFC3339Nano, master.ConnectedAt)
		if parseErr == nil {
			if snapshot, fresh := m.readActiveMasterSnapshot(parseLogin(master.Login), master.Server, connectedAt); fresh {
				positions = snapshot.Positions
				active = true
			}
		}
	}
	return map[string]any{
		"active":             active,
		"positions":          positions,
		"copy_enabled":       client.CopyEnabled,
		"dry_run":            client.DryRun,
		"lot_mode":           client.LotMode,
		"ratio_lot":          client.RatioLot,
		"fixed_lot":          client.FixedLot,
		"max_lot_per_order":  client.MaxLotPerOrder,
		"max_lot_total":      client.MaxLotTotal,
		"max_open_positions": client.MaxOpenPositions,
		"history_cursor":     client.HistoryCursor,
		"history_cursor_msc": client.HistoryCursorMSC,
		"copy_after":         client.CopyAfter,
		"updated_at":         now.Unix(),
	}
}

func parseLogin(value string) int64 {
	login, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return login
}

func writeCopyBridgeResponse(path string, id uint64, status int, body []byte) error {
	temp := fmt.Sprintf("%s.%d.tmp", path, id)
	content := append([]byte(fmt.Sprintf("%d %d\n", id, status)), body...)
	if err := os.WriteFile(temp, content, 0o600); err != nil {
		return err
	}
	_ = os.Remove(path)
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func (m *copyManager) pauseMaster() error {
	m.connectMu.Lock()
	defer m.connectMu.Unlock()
	_, err := m.db.Exec(`UPDATE copy_clients SET copy_enabled=0, status='paused', message='Copy trading dijeda karena akun utama dihentikan.', updated_at=?`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	pausedAt := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = m.db.Exec(`UPDATE telegram_signal_settings SET live_enabled=0,updated_at=? WHERE id=1`, pausedAt)
	_, _ = m.db.Exec(`UPDATE telegram_signals SET status='DRY_RUN',reason='Akun utama dijeda; signal lama tidak otomatis dikirim.' WHERE status IN ('PENDING','CLAIMED')`)
	_ = m.refreshClients()
	m.stopSession("master")
	if err := m.saveMasterStatus("paused", "Akun utama dijeda; client tidak menerima entry baru."); err != nil {
		return err
	}
	return nil
}

func (m *copyManager) setClientMode(ctx context.Context, id int64, enabled, dryRun bool) (copyClientRecord, error) {
	m.connectMu.Lock()
	defer m.connectMu.Unlock()
	if err := ctx.Err(); err != nil {
		return copyClientRecord{}, err
	}
	client, err := m.client(id)
	if err != nil {
		return client, errors.New("akun client tidak ditemukan")
	}
	if enabled {
		lastSeen, parseErr := time.Parse(time.RFC3339Nano, client.LastSeenAt)
		if parseErr != nil || time.Since(lastSeen) > copyClientFreshness || lastSeen.After(time.Now().Add(5*time.Second)) {
			return client, errors.New("terminal client offline; sambungkan ulang akun client sebelum memulai copy")
		}
		var master copyMasterRecord
		if err := m.db.QueryRow(`SELECT login, server, connected_at FROM copy_master WHERE id=1`).Scan(&master.Login, &master.Server, &master.ConnectedAt); err != nil {
			return client, errors.New("sambungkan akun utama sebelum mengaktifkan client")
		}
		masterConnectedAt, parseErr := time.Parse(time.RFC3339Nano, master.ConnectedAt)
		if parseErr != nil {
			return client, errors.New("status akun utama tidak valid")
		}
		if _, active := m.readActiveMasterSnapshot(parseLogin(master.Login), master.Server, masterConnectedAt); !active {
			return client, errors.New("akun utama belum menerbitkan snapshot aktif")
		}
	}
	copyAfter := client.CopyAfter
	if enabled {
		copyAfter = time.Now().Unix()
	}
	_, err = m.db.Exec(`UPDATE copy_clients SET copy_enabled=?, dry_run=?, copy_after=?, status=?, message='', updated_at=? WHERE id=?`, boolInt(enabled), boolInt(dryRun), copyAfter, clientModeStatus(enabled, dryRun), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return client, err
	}
	client, err = m.client(id)
	if err != nil {
		return client, err
	}
	_ = m.refreshClients()
	client, _ = m.client(id)
	return client, nil
}

func (m *copyManager) pauseClient(id int64) error {
	client, err := m.client(id)
	if err != nil {
		return errors.New("akun client tidak ditemukan")
	}
	_, err = m.db.Exec(`UPDATE copy_clients SET copy_enabled=0, status='paused', message='Copy trading dijeda; posisi yang sudah dibuka tetap dikelola manual.', updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	_ = m.refreshClients()
	_ = client
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func clientModeStatus(enabled, dryRun bool) string {
	if !enabled {
		return "paused"
	}
	if dryRun {
		return "simulasi"
	}
	return "copying"
}
