package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	// Schema 3 is supported by CPA v7.2.135 and contains every callback used by CPA View.
	// Newer hosts remain backward compatible with a plugin registering schema 3.
	schemaVersion            = 3
	methodPluginRegister     = "plugin.register"
	methodPluginReconfigure  = "plugin.reconfigure"
	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"
	methodUsageHandle        = "usage.handle"
	methodRequestComplete    = "request.complete"
	methodHostAuthList       = "host.auth.list"
	methodHostAuthSave       = "host.auth.save"
)

type configRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}
type pluginRegistration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}
type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}
type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}
type capabilities struct {
	ManagementAPI          bool `json:"management_api,omitempty"`
	UsagePlugin            bool `json:"usage_plugin,omitempty"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin,omitempty"`
}
type managementRegistration struct {
	Routes    []managementRoute `json:"Routes,omitempty"`
	Resources []resourceRoute   `json:"Resources,omitempty"`
}
type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}
type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}
type managementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}
type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type authListResponse struct {
	Files []authEntry `json:"files"`
}
type authEntry struct {
	ID          string         `json:"id,omitempty"`
	AuthIndex   string         `json:"auth_index,omitempty"`
	Name        string         `json:"name"`
	Type        string         `json:"type,omitempty"`
	Provider    string         `json:"provider,omitempty"`
	Label       string         `json:"label,omitempty"`
	Status      string         `json:"status,omitempty"`
	Disabled    bool           `json:"disabled,omitempty"`
	Email       string         `json:"email,omitempty"`
	Success     int64          `json:"success,omitempty"`
	Failed      int64          `json:"failed,omitempty"`
	UpdatedAt   time.Time      `json:"updated_at,omitempty"`
	LastRefresh time.Time      `json:"last_refresh,omitempty"`
	Quota       map[string]any `json:"quota,omitempty"`
}
type authSaveRequest struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

type usageRecord struct {
	Model       string        `json:"Model"`
	AuthID      string        `json:"AuthID"`
	AuthIndex   string        `json:"AuthIndex"`
	RequestedAt time.Time     `json:"RequestedAt"`
	Latency     time.Duration `json:"Latency"`
	Failed      bool          `json:"Failed"`
	Detail      usageDetail   `json:"Detail"`
}
type usageDetail struct {
	InputTokens  int64 `json:"InputTokens"`
	OutputTokens int64 `json:"OutputTokens"`
	CachedTokens int64 `json:"CachedTokens"`
	TotalTokens  int64 `json:"TotalTokens"`
}

type accountRow struct {
	AuthIndex     string         `json:"auth_index"`
	Name          string         `json:"name"`
	Email         string         `json:"email,omitempty"`
	Type          string         `json:"type,omitempty"`
	Status        string         `json:"status,omitempty"`
	Disabled      bool           `json:"disabled"`
	RequestCount  int64          `json:"request_count"`
	SuccessCount  int64          `json:"success_count"`
	FailedCount   int64          `json:"failed_count"`
	SuccessRate   *float64       `json:"success_rate,omitempty"`
	TotalTokens   int64          `json:"total_tokens"`
	InputTokens   int64          `json:"input_tokens"`
	OutputTokens  int64          `json:"output_tokens"`
	CachedTokens  int64          `json:"cached_tokens"`
	LastActivity  *time.Time     `json:"last_activity,omitempty"`
	Groups        []string       `json:"groups"`
	EstimatedCost float64        `json:"estimated_cost"`
	Quota         map[string]any `json:"quota,omitempty"`
}
type usageHourly struct {
	Hour          string  `json:"hour"`
	AuthIndex     string  `json:"auth_index"`
	Model         string  `json:"model"`
	RequestCount  int64   `json:"request_count"`
	SuccessCount  int64   `json:"success_count"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CachedTokens  int64   `json:"cached_tokens"`
	LatencyMS     int64   `json:"latency_ms"`
	EstimatedCost float64 `json:"estimated_cost"`
}

type modelPrice struct {
	Model      string    `json:"model"`
	Input      float64   `json:"input"`
	Output     float64   `json:"output"`
	Cached     float64   `json:"cached"`
	CacheWrite float64   `json:"cache_write"`
	Multiplier float64   `json:"multiplier"`
	Source     string    `json:"source"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type cpaAuth struct {
	Type             string `json:"type"`
	AccessToken      string `json:"access_token,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	IDToken          string `json:"id_token,omitempty"`
	Email            string `json:"email,omitempty"`
	AccountID        string `json:"account_id,omitempty"`
	ChatGPTAccountID string `json:"chatgpt_account_id,omitempty"`
	PlanType         string `json:"plan_type,omitempty"`
	ChatGPTPlanType  string `json:"chatgpt_plan_type,omitempty"`
	Expired          string `json:"expired,omitempty"`
	LastRefresh      string `json:"last_refresh,omitempty"`
}
type sub2apiAccount struct {
	Platform    string             `json:"platform"`
	Type        string             `json:"type"`
	Credentials sub2apiCredentials `json:"credentials"`
}
type sub2apiCredentials struct {
	AccessToken      string          `json:"access_token"`
	RefreshToken     string          `json:"refresh_token"`
	IDToken          string          `json:"id_token"`
	Email            string          `json:"email"`
	ChatGPTAccountID string          `json:"chatgpt_account_id"`
	PlanType         string          `json:"plan_type"`
	ExpiresAt        json.RawMessage `json:"expires_at"`
}
type previewRequest struct {
	Filename      string `json:"filename"`
	Content       string `json:"content"`
	ContentBase64 string `json:"content_base64"`
}
type previewResponse struct {
	PreviewID  string         `json:"preview_id"`
	ExpiresAt  time.Time      `json:"expires_at"`
	Format     string         `json:"format"`
	Accounts   []redactedAuth `json:"accounts"`
	Duplicates int            `json:"duplicates"`
	Errors     []string       `json:"errors"`
}
type redactedAuth struct {
	Index     int    `json:"index"`
	Type      string `json:"type"`
	Email     string `json:"email,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
	Existing  bool   `json:"existing"`
}
