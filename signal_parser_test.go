package main

import "testing"

func TestNormalizeTelegramSellZoneCreatesTwoOneToOneTargets(t *testing.T) {
	sl := 4070.0
	parsed := parsedTelegramSignal{
		IsSignal:   true,
		Type:       "NEW_SIGNAL",
		Action:     "SELL",
		Symbol:     "XAUUSD",
		SL:         &sl,
		Confidence: 0.99,
		Reason:     "signal",
	}
	message := "XAUUSD SELL NOW 4054:::4063\nTP1 4050\nSL 4070"
	normalized, err := normalizeTelegramSignal(parsed, message)
	if err != nil {
		t.Fatalf("normalizeTelegramSignal() error = %v", err)
	}
	if normalized.Action != "SELL" || normalized.Order != "AUTO" || normalized.EntryMode != "PARTIAL" {
		t.Fatalf("unexpected normalized signal: %#v", normalized)
	}
	if normalized.Low == nil || *normalized.Low != 4054 || normalized.High == nil || *normalized.High != 4063 {
		t.Fatalf("entry zone = (%v, %v), want (4054, 4063)", normalized.Low, normalized.High)
	}
	if len(normalized.TP) != 2 || normalized.TP[0] != 4038 || normalized.TP[1] != 4056 {
		t.Fatalf("targets = %v, want [4038 4056]", normalized.TP)
	}
}

func TestNormalizeTelegramSignalRejectsDirectionOrStopLossConflict(t *testing.T) {
	sl := 4070.0
	parsed := parsedTelegramSignal{IsSignal: true, Type: "NEW_SIGNAL", Action: "BUY", Symbol: "XAUUSD", SL: &sl, Confidence: 0.99}
	if _, err := normalizeTelegramSignal(parsed, "XAUUSD SELL NOW 4054:::4063 SL 4070"); err == nil {
		t.Fatal("expected explicit action mismatch to be rejected")
	}
	parsed.Action = "SELL"
	sl = 4050
	if _, err := normalizeTelegramSignal(parsed, "XAUUSD SELL NOW 4054:::4063 SL 4050"); err == nil {
		t.Fatal("expected stop loss below a SELL entry zone to be rejected")
	}
}

func TestSignalRangeAcceptsSupportedSeparators(t *testing.T) {
	for _, message := range []string{"XAUUSD SELL 4054:::4063", "XAUUSD SELL 4054-4063", "XAUUSD SELL 4054–4063", "XAUUSD SELL 4054—4063", "XAUUSD SELL 4054 to 4063", "XAUUSD SELL 4054 sampai 4063"} {
		if !isSignalCandidate(message) {
			t.Errorf("expected signal candidate for %q", message)
		}
	}
}
