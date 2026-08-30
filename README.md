# CPA View

CPA View is a deliberately small native CLIProxyAPI plugin for account-pool visibility, safe multi-format import, custom groups, and usage dashboards.

## Current scope

- Native C ABI v1 / RPC schema v3 registration (compatible with CPA v7.2.135).
- Management Center resource with account pool and dashboard tabs.
- The public resource embeds a redacted account/usage snapshot, so the page can
  be opened without a Management Key. Management routes remain protected by
  CPA and prompt for a key only when an operation needs fresh data or writes.
- `host.auth.list` and `host.auth.save` callbacks; the plugin never edits CPA auth files directly.
- JSON, JSON array, NDJSON/JSONL, TXT and ZIP import parsing.
- CPA native Codex auth and sub2api account conversion.
- 10 MB input, 1,000 account, JSON depth 20, ZIP path and expansion limits.
- Redacted previews with a 10 minute expiry and duplicate detection.
- Usage callback aggregation in a plugin-owned statistics file (credentials are never persisted).

## Build

Go 1.24+, Node.js 20+, npm and a CGO toolchain are required by CLIProxyAPI's native plugin ABI.

```bash
make verify
```

The local environment used to scaffold this project did not have Go on PATH; CI can build all target platforms with the workflow added in a later phase.

## Configuration

```yaml
plugins:
  configs:
    cpa-view:
      enabled: true
      priority: 20
      data_dir: data/cpa-view
      standalone_addr: ":8328"
```

Only aggregate usage and group membership are stored under `data_dir`. Raw auth JSON and tokens are held in memory only while a preview is pending.

When `standalone_addr` is set, CPA View also serves a narrowly scoped standalone
HTTP surface: redacted accounts, groups and dashboard reads, plus bounded import
preview and commit. It does not expose group, price, status, delete, or other CPA
management writes. Bind the container port to host loopback and publish it only
through a rate-limited HTTPS reverse proxy.
