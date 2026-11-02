package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func removeAccountTerminalDirectory(probe *terminalProbe, login int64, server string) error {
	if probe == nil || strings.TrimSpace(probe.runtimeRoot) == "" {
		return errors.New("direktori runtime terminal tidak tersedia")
	}
	root, err := filepath.Abs(probe.runtimeRoot)
	if err != nil {
		return err
	}
	target, err := terminalAccountRuntimePath(root, login, server)
	if err != nil {
		return err
	}
	if target == root || !pathWithin(root, target) {
		return errors.New("folder akun di luar direktori runtime")
	}
	if strings.TrimSpace(probe.terminalPath) != "" {
		sourceTerminal, absErr := filepath.Abs(probe.terminalPath)
		if absErr != nil {
			return absErr
		}
		sourceDir := filepath.Dir(sourceTerminal)
		if pathWithin(sourceDir, target) || pathWithin(target, sourceDir) {
			return errors.New("folder akun bertumpang tindih dengan terminal template")
		}
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("folder runtime akun bukan direktori biasa")
	}
	killTerminalsIn(target)
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("folder terminal akun tidak dapat dihapus: %w", err)
	}
	return nil
}

func accountIDFromPath(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("ID akun tidak valid")
	}
	return id, nil
}

func (a *app) managedTerminalProbe() (*terminalProbe, error) {
	probe, ok := a.probe.(*terminalProbe)
	if !ok || probe == nil {
		return nil, errors.New("terminal runtime tidak tersedia")
	}
	return probe, nil
}

func (a *app) handleDeleteSavedAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var ignored map[string]any
	if !decodeLocalJSON(w, r, &ignored) {
		return
	}
	id, err := accountIDFromPath(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var loginText, server string
	if err := a.db.QueryRow(`SELECT login,server FROM mt5_accounts WHERE id=?`, id).Scan(&loginText, &server); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Riwayat akun tidak ditemukan."})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun tidak dapat dibaca."})
		}
		return
	}
	login, parseErr := strconv.ParseInt(strings.TrimSpace(loginText), 10, 64)
	if parseErr != nil || login <= 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Identitas akun tersimpan tidak valid."})
		return
	}
	probe, err := a.managedTerminalProbe()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Terminal runtime tidak tersedia; akun tetap disimpan."})
		return
	}
	probe.mu.Lock()
	err = removeAccountTerminalDirectory(probe, login, server)
	probe.mu.Unlock()
	if err != nil {
		log.Printf("saved MT5 runtime cleanup failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Folder terminal verifikasi akun tidak dapat dibersihkan; data akun tetap disimpan."})
		return
	}
	if _, err := a.db.Exec(`DELETE FROM mt5_accounts WHERE id=?`, id); err != nil {
		log.Printf("saved MT5 account delete failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Folder terminal sudah dibersihkan, tetapi catatan akun belum dapat dihapus."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "message": "Catatan akun dan folder terminal verifikasinya dihapus."})
}

func (a *app) handleDeleteCopyClient(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var ignored map[string]any
	if !decodeLocalJSON(w, r, &ignored) {
		return
	}
	id, err := accountIDFromPath(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	manager := a.copier
	manager.connectMu.Lock()
	defer manager.connectMu.Unlock()
	client, err := manager.client(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Akun client tidak ditemukan."})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun client tidak dapat dibaca."})
		}
		return
	}
	_, _ = manager.db.Exec(`UPDATE copy_clients SET copy_enabled=0,status='deleting',message='Akun client sedang dihapus.',updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	_ = manager.refreshClients()
	manager.stopSession(clientSessionKey(id))

	manager.probe.mu.Lock()
	err = removeAccountTerminalDirectory(manager.probe, parseLogin(client.Login), client.Server)
	manager.probe.mu.Unlock()
	if err != nil {
		log.Printf("copy client runtime cleanup failed: %v", err)
		_ = manager.saveClientStatus(id, "paused", "Folder terminal gagal dibersihkan; akun client dipertahankan.")
		_ = manager.refreshClients()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Folder terminal akun client tidak dapat dibersihkan; akun tetap tersimpan dalam keadaan dijeda."})
		return
	}

	probe, err := a.managedTerminalProbe()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Terminal runtime verifikasi tidak tersedia; akun client tetap disimpan."})
		return
	}
	probe.mu.Lock()
	err = removeAccountTerminalDirectory(probe, parseLogin(client.Login), client.Server)
	probe.mu.Unlock()
	if err != nil {
		log.Printf("copy client verification runtime cleanup failed: %v", err)
		_ = manager.saveClientStatus(id, "paused", "Folder terminal verifikasi gagal dibersihkan; akun client dipertahankan.")
		_ = manager.refreshClients()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Folder verifikasi MT5 tidak dapat dibersihkan; akun client tetap tersimpan dalam keadaan dijeda."})
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun client tidak dapat dihapus dari database."})
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM copy_clients WHERE id=?`, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun client tidak dapat dihapus dari database."})
		return
	}
	if _, err = tx.Exec(`DELETE FROM mt5_accounts WHERE login=? AND lower(server)=lower(?)`, client.Login, client.Server); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Riwayat akun client tidak dapat dihapus."})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Perubahan akun client tidak dapat disimpan."})
		return
	}
	if a.auth != nil && a.auth.db != nil {
		if _, err := a.auth.db.Exec(`DELETE FROM portal_client_accounts WHERE client_id=?`, id); err != nil {
			log.Printf("portal client link cleanup failed for deleted client %d: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun dan folder terminal sudah dihapus, tetapi tautan profil client belum dapat dibersihkan."})
			return
		}
	}
	_ = manager.refreshClients()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "message": "Akun client dan folder terminal lokalnya dihapus. Posisi yang sudah ada tetap berada di akun broker."})
}

func (a *app) handleDeleteCopyMaster(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requirePortalAPI(w, r, "admin", true); !ok {
		return
	}
	var ignored map[string]any
	if !decodeLocalJSON(w, r, &ignored) {
		return
	}

	manager := a.copier
	manager.connectMu.Lock()
	defer manager.connectMu.Unlock()
	var loginText, server string
	if err := a.db.QueryRow(`SELECT login,server FROM copy_master WHERE id=1`).Scan(&loginText, &server); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Akun utama tidak ditemukan."})
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun utama tidak dapat dibaca."})
		}
		return
	}
	login, parseErr := strconv.ParseInt(strings.TrimSpace(loginText), 10, 64)
	if parseErr != nil || login <= 0 {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Identitas akun utama tidak valid."})
		return
	}
	if err := a.signals.setLive(r.Context(), false); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Mode signal live tidak dapat dimatikan."})
		return
	}
	_, _ = manager.db.Exec(`UPDATE copy_clients SET copy_enabled=0,status='paused',message='Akun utama dihapus; aktifkan ulang setelah memilih master baru.',updated_at=?`, time.Now().UTC().Format(time.RFC3339Nano))
	_, _ = manager.db.Exec(`UPDATE copy_master SET status='paused',message='Akun utama sedang dihapus.',updated_at=? WHERE id=1`, time.Now().UTC().Format(time.RFC3339Nano))
	_ = manager.refreshClients()
	manager.stopSession("master")

	manager.probe.mu.Lock()
	err := removeAccountTerminalDirectory(manager.probe, login, server)
	manager.probe.mu.Unlock()
	if err != nil {
		log.Printf("copy master runtime cleanup failed: %v", err)
		_ = manager.saveMasterStatus("paused", "Folder terminal gagal dibersihkan; akun utama dipertahankan.")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Folder terminal akun utama tidak dapat dibersihkan; akun tetap tersimpan dalam keadaan dijeda."})
		return
	}
	probe, err := a.managedTerminalProbe()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Terminal runtime verifikasi tidak tersedia; akun utama tetap disimpan."})
		return
	}
	probe.mu.Lock()
	err = removeAccountTerminalDirectory(probe, login, server)
	probe.mu.Unlock()
	if err != nil {
		log.Printf("copy master verification runtime cleanup failed: %v", err)
		_ = manager.saveMasterStatus("paused", "Folder terminal verifikasi gagal dibersihkan; akun utama dipertahankan.")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Folder verifikasi MT5 tidak dapat dibersihkan; akun utama tetap tersimpan dalam keadaan dijeda."})
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun utama tidak dapat dihapus dari database."})
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM copy_master WHERE id=1`); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Akun utama tidak dapat dihapus dari database."})
		return
	}
	if _, err = tx.Exec(`DELETE FROM mt5_accounts WHERE login=? AND lower(server)=lower(?)`, loginText, server); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Riwayat akun utama tidak dapat dihapus."})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Perubahan akun utama tidak dapat disimpan."})
		return
	}
	_ = manager.refreshClients()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "message": "Akun utama dan folder terminalnya dihapus. Akun client dijeda; posisi broker tidak ditutup."})
}
