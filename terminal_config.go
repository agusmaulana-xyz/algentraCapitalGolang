package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// enableAutoTrading mirrors alok's terminal preparation for the login probe.
// It only enables the local Expert API; MT5LoginProbe does not place orders.
func enableAutoTrading(terminalDir string) error {
	path := filepath.Join(terminalDir, "Config", "common.ini")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	encoding, bom, text := "utf-8", []byte{}, string(raw)
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		bom = raw[:3]
	}
	if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe {
		encoding, bom, text = "utf-16le", raw[:2], decodeUTF16Config(raw[2:], binary.LittleEndian)
	}
	if len(raw) >= 2 && raw[0] == 0xfe && raw[1] == 0xff {
		encoding, bom, text = "utf-16be", raw[:2], decodeUTF16Config(raw[2:], binary.BigEndian)
	}
	text = strings.TrimPrefix(text, "\ufeff")
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	keys := []struct{ key, value string }{
		{"allowlivetrading", "AllowLiveTrading=1"},
		{"enabled", "Enabled=1"},
		{"account", "Account=0"},
		{"profile", "Profile=0"},
	}
	var output []string
	present := map[string]bool{}
	inExperts, foundExperts := false, false
	flush := func() {
		for _, item := range keys {
			if !present[item.key] {
				output = append(output, item.value)
			}
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inExperts {
				flush()
			}
			inExperts = strings.EqualFold(strings.TrimSpace(trimmed[1:len(trimmed)-1]), "experts")
			foundExperts = foundExperts || inExperts
			present = map[string]bool{}
			output = append(output, line)
			continue
		}
		if inExperts && strings.Contains(trimmed, "=") && !strings.HasPrefix(trimmed, ";") && !strings.HasPrefix(trimmed, "#") {
			key, _, _ := strings.Cut(trimmed, "=")
			key = strings.ToLower(strings.TrimSpace(key))
			for _, item := range keys {
				if key == item.key {
					output = append(output, item.value)
					present[key] = true
					goto nextLine
				}
			}
		}
		output = append(output, line)
	nextLine:
	}
	if inExperts {
		flush()
	}
	if !foundExperts {
		if len(output) > 0 && strings.TrimSpace(output[len(output)-1]) != "" {
			output = append(output, "")
		}
		output = append(output, "[Experts]")
		for _, item := range keys {
			output = append(output, item.value)
		}
	}
	rendered := strings.Join(output, newline) + newline
	var encoded []byte
	switch encoding {
	case "utf-16le":
		encoded = encodeUTF16Config(rendered, binary.LittleEndian)
	case "utf-16be":
		encoded = encodeUTF16Config(rendered, binary.BigEndian)
	default:
		encoded = []byte(rendered)
	}
	encoded = append(bom, encoded...)
	if bytes.Equal(raw, encoded) {
		return nil
	}
	temp := path + ".mt5login.tmp"
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func decodeUTF16Config(raw []byte, order binary.ByteOrder) string {
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-1]
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = order.Uint16(raw[i*2:])
	}
	return string(utf16.Decode(units))
}

func encodeUTF16Config(value string, order binary.ByteOrder) []byte {
	units := utf16.Encode([]rune(value))
	output := make([]byte, len(units)*2)
	for i, unit := range units {
		order.PutUint16(output[i*2:], unit)
	}
	return output
}
