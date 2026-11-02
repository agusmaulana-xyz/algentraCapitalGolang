package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type telegramSignalService struct {
	db      *sql.DB
	parser  *geminiSignalParser
	manager *telegramManager
}

type telegramSignalRecord struct {
	ID         int64          `json:"id"`
	Group      string         `json:"group"`
	Action     string         `json:"action"`
	Symbol     string         `json:"symbol"`
	EntryLow   *float64       `json:"entry_low"`
	EntryHigh  *float64       `json:"entry_high"`
	SL         *float64       `json:"sl"`
	TP         []float64      `json:"tp"`
	Confidence *float64       `json:"confidence"`
	Status     string         `json:"status"`
	Reason     string         `json:"reason"`
	CreatedAt  string         `json:"created_at"`
	Results    map[string]int `json:"results,omitempty"`
	NetProfit  float64        `json:"net_profit"`
}

func newTelegramSignalService(db *sql.DB, parser *geminiSignalParser, manager *telegramManager) *telegramSignalService {
	return &telegramSignalService{db: db, parser: parser, manager: manager}
}

func (s *telegramSignalService) processMessage(ctx context.Context, message telegramIncomingMessage) error {
	var enabled int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_signal_sources WHERE chat_id=? AND enabled=1`, message.ChatID).Scan(&enabled); err != nil {
		return err
	}
	if enabled == 0 || message.Text == "" {
		return nil
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	created := message.CreatedAt.UTC()
	createdText := created.Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO telegram_signals(chat_id,group_name,message_id,sender_name,raw_text,status,telegram_created_at,created_at,expires_at) VALUES(?,?,?,?,?,'PROCESSING',?,?,?)`, message.ChatID, groupNameForID(ctx, s.db, message.ChatID), message.MessageID, limitText(message.SenderName, 100), limitText(message.Text, 12000), createdText, time.Now().UTC().Format(time.RFC3339Nano), created.Add(time.Hour).Unix())
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		return nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	finish := func(status, reason string, parsed *parsedTelegramSignal) error {
		var parsedJSON, action, symbol, tpJSON any = "", "", "", "[]"
		var low, high, sl, confidence any
		if parsed != nil {
			encoded, _ := json.Marshal(parsed)
			parsedJSON, action, symbol, confidence = string(encoded), parsed.Action, parsed.Symbol, parsed.Confidence
			if parsed.Low != nil {
				low = *parsed.Low
			}
			if parsed.High != nil {
				high = *parsed.High
			}
			if parsed.SL != nil {
				sl = *parsed.SL
			}
			if data, marshalErr := json.Marshal(parsed.TP); marshalErr == nil {
				tpJSON = string(data)
			}
		}
		if status == "PENDING" {
			_, updateErr := s.db.ExecContext(ctx, `UPDATE telegram_signals SET parsed_json=?,action=?,symbol=?,entry_low=?,entry_high=?,sl=?,tp_json=?,confidence=?,status=CASE WHEN (SELECT live_enabled FROM telegram_signal_settings WHERE id=1)=1 THEN 'PENDING' ELSE 'DRY_RUN' END,reason=CASE WHEN (SELECT live_enabled FROM telegram_signal_settings WHERE id=1)=1 THEN ? ELSE 'Mode live dinonaktifkan sebelum signal masuk antrean.' END WHERE id=?`, parsedJSON, action, symbol, low, high, sl, tpJSON, confidence, limitText(reason, 300), id)
			return updateErr
		}
		_, updateErr := s.db.ExecContext(ctx, `UPDATE telegram_signals SET parsed_json=?,action=?,symbol=?,entry_low=?,entry_high=?,sl=?,tp_json=?,confidence=?,status=?,reason=? WHERE id=?`, parsedJSON, action, symbol, low, high, sl, tpJSON, confidence, status, limitText(reason, 300), id)
		return updateErr
	}
	if time.Since(created) > time.Hour {
		return finish("EXPIRED", "Pesan sinyal sudah berusia lebih dari satu jam.", nil)
	}
	parsed, err := s.parser.parse(ctx, message.Text)
	if err != nil {
		return finish("REJECTED", err.Error(), nil)
	}
	if !parsed.IsSignal || !strings.EqualFold(parsed.Type, "NEW_SIGNAL") {
		return finish("IGNORED", parsed.Reason, &parsed)
	}
	var live int
	if err := s.db.QueryRowContext(ctx, `SELECT live_enabled FROM telegram_signal_settings WHERE id=1`).Scan(&live); err != nil {
		return err
	}
	if live != 1 {
		return finish("DRY_RUN", "Parsing berhasil; mode live belum diaktifkan.", &parsed)
	}
	return finish("PENDING", "Menunggu EA MT5 mengambil sinyal.", &parsed)
}

func groupNameForID(ctx context.Context, db *sql.DB, chatID string) string {
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM telegram_signal_sources WHERE chat_id=?`, chatID).Scan(&name); err != nil || name == "" {
		return "Grup Telegram"
	}
	return name
}

func (s *telegramSignalService) settings(ctx context.Context) (bool, error) {
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT live_enabled FROM telegram_signal_settings WHERE id=1`).Scan(&enabled)
	return enabled == 1, err
}

func (s *telegramSignalService) setLive(ctx context.Context, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `UPDATE telegram_signal_settings SET live_enabled=?,updated_at=? WHERE id=1`, value, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if !enabled {
		if _, err := transaction.ExecContext(ctx, `UPDATE telegram_signals SET status='DRY_RUN',reason='Mode live dinonaktifkan; signal lama tidak otomatis dikirim.' WHERE status IN ('PENDING','CLAIMED')`); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (s *telegramSignalService) heartbeat(ctx context.Context, terminal, version, symbol string, active bool) error {
	if !active {
		return errors.New("EA MT5 tidak melaporkan status aktif")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE telegram_ea_heartbeat SET received_at=?,terminal=?,version=?,symbol=? WHERE id=1`, time.Now().UTC().Format(time.RFC3339Nano), limitText(terminal, 100), limitText(version, 40), limitText(symbol, 40))
	return err
}

func (s *telegramSignalService) heartbeatAt(ctx context.Context) (time.Time, string, string, string, error) {
	var raw, terminal, version, symbol string
	if err := s.db.QueryRowContext(ctx, `SELECT received_at,terminal,version,symbol FROM telegram_ea_heartbeat WHERE id=1`).Scan(&raw, &terminal, &version, &symbol); err != nil {
		return time.Time{}, "", "", "", err
	}
	parsed, _ := time.Parse(time.RFC3339Nano, raw)
	return parsed, terminal, version, symbol, nil
}

type signalExecutionReport struct {
	SignalID int64   `json:"signal_id"`
	Status   string  `json:"status"`
	Ticket   string  `json:"ticket"`
	Symbol   string  `json:"symbol"`
	Action   string  `json:"action"`
	Lots     float64 `json:"lots"`
	Price    float64 `json:"exec_price"`
	Reason   string  `json:"reason"`
	Leg      int     `json:"leg"`
}

func (s *telegramSignalService) recordExecution(ctx context.Context, report signalExecutionReport) error {
	if report.SignalID <= 0 || report.Leg < 1 || report.Leg > 2 || !finiteNumber(report.Lots) || report.Lots < 0 || !finiteNumber(report.Price) || report.Price < 0 {
		return errors.New("laporan EA tidak valid")
	}
	status := strings.ToUpper(strings.TrimSpace(report.Status))
	if status != "EXECUTED" && status != "FAILED" && status != "REJECTED" {
		return errors.New("status laporan EA tidak dikenal")
	}
	var expectedAction, expectedSymbol, signalStatus string
	var low, high, sl sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT action,symbol,entry_low,entry_high,sl,status FROM telegram_signals WHERE id=?`, report.SignalID).Scan(&expectedAction, &expectedSymbol, &low, &high, &sl, &signalStatus); err != nil {
		return err
	}
	if signalStatus != "PENDING" && signalStatus != "CLAIMED" && signalStatus != "ORDERED" {
		return errors.New("sinyal tidak berada dalam status eksekusi")
	}
	if !strings.EqualFold(expectedAction, report.Action) || !isGoldSymbol(report.Symbol) || !isGoldSymbol(expectedSymbol) {
		return errors.New("laporan EA tidak cocok dengan arah atau simbol sinyal")
	}
	if status == "EXECUTED" {
		if report.Ticket == "" || report.Price <= 0 || !low.Valid || !high.Valid || !sl.Valid || report.Price < low.Float64-1e-7 || report.Price > high.Float64+1e-7 {
			return errors.New("harga/tiket order EA tidak cocok dengan zona entry")
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO telegram_signal_orders(signal_id,leg,status,ticket,symbol,action,lots,entry_price,reason,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(signal_id,leg) DO UPDATE SET status=excluded.status,ticket=excluded.ticket,symbol=excluded.symbol,action=excluded.action,lots=excluded.lots,entry_price=excluded.entry_price,reason=excluded.reason,updated_at=excluded.updated_at`, report.SignalID, report.Leg, status, report.Ticket, strings.ToUpper(report.Symbol), expectedAction, report.Lots, report.Price, limitText(report.Reason, 300), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if status == "EXECUTED" {
		_, err = s.db.ExecContext(ctx, `UPDATE telegram_signals SET status='ORDERED',reason='EA MT5 mengirim order pending/market.' WHERE id=?`, report.SignalID)
	} else if status == "FAILED" || status == "REJECTED" {
		_, err = s.db.ExecContext(ctx, `UPDATE telegram_signals SET status='REJECTED',reason=? WHERE id=?`, limitText(report.Reason, 300), report.SignalID)
	}
	return err
}

type signalResultReport struct {
	Ticket   string  `json:"ticket"`
	Profit   float64 `json:"profit"`
	Result   string  `json:"result"`
	ClosedAt string  `json:"closed_at"`
}

func (s *telegramSignalService) recordResult(ctx context.Context, result signalResultReport) error {
	result.Ticket = strings.TrimSpace(result.Ticket)
	result.Result = strings.ToUpper(strings.TrimSpace(result.Result))
	if result.Ticket == "" || !finiteNumber(result.Profit) || (result.Result != "WIN" && result.Result != "LOSS" && result.Result != "BE" && result.Result != "EXPIRED") {
		return errors.New("hasil transaksi EA tidak valid")
	}
	closed := time.Now().UTC()
	if result.ClosedAt != "" {
		var err error
		closed, err = parseEAClosedAt(result.ClosedAt)
		if err != nil {
			return errors.New("waktu hasil transaksi tidak valid")
		}
	}
	var signalID int64
	var leg int
	if err := s.db.QueryRowContext(ctx, `SELECT signal_id,leg FROM telegram_signal_orders WHERE ticket=?`, result.Ticket).Scan(&signalID, &leg); err != nil {
		return errors.New("tiket order tidak dikenal")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO telegram_signal_results(ticket,signal_id,leg,profit,result,closed_at,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(ticket) DO UPDATE SET profit=excluded.profit,result=excluded.result,closed_at=excluded.closed_at,updated_at=excluded.updated_at`, result.Ticket, signalID, leg, result.Profit, result.Result, closed.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func parseEAClosedAt(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("waktu tidak valid")
}

func (s *telegramSignalService) list(ctx context.Context, limit int) ([]telegramSignalRecord, map[string]any, float64, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,group_name,action,symbol,entry_low,entry_high,sl,tp_json,confidence,status,reason,created_at FROM telegram_signals ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()
	items := make([]telegramSignalRecord, 0)
	for rows.Next() {
		var item telegramSignalRecord
		var low, high, sl, confidence sql.NullFloat64
		var tpJSON string
		if err := rows.Scan(&item.ID, &item.Group, &item.Action, &item.Symbol, &low, &high, &sl, &tpJSON, &confidence, &item.Status, &item.Reason, &item.CreatedAt); err != nil {
			return nil, nil, 0, err
		}
		if low.Valid {
			item.EntryLow = &low.Float64
		}
		if high.Valid {
			item.EntryHigh = &high.Float64
		}
		if sl.Valid {
			item.SL = &sl.Float64
		}
		if confidence.Valid {
			item.Confidence = &confidence.Float64
		}
		_ = json.Unmarshal([]byte(tpJSON), &item.TP)
		item.Results = map[string]int{}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, 0, err
	}
	for index := range items {
		resultRows, queryErr := s.db.QueryContext(ctx, `SELECT result,COUNT(*),COALESCE(SUM(profit),0) FROM telegram_signal_results WHERE signal_id=? GROUP BY result`, items[index].ID)
		if queryErr != nil {
			return nil, nil, 0, queryErr
		}
		for resultRows.Next() {
			var name string
			var count int
			var profit float64
			if err := resultRows.Scan(&name, &count, &profit); err != nil {
				resultRows.Close()
				return nil, nil, 0, err
			}
			items[index].Results[name] = count
			items[index].NetProfit += profit
		}
		if err := resultRows.Err(); err != nil {
			resultRows.Close()
			return nil, nil, 0, err
		}
		_ = resultRows.Close()
	}
	stats := map[string]any{}
	var total, wins, losses, breakEven int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_signals`).Scan(&total); err != nil {
		return nil, nil, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_signal_results WHERE result='WIN'`).Scan(&wins); err != nil {
		return nil, nil, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_signal_results WHERE result='LOSS'`).Scan(&losses); err != nil {
		return nil, nil, 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM telegram_signal_results WHERE result='BE'`).Scan(&breakEven); err != nil {
		return nil, nil, 0, err
	}
	stats["total"], stats["wins"], stats["losses"], stats["break_even"] = total, wins, losses, breakEven
	var netProfit float64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(profit),0) FROM telegram_signal_results`).Scan(&netProfit); err != nil {
		return nil, nil, 0, err
	}
	stats["net_profit"] = netProfit
	closed := wins + losses
	winrate := 0.0
	if closed > 0 {
		winrate = float64(wins) * 100 / float64(closed)
	}
	return items, stats, winrate, nil
}
