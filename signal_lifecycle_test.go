package main

import (
	"context"
	"testing"
	"time"
)

func TestDisableSignalLiveMovesQueuedSignalToDryRun(t *testing.T) {
	db, err := openDatabase(":memory:")
	if err != nil {
		t.Fatalf("openDatabase() error = %v", err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO telegram_signals(chat_id,group_name,message_id,raw_text,status,telegram_created_at,created_at,expires_at) VALUES('-1','Test','11','signal','PENDING',?,?,?)`, now, now, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("insert pending signal: %v", err)
	}
	service := newTelegramSignalService(db, nil, nil)
	if err := service.setLive(context.Background(), false); err != nil {
		t.Fatalf("setLive(false): %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM telegram_signals WHERE message_id='11'`).Scan(&status); err != nil {
		t.Fatalf("read signal status: %v", err)
	}
	if status != "DRY_RUN" {
		t.Fatalf("queued signal status = %q, want DRY_RUN", status)
	}
}
