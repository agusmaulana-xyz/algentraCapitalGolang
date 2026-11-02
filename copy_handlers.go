package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type copyLoginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	Server   string `json:"server"`
	Company  string `json:"company"`
}

type copyClientSettings struct {
	LotMode          string  `json:"lot_mode"`
	RatioLot         float64 `json:"ratio_lot"`
	FixedLot         float64 `json:"fixed_lot"`
	MaxLotPerOrder   float64 `json:"max_lot_per_order"`
	MaxLotTotal      float64 `json:"max_lot_total"`
	MaxOpenPositions int     `json:"max_open_positions"`
}

type copyClientLoginRequest struct {
	Login            string  `json:"login"`
	Password         string  `json:"password"`
	Server           string  `json:"server"`
	Company          string  `json:"company"`
	LotMode          string  `json:"lot_mode"`
	RatioLot         float64 `json:"ratio_lot"`
	FixedLot         float64 `json:"fixed_lot"`
	MaxLotPerOrder   float64 `json:"max_lot_per_order"`
	MaxLotTotal      float64 `json:"max_lot_total"`
	MaxOpenPositions int     `json:"max_open_positions"`
}

type copyClientModeRequest struct {
	Enabled bool `json:"enabled"`
	DryRun  bool `json:"dry_run"`
}

type copyMasterView struct {
	Login     string `json:"login"`
	Server    string `json:"server"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	Symbol    string `json:"symbol,omitempty"`
	UpdatedAt string `json:"updated_at"`
	Positions int    `json:"positions"`
	Connected bool   `json:"connected"`
}

type copyClientView struct {
	ID               int64    `json:"id"`
	Login            string   `json:"login"`
	Server           string   `json:"server"`
	Status           string   `json:"status"`
	Message          string   `json:"message"`
	LotMode          string   `json:"lot_mode"`
	RatioLot         float64  `json:"ratio_lot"`
	FixedLot         float64  `json:"fixed_lot"`
	MaxLotPerOrder   float64  `json:"max_lot_per_order"`
	MaxLotTotal      float64  `json:"max_lot_total"`
	MaxOpenPositions int      `json:"max_open_positions"`
	CopyEnabled      bool     `json:"copy_enabled"`
	DryRun           bool     `json:"dry_run"`
	LastSeenAt       string   `json:"last_seen_at,omitempty"`
	Balance          *float64 `json:"balance,omitempty"`
	Currency         string   `json:"currency,omitempty"`
}

func (a *app) handleCopyMaster(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var input copyLoginRequest
	if !decodeLocalJSON(w, r, &input) {
		return
	}
	creds, err := validateCopyCredentials(input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout+90*time.Second)
	defer cancel()
	outcome, err := a.copier.connectMaster(ctx, creds)
	if !outcome.Valid {
		message := outcome.Message
		if message == "" && err != nil {
			message = limitText(err.Error(), 240)
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "message": message})
		return
	}
	if saveErr := a.recordCopyLogin(creds, outcome); saveErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Hasil login utama tidak dapat disimpan."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "message": "Akun utama terhubung dan siap menerbitkan posisi.", "login": strconv.FormatInt(creds.Login, 10), "server": creds.Server})
}

func (a *app) handleCopyClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var input copyClientLoginRequest
	if !decodeLocalJSON(w, r, &input) {
		return
	}
	creds, err := validateCopyCredentials(copyLoginRequest{Login: input.Login, Password: input.Password, Server: input.Server, Company: input.Company})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout+90*time.Second)
	defer cancel()
	client, outcome, err := a.copier.connectClient(ctx, creds, copyClientSettings{LotMode: input.LotMode, RatioLot: input.RatioLot, FixedLot: input.FixedLot, MaxLotPerOrder: input.MaxLotPerOrder, MaxLotTotal: input.MaxLotTotal, MaxOpenPositions: input.MaxOpenPositions})
	if !outcome.Valid {
		message := outcome.Message
		if message == "" && err != nil {
			message = limitText(err.Error(), 240)
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "message": message})
		return
	}
	if saveErr := a.recordCopyLogin(creds, outcome); saveErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Hasil login client tidak dapat disimpan."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "message": "Akun client tersambung dan standby. Pilih simulasi atau aktifkan order live.", "client": copyClientViewFromRecord(client)})
}

func (a *app) handleCopyClientMode(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var input copyClientModeRequest
	if !decodeLocalJSON(w, r, &input) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ID akun client tidak valid."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	client, err := a.copier.setClientMode(ctx, id, input.Enabled, input.DryRun)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": copyClientViewFromRecord(client)})
}

func (a *app) handlePauseCopyClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var ignored map[string]any
	if !decodeLocalJSON(w, r, &ignored) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ID akun client tidak valid."})
		return
	}
	if err := a.copier.pauseClient(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused", "message": "Copy dijeda. Posisi client yang sudah terbuka tetap berada di akun client."})
}

func (a *app) handlePauseCopyMaster(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var ignored map[string]any
	if !decodeLocalJSON(w, r, &ignored) {
		return
	}
	if err := a.copier.pauseMaster(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun utama tidak dapat dijeda."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused", "message": "Akun utama dijeda dan semua client berhenti menerima sinyal baru."})
}

func (a *app) handleCopyStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", false); !ok {
		return
	}
	masterView, err := a.copyMasterStatus()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Status akun utama tidak dapat dibaca."})
		return
	}
	clients, err := a.copyClientStatuses()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Status akun client tidak dapat dibaca."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"master": masterView, "clients": clients})
}

func (a *app) copyMasterStatus() (*copyMasterView, error) {
	var master copyMasterRecord
	err := a.db.QueryRow(`SELECT login, server, status, message, symbol, connected_at, updated_at FROM copy_master WHERE id=1`).Scan(&master.Login, &master.Server, &master.Status, &master.Message, &master.Symbol, &master.ConnectedAt, &master.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	view := &copyMasterView{Login: master.Login, Server: master.Server, Status: master.Status, Message: master.Message, Symbol: master.Symbol, UpdatedAt: master.UpdatedAt}
	connectedAt, parseErr := time.Parse(time.RFC3339Nano, master.ConnectedAt)
	if parseErr == nil {
		if snapshot, active := a.copier.readActiveMasterSnapshot(parseLogin(master.Login), master.Server, connectedAt); active {
			view.Connected = true
			view.Positions = len(snapshot.Positions)
			view.Status = "connected"
			view.Message = "Akun utama aktif dan mengirim snapshot posisi."
			view.UpdatedAt = time.Unix(snapshot.UpdatedAt, 0).UTC().Format(time.RFC3339Nano)
		} else if view.Status == "connected" {
			view.Status = "offline"
			view.Message = "Snapshot akun utama tidak diperbarui; client menunggu koneksi master."
		}
	}
	return view, nil
}

func (a *app) copyClientStatuses() ([]copyClientView, error) {
	rows, err := a.db.Query(`SELECT id, login, server, status, message, account_token, lot_mode, ratio_lot, lot_tetap, max_lot_per_order, max_lot_total, max_open_positions, copy_enabled, dry_run, copy_after, last_seen_at, balance, currency, history_cursor, history_cursor_msc, updated_at FROM copy_clients ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	clients := make([]copyClientView, 0)
	now := time.Now()
	for rows.Next() {
		var record copyClientRecord
		var enabled, dryRun int
		if err := rows.Scan(&record.ID, &record.Login, &record.Server, &record.Status, &record.Message, &record.AccountToken, &record.LotMode, &record.RatioLot, &record.FixedLot, &record.MaxLotPerOrder, &record.MaxLotTotal, &record.MaxOpenPositions, &enabled, &dryRun, &record.CopyAfter, &record.LastSeenAt, &record.Balance, &record.Currency, &record.HistoryCursor, &record.HistoryCursorMSC, &record.UpdatedAt); err != nil {
			return nil, err
		}
		record.CopyEnabled, record.DryRun = enabled != 0, dryRun != 0
		view := copyClientViewFromRecord(record)
		if view.LastSeenAt != "" {
			seen, parseErr := time.Parse(time.RFC3339Nano, view.LastSeenAt)
			if parseErr != nil || now.Sub(seen) > copyClientFreshness || seen.After(now.Add(5*time.Second)) {
				view.Status = "offline"
				view.Message = "Terminal client tidak mengirim heartbeat. Sambungkan ulang jika terminal berhenti."
			} else if view.CopyEnabled {
				if view.DryRun {
					view.Status = "simulasi"
				} else {
					view.Status = "copying"
				}
			} else if view.Status != "error" && view.Status != "paused" {
				view.Status = "standby"
			}
		} else if view.Status != "error" && view.Status != "paused" {
			view.Status = "starting"
		}
		clients = append(clients, view)
	}
	return clients, rows.Err()
}

func copyClientViewFromRecord(record copyClientRecord) copyClientView {
	view := copyClientView{
		ID: record.ID, Login: record.Login, Server: record.Server, Status: record.Status, Message: record.Message,
		LotMode: record.LotMode, RatioLot: record.RatioLot, FixedLot: record.FixedLot,
		MaxLotPerOrder: record.MaxLotPerOrder, MaxLotTotal: record.MaxLotTotal, MaxOpenPositions: record.MaxOpenPositions,
		CopyEnabled: record.CopyEnabled, DryRun: record.DryRun, LastSeenAt: record.LastSeenAt, Currency: record.Currency,
	}
	if record.Balance.Valid {
		value := record.Balance.Float64
		view.Balance = &value
	}
	return view
}

func decodeLocalJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	if !requestIsLocal(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses hanya diizinkan dari browser lokal."})
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Origin permintaan tidak diizinkan."})
		return false
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Gunakan content type application/json."})
		return false
	}
	return decodeJSONBody(w, r, value) == nil
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Data permintaan tidak valid."})
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Kirim satu object JSON saja."})
		return errors.New("JSON extra")
	}
	return nil
}

func validateCopyCredentials(input copyLoginRequest) (credentials, error) {
	login, err := strconv.ParseInt(strings.TrimSpace(input.Login), 10, 64)
	server, company := strings.TrimSpace(input.Server), strings.TrimSpace(input.Company)
	if err != nil || login <= 0 || server == "" || len(server) > 120 || len(company) > 160 || len(input.Password) == 0 || len(input.Password) > 128 || strings.ContainsAny(server+company+input.Password, "\r\n\x00") {
		return credentials{}, errors.New("Nomor login, server, atau password tidak valid.")
	}
	return credentials{Login: login, Password: input.Password, Server: server, Company: company}, nil
}

func normalizeCopyClientSettings(settings copyClientSettings) (copyClientSettings, error) {
	if settings.LotMode == "" {
		settings.LotMode = "rasio"
	}
	if settings.RatioLot == 0 {
		settings.RatioLot = 1
	}
	if settings.FixedLot == 0 {
		settings.FixedLot = 0.01
	}
	if settings.MaxLotPerOrder == 0 {
		settings.MaxLotPerOrder = 0.1
	}
	if settings.MaxLotTotal == 0 {
		settings.MaxLotTotal = 0.5
	}
	if settings.MaxOpenPositions == 0 {
		settings.MaxOpenPositions = 10
	}
	if settings.LotMode != "rasio" && settings.LotMode != "tetap" {
		return settings, errors.New("Mode lot client harus rasio atau tetap.")
	}
	if !finiteNumber(settings.RatioLot) || settings.RatioLot < 0.01 || settings.RatioLot > 5 ||
		!finiteNumber(settings.FixedLot) || settings.FixedLot < 0.01 || settings.FixedLot > 5 ||
		!finiteNumber(settings.MaxLotPerOrder) || settings.MaxLotPerOrder < 0.01 || settings.MaxLotPerOrder > 5 ||
		!finiteNumber(settings.MaxLotTotal) || settings.MaxLotTotal < settings.MaxLotPerOrder || settings.MaxLotTotal > 10 ||
		settings.MaxOpenPositions < 1 || settings.MaxOpenPositions > 100 {
		return settings, errors.New("Pengaturan lot atau batas risiko client tidak valid.")
	}
	return settings, nil
}

func (a *app) recordCopyLogin(creds credentials, outcome probeOutcome) error {
	status := "failed"
	if outcome.Valid {
		status = "connected"
	}
	message := limitText(outcome.Message, 240)
	if outcome.Valid && message == "" {
		message = "Login broker berhasil diverifikasi."
	}
	symbol := strings.TrimSpace(outcome.Symbol)
	var balanceValue any
	currency := ""
	if outcome.Valid && outcome.Balance != nil && !math.IsNaN(*outcome.Balance) && !math.IsInf(*outcome.Balance, 0) && *outcome.Balance >= 0 {
		if strings.TrimSpace(outcome.Currency) != "" && len(outcome.Currency) <= 12 {
			balanceValue = *outcome.Balance
			currency = strings.TrimSpace(outcome.Currency)
		}
	}
	_, err := a.db.Exec(`INSERT INTO mt5_accounts(login, server, status, message, updated_at, symbol, balance, currency) VALUES(?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(login, server) DO UPDATE SET status=excluded.status, message=excluded.message, updated_at=excluded.updated_at, symbol=excluded.symbol, balance=excluded.balance, currency=excluded.currency`, strconv.FormatInt(creds.Login, 10), creds.Server, status, message, time.Now().UTC().Format(time.RFC3339Nano), symbol, balanceValue, currency)
	return err
}
