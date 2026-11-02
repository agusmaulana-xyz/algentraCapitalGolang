package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *app) handleAdminSignalsPage(w http.ResponseWriter, r *http.Request) {
	if a.requirePortalPage(w, r, "admin") {
		a.servePortalPage(w, "signal-management.html")
	}
}

func (a *app) handleSignalDashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", false); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	telegramStatus := a.telegram.status(ctx)
	groups, err := a.savedSignalSources(ctx)
	if err != nil {
		log.Printf("signal source list failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Daftar grup signal tidak dapat dibaca."})
		return
	}
	items, stats, winrate, err := a.signals.list(ctx, 50)
	if err != nil {
		log.Printf("signal dashboard query failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Riwayat signal tidak dapat dibaca."})
		return
	}
	live, err := a.signals.settings(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Pengaturan signal tidak dapat dibaca."})
		return
	}
	master, _ := a.copyMasterStatus()
	heartbeat, terminal, version, symbol, _ := a.signals.heartbeatAt(ctx)
	heartbeatFresh := !heartbeat.IsZero() && time.Since(heartbeat) <= 45*time.Second && !heartbeat.After(time.Now().Add(5*time.Second))
	writeJSON(w, http.StatusOK, map[string]any{
		"telegram": telegramStatus, "groups": groups,
		"gemini":       map[string]any{"configured": a.signals.parser.configured(), "model": a.geminiModel},
		"master":       master,
		"ea":           map[string]any{"connected": heartbeatFresh, "heartbeat_at": heartbeat, "terminal": terminal, "version": version, "symbol": symbol},
		"live_enabled": live, "signals": items, "stats": stats, "winrate": winrate,
		"webrequest_url": a.signalServerURL(),
	})
}

func (a *app) savedSignalSources(ctx context.Context) ([]map[string]any, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT chat_id,name,username,enabled,alias,updated_at FROM telegram_signal_sources ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, username, alias, updated string
		var enabled int
		if err := rows.Scan(&id, &name, &username, &enabled, &alias, &updated); err != nil {
			return nil, err
		}
		groups = append(groups, map[string]any{"chat_id": id, "name": name, "username": username, "enabled": enabled == 1, "alias": alias, "updated_at": updated})
	}
	return groups, rows.Err()
}

func (a *app) handleTelegramSendCode(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var request struct {
		Phone string `json:"phone"`
	}
	if !decodeLocalJSON(w, r, &request) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	result, err := a.telegram.sendCode(ctx, request.Phone)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleTelegramVerifyCode(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var request struct {
		Code string `json:"code"`
	}
	if !decodeLocalJSON(w, r, &request) {
		return
	}
	if len(strings.TrimSpace(request.Code)) < 4 || len(request.Code) > 16 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Kode Telegram tidak valid."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	result, err := a.telegram.verifyCode(ctx, request.Code)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleTelegramVerify2FA(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var request struct {
		Password string `json:"password"`
	}
	if !decodeLocalJSON(w, r, &request) {
		return
	}
	if len(request.Password) < 1 || len(request.Password) > 256 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Password 2FA tidak valid."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	result, err := a.telegram.verify2FA(ctx, request.Password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Verifikasi 2FA Telegram gagal. Periksa password lalu coba lagi."})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleTelegramLogout(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	if err := a.telegram.logout(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Sesi Telegram tidak dapat dikeluarkan."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

func (a *app) handleTelegramGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", false); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	groups, err := a.telegram.groups(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	saved, err := a.savedSignalSources(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Daftar grup tersimpan tidak dapat dibaca."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "saved": saved})
}

func (a *app) handleSaveTelegramGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var request struct {
		Groups []struct {
			ChatID  string `json:"chat_id"`
			Enabled bool   `json:"enabled"`
			Alias   string `json:"alias"`
		} `json:"groups"`
	}
	if !decodeLocalJSON(w, r, &request) {
		return
	}
	if len(request.Groups) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Pilih maksimal 100 grup."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	available, err := a.telegram.groups(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	byID := make(map[string]telegramDialogGroup, len(available))
	for _, group := range available {
		byID[group.ChatID] = group
	}
	transaction, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Grup tidak dapat disimpan."})
		return
	}
	defer transaction.Rollback()
	if _, err = transaction.ExecContext(ctx, `UPDATE telegram_signal_sources SET enabled=0,updated_at=?`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Grup tidak dapat disimpan."})
		return
	}
	for _, item := range request.Groups {
		group, ok := byID[item.ChatID]
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Salah satu grup tidak termasuk dialog akun Telegram ini."})
			return
		}
		alias := limitText(item.Alias, 80)
		enabled := 0
		if item.Enabled {
			enabled = 1
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO telegram_signal_sources(chat_id,name,username,enabled,alias,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(chat_id) DO UPDATE SET name=excluded.name,username=excluded.username,enabled=excluded.enabled,alias=excluded.alias,updated_at=excluded.updated_at`, group.ChatID, limitText(group.Name, 160), limitText(group.Username, 80), enabled, alias, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Grup tidak dapat disimpan."})
			return
		}
	}
	if err = transaction.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Grup tidak dapat disimpan."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (a *app) handleSignalLive(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var request struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeLocalJSON(w, r, &request) {
		return
	}
	if request.Enabled {
		if !a.signals.parser.configured() {
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "Konfigurasikan GEMINI_API_KEY terlebih dahulu."})
			return
		}
		status := a.telegram.status(r.Context())
		if status["connected"] != true {
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "Login akun Telegram terlebih dahulu."})
			return
		}
		var sources int
		if err := a.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM telegram_signal_sources WHERE enabled=1`).Scan(&sources); err != nil || sources == 0 {
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "Aktifkan minimal satu grup sumber signal."})
			return
		}
		master, err := a.copyMasterStatus()
		if err != nil || master == nil || !master.Connected || master.Status != "connected" {
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "Hubungkan akun utama MT5 terlebih dahulu."})
			return
		}
		heartbeat, _, _, _, err := a.signals.heartbeatAt(r.Context())
		if err != nil || heartbeat.IsZero() || time.Since(heartbeat) > 45*time.Second {
			writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "EA Telegram di terminal MT5 belum mengirim heartbeat."})
			return
		}
	}
	if err := a.signals.setLive(r.Context(), request.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Mode signal tidak dapat diubah."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"live_enabled": request.Enabled})
}

func (a *app) handleSignalParseTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var request struct {
		Message string `json:"message"`
	}
	if !decodeLocalJSON(w, r, &request) {
		return
	}
	parsed, err := a.signals.parser.parse(r.Context(), request.Message)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signal": parsed, "queued": false})
}

func (a *app) signalServerURL() string {
	if a.copier.serverURL != "" {
		return a.copier.serverURL
	}
	return "http://" + a.config.Address
}

func (a *app) authorizeSignalEA(w http.ResponseWriter, r *http.Request) bool {
	if !requestIsLocal(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses EA hanya diizinkan dari komputer lokal."})
		return false
	}
	provided, expected := []byte(r.Header.Get("X-API-Key")), []byte(a.signalEAKey)
	if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "API key EA tidak valid."})
		return false
	}
	return true
}

func (a *app) handleSignalMasterSnapshot(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSignalEA(w, r) {
		return
	}
	var request struct {
		Login     string           `json:"terminal_login"`
		Server    string           `json:"terminal_server"`
		Positions []masterPosition `json:"positions"`
		Balance   float64          `json:"balance"`
		Equity    float64          `json:"equity"`
		Floating  float64          `json:"floating_profit"`
		Currency  string           `json:"currency"`
		TradeMode string           `json:"trade_mode"`
		Quotes    json.RawMessage  `json:"market_quotes"`
	}
	if err := decodeSignalSnapshotJSON(w, r, &request); err != nil {
		return
	}
	if len(request.Positions) > 500 || strings.TrimSpace(request.Login) == "" || len(request.Server) > 120 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Snapshot master tidak valid."})
		return
	}
	var masterLogin, masterServer, masterStatus, connectedRaw string
	if err := a.db.QueryRow(`SELECT login,server,status,connected_at FROM copy_master WHERE id=1`).Scan(&masterLogin, &masterServer, &masterStatus, &connectedRaw); err != nil || masterStatus == "paused" || masterStatus == "stopped" || masterStatus == "error" || request.Login != masterLogin || !strings.EqualFold(strings.TrimSpace(request.Server), strings.TrimSpace(masterServer)) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "EA tidak terhubung ke akun master aktif."})
		return
	}
	connectedAt, _ := time.Parse(time.RFC3339Nano, connectedRaw)
	old := map[string]int64{}
	if previous, err := a.copier.readMasterSnapshot(); err == nil {
		for _, p := range previous.Positions {
			old[p.Ticket] = p.OpenedAt
		}
	}
	now := time.Now().UTC()
	for i := range request.Positions {
		p := &request.Positions[i]
		if p.Ticket == "" || len(p.Ticket) > 32 || len(p.Symbol) > 32 || (p.Action != "BUY" && p.Action != "SELL") || !finiteNumber(p.Lots) || p.Lots <= 0 || !finiteNumber(p.EntryPrice) || !finiteNumber(p.StopLoss) || !finiteNumber(p.TakeProfit) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Snapshot berisi posisi tidak valid."})
			return
		}
		if p.OpenedAt <= 0 {
			p.OpenedAt = old[p.Ticket]
			if p.OpenedAt <= 0 {
				p.OpenedAt = now.Unix()
			}
		}
	}
	updated := now.Unix()
	if updated < connectedAt.Unix()+1 {
		updated = connectedAt.Unix() + 1
	}
	snapshot := masterSnapshot{Active: true, AccountLogin: masterLogin, Server: masterServer, UpdatedAt: updated, Positions: request.Positions}
	data, err := json.Marshal(snapshot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Snapshot tidak dapat disimpan."})
		return
	}
	path := filepath.Join(a.copier.commonDir, copyMasterSnapshotName)
	temp, err := os.CreateTemp(a.copier.commonDir, "copy-master-*.tmp")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Snapshot tidak dapat disimpan."})
		return
	}
	name := temp.Name()
	defer os.Remove(name)
	_ = temp.Chmod(0o600)
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Snapshot tidak dapat disimpan."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": true, "updated_at": updated})
}

func (a *app) handleSignalPending(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSignalEA(w, r) {
		return
	}
	live, err := a.signals.settings(r.Context())
	if err != nil || !live {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	heartbeat, _, _, _, err := a.signals.heartbeatAt(r.Context())
	if err != nil || heartbeat.IsZero() || time.Since(heartbeat) > 90*time.Second || heartbeat.After(time.Now().Add(5*time.Second)) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Heartbeat EA kedaluwarsa."})
		return
	}
	master, masterErr := a.copyMasterStatus()
	if masterErr != nil || master == nil || !master.Connected || master.Status != "connected" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Akun utama MT5 belum siap."})
		return
	}
	_, _ = a.db.ExecContext(r.Context(), `UPDATE telegram_signals SET status='EXPIRED',reason='Sinyal kedaluwarsa setelah satu jam.' WHERE status IN ('PENDING','CLAIMED') AND expires_at>0 AND expires_at<?`, time.Now().Unix())
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Antrean signal tidak tersedia."})
		return
	}
	defer tx.Rollback()
	var item struct {
		ID             int64
		Action, Symbol string
		Low, High, SL  float64
		TP             string
		Created        string
	}
	err = tx.QueryRowContext(r.Context(), `SELECT id,action,symbol,entry_low,entry_high,sl,tp_json,telegram_created_at FROM telegram_signals WHERE status='PENDING' OR (status='CLAIMED' AND claimed_at<?) ORDER BY id LIMIT 1`, time.Now().Add(-90*time.Second).Unix()).Scan(&item.ID, &item.Action, &item.Symbol, &item.Low, &item.High, &item.SL, &item.TP, &item.Created)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Antrean signal tidak dapat dibaca."})
		return
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(r.Context(), `UPDATE telegram_signals SET status='CLAIMED',claimed_at=? WHERE id=?`, now.Unix(), item.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Signal tidak dapat diklaim."})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Signal tidak dapat diklaim."})
		return
	}
	created, _ := time.Parse(time.RFC3339Nano, item.Created)
	var targets []float64
	_ = json.Unmarshal([]byte(item.TP), &targets)
	writeJSON(w, http.StatusOK, map[string]any{"items": []any{map[string]any{"signal_id": item.ID, "action": item.Action, "order_type": "AUTO", "symbol": item.Symbol, "entry_low": item.Low, "entry_high": item.High, "sl": item.SL, "tp": targets, "entry_mode": "PARTIAL", "created_epoch": created.Unix()}}})
}

func (a *app) handleSignalHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSignalEA(w, r) {
		return
	}
	var request struct {
		Active   bool   `json:"active"`
		Terminal string `json:"terminal"`
		Version  string `json:"version"`
		Symbol   string `json:"symbol"`
	}
	if !decodeSignalBody(w, r, &request) {
		return
	}
	if err := a.signals.heartbeat(r.Context(), request.Terminal, request.Version, request.Symbol, request.Active); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleSignalExecutionReport(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSignalEA(w, r) {
		return
	}
	var report signalExecutionReport
	if !decodeSignalBody(w, r, &report) {
		return
	}
	if err := a.signals.recordExecution(r.Context(), report); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleSignalResult(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeSignalEA(w, r) {
		return
	}
	var report signalResultReport
	if !decodeSignalBody(w, r, &report) {
		return
	}
	if err := a.signals.recordResult(r.Context(), report); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func decodeSignalBody(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Isi permintaan tidak valid."})
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Permintaan memiliki data tambahan."})
		return false
	}
	return true
}

func decodeSignalSnapshotJSON(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Snapshot master tidak valid."})
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Snapshot memiliki data tambahan."})
		return err
	}
	return nil
}
