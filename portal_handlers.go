package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"
)

type portalAuthRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type portalMT5Request struct {
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

type portalModeRequest struct {
	Enabled     bool `json:"enabled"`
	DryRun      bool `json:"dry_run"`
	Acknowledge bool `json:"acknowledge_live_risk"`
}

func (a *app) servePortalPage(w http.ResponseWriter, fileName string) {
	data, err := webFiles.ReadFile("web/" + fileName)
	if err != nil {
		log.Printf("portal page %q could not be read: %v", fileName, err)
		http.Error(w, "Halaman tidak tersedia.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (a *app) handleStrategiesPage(w http.ResponseWriter, _ *http.Request) {
	a.servePortalPage(w, "strategies.html")
}

func (a *app) handleClientLoginPage(w http.ResponseWriter, _ *http.Request) {
	a.servePortalPage(w, "login.html")
}

func (a *app) handleClientRegisterPage(w http.ResponseWriter, _ *http.Request) {
	a.servePortalPage(w, "register.html")
}

func (a *app) handleClientDashboardPage(w http.ResponseWriter, r *http.Request) {
	if a.requirePortalPage(w, r, "client") {
		a.servePortalPage(w, "client-dashboard.html")
	}
}

func (a *app) handleAdminLoginPage(w http.ResponseWriter, r *http.Request) {
	if a.auth != nil {
		if session, err := a.auth.sessionFromRequest(r); err == nil {
			if session.Role == "admin" {
				http.Redirect(w, r, "/admin", http.StatusSeeOther)
				return
			}
			if session.Role == "client" {
				http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
				return
			}
		}
	}
	a.servePortalPage(w, "admin-login.html")
}

func (a *app) handleAdminDashboardPage(w http.ResponseWriter, r *http.Request) {
	if a.requirePortalPage(w, r, "admin") {
		a.servePortalPage(w, "index.html")
	}
}

func (a *app) handlePortalSession(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses hanya diizinkan dari browser lokal."})
		return
	}
	if a.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Autentikasi portal belum siap."})
		return
	}
	session, err := a.auth.sessionFromRequest(r)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	if err != nil {
		log.Printf("portal session lookup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Sesi tidak dapat diperiksa."})
		return
	}
	response := map[string]any{
		"authenticated": true,
		"role":          session.Role,
		"name":          session.DisplayName,
		"csrf_token":    session.CSRFToken,
	}
	if session.Role == "admin" {
		response["username"] = a.auth.adminUsername
	} else {
		response["email"] = session.Email
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleAdminSetupStatus(w http.ResponseWriter, r *http.Request) {
	if !requestIsLocal(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses hanya diizinkan dari browser lokal."})
		return
	}
	if a.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Autentikasi portal belum siap."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": a.auth.adminHash != "", "message": a.auth.adminSetupError})
}

func (a *app) handlePortalRegister(w http.ResponseWriter, r *http.Request) {
	if !requireSameOriginPost(w, r) {
		return
	}
	if a.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Registrasi belum tersedia."})
		return
	}
	var input portalAuthRequest
	if err := decodeJSONBody(w, r, &input); err != nil {
		return
	}
	key := a.auth.attemptKey(r, input.Email)
	if allowed, wait := a.auth.allowAttempt(key); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Terlalu banyak percobaan. Tunggu beberapa menit lalu coba lagi."})
		return
	}
	user, err := a.auth.createUser(input.Name, input.Email, input.Password)
	if err != nil {
		a.auth.recordAttempt(key, false)
		status := http.StatusBadRequest
		if err.Error() == "email sudah terdaftar" {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	csrf, err := a.auth.issueSession(w, r, sql.NullInt64{Int64: user.ID, Valid: true}, "client", user.Email, user.Name)
	if err != nil {
		log.Printf("portal session creation failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun dibuat, tetapi sesi login tidak dapat disiapkan. Silakan login."})
		return
	}
	a.auth.recordAttempt(key, true)
	writeJSON(w, http.StatusCreated, map[string]any{"authenticated": true, "role": "client", "name": user.Name, "email": user.Email, "csrf_token": csrf})
}

func (a *app) handlePortalClientLogin(w http.ResponseWriter, r *http.Request) {
	if !requireSameOriginPost(w, r) {
		return
	}
	if a.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Login belum tersedia."})
		return
	}
	var input portalAuthRequest
	if err := decodeJSONBody(w, r, &input); err != nil {
		return
	}
	key := a.auth.attemptKey(r, input.Email)
	if allowed, wait := a.auth.allowAttempt(key); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Terlalu banyak percobaan. Tunggu beberapa menit lalu coba lagi."})
		return
	}
	if len(input.Password) > 128 {
		a.auth.recordAttempt(key, false)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Email atau password tidak cocok."})
		return
	}
	email, emailErr := normalizePortalEmail(input.Email)
	var user portalUser
	valid := false
	var err error
	if emailErr != nil {
		verifyPortalPassword(a.auth.dummyHash, input.Password)
	} else {
		user, valid, err = a.auth.authenticateClient(email, input.Password)
	}
	if err != nil {
		log.Printf("client login lookup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Login tidak dapat diproses."})
		return
	}
	if !valid {
		a.auth.recordAttempt(key, false)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Email atau password tidak cocok."})
		return
	}
	csrf, err := a.auth.issueSession(w, r, sql.NullInt64{Int64: user.ID, Valid: true}, "client", user.Email, user.Name)
	if err != nil {
		log.Printf("portal session creation failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Sesi login tidak dapat dibuat."})
		return
	}
	a.auth.recordAttempt(key, true)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "role": "client", "name": user.Name, "email": user.Email, "csrf_token": csrf})
}

func (a *app) handlePortalAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !requireSameOriginPost(w, r) {
		return
	}
	if a.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Login admin belum tersedia."})
		return
	}
	var input portalAuthRequest
	if err := decodeJSONBody(w, r, &input); err != nil {
		return
	}
	if a.auth.adminHash == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": a.auth.adminSetupError})
		return
	}
	key := a.auth.attemptKey(r, input.Username)
	if allowed, wait := a.auth.allowAttempt(key); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()))))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Terlalu banyak percobaan. Tunggu beberapa menit lalu coba lagi."})
		return
	}
	if len(input.Password) > 128 {
		a.auth.recordAttempt(key, false)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Username atau password admin tidak cocok."})
		return
	}
	if !a.auth.authenticateAdmin(input.Username, input.Password) {
		a.auth.recordAttempt(key, false)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Username atau password admin tidak cocok."})
		return
	}
	csrf, err := a.auth.issueSession(w, r, sql.NullInt64{}, "admin", a.auth.adminUsername, "Administrator")
	if err != nil {
		log.Printf("admin session creation failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Sesi admin tidak dapat dibuat."})
		return
	}
	a.auth.recordAttempt(key, true)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "role": "admin", "name": "Administrator", "username": a.auth.adminUsername, "csrf_token": csrf})
}

func (a *app) handlePortalLogout(w http.ResponseWriter, r *http.Request) {
	if !requireSameOriginPost(w, r) {
		return
	}
	if a.auth == nil {
		clearPortalCookie(w, r)
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
		return
	}
	session, err := a.auth.sessionFromRequest(r)
	if err != nil || subtleTokenMismatch(r.Header.Get("X-CSRF-Token"), session.CSRFToken) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Sesi atau token keamanan tidak valid."})
		return
	}
	if err := a.auth.deleteSession(r); err != nil {
		log.Printf("portal session deletion failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Logout tidak dapat diselesaikan."})
		return
	}
	clearPortalCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

func subtleTokenMismatch(got, want string) bool {
	if len(got) != len(want) {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1
}

func (a *app) handlePublicStrategies(w http.ResponseWriter, _ *http.Request) {
	master, err := a.copyMasterStatus()
	if err != nil {
		log.Printf("public strategy status failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Status strategi tidak dapat dibaca."})
		return
	}
	status := "standby"
	if master != nil && master.Connected {
		status = "active"
	}
	writeJSON(w, http.StatusOK, map[string]any{"strategies": []map[string]any{{
		"id": "algentra-core", "name": "Algentra Core", "category": "MT5 Copy Strategy",
		"status": status, "description": "Satu alur strategi yang dikelola admin, dengan batas lot dan mode simulasi pada setiap akun client.",
		"risk_label": "Risiko dikontrol per akun", "execution": "MetaTrader 5",
		"performance_available": false, "performance_note": "Belum ada histori kinerja terverifikasi untuk ditampilkan.",
	}}})
}

func (a *app) handleClientDashboard(w http.ResponseWriter, r *http.Request) {
	session, ok := a.requirePortalAPI(w, r, "client", false)
	if !ok {
		return
	}
	clientID, linked, err := a.auth.clientIDForUser(session.UserID.Int64)
	if err != nil {
		log.Printf("client account mapping lookup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun client tidak dapat dibaca."})
		return
	}
	var client *copyClientView
	if linked {
		clients, err := a.copyClientStatuses()
		if err != nil {
			log.Printf("client account status lookup failed: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Status akun MT5 tidak dapat dibaca."})
			return
		}
		for i := range clients {
			if clients[i].ID == clientID {
				client = &clients[i]
				break
			}
		}
	}
	master, err := a.copyMasterStatus()
	if err != nil {
		log.Printf("client master status lookup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Status strategi tidak dapat dibaca."})
		return
	}
	masterConnected := master != nil && master.Connected
	insights := []string{}
	if client == nil {
		insights = append(insights, "Hubungkan akun MT5 untuk melihat status strategi dan batas risiko akun Anda.")
	} else {
		if !masterConnected {
			insights = append(insights, "Sumber strategi sedang offline. Akun Anda tetap menunggu tanpa mengirim order baru.")
		}
		if !client.CopyEnabled {
			insights = append(insights, "Copy dalam keadaan jeda. Tidak ada order baru yang akan dikirim ke broker.")
		} else if client.DryRun {
			insights = append(insights, "Mode simulasi aktif. Aturan lot dihitung tanpa mengirim order ke broker.")
		} else {
			insights = append(insights, "Mode live aktif. Batas per entry, total lot, dan posisi berlaku pada akun client.")
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":     map[string]string{"name": session.DisplayName, "email": session.Email},
		"account":  client,
		"strategy": map[string]any{"id": "algentra-core", "name": "Algentra Core", "status": map[bool]string{true: "active", false: "standby"}[masterConnected]},
		"insights": insights,
	})
}

func (a *app) handleClientMT5Login(w http.ResponseWriter, r *http.Request) {
	session, ok := a.requirePortalAPI(w, r, "client", true)
	if !ok {
		return
	}
	var input portalMT5Request
	if !decodeLocalJSON(w, r, &input) {
		return
	}
	creds, err := validateCopyCredentials(copyLoginRequest{Login: input.Login, Password: input.Password, Server: input.Server, Company: input.Company})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.copier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Copy trading belum siap."})
		return
	}
	a.auth.linkMu.Lock()
	defer a.auth.linkMu.Unlock()
	var existingClientID int64
	existingErr := a.db.QueryRow(`SELECT id FROM copy_clients WHERE login=? AND lower(server)=lower(?)`, strconv.FormatInt(creds.Login, 10), creds.Server).Scan(&existingClientID)
	existingAccount := existingErr == nil
	if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun client tidak dapat diperiksa."})
		return
	}
	linkedID, linked, err := a.auth.clientIDForUser(session.UserID.Int64)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Profil client tidak dapat diperiksa."})
		return
	}
	if linked && (!existingAccount || existingClientID != linkedID) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Profil hanya dapat menghubungkan satu akun MT5. Hubungi admin untuk mengganti akun."})
		return
	}
	if !linked && existingAccount {
		ownerID, owned, ownerErr := a.auth.clientIDOwner(existingClientID)
		if ownerErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Kepemilikan akun MT5 tidak dapat diperiksa."})
			return
		}
		if owned && ownerID != session.UserID.Int64 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Akun MT5 ini sudah terhubung ke profil lain."})
			return
		}
	}
	settings := copyClientSettings{LotMode: input.LotMode, RatioLot: input.RatioLot, FixedLot: input.FixedLot, MaxLotPerOrder: input.MaxLotPerOrder, MaxLotTotal: input.MaxLotTotal, MaxOpenPositions: input.MaxOpenPositions}
	if _, err := normalizeCopyClientSettings(settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout+90*time.Second)
	defer cancel()
	client, outcome, err := a.copier.connectClient(ctx, creds, settings)
	if !outcome.Valid {
		writeJSON(w, http.StatusOK, map[string]any{"valid": false, "message": outcome.Message})
		return
	}
	if err != nil {
		_ = a.recordCopyLogin(creds, outcome)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	if err := a.auth.linkClientAccount(session.UserID.Int64, client.ID); err != nil {
		log.Printf("client account linking failed: %v", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if err := a.recordCopyLogin(creds, outcome); err != nil {
		log.Printf("client login result save failed: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "message": "Akun MT5 tersambung dalam mode standby.", "account": copyClientViewFromRecord(client)})
}

func (a *app) handleClientMode(w http.ResponseWriter, r *http.Request) {
	session, ok := a.requirePortalAPI(w, r, "client", true)
	if !ok {
		return
	}
	var input portalModeRequest
	if !decodeLocalJSON(w, r, &input) {
		return
	}
	if input.Enabled && !input.DryRun && !input.Acknowledge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Konfirmasi risiko live diperlukan sebelum order dikirim."})
		return
	}
	clientID, linked, err := a.auth.clientIDForUser(session.UserID.Int64)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun client tidak dapat diperiksa."})
		return
	}
	if !linked {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Hubungkan akun MT5 sebelum mengubah mode copy."})
		return
	}
	client, err := a.copier.setClientMode(r.Context(), clientID, input.Enabled, input.DryRun)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": limitText(err.Error(), 240)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": copyClientViewFromRecord(client)})
}

func (a *app) handleClientPause(w http.ResponseWriter, r *http.Request) {
	session, ok := a.requirePortalAPI(w, r, "client", true)
	if !ok {
		return
	}
	if !decodeLocalJSON(w, r, &map[string]any{}) {
		return
	}
	clientID, linked, err := a.auth.clientIDForUser(session.UserID.Int64)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun client tidak dapat diperiksa."})
		return
	}
	if !linked {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Belum ada akun MT5 yang terhubung."})
		return
	}
	if err := a.copier.pauseClient(clientID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused", "message": "Copy dijeda. Posisi yang sudah terbuka tetap berada di akun MT5 Anda."})
}
