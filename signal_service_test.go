package main

import (
	"context"
	"testing"
	"time"
)

func TestSignalResultsStoreExpiryAndPerSignalStats(t *testing.T) {
	db, err := openDatabase(":memory:")
	if err != nil {
		t.Fatalf("openDatabase() error = %v", err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := db.Exec(`INSERT INTO telegram_signals(chat_id,group_name,message_id,raw_text,status,telegram_created_at,created_at,expires_at) VALUES('-1','Test','10','signal','ORDERED',?,?,?)`, now, now, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("insert signal: %v", err)
	}
	signalID, _ := result.LastInsertId()
	for leg, ticket := range []string{"1001", "1002"} {
		if _, err := db.Exec(`INSERT INTO telegram_signal_orders(signal_id,leg,status,ticket,updated_at) VALUES(?,?,'EXECUTED',?,?)`, signalID, leg+1, ticket, now); err != nil {
			t.Fatalf("insert order: %v", err)
		}
	}
	service := newTelegramSignalService(db, nil, nil)
	if err := service.recordResult(context.Background(), signalResultReport{Ticket: "1001", Profit: 12.5, Result: "WIN", ClosedAt: now}); err != nil {
		t.Fatalf("record winning result: %v", err)
	}
	if err := service.recordResult(context.Background(), signalResultReport{Ticket: "1002", Profit: 0, Result: "EXPIRED"}); err != nil {
		t.Fatalf("record expiry without close timestamp: %v", err)
	}
	items, stats, winrate, err := service.list(context.Background(), 50)
	if err != nil {
		t.Fatalf("list() error = %v", err)
	}
	if len(items) != 1 || items[0].Results["WIN"] != 1 || items[0].Results["EXPIRED"] != 1 || items[0].NetProfit != 12.5 {
		t.Fatalf("unexpected per-signal result: %#v", items)
	}
	if stats["wins"] != 1 || stats["losses"] != 0 || stats["net_profit"] != 12.5 || winrate != 100 {
		t.Fatalf("unexpected aggregate stats: %#v winrate=%v", stats, winrate)
	}
}
