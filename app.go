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
	"net/url"
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
	mu         sync.Mutex
	dataDir    string
	previews   map[string]pendingPreview
	groups     map[string]map[string]bool
	hourly     []usageHourly
	prices     map[string]modelPrice
	serverMu   sync.Mutex
	server     *http.Server
	serverAddr string
}
type pendingPreview struct {
	expiresAt time.Time
	accounts  []cpaAuth
	response  previewResponse
}

func newApp() *pluginApp {
	return &pluginApp{dataDir: "data/cpa-view", previews: map[string]pendingPreview{}, groups: map[string]map[string]bool{"Codex": {}}, prices: map[string]modelPrice{}}
}
func closeApp() { app.closeStandalone() }
func (a *pluginApp) configure(raw []byte) {
	a.mu.Lock()
	standaloneAddr := a.serverAddr
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data_dir:") {
			if v := strings.TrimSpace(strings.TrimPrefix(line, "data_dir:")); v != "" {
				a.dataDir = strings.Trim(v, "\"'")
			}
		}
		if strings.HasPrefix(line, "standalone_addr:") {
			standaloneAddr = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "standalone_addr:")), "\"'")
		}
	}
	_ = os.MkdirAll(a.dataDir, 0o700)
	a.loadHourly()
	a.loadGroups()
	a.loadPrices()
	a.mu.Unlock()
	if standaloneAddr != "" {
		a.startStandalone(standaloneAddr)
	}
}
func (a *pluginApp) registration() pluginRegistration {
	return pluginRegistration{SchemaVersion: schemaVersion, Metadata: metadata{Name: "CPA View", Version: "0.2.0", Author: "howarddong711", GitHubRepository: "https://github.com/howarddong711/cpa-view", Logo: "https://raw.githubusercontent.com/howarddong711/cpa-view/main/assets/logo.svg", ConfigFields: []configField{{Name: "data_dir", Type: "string", Description: "Directory for aggregate usage and group data only."}, {Name: "standalone_addr", Type: "string", Description: "Optional internal listen address for the standalone read and import page."}}}, Capabilities: capabilities{ManagementAPI: true, UsagePlugin: true}}
}

func (a *pluginApp) startStandalone(addr string) {
	a.serverMu.Lock()
	defer a.serverMu.Unlock()
	if a.server != nil {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleStandaloneHTTP)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	a.server = server
	a.serverAddr = addr
	go func() { _ = server.ListenAndServe() }()
}

func (a *pluginApp) closeStandalone() {
	a.serverMu.Lock()
	server := a.server
	a.server = nil
	a.serverMu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
}

func (a *pluginApp) handleStandaloneHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")

	var resp managementResponse
	var err error
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		resp = a.renderStandaloneIndex(r.Context())
	case r.Method == http.MethodGet && r.URL.Path == "/api/accounts":
		resp, err = a.accounts(r.Context(), managementRequest{})
	case r.Method == http.MethodGet && r.URL.Path == "/api/groups":
		resp, err = a.groupsResponse()
	case r.Method == http.MethodGet && r.URL.Path == "/api/dashboard":
		resp, err = a.dashboard(managementRequest{Method: http.MethodGet, Query: r.URL.Query()})
	case r.Method == http.MethodPost && (r.URL.Path == "/api/imports/preview" || r.URL.Path == "/api/imports/commit"):
		r.Body = http.MaxBytesReader(w, r.Body, maxInputBytes+(4<<20))
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			resp, err = jsonResponse(http.StatusRequestEntityTooLarge, map[string]any{"error": "input_too_large"})
			break
		}
		req := managementRequest{Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone(), Query: r.URL.Query(), Body: body}
		if r.URL.Path == "/api/imports/preview" {
			resp, err = a.preview(req)
		} else {
			resp, err = a.commit(r.Context(), req)
		}
	default:
		resp, err = jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
	}
	if err != nil {
		resp, _ = jsonResponse(http.StatusInternalServerError, map[string]any{"error": "request_failed"})
	}
	for key, values := range resp.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
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
			{Method: "GET", Path: "/cpa-view/prices", Description: "List model prices"},
			{Method: "PUT", Path: "/cpa-view/prices", Description: "Save model price"},
			{Method: "POST", Path: "/cpa-view/prices/sync", Description: "Sync official model prices"},
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
		return a.renderIndex(ctx), nil
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
	case req.Method == http.MethodGet && path == "/prices":
		return a.pricesResponse()
	case req.Method == http.MethodPut && path == "/prices":
		return a.savePrice(req)
	case req.Method == http.MethodPost && path == "/prices/sync":
		return a.syncPrices()
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "not_found"})
	}
}

// renderIndex serves a redacted, read-only snapshot with the public resource.
// CPA protects management routes with its Management Key before the plugin is
// called, so embedding this snapshot is the only safe way to make the page
// useful without weakening CPA's global management authentication.
func (a *pluginApp) renderIndex(ctx context.Context) managementResponse {
	type snapshot struct {
		Accounts  []accountRow   `json:"accounts"`
		Groups    []string       `json:"groups"`
		Dashboard map[string]any `json:"dashboard"`
	}
	out := snapshot{Accounts: []accountRow{}, Groups: []string{"全部", "Codex"}, Dashboard: map[string]any{}}
	if resp, err := a.accounts(ctx, managementRequest{}); err == nil {
		var v struct {
			Accounts []accountRow `json:"accounts"`
		}
		if json.Unmarshal(resp.Body, &v) == nil {
			out.Accounts = v.Accounts
		}
	}
	if resp, err := a.groupsResponse(); err == nil {
		var v struct {
			Groups []string `json:"groups"`
		}
		if json.Unmarshal(resp.Body, &v) == nil && len(v.Groups) > 0 {
			out.Groups = v.Groups
		}
	}
	if resp, err := a.dashboard(managementRequest{Method: http.MethodGet, Query: url.Values{"days": []string{"7"}}}); err == nil {
		_ = json.Unmarshal(resp.Body, &out.Dashboard)
	}
	payload, _ := json.Marshal(out)
	// Keep the snapshot inside a script context without allowing account names
	// or other redacted fields to terminate the script element.
	safe := strings.NewReplacer("<", "\\u003c", ">", "\\u003e", "&", "\\u0026", "\u2028", "\\u2028", "\u2029", "\\u2029").Replace(string(payload))
	body := bytes.Replace(embeddedIndex, []byte("window.__CPA_VIEW_SNAPSHOT__=null;"), []byte("window.__CPA_VIEW_SNAPSHOT__="+safe+";"), 1)
	return htmlResponse(http.StatusOK, body)
}

func (a *pluginApp) renderStandaloneIndex(ctx context.Context) managementResponse {
	resp := a.renderIndex(ctx)
	resp.Body = bytes.Replace(resp.Body, []byte("window.__CPA_VIEW_STANDALONE__=false;"), []byte("window.__CPA_VIEW_STANDALONE__=true;"), 1)
	return resp
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
	usageByAuth := map[string]usageHourly{}
	costByAuth := map[string]float64{}
	for _, usage := range a.hourly {
		total := usageByAuth[usage.AuthIndex]
		total.RequestCount += usage.RequestCount
		total.SuccessCount += usage.SuccessCount
		total.InputTokens += usage.InputTokens
		total.OutputTokens += usage.OutputTokens
		total.CachedTokens += usage.CachedTokens
		usageByAuth[usage.AuthIndex] = total
		costByAuth[usage.AuthIndex] += a.costForUsage(usage)
	}
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
		u := usageByAuth[f.AuthIndex]
		row := accountRow{AuthIndex: f.AuthIndex, Name: redactName(f.Name, f.Email), Email: redactEmail(f.Email), Type: f.Type, Status: f.Status, Disabled: f.Disabled, RequestCount: f.Success + f.Failed + u.RequestCount, SuccessCount: f.Success + u.SuccessCount, FailedCount: f.Failed + (u.RequestCount - u.SuccessCount), TotalTokens: u.InputTokens + u.OutputTokens, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.CachedTokens, Groups: uniqueStrings(groups), EstimatedCost: costByAuth[f.AuthIndex], Quota: f.Quota}
		if u.RequestCount > 0 {
			rate := float64(u.SuccessCount) / float64(u.RequestCount) * 100
			row.SuccessRate = &rate
		}
		if row.SuccessRate == nil && row.RequestCount > 0 {
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
	// Resolve existing names first so the default behavior never overwrites a CPA auth file.
	existingNames := map[string]bool{}
	if raw, err := callHost(methodHostAuthList, map[string]any{}); err == nil {
		var list authListResponse
		if json.Unmarshal(raw, &list) == nil {
			for _, item := range list.Files {
				if item.Name != "" {
					existingNames[strings.ToLower(item.Name)] = true
				}
			}
		}
	}
	imported, skipped := 0, 0
	for i, acc := range p.accounts {
		raw, _ := json.Marshal(acc)
		name := safeFileName(acc.Email)
		if name == "" {
			name = fmt.Sprintf("cpa-view-%d.json", i+1)
		}
		name = nextAvailableName(name, existingNames)
		_, err := callHost(methodHostAuthSave, authSaveRequest{Name: name, JSON: raw})
		if err != nil {
			skipped++
			continue
		}
		existingNames[strings.ToLower(name)] = true
		imported++
	}
	return jsonResponse(http.StatusOK, map[string]any{"imported": imported, "skipped": skipped})
}

func nextAvailableName(name string, existing map[string]bool) string {
	if !existing[strings.ToLower(name)] {
		return name
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d.json", base, i)
		if !existing[strings.ToLower(candidate)] {
			return candidate
		}
	}
	return fmt.Sprintf("cpa-view-%d.json", time.Now().UnixNano())
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
	return jsonResponse(http.StatusOK, map[string]any{"range_days": days, "requests": total, "success_rate": rate, "total_tokens": inTok + outTok, "input_tokens": inTok, "output_tokens": outTok, "cached_tokens": cached, "cache_hit_rate": cacheRate(cached, inTok), "rpm": float64(total) / float64(days*24*60), "tpm": float64(inTok+outTok) / float64(days*24*60), "estimated_cost": a.costForSince(since), "token_trend": trend, "model_ranking": byModel, "account_ranking": byAccount})
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

func (a *pluginApp) loadPrices() {
	b, err := os.ReadFile(filepath.Join(a.dataDir, "model_prices.json"))
	if err == nil {
		var p map[string]modelPrice
		if json.Unmarshal(b, &p) == nil {
			a.prices = p
		}
	}
}
func (a *pluginApp) persistPrices() {
	_ = os.MkdirAll(a.dataDir, 0o700)
	b, _ := json.Marshal(a.prices)
	_ = os.WriteFile(filepath.Join(a.dataDir, "model_prices.json"), b, 0o600)
}
func (a *pluginApp) pricesResponse() (managementResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows := make([]modelPrice, 0, len(a.prices))
	for _, p := range a.prices {
		rows = append(rows, p)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
	return jsonResponse(http.StatusOK, map[string]any{"prices": rows, "source": "models.dev", "updated_at": time.Now()})
}
func (a *pluginApp) savePrice(req managementRequest) (managementResponse, error) {
	var p modelPrice
	if json.Unmarshal(req.Body, &p) != nil || strings.TrimSpace(p.Model) == "" || p.Input < 0 || p.Output < 0 || p.Cached < 0 || p.CacheWrite < 0 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "invalid_price"})
	}
	p.Model = strings.TrimSpace(p.Model)
	p.Source = "custom"
	p.UpdatedAt = time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prices[p.Model] = p
	a.persistPrices()
	return jsonResponse(http.StatusOK, p)
}
func (a *pluginApp) syncPrices() (managementResponse, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get("https://models.dev/api.json")
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "price_source_unavailable"})
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "price_source_unavailable"})
	}
	var providers map[string]struct {
		Models map[string]struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Cost struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cache_read"`
				CacheWrite float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 20<<20)).Decode(&providers); err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"error": "invalid_price_source"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for _, provider := range providers {
		for key, m := range provider.Models {
			if m.Cost.Input == 0 && m.Cost.Output == 0 {
				continue
			}
			model := m.ID
			if model == "" {
				model = key
			}
			a.prices[model] = modelPrice{Model: model, Input: m.Cost.Input, Output: m.Cost.Output, Cached: m.Cost.CacheRead, CacheWrite: m.Cost.CacheWrite, Multiplier: 1, Source: "models.dev", UpdatedAt: time.Now()}
			count++
		}
	}
	a.persistPrices()
	return jsonResponse(http.StatusOK, map[string]any{"synced": count, "source": "models.dev"})
}

func (a *pluginApp) costForUsage(u usageHourly) float64 {
	p, ok := a.prices[u.Model]
	if !ok {
		return 0
	}
	m := p.Multiplier
	if m == 0 {
		m = 1
	}
	return m * (float64(u.InputTokens)*p.Input + float64(u.OutputTokens)*p.Output + float64(u.CachedTokens)*p.Cached) / 1_000_000
}
func (a *pluginApp) costForSince(since time.Time) float64 {
	var total float64
	for _, u := range a.hourly {
		t, e := time.Parse(time.RFC3339, u.Hour)
		if e == nil && !t.Before(since) {
			total += a.costForUsage(u)
		}
	}
	return total
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
