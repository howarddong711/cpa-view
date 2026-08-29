# Implementation status

## Phase 1 — complete

Created the native C ABI v1 / RPC schema v4 plugin skeleton, plugin-store manifest, CI workflow, embedded static page, and public GitHub repository.

## Phase 2 — MVP complete

Implemented CPA JSON, sub2api account/package, JSON array, NDJSON/JSONL, TXT and ZIP recognition; Codex OAuth conversion; bounded previews; duplicate fingerprints; token-safe redaction; 10-minute preview expiry; and CPA `host.auth.save` commit flow.

## Phase 3 — MVP complete

Added account-pool listing through `host.auth.list`, default `全部`/`Codex` groups, custom group create/rename/delete APIs, separate group persistence, search and group filters. Existing auth filenames are checked before commit and are never overwritten by default.

## Phase 4 — lightweight implementation

`usage.handle` aggregates hourly request/token counters into `usage_hourly.json` under the plugin data directory. This file contains statistics only; no credentials are stored. SQLite can replace this persistence layer in a later compatibility pass if the target CPA store standard requires it.

## Phase 5 — MVP complete

Added a compact dashboard with range filters, request/success/token/cache/RPM/TPM/cost cards, token trend bars, model/account rankings and a recent-activity placeholder when CPA does not provide enough detail.

## Phase 6 — initial verification

Go formatting, unit tests, `go vet`, static UI build and macOS arm64 c-shared export checks pass with Go 1.24.6. Remaining work is cross-platform release packaging, deeper host integration tests against a running CPA, and final plugin-store release metadata.

