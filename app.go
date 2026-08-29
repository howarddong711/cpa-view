package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxInputBytes  = 10 << 20
	maxAccounts    = 1000
	maxJSONDepth   = 20
	maxZipEntries  = 100
	maxZipExpanded = 50 << 20
	previewTTL     = 10 * time.Minute
)

type pluginApp struct {
	mu       sync.Mutex
	dataDir  string
	previews map[string]pendingPreview
	groups   map[string]map[string]bool
	hourly   []usageHourly
}
type pendingPreview struct {
	expiresAt time.Time
	accounts  []cpaAuth
	response  previewResponse
}

func newApp() *pluginApp {
	return &pluginApp{dataDir: "data/cpa-view", previews: map[string]pendingPreview{}, groups: map[string]map[string]bool{"Codex": {}}}
}
func closeApp() {}
func (a *pluginApp) configure(raw []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data_dir:") {
			if v := strings.TrimSpace(strings.TrimPrefix(line, "data_dir:")); v != "" {
				a.dataDir = strings.Trim(v, "\"'")
			}
		}
	}
	_ = os.MkdirAll(a.dataDir, 0o700)
	a.loadHourly()
	a.loadGroups()
}
func (a *pluginApp) registration() pluginRegistration {
	return pluginRegistration{SchemaVersion: schemaVersion, Metadata: metadata{Name: "CPA View", Version: "0.1.0", Author: "howarddong711", GitHubRepository: "https://github.com/howarddong711/cpa-view", Logo: "https://raw.githubusercontent.com/howarddong711/cpa-view/main/assets/logo.svg", ConfigFields: []configField{{Name: "data_dir", Type: "string", Description: "Directory for aggregate usage and group data only."}}}, Capabilities: capabilities{ManagementAPI: true, UsagePlugin: true}}
}
func (a *pluginApp) managementRegistration() managementRegistration {
	return managementRegistration{
		Resources: []resourceRoute{{Path: "/app", Menu: "CPA View", Description: "Account pool and usage dashboard"}},
		Routes: []managementRoute{
			{Method: "GET", Path: "/cpa-view/accounts", Description: "List redacted accounts"},
			{Method: "POST", Path: "/cpa-view/imports/preview", Description: "Preview a safe account import"},
			{Method: "POST", Path: "/cpa-view/imports/commit", Description: "Commit a pending import via CPA Auth callback"},
			{Method: "GET", Path: "/cpa-view/groups", Description: "List account groups"},
			{Method: "POST", Path: "/cpa-view/groups", Description: "Create account group"},
			{Method: "PATCH", Path: "/cpa-view/groups", Description: "Rename account group"},
			{Method: "DELETE", Path: "/cpa-view/groups", Description: "Delete account group"},
			{Method: "GET", Path: "/cpa-view/dashboard", Description: "Usage dashboard data"},
		},
	}
}

func (a *pluginApp) handleManagement(ctx context.Context, req managementRequest) (managementResponse, error) {
	path := strings.TrimPrefix(req.Path, "/v0/management")
	path = strings.TrimPrefix(path, "/cpa-view")
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		path = "/"
	}
	switch {
	case req.Method == http.MethodGet && (path == "/" || strings.HasSuffix(path, "/app")):
		return htmlResponse(http.StatusOK, embeddedIndex), nil
	case req.Method == http.MethodGet && path == "/accounts":
		return a.accounts(ctx, req)
	case req.Method == http.MethodPost && path == "/imports/preview":
		return a.preview(req)
	case req.Method == http.MethodPost && path == "/imports/commit":
		return a.commit(ctx, req)
	case req.Method == http.MethodGet && path == "/groups":
		return a.groupsResponse()
	case req.Method == http.MethodPost && path == "/groups":
		return a.createGroup(req)
	case req.Method == http.MethodPatch && path == "/groups":
		return a.renameGroup(req)
	case req.Method == http.MethodDelete && path == "/groups":
		return a.deleteGroup(req)
	case req.Method == http.MethodGet && path == "/dashboard":
		return a.dashboard(req)
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
	}
}

func (a *pluginApp) accounts(ctx context.Context, _ managementRequest) (managementResponse, error) {
	raw, err := callHost(methodHostAuthList, map[string]any{})
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "auth_list_unavailable"})
	}
	var list authListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "invalid_auth_list"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rows := make([]accountRow, 0, len(list.Files))
	for _, f := range list.Files {
		groups := []string{"全部"}
		if strings.EqualFold(f.Type, "codex") || strings.EqualFold(f.Provider, "codex") {
			groups = append(groups, "Codex")
		}
		for g, ids := range a.groups {
			if ids[f.AuthIndex] {
				groups = append(groups, g)
			}
		}
		row := accountRow{AuthIndex: f.AuthIndex, Name: redactName(f.Name, f.Email), Email: redactEmail(f.Email), Type: f.Type, Status: f.Status, Disabled: f.Disabled, RequestCount: f.Success + f.Failed, Groups: uniqueStrings(groups)}
		if row.RequestCount > 0 {
			rate := float64(f.Success) / float64(row.RequestCount) * 100
			row.SuccessRate = &rate
		}
		if !f.UpdatedAt.IsZero() {
			t := f.UpdatedAt
			row.LastActivity = &t
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return jsonResponse(http.StatusOK, map[string]any{"accounts": rows})
}

func (a *pluginApp) preview(req managementRequest) (managementResponse, error) {
	var in previewRequest
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_json_body"})
	}
	data, err := decodeInput(in)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": safeError(err)})
	}
	accounts, format, errors := parseImport(data, in.Filename)
	if len(accounts) > maxAccounts {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "account_limit_exceeded"})
	}
	seen := map[string]bool{}
	redacted := make([]redactedAuth, 0, len(accounts))
	duplicates := 0
	for i, acc := range accounts {
		fp := fingerprint(acc)
		existing := seen[fp]
		if existing {
			duplicates++
		}
		seen[fp] = true
		redacted = append(redacted, redactedAuth{Index: i, Type: acc.Type, Email: redactEmail(acc.Email), AccountID: redactID(acc.AccountID), PlanType: acc.PlanType, Existing: existing})
	}
	id := randomID()
	expires := time.Now().Add(previewTTL)
	a.mu.Lock()
	a.previews[id] = pendingPreview{expiresAt: expires, accounts: accounts, response: previewResponse{PreviewID: id, ExpiresAt: expires, Format: format, Accounts: redacted, Duplicates: duplicates, Errors: errors}}
	a.mu.Unlock()
	return jsonResponse(http.StatusOK, a.previews[id].response)
}

func (a *pluginApp) commit(_ context.Context, req managementRequest) (managementResponse, error) {
	var in struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(req.Body, &in); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_json_body"})
	}
	a.mu.Lock()
	p, ok := a.previews[in.PreviewID]
	if ok && time.Now().After(p.expiresAt) {
		delete(a.previews, in.PreviewID)
		ok = false
	}
	if ok {
		delete(a.previews, in.PreviewID)
	}
	a.mu.Unlock()
	if !ok {
		return jsonResponse(http.StatusGone, map[string]any{"error": "preview_expired"})
	}
	imported, skipped := 0, 0
	for i, acc := range p.accounts {
		raw, _ := json.Marshal(acc)
		name := safeFileName(acc.Email)
		if name == "" {
			name = fmt.Sprintf("cpa-view-%d.json", i+1)
		}
		_, err := callHost(methodHostAuthSave, authSaveRequest{Name: name, JSON: raw})
		if err != nil {
			skipped++
			continue
		}
		imported++
	}
	return jsonResponse(http.StatusOK, map[string]any{"imported": imported, "skipped": skipped})
}

func (a *pluginApp) groupsResponse() (managementResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := []string{"全部", "Codex"}
	for g := range a.groups {
		if g != "Codex" {
			names = append(names, g)
		}
	}
	sort.Strings(names[2:])
	return jsonResponse(http.StatusOK, map[string]any{"groups": names})
}
func (a *pluginApp) createGroup(req managementRequest) (managementResponse, error) {
	var in struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(req.Body, &in) != nil || !validGroup(in.Name) {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_group"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.groups[in.Name]; ok || in.Name == "全部" {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "group_exists"})
	}
	a.groups[in.Name] = map[string]bool{}
	a.persistGroups()
	return jsonResponse(http.StatusOK, map[string]any{"name": in.Name})
}
func (a *pluginApp) renameGroup(req managementRequest) (managementResponse, error) {
	var in struct {
		OldName string `json:"old_name"`
		Name    string `json:"name"`
	}
	if json.Unmarshal(req.Body, &in) != nil || !validGroup(in.Name) || in.OldName == "Codex" || in.OldName == "全部" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_group"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ids, ok := a.groups[in.OldName]
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "group_not_found"})
	}
	if _, exists := a.groups[in.Name]; exists {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "group_exists"})
	}
	delete(a.groups, in.OldName)
	a.groups[in.Name] = ids
	a.persistGroups()
	return jsonResponse(http.StatusOK, map[string]any{"name": in.Name})
}
func (a *pluginApp) deleteGroup(req managementRequest) (managementResponse, error) {
	name := string(req.Body)
	var in struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(req.Body, &in) == nil && in.Name != "" {
		name = in.Name
	}
	name = strings.Trim(name, " \"{}")
	if name == "Codex" || name == "全部" {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "protected_group"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.groups, name)
	a.persistGroups()
	return jsonResponse(http.StatusOK, map[string]any{"deleted": name})
}

func (a *pluginApp) dashboard(req managementRequest) (managementResponse, error) {
	days := 7
	if v := req.Query.Get("days"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	a.mu.Lock()
	defer a.mu.Unlock()
	var total, success, inTok, outTok, cached int64
	byModel := map[string]int64{}
	byAccount := map[string]int64{}
	trend := map[string]map[string]int64{}
	for _, u := range a.hourly {
		t, e := time.Parse(time.RFC3339, u.Hour)
		if e != nil || t.Before(since) {
			continue
		}
		total += u.RequestCount
		success += u.SuccessCount
		inTok += u.InputTokens
		outTok += u.OutputTokens
		cached += u.CachedTokens
		byModel[u.Model] += u.RequestCount
		byAccount[u.AuthIndex] += u.RequestCount
		if trend[u.Hour] == nil {
			trend[u.Hour] = map[string]int64{}
		}
		trend[u.Hour]["input"] += u.InputTokens
		trend[u.Hour]["output"] += u.OutputTokens
	}
	rate := float64(0)
	if total > 0 {
		rate = float64(success) * 100 / float64(total)
	}
	return jsonResponse(http.StatusOK, map[string]any{"range_days": days, "requests": total, "success_rate": rate, "total_tokens": inTok + outTok, "input_tokens": inTok, "output_tokens": outTok, "cached_tokens": cached, "cache_hit_rate": cacheRate(cached, inTok), "rpm": float64(total) / float64(days*24*60), "tpm": float64(inTok+outTok) / float64(days*24*60), "estimated_cost": estimateCost(inTok, outTok), "token_trend": trend, "model_ranking": byModel, "account_ranking": byAccount})
}

func (a *pluginApp) handleUsage(r usageRecord) {
	now := r.RequestedAt
	if now.IsZero() {
		now = time.Now()
	}
	hour := now.UTC().Truncate(time.Hour).Format(time.RFC3339)
	a.mu.Lock()
	defer a.mu.Unlock()
	idx := r.AuthIndex
	if idx == "" {
		idx = r.AuthID
	}
	for i := range a.hourly {
		u := &a.hourly[i]
		if u.Hour == hour && u.AuthIndex == idx && u.Model == r.Model {
			u.RequestCount++
			if !r.Failed {
				u.SuccessCount++
			}
			u.InputTokens += r.Detail.InputTokens
			u.OutputTokens += r.Detail.OutputTokens
			u.CachedTokens += r.Detail.CachedTokens
			u.LatencyMS += r.Latency.Milliseconds()
			a.persistHourly()
			return
		}
	}
	a.hourly = append(a.hourly, usageHourly{Hour: hour, AuthIndex: idx, Model: r.Model, RequestCount: 1, SuccessCount: boolInt(!r.Failed), InputTokens: r.Detail.InputTokens, OutputTokens: r.Detail.OutputTokens, CachedTokens: r.Detail.CachedTokens, LatencyMS: r.Latency.Milliseconds()})
	a.persistHourly()
}
func (a *pluginApp) loadHourly() {
	b, e := os.ReadFile(filepath.Join(a.dataDir, "usage_hourly.json"))
	if e == nil {
		_ = json.Unmarshal(b, &a.hourly)
	}
}

func (a *pluginApp) loadGroups() {
	b, err := os.ReadFile(filepath.Join(a.dataDir, "account_groups.json"))
	if err == nil {
		var groups map[string]map[string]bool
		if json.Unmarshal(b, &groups) == nil {
			for name, ids := range groups {
				if validGroup(name) && name != "全部" && name != "Codex" {
					a.groups[name] = ids
				}
			}
		}
	}
}

func (a *pluginApp) persistGroups() {
	_ = os.MkdirAll(a.dataDir, 0o700)
	b, _ := json.Marshal(a.groups)
	_ = os.WriteFile(filepath.Join(a.dataDir, "account_groups.json"), b, 0o600)
}
func (a *pluginApp) persistHourly() {
	_ = os.MkdirAll(a.dataDir, 0o700)
	b, _ := json.Marshal(a.hourly)
	_ = os.WriteFile(filepath.Join(a.dataDir, "usage_hourly.json"), b, 0o600)
}

func decodeInput(in previewRequest) ([]byte, error) {
	if in.ContentBase64 != "" {
		b, e := base64.StdEncoding.DecodeString(in.ContentBase64)
		if e != nil {
			return nil, fmt.Errorf("invalid base64")
		}
		if len(b) > maxInputBytes {
			return nil, fmt.Errorf("input too large")
		}
		return b, nil
	}
	b := []byte(in.Content)
	if len(b) > maxInputBytes {
		return nil, fmt.Errorf("input too large")
	}
	return b, nil
}
func parseImport(data []byte, filename string) ([]cpaAuth, string, []string) {
	if strings.HasSuffix(strings.ToLower(filename), ".zip") || bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return parseZip(data)
	}
	var raws []json.RawMessage
	format := "json"
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, format, []string{"empty input"}
	}
	if json.Valid(trimmed) {
		var v any
		if json.Unmarshal(trimmed, &v) == nil {
			switch x := v.(type) {
			case []any:
				for _, item := range x {
					b, _ := json.Marshal(item)
					raws = append(raws, b)
				}
				format = "json-array"
			case map[string]any:
				if arr, ok := x["accounts"].([]any); ok {
					for _, item := range arr {
						b, _ := json.Marshal(item)
						raws = append(raws, b)
					}
					format = "sub2api-package"
				} else {
					raws = append(raws, trimmed)
				}
			}
		}
	} else {
		format = "ndjson"
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if json.Valid([]byte(line)) {
				raws = append(raws, []byte(line))
			} else if len(raws) < maxAccounts {
			}
		}
	}
	var out []cpaAuth
	var errs []string
	for i, raw := range raws {
		if depth(raw) > maxJSONDepth {
			errs = append(errs, fmt.Sprintf("record %d exceeds JSON depth limit", i+1))
			continue
		}
		acc, err := convertRecord(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("record %d: %v", i+1, err))
			continue
		}
		if fingerprint(acc) != "" {
			out = append(out, acc)
		}
	}
	return out, format, errs
}
func parseZip(data []byte) ([]cpaAuth, string, []string) {
	r, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if e != nil {
		return nil, "zip", []string{"invalid zip"}
	}
	if len(r.File) > maxZipEntries {
		return nil, "zip", []string{"too many zip entries"}
	}
	var all []cpaAuth
	var errs []string
	var expanded int64
	for _, f := range r.File {
		if f.FileInfo().IsDir() || strings.Contains(f.Name, "..") || strings.HasPrefix(f.Name, "/") {
			continue
		}
		if f.UncompressedSize64 > maxZipExpanded || expanded+int64(f.UncompressedSize64) > maxZipExpanded {
			errs = append(errs, "zip expansion limit exceeded")
			break
		}
		rc, er := f.Open()
		if er != nil {
			continue
		}
		b, er := io.ReadAll(io.LimitReader(rc, maxInputBytes))
		rc.Close()
		if er != nil {
			continue
		}
		expanded += int64(len(b))
		a, _, es := parseImport(b, f.Name)
		all = append(all, a...)
		errs = append(errs, es...)
		if len(all) >= maxAccounts {
			break
		}
	}
	return all, "zip", errs
}
func convertRecord(raw []byte) (cpaAuth, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return cpaAuth{}, fmt.Errorf("invalid JSON")
	}
	if _, ok := obj["credentials"]; ok || obj["platform"] != nil {
		var s sub2apiAccount
		if json.Unmarshal(raw, &s) != nil {
			return cpaAuth{}, fmt.Errorf("invalid sub2api account")
		}
		if !strings.Contains(strings.ToLower(s.Platform+" "+s.Type), "openai") && !strings.Contains(strings.ToLower(s.Platform+" "+s.Type), "codex") {
			return cpaAuth{}, fmt.Errorf("unsupported platform")
		}
		return cpaAuth{Type: "codex", AccessToken: s.Credentials.AccessToken, RefreshToken: s.Credentials.RefreshToken, IDToken: s.Credentials.IDToken, Email: s.Credentials.Email, AccountID: s.Credentials.ChatGPTAccountID, ChatGPTAccountID: s.Credentials.ChatGPTAccountID, PlanType: s.Credentials.PlanType, ChatGPTPlanType: s.Credentials.PlanType, Expired: string(s.Credentials.ExpiresAt)}, nil
	}
	var c cpaAuth
	if err := json.Unmarshal(raw, &c); err != nil {
		return cpaAuth{}, fmt.Errorf("invalid CPA auth")
	}
	if c.Type == "" {
		c.Type = "codex"
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return cpaAuth{}, fmt.Errorf("missing OAuth token")
	}
	return c, nil
}
func depth(raw []byte) int {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return maxJSONDepth + 1
	}
	var walk func(any, int) int
	walk = func(x any, d int) int {
		m := d
		switch y := x.(type) {
		case map[string]any:
			for _, v := range y {
				if n := walk(v, d+1); n > m {
					m = n
				}
			}
		case []any:
			for _, v := range y {
				if n := walk(v, d+1); n > m {
					m = n
				}
			}
		}
		return m
	}
	return walk(v, 1)
}
func fingerprint(a cpaAuth) string {
	key := strings.ToLower(strings.TrimSpace(a.Type)) + "|" + strings.ToLower(strings.TrimSpace(a.Email)) + "|" + a.AccountID + "|" + a.ChatGPTAccountID
	if key == "|||" {
		key = a.AccessToken + "|" + a.RefreshToken
	}
	if key == "|" {
		return ""
	}
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}
func randomID() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	return hex.EncodeToString(h[:8])
}
func jsonResponse(status int, v any) (managementResponse, error) {
	b, _ := json.Marshal(v)
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json"}, "Cache-Control": []string{"no-store"}}, Body: b}, nil
}
func redactEmail(s string) string {
	p := strings.SplitN(s, "@", 2)
	if len(p) != 2 {
		return redactID(s)
	}
	local := p[0]
	if len(local) > 2 {
		local = local[:1] + "***" + local[len(local)-1:]
	} else {
		local = "***"
	}
	return local + "@" + p[1]
}
func redactName(name, email string) string {
	if email != "" {
		return redactEmail(email)
	}
	return redactID(name)
}
func redactID(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:3] + "…" + s[len(s)-3:]
}
func safeFileName(email string) string {
	email = strings.ReplaceAll(email, "@", "_")
	email = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, email)
	if email == "" {
		return ""
	}
	return email + ".json"
}
func validGroup(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && len(s) <= 40 && !strings.ContainsAny(s, "/\\\r\n")
}
func uniqueStrings(in []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	return out
}
func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
func cacheRate(cached, input int64) float64 {
	if input <= 0 {
		return 0
	}
	return float64(cached) * 100 / float64(input)
}
func estimateCost(input, output int64) float64 {
	return float64(input)*0.000002 + float64(output)*0.000008
}
