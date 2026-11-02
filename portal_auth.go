package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	portalSessionCookie = "algentra_session"
	portalSessionAge    = 7 * 24 * time.Hour
	passwordRounds      = 310000
)

type portalAuth struct {
	db              *sql.DB
	adminUsername   string
	adminHash       string
	adminSetupError string
	dummyHash       string

	mu       sync.Mutex
	linkMu   sync.Mutex
	attempts map[string]loginAttempt
}

type loginAttempt struct {
	count     int
	resetAt   time.Time
	blockedTo time.Time
}

type portalUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	CreatedAt string `json:"created_at"`
}

type portalSession struct {
	UserID      sql.NullInt64
	Role        string
	Email       string
	DisplayName string
	CSRFToken   string
	ExpiresAt   int64
}

func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || strings.ContainsAny(key, " \t\r\n\x00") {
			return fmt.Errorf("format .env tidak valid pada baris %d", lineNumber)
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func openPortalDatabase(path string) (*sql.DB, error) {
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
CREATE TABLE IF NOT EXISTS portal_users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	email TEXT NOT NULL COLLATE NOCASE UNIQUE,
	password_hash TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS portal_sessions (
	session_hash TEXT PRIMARY KEY,
	user_id INTEGER REFERENCES portal_users(id) ON DELETE CASCADE,
	role TEXT NOT NULL CHECK (role IN ('admin', 'client')),
	email TEXT NOT NULL,
	display_name TEXT NOT NULL,
	csrf_token TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_portal_sessions_expiry ON portal_sessions(expires_at);
CREATE TABLE IF NOT EXISTS portal_client_accounts (
	user_id INTEGER PRIMARY KEY REFERENCES portal_users(id) ON DELETE CASCADE,
	client_id INTEGER NOT NULL UNIQUE,
	connected_at TEXT NOT NULL
);
`
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func newPortalAuth(db *sql.DB, adminUsername, adminPassword string) (*portalAuth, error) {
	auth := &portalAuth{db: db, attempts: make(map[string]loginAttempt)}
	dummyHash, err := hashPortalPassword("not-a-real-account-password")
	if err != nil {
		return nil, err
	}
	auth.dummyHash = dummyHash

	adminUsername = strings.ToLower(strings.TrimSpace(adminUsername))
	if adminUsername == "" && adminPassword == "" {
		auth.adminSetupError = "Admin belum dikonfigurasi. Atur ALGENTRA_ADMIN_USERNAME dan ALGENTRA_ADMIN_PASSWORD di file .env."
		return auth, nil
	}
	if _, err := normalizeAdminUsername(adminUsername); err != nil {
		auth.adminSetupError = "ALGENTRA_ADMIN_USERNAME pada file .env tidak valid."
		return auth, nil
	}
	if len(adminPassword) < 12 || len(adminPassword) > 128 {
		auth.adminSetupError = "ALGENTRA_ADMIN_PASSWORD harus berisi 12 sampai 128 karakter."
		return auth, nil
	}
	auth.adminUsername = adminUsername
	auth.adminHash, err = hashPortalPassword(adminPassword)
	if err != nil {
		return nil, err
	}
	return auth, nil
}

func normalizeAdminUsername(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 32 {
		return "", errors.New("username admin harus berisi 3 sampai 32 karakter")
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			continue
		}
		return "", errors.New("username admin hanya boleh berisi huruf, angka, titik, garis bawah, dan tanda hubung")
	}
	return strings.ToLower(value), nil
}

func normalizePortalEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 5 || len(value) > 254 || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("email tidak valid")
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || !strings.EqualFold(parsed.Address, value) || parsed.Name != "" {
		return "", errors.New("email tidak valid")
	}
	return strings.ToLower(value), nil
}

func validatePortalPassword(password string) error {
	if len(password) < 12 || len(password) > 128 || strings.ContainsRune(password, '\x00') {
		return errors.New("password harus berisi 12 sampai 128 karakter")
	}
	return nil
}

func hashPortalPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	derived := pbkdf2SHA256([]byte(password), salt, passwordRounds, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordRounds, hex.EncodeToString(salt), hex.EncodeToString(derived)), nil
}

func verifyPortalPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds < 100000 || rounds > 1000000 {
		return false
	}
	salt, saltErr := hex.DecodeString(parts[2])
	want, wantErr := hex.DecodeString(parts[3])
	if saltErr != nil || wantErr != nil || len(salt) < 16 || len(salt) > 64 || len(want) != 32 {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, rounds, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	const hashLength = 32
	blocks := (keyLength + hashLength - 1) / hashLength
	derived := make([]byte, 0, blocks*hashLength)
	var blockIndex [4]byte
	for block := 1; block <= blocks; block++ {
		blockIndex[0] = byte(block >> 24)
		blockIndex[1] = byte(block >> 16)
		blockIndex[2] = byte(block >> 8)
		blockIndex[3] = byte(block)
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write(blockIndex[:])
		u := mac.Sum(nil)
		result := append([]byte(nil), u...)
		for iteration := 1; iteration < iterations; iteration++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for i := range result {
				result[i] ^= u[i]
			}
		}
		derived = append(derived, result...)
	}
	return derived[:keyLength]
}

func (a *portalAuth) createUser(name, email, password string) (portalUser, error) {
	name = strings.TrimSpace(name)
	if len([]rune(name)) < 2 || len([]rune(name)) > 60 || strings.ContainsAny(name, "\r\n\x00") {
		return portalUser{}, errors.New("nama harus berisi 2 sampai 60 karakter")
	}
	email, err := normalizePortalEmail(email)
	if err != nil {
		return portalUser{}, err
	}
	if err := validatePortalPassword(password); err != nil {
		return portalUser{}, err
	}
	passwordHash, err := hashPortalPassword(password)
	if err != nil {
		return portalUser{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := a.db.Exec(`INSERT INTO portal_users(name, email, password_hash, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`, name, email, passwordHash, now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return portalUser{}, errors.New("email sudah terdaftar")
		}
		return portalUser{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return portalUser{}, err
	}
	return portalUser{ID: id, Name: name, Email: email, CreatedAt: now}, nil
}

func (a *portalAuth) authenticateClient(email, password string) (portalUser, bool, error) {
	var user portalUser
	var passwordHash string
	err := a.db.QueryRow(`SELECT id, name, email, password_hash, created_at FROM portal_users WHERE email=? COLLATE NOCASE`, email).
		Scan(&user.ID, &user.Name, &user.Email, &passwordHash, &user.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		verifyPortalPassword(a.dummyHash, password)
		return portalUser{}, false, nil
	}
	if err != nil {
		return portalUser{}, false, err
	}
	return user, verifyPortalPassword(passwordHash, password), nil
}

func (a *portalAuth) authenticateAdmin(username, password string) bool {
	if a.adminHash == "" {
		return false
	}
	matched := subtle.ConstantTimeCompare([]byte(strings.ToLower(strings.TrimSpace(username))), []byte(a.adminUsername)) == 1
	passwordMatches := verifyPortalPassword(a.adminHash, password)
	return matched && passwordMatches
}

func (a *portalAuth) allowAttempt(key string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	attempt := a.attempts[key]
	if now.Before(attempt.blockedTo) {
		return false, time.Until(attempt.blockedTo)
	}
	if !now.Before(attempt.resetAt) {
		delete(a.attempts, key)
	}
	return true, 0
}

func (a *portalAuth) recordAttempt(key string, success bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if success {
		delete(a.attempts, key)
		return
	}
	now := time.Now()
	attempt := a.attempts[key]
	if !now.Before(attempt.resetAt) {
		attempt = loginAttempt{resetAt: now.Add(15 * time.Minute)}
	}
	attempt.count++
	if attempt.count >= 10 {
		attempt.blockedTo = now.Add(15 * time.Minute)
	}
	a.attempts[key] = attempt
}

func (a *portalAuth) attemptKey(r *http.Request, email string) string {
	return strings.ToLower(strings.TrimSpace(r.RemoteAddr)) + ":" + strings.ToLower(strings.TrimSpace(email))
}

func (a *portalAuth) issueSession(w http.ResponseWriter, r *http.Request, userID sql.NullInt64, role, email, name string) (string, error) {
	tokenBytes := make([]byte, 32)
	csrfBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	if _, err := rand.Read(csrfBytes); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	csrf := base64.RawURLEncoding.EncodeToString(csrfBytes)
	tokenHash := sha256.Sum256([]byte(token))
	expires := time.Now().Add(portalSessionAge)
	_, err := a.db.Exec(`INSERT INTO portal_sessions(session_hash, user_id, role, email, display_name, csrf_token, expires_at, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		hex.EncodeToString(tokenHash[:]), nullableIntValue(userID), role, email, name, csrf, expires.Unix(), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return "", err
	}
	_, _ = a.db.Exec(`DELETE FROM portal_sessions WHERE expires_at <= ?`, time.Now().Unix())
	http.SetCookie(w, &http.Cookie{
		Name: portalSessionCookie, Value: token, Path: "/", Expires: expires,
		MaxAge: int(portalSessionAge.Seconds()), HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
	return csrf, nil
}

func nullableIntValue(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}

func (a *portalAuth) sessionFromRequest(r *http.Request) (portalSession, error) {
	cookie, err := r.Cookie(portalSessionCookie)
	if err != nil || cookie.Value == "" {
		return portalSession{}, sql.ErrNoRows
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	var session portalSession
	err = a.db.QueryRow(`SELECT user_id, role, email, display_name, csrf_token, expires_at FROM portal_sessions WHERE session_hash=?`, hex.EncodeToString(hash[:])).
		Scan(&session.UserID, &session.Role, &session.Email, &session.DisplayName, &session.CSRFToken, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return portalSession{}, sql.ErrNoRows
	}
	if err != nil {
		return portalSession{}, err
	}
	if session.ExpiresAt <= time.Now().Unix() {
		_, _ = a.db.Exec(`DELETE FROM portal_sessions WHERE session_hash=?`, hex.EncodeToString(hash[:]))
		return portalSession{}, sql.ErrNoRows
	}
	return session, nil
}

func (a *portalAuth) deleteSession(r *http.Request) error {
	cookie, err := r.Cookie(portalSessionCookie)
	if err != nil || cookie.Value == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	_, err = a.db.Exec(`DELETE FROM portal_sessions WHERE session_hash=?`, hex.EncodeToString(hash[:]))
	return err
}

func clearPortalCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: portalSessionCookie, Value: "", Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
}

func (a *portalAuth) clientIDForUser(userID int64) (int64, bool, error) {
	var clientID int64
	err := a.db.QueryRow(`SELECT client_id FROM portal_client_accounts WHERE user_id=?`, userID).Scan(&clientID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return clientID, err == nil, err
}

func (a *portalAuth) clientIDOwner(clientID int64) (int64, bool, error) {
	var userID int64
	err := a.db.QueryRow(`SELECT user_id FROM portal_client_accounts WHERE client_id=?`, clientID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return userID, err == nil, err
}

func (a *portalAuth) linkClientAccount(userID, clientID int64) error {
	transaction, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var existing int64
	err = transaction.QueryRow(`SELECT client_id FROM portal_client_accounts WHERE user_id=?`, userID).Scan(&existing)
	if err == nil {
		if existing != clientID {
			return errors.New("profil hanya dapat menghubungkan satu akun MT5; hubungi admin untuk mengganti akun")
		}
		return transaction.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var owner int64
	err = transaction.QueryRow(`SELECT user_id FROM portal_client_accounts WHERE client_id=?`, clientID).Scan(&owner)
	if err == nil && owner != userID {
		return errors.New("akun MT5 ini sudah terhubung ke profil lain")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = transaction.Exec(`INSERT INTO portal_client_accounts(user_id, client_id, connected_at) VALUES(?, ?, ?)`, userID, clientID, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return errors.New("akun MT5 tidak dapat ditautkan ke profil ini")
	}
	return transaction.Commit()
}

func (a *app) requirePortalAPI(w http.ResponseWriter, r *http.Request, role string, csrf bool) (portalSession, bool) {
	if !requestIsLocal(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses hanya diizinkan dari browser lokal."})
		return portalSession{}, false
	}
	if a.auth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Autentikasi portal belum siap."})
		return portalSession{}, false
	}
	session, err := a.auth.sessionFromRequest(r)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Sesi berakhir. Silakan login kembali."})
		return portalSession{}, false
	}
	if err != nil {
		log.Printf("portal session lookup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Sesi tidak dapat diperiksa."})
		return portalSession{}, false
	}
	if session.Role != role {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses tidak diizinkan untuk peran akun ini."})
		return portalSession{}, false
	}
	if csrf {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin == "" || !sameOrigin(origin, r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Origin permintaan tidak diizinkan."})
			return portalSession{}, false
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRFToken)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Token keamanan tidak valid. Muat ulang halaman lalu coba lagi."})
			return portalSession{}, false
		}
	}
	return session, true
}

func (a *app) requirePortalPage(w http.ResponseWriter, r *http.Request, role string) bool {
	if !requestIsLocal(r) {
		http.Error(w, "Akses hanya diizinkan dari browser lokal.", http.StatusForbidden)
		return false
	}
	if a.auth == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return false
	}
	session, err := a.auth.sessionFromRequest(r)
	if err != nil {
		if role == "admin" {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
		return false
	}
	if session.Role != role {
		if session.Role == "admin" {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		}
		return false
	}
	return true
}

func requireSameOriginPost(w http.ResponseWriter, r *http.Request) bool {
	if !requestIsLocal(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Akses hanya diizinkan dari browser lokal."})
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		if referer := r.Referer(); referer != "" {
			parsed, parseErr := url.Parse(referer)
			if parseErr == nil {
				origin = parsed.Scheme + "://" + parsed.Host
			}
		}
	}
	if origin == "" || !sameOrigin(origin, r.Host) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Origin permintaan tidak diizinkan."})
		return false
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Gunakan content type application/json."})
		return false
	}
	return true
}
