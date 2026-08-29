package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"testing"
)

func TestConvertSub2APIAccount(t *testing.T) {
	raw := []byte(`{"platform":"openai","type":"oauth","credentials":{"access_token":"secret-access","refresh_token":"secret-refresh","id_token":"id","email":"alice@example.com","chatgpt_account_id":"acct-123","plan_type":"Plus","expires_at":"2026-09-01T00:00:00Z"}}`)
	a, err := convertRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != "codex" || a.Email != "alice@example.com" || a.ChatGPTAccountID != "acct-123" {
		t.Fatalf("unexpected conversion: %#v", a)
	}
	preview := redactedAuth{Email: redactEmail(a.Email), AccountID: redactID(a.AccountID)}
	b, _ := json.Marshal(preview)
	if bytes.Contains(b, []byte("secret")) {
		t.Fatal("preview leaked credential")
	}
}

func TestParseNDJSONAndDedupFingerprint(t *testing.T) {
	data := []byte(`{"type":"codex","access_token":"a","refresh_token":"b","email":"a@example.com"}
{"type":"codex","access_token":"a","refresh_token":"b","email":"a@example.com"}`)
	accounts, format, errs := parseImport(data, "accounts.jsonl")
	if format != "ndjson" || len(errs) != 0 || len(accounts) != 2 {
		t.Fatalf("format=%s accounts=%d errors=%v", format, len(accounts), errs)
	}
	if fingerprint(accounts[0]) != fingerprint(accounts[1]) {
		t.Fatal("expected stable duplicate fingerprint")
	}
}

func TestZipLimitsAndParsing(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("accounts.json")
	_, _ = w.Write([]byte(`[{"type":"codex","access_token":"a","email":"zip@example.com"}]`))
	_ = zw.Close()
	accounts, format, errs := parseImport(buf.Bytes(), "accounts.zip")
	if format != "zip" || len(accounts) != 1 || len(errs) != 0 {
		t.Fatalf("format=%s accounts=%d errors=%v", format, len(accounts), errs)
	}
}

func TestInputLimit(t *testing.T) {
	_, err := decodeInput(previewRequest{Content: string(bytes.Repeat([]byte("x"), maxInputBytes+1))})
	if err == nil {
		t.Fatal("expected input size limit")
	}
}
