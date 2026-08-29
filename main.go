package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

var app = newApp()

func main() {}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		var cfg configRequest
		if len(request) > 0 {
			_ = json.Unmarshal(request, &cfg)
		}
		app.configure(cfg.ConfigYAML)
		return okEnvelope(app.registration())
	case methodManagementRegister:
		return okEnvelope(app.managementRegistration())
	case methodManagementHandle:
		var req managementRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode management request")
			}
		}
		resp, err := app.handleManagement(context.Background(), req)
		if err != nil {
			return nil, err
		}
		return okEnvelope(resp)
	case methodUsageHandle:
		var record usageRecord
		if len(request) > 0 {
			if err := json.Unmarshal(request, &record); err != nil {
				return nil, fmt.Errorf("decode usage record")
			}
		}
		app.handleUsage(record)
		return okEnvelope(struct{}{})
	case methodRequestComplete:
		return okEnvelope(struct{}{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func okEnvelope(v any) ([]byte, error) { return json.Marshal(envelope{OK: true, Result: mustJSON(v)}) }
func mustJSON(v any) json.RawMessage   { b, _ := json.Marshal(v); return b }
func errorEnvelope(code, message string) []byte {
	b, _ := json.Marshal(envelope{OK: false, Error: &wireError{Code: code, Message: message}})
	return b
}
func safeError(err error) string {
	if err == nil {
		return "plugin error"
	}
	msg := err.Error()
	for _, secret := range []string{"access_token", "refresh_token", "id_token", "api_key", "cookie"} {
		if strings.Contains(strings.ToLower(msg), secret) {
			return "request failed"
		}
	}
	return msg
}
func htmlResponse(status int, body []byte) managementResponse {
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: body}
}
