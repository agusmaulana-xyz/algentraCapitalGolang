package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
)

type telegramIncomingMessage struct {
	ChatID     string
	MessageID  string
	SenderID   string
	SenderName string
	Text       string
	CreatedAt  time.Time
}

type telegramDialogGroup struct {
	ChatID   string `json:"chat_id"`
	Name     string `json:"name"`
	Username string `json:"username,omitempty"`
}

type telegramManager struct {
	apiID       int
	apiHash     string
	sessionPath string

	mu             sync.RWMutex
	client         *telegram.Client
	authClient     *auth.Client
	api            *tg.Client
	started        bool
	connected      bool
	pendingPhone   string
	phoneCodeHash  string
	awaiting2FA    bool
	accountName    string
	messageHandler func(context.Context, telegramIncomingMessage) error
	messageQueue   chan telegramIncomingMessage
}

func newTelegramManager(apiID int, apiHash, sessionPath string) *telegramManager {
	return &telegramManager{
		apiID:        apiID,
		apiHash:      strings.TrimSpace(apiHash),
		sessionPath:  sessionPath,
		messageQueue: make(chan telegramIncomingMessage, 256),
	}
}

func (m *telegramManager) configured() bool {
	return m != nil && m.apiID > 0 && len(m.apiHash) == 32
}

func (m *telegramManager) setMessageHandler(handler func(context.Context, telegramIncomingMessage) error) {
	m.mu.Lock()
	m.messageHandler = handler
	m.mu.Unlock()
}

func (m *telegramManager) start(ctx context.Context) error {
	if !m.configured() {
		return nil
	}
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	m.mu.Unlock()
	if m.sessionPath == "" {
		return errors.New("path sesi Telegram kosong")
	}
	if err := os.MkdirAll(filepath.Dir(m.sessionPath), 0o700); err != nil {
		return fmt.Errorf("direktori sesi Telegram tidak dapat dibuat: %w", err)
	}
	_ = os.Chmod(filepath.Dir(m.sessionPath), 0o700)
	go m.consumeMessages(ctx)
	go m.run(ctx)
	return nil
}

func (m *telegramManager) run(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		dispatcher := tg.NewUpdateDispatcher()
		dispatcher.OnNewMessage(func(updateCtx context.Context, entities tg.Entities, update *tg.UpdateNewMessage) error {
			m.handleUpdate(updateCtx, entities, update.Message)
			return nil
		})
		dispatcher.OnNewChannelMessage(func(updateCtx context.Context, entities tg.Entities, update *tg.UpdateNewChannelMessage) error {
			m.handleUpdate(updateCtx, entities, update.Message)
			return nil
		})
		client := telegram.NewClient(m.apiID, m.apiHash, telegram.Options{
			SessionStorage: &session.FileStorage{Path: m.sessionPath},
			UpdateHandler:  dispatcher,
			OnConnectionState: func(state telegram.ConnectionState) {
				m.mu.Lock()
				m.connected = state == telegram.ConnectionStateReady
				m.mu.Unlock()
			},
			OnDead: func(error) {
				m.mu.Lock()
				m.connected = false
				m.mu.Unlock()
			},
		})
		api := client.API()
		m.mu.Lock()
		m.client = client
		m.api = api
		m.authClient = auth.NewClient(api, rand.Reader, m.apiID, m.apiHash)
		m.mu.Unlock()
		err := client.Run(ctx, func(runCtx context.Context) error {
			m.mu.Lock()
			m.connected = true
			m.mu.Unlock()
			if status, err := m.authService().Status(runCtx); err == nil && status.Authorized {
				m.mu.Lock()
				m.accountName = telegramUserName(status.User)
				m.mu.Unlock()
			}
			<-runCtx.Done()
			return nil
		})
		m.mu.Lock()
		m.connected = false
		if err != nil && ctx.Err() == nil {
			m.accountName = ""
		}
		m.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
		}
	}
}

func (m *telegramManager) consumeMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-m.messageQueue:
			m.mu.RLock()
			handler := m.messageHandler
			m.mu.RUnlock()
			if handler != nil {
				if err := handler(ctx, message); err != nil {
					log.Printf("telegram message processing failed: %T", err)
				}
			}
		}
	}
}

func (m *telegramManager) handleUpdate(ctx context.Context, entities tg.Entities, messageClass tg.MessageClass) {
	message, ok := messageClass.(*tg.Message)
	if !ok || message.Out || strings.TrimSpace(message.Message) == "" || message.PeerID == nil {
		return
	}
	var chatID string
	switch peer := message.PeerID.(type) {
	case *tg.PeerChat:
		chatID = fmt.Sprintf("-%d", peer.ChatID)
	case *tg.PeerChannel:
		chatID = fmt.Sprintf("-%d", 1000000000000+peer.ChannelID)
	default:
		return
	}
	var senderID, senderName string
	if from, exists := message.GetFromID(); exists {
		if peer, ok := from.(*tg.PeerUser); ok {
			senderID = fmt.Sprintf("%d", peer.UserID)
			if sender, found := entities.Users[peer.UserID]; found && sender != nil {
				senderName = telegramUserName(sender)
			}
		}
	}
	createdAt := time.Unix(int64(message.Date), 0).UTC()
	if message.Date <= 0 {
		createdAt = time.Now().UTC()
	}
	item := telegramIncomingMessage{
		ChatID: chatID, MessageID: fmt.Sprintf("%d", message.ID), SenderID: senderID,
		SenderName: senderName, Text: strings.TrimSpace(message.Message), CreatedAt: createdAt,
	}
	select {
	case m.messageQueue <- item:
	case <-ctx.Done():
	default:
		log.Printf("telegram signal queue full; message skipped")
	}
}

func (m *telegramManager) waitConnected(ctx context.Context, timeout time.Duration) error {
	if !m.configured() {
		return errors.New("TELEGRAM_API_ID dan TELEGRAM_API_HASH belum dikonfigurasi")
	}
	m.mu.RLock()
	started := m.started
	m.mu.RUnlock()
	if !started {
		return errors.New("listener Telegram belum berjalan")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.RLock()
		connected := m.connected
		m.mu.RUnlock()
		if connected {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("waktu tunggu koneksi Telegram habis")
		case <-ticker.C:
		}
	}
}

func (m *telegramManager) sendCode(ctx context.Context, phone string) (map[string]any, error) {
	phone = strings.TrimSpace(phone)
	if len(phone) < 7 || len(phone) > 24 || !strings.HasPrefix(phone, "+") {
		return nil, errors.New("nomor telepon harus berformat internasional, contoh +628123456789")
	}
	if err := m.waitConnected(ctx, 30*time.Second); err != nil {
		return nil, err
	}
	client := m.authService()
	if client == nil {
		return nil, errors.New("koneksi Telegram belum siap")
	}
	sent, err := client.SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return nil, err
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok || code.PhoneCodeHash == "" {
		return nil, errors.New("Telegram tidak mengembalikan hash kode login")
	}
	m.mu.Lock()
	m.pendingPhone, m.phoneCodeHash, m.awaiting2FA = phone, code.PhoneCodeHash, false
	m.mu.Unlock()
	return map[string]any{"status": "code_sent", "phone": phone[:3] + "••••" + phone[len(phone)-3:]}, nil
}

func (m *telegramManager) verifyCode(ctx context.Context, code string) (map[string]any, error) {
	m.mu.RLock()
	phone, hash := m.pendingPhone, m.phoneCodeHash
	m.mu.RUnlock()
	if phone == "" || hash == "" {
		return nil, errors.New("minta kode Telegram terlebih dahulu")
	}
	if err := m.waitConnected(ctx, 30*time.Second); err != nil {
		return nil, err
	}
	client := m.authService()
	if client == nil {
		return nil, errors.New("koneksi Telegram belum siap")
	}
	result, err := client.SignIn(ctx, phone, strings.TrimSpace(code), hash)
	if errors.Is(err, auth.ErrPasswordAuthNeeded) {
		m.mu.Lock()
		m.awaiting2FA = true
		m.mu.Unlock()
		return map[string]any{"status": "two_factor_required", "requires_2fa": true}, nil
	}
	if err != nil {
		return nil, err
	}
	user, _ := result.User.(*tg.User)
	return m.finishLogin(user), nil
}

func (m *telegramManager) verify2FA(ctx context.Context, password string) (map[string]any, error) {
	m.mu.RLock()
	awaiting := m.awaiting2FA
	m.mu.RUnlock()
	if !awaiting {
		return nil, errors.New("login Telegram belum meminta password 2FA")
	}
	if err := m.waitConnected(ctx, 30*time.Second); err != nil {
		return nil, err
	}
	client := m.authService()
	if client == nil {
		return nil, errors.New("koneksi Telegram belum siap")
	}
	result, err := client.Password(ctx, password)
	if err != nil {
		return nil, err
	}
	user, _ := result.User.(*tg.User)
	return m.finishLogin(user), nil
}

func (m *telegramManager) finishLogin(user *tg.User) map[string]any {
	name := telegramUserName(user)
	m.mu.Lock()
	m.pendingPhone, m.phoneCodeHash, m.awaiting2FA, m.accountName = "", "", false, name
	m.mu.Unlock()
	_ = os.Chmod(m.sessionPath, 0o600)
	return map[string]any{"status": "connected", "account_name": name}
}

func (m *telegramManager) status(ctx context.Context) map[string]any {
	result := map[string]any{"configured": m.configured(), "connected": false, "state": "disconnected", "account_name": nil}
	if !m.configured() {
		return result
	}
	m.mu.RLock()
	connected, pending, awaiting, name := m.connected, m.pendingPhone != "", m.awaiting2FA, m.accountName
	m.mu.RUnlock()
	if connected && name != "" {
		client := m.authService()
		if client != nil {
			if status, err := client.Status(ctx); err == nil && status.Authorized {
				result["connected"], result["state"], result["account_name"] = true, "connected", name
				return result
			}
		}
	}
	if awaiting {
		result["state"] = "awaiting_2fa"
	} else if pending {
		result["state"] = "awaiting_code"
	}
	return result
}

func (m *telegramManager) logout(ctx context.Context) error {
	if err := m.waitConnected(ctx, 5*time.Second); err == nil {
		if api := m.rawAPI(); api != nil {
			if _, err := api.AuthLogOut(ctx); err != nil {
				return err
			}
		}
	}
	m.mu.Lock()
	m.pendingPhone, m.phoneCodeHash, m.accountName = "", "", ""
	m.awaiting2FA = false
	m.mu.Unlock()
	if err := os.Remove(m.sessionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *telegramManager) groups(ctx context.Context) ([]telegramDialogGroup, error) {
	if err := m.waitConnected(ctx, 15*time.Second); err != nil {
		return nil, err
	}
	client := m.authService()
	api := m.rawAPI()
	if client == nil || api == nil {
		return nil, errors.New("akun Telegram belum login")
	}
	status, err := client.Status(ctx)
	if err != nil || !status.Authorized {
		return nil, errors.New("akun Telegram belum login")
	}
	elements, err := query.NewQuery(api).GetDialogs().BatchSize(100).Collect(ctx)
	if err != nil {
		return nil, err
	}
	groups := make([]telegramDialogGroup, 0, len(elements))
	for _, element := range elements {
		dialog, ok := element.Dialog.(*tg.Dialog)
		if !ok || dialog.Peer == nil {
			continue
		}
		var id int64
		var name, username string
		switch peer := dialog.Peer.(type) {
		case *tg.PeerChat:
			id = -peer.ChatID
			if entity, found := element.Entities.Chat(peer.ChatID); found {
				name = entity.Title
			}
		case *tg.PeerChannel:
			id = -1000000000000 - peer.ChannelID
			if entity, found := element.Entities.Channel(peer.ChannelID); found {
				name, username = entity.Title, entity.Username
			}
		default:
			continue
		}
		if name == "" {
			name = "Grup Telegram"
		}
		groups = append(groups, telegramDialogGroup{ChatID: fmt.Sprintf("%d", id), Name: name, Username: username})
	}
	return groups, nil
}

func (m *telegramManager) authService() *auth.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.connected {
		return nil
	}
	return m.authClient
}

func (m *telegramManager) rawAPI() *tg.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.connected {
		return nil
	}
	return m.api
}

func telegramUserName(user *tg.User) string {
	if user == nil {
		return ""
	}
	name := strings.TrimSpace(strings.Join([]string{user.FirstName, user.LastName}, " "))
	if name == "" {
		name = user.Username
	}
	return name
}
