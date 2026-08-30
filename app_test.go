package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestStandaloneRouteWhitelist(t *testing.T) {
	a := newApp()

	root := httptest.NewRecorder()
	a.handleStandaloneHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "window.__CPA_VIEW_STANDALONE__=true") {
		t.Fatalf("standalone page was not rendered: status=%d", root.Code)
	}

	head := httptest.NewRecorder()
	a.handleStandaloneHTTP(head, httptest.NewRequest(http.MethodHead, "/", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("standalone HEAD should return status only: status=%d body=%d", head.Code, head.Body.Len())
	}

	blocked := httptest.NewRecorder()
	a.handleStandaloneHTTP(blocked, httptest.NewRequest(http.MethodPut, "/api/prices", strings.NewReader(`{}`)))
	if blocked.Code != http.StatusNotFound {
		t.Fatalf("write route must not be public: status=%d", blocked.Code)
	}

	preview := httptest.NewRecorder()
	a.handleStandaloneHTTP(preview, httptest.NewRequest(http.MethodPost, "/api/imports/preview", strings.NewReader(`{"content":""}`)))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview route should be public: status=%d", preview.Code)
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
