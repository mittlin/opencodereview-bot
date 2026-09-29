# AGENTS.md — OpenCodeReview Bot

## Project Overview
Go-based AI code review bot with two entrypoints:
- **`ocr-bot`** (CLI): `github.com/alibaba/open-code-review/cmd/opencodereview/` — runs reviews locally via `ocr review`
- **`ocr-bot-server`** (HTTP): `bot.go` — GitLab webhook receiver on port 9999

Both binaries built from same codebase. Dockerfile builds both.
Upstream `internal/` and `cmd/opencodereview/` are provided via git submodule at `upstream/` (tag v1.11.7).

## Build & Run
```bash
# Initialize submodule
git submodule update --init --recursive

# Local build
go build -o ocr-bot ./cmd/opencodereview/
go build -o ocr-bot-server ./bot.go

# Docker (production)
docker build -t ocr-bot:latest .
docker run -d --name ocr-bot -p 9999:9999 \
  -e BOT_TOKEN=... -e WEBHOOK_SECRET=... -e GITLAB_URL=... \
  -e GITLAB_GROUP_TOKEN=... -e LLM_URL=... -e LLM_TOKEN=... -e LLM_MODEL=... \
  ocr-bot:latest
```

## Key Commands
| Command | Purpose |
|---------|---------|
| `ocr review --from A --to B` | Review diff range |
| `ocr review --commit SHA` | Review single commit |
| `ocr scan` | Scan all files (no diff) |
| `ocr config set key value` | Set config |
| `docker logs -f ocr-bot` | View server logs |
| `curl localhost:9999/health` | Health check |

## Required Env Vars (server)
- `BOT_TOKEN` — Bearer token for `/review` API
- `WEBHOOK_SECRET` — GitLab webhook `X-Gitlab-Token` header
- `GITLAB_URL` — GitLab API base (e.g. `http://gitlab.example.com`)
- `GITLAB_GROUP_TOKEN` — Group access token (`api` + `read_repository` scopes)
- `LLM_URL` — LLM endpoint (OpenAI-compatible)
- `LLM_TOKEN` — LLM API key
- `LLM_MODEL` — Model name

Optional: `PORT=9999`, `OCR_LANGUAGE=Chinese`, `OCR_LLM_TIMEOUT=900`, `OCR_REVIEW_TIMEOUT=60`, `OCR_PER_FILE_TIMEOUT=30`, `OCR_MAX_TOKENS_BUDGET=`, `OCR_EFFORT=`, `OCR_PROVIDER=`, `OCR_TASK_CONCURRENCY=1`, `OCR_SCAN_CONCURRENCY=1`

## Webhook Events
Single endpoint `/webhook` dispatches by `object_kind`:
- `push` → reviews `before`→`after`, posts to commit discussions
- `merge_request` (open/reopen/update) → reviews MR diff, posts to MR discussions
- `release` → reviews prev tag → current tag, posts to new tag's commit
- `issue` (open/reopen with trigger label) → queues project for nightly scan
- `note` (comment on issue) → `@ocr-bot review` triggers immediate scan (processed by background worker, 5s polling); `@ocr-bot rescan` queues for nightly scan

## Nightly Scan
Scheduled scan of all projects in a GitLab group (enabled via `OCR_GROUP_ID`):
- **Cron**: Default `0 21 * * *` (9 PM daily), scan window 21:00–08:00
- **Queue**: Priority queue with two queues (`nightly_queue`, `immediate_queue`) persisted to `/data/ocr-reviews/ocr-queue.json`; immediate queue processed by background worker (5s polling)
- **Chunking**: Large repos split into 500-file chunks, progress saved for resume
- **Per-file issues**: Each file gets its own GitLab issue with labels `ocr-result`, `nightly-scan`/`immediate-review`, `file:<name>`
- **Auto-close**: Clean re-scan (0 findings) closes all file issues for the project
- **Stale cleanup**: Files no longer in scan get their old issues closed

### Scan-Related Env Vars
| Variable | Default | Description |
|----------|---------|-------------|
| `OCR_GROUP_ID` | `""` | GitLab group ID (required to enable scans) |
| `OCR_SCAN_ENABLED` | `true` | Enable/disable nightly scans |
| `OCR_SCHEDULE_CRON` | `0 21 * * *` | Cron expression (6-field with seconds) |
| `OCR_SCAN_WINDOW_END` | `08:00` | Hard stop time (next day) |
| `OCR_SCAN_EXCLUDES` | `**/generated/*,**/testdata/*,**/vendor/*` | File exclude patterns |
| `OCR_SCAN_CHUNK_SIZE` | `500` | Files per chunk |
| `OCR_SCAN_CHUNK_TIMEOUT` | `30` | Minutes per chunk |
| `OCR_SCAN_HARD_DEADLINE_BUFFER` | `30` | Minutes before window end to stop |
| `OCR_CREATE_ISSUES` | `true` | Enable issue creation |
| `OCR_ISSUE_LABEL` | `ocr-review` | Label for created issues |
| `OCR_ISSUE_TITLE_PREFIX` | `[OCR] ` | Prefix for issue titles |
| `OCR_ISSUE_TRIGGER_LABELS` | `ocr-scan,ocr-trigger` | Labels that trigger priority queue |
| `OCR_AUTO_CLOSE_ON_CLEAN_RESCAN` | `true` | Auto-close issues on clean re-scan |
| `OCR_RESCAN_TRIGGER_PHRASE` | `@ocr-bot review` | Comment phrase to trigger re-scan |
| `OCR_TASK_CONCURRENCY` | `1` | Max concurrent review tasks across all event types |
| `OCR_SCAN_CONCURRENCY` | `1` | Concurrent file-group workers in `ocr scan` |

## scan/ Package
7 source files + 1 test file in `scan/`:

| File | Purpose |
|------|---------|
| `config.go` | `Config` struct, `LoadConfig()`, `ConfigLLM()`, `OCRHomeDir` |
| `queue.go` | `NightlyQueue`, `ImmediateQueue`, `PriorityEntry`, `CompletedEntry`, `ScanMetadata`, `LoadQueue()`, `SaveQueue()`, `PopImmediateQueue()`, `AddToImmediateQueue()` |
| `gitlab.go` | GitLab API: `ListGroupProjects`, `CreateOrUpdateFileIssue`, `FindExistingFileIssues`, `CloseStaleFileIssues`, `TriggerImmediateScan`, `TriggerRescanOnComment` |
| `handlers.go` | `IssueEvent`, `NoteEvent`, `IssueHandler`, `NoteHandler`, `ScanStatusHandler` |
| `scheduler.go` | `StartScheduler`, `runNightlyScan`, `runProjectScan`, `runImmediateScan`, `scanChunksAndReport`, `runScanChunk`, `StartImmediateQueueWorker`, `StopImmediateQueueWorker`, `processImmediateQueue` |
| `format.go` | `ReviewComment`, `ConvertComments`, `FormatCommentBody`, `FormatIssueBody`, `FormatFileIssueBody`, `SortCommentsBySeverity` |
| `ocr.go` | `RunOCR`, `AppendOCRArgs`, `BuildOCREnv` |
| `git.go` | `CloneRepo`, `InitSubmodulesWithAuth` |
| `replay_test.go` | `TestReplayScanResult` — local JSON verification test |

## Architecture Notes
- **Concurrency**: Unified semaphore via `GetTaskSemaphore()` limits concurrent reviews across all event types (push, MR, release, immediate, nightly, manual) via `OCR_TASK_CONCURRENCY` (default 1)
- **ImmediateQueue**: Separate FIFO queue with 5s polling background worker; queued immediate reviews processed when semaphore available (no longer wait for nightly cron)
- **Clone**: Uses `git clone --depth 50` with token embedded in URL (`oauth2:token@host`)
- **Review execution**: Spawns `/usr/local/bin/ocr` binary with env vars passed through
- **LLM config**: `ConfigLLM()` writes LLM settings to OCR config.json via `ocr config set`
- **Telemetry**: OpenTelemetry (OTLP gRPC) initialized in CLI (`main.go:20-22`)
- **Config**: Loaded from env vars + `config.json`
- **Severity sorting**: All display points (issues, MR comments, commit comments) sort by severity: critical → high → medium → low
- **Per-file issues**: Each scanned file gets its own GitLab issue; nightly scans use `nightly-scan` label, immediate reviews use `immediate-review` label; stale files get old issues closed
- **Summary comment**: Posted on trigger issue with markdown table of file → findings → severity → issue link

## HTTP Endpoints
| Endpoint | Auth | Description |
|----------|------|-------------|
| `POST /webhook` | `X-Gitlab-Token` | GitLab webhook dispatcher |
| `POST /review` | Bearer token | Manual review API |
| `GET /review/<id>` | Bearer token | Download review JSON report |
| `GET /health` | None | Health check |
| `GET /status` | None | Review queue status |
| `GET /scan-status` | None | Scan queue + metadata (last run, duration, next run) |

## Testing
Tests exist in `upstream/internal/*/*_test.go` and `upstream/cmd/opencodereview/*_test.go`. Scan package tests:
```bash
go test ./scan/...
```
Replay test (requires local JSON + GitLab token):
```bash
docker run --rm -v $(pwd)/scan-*.json:/data/scan.json:ro \
  -e GITLAB_URL=... -e GITLAB_GROUP_TOKEN=xxx -e GITLAB_PROJECT_ID=318 \
  ocr-bot:builder go test -run TestReplayScanResult -v ./scan/ -count=1
```

## Common Gotchas
- GitLab token must have `read_repository` scope for private repo clones
- Webhook secret must match `X-Gitlab-Token` header exactly
- LLM timeout: Qwen3.6-27B needs ~900s (`OCR_LLM_TIMEOUT`)
- Binary path in Docker is `/usr/local/bin/ocr` and `/usr/local/bin/ocr-bot-server`
- `config.json` is gitignored (contains secrets)
- Use `OCR_LLM_TOKEN` (not `OCR_LLM_AUTH_TOKEN`) for LLM authentication
- `OCR_GROUP_ID` must be set for nightly scans to start
- Scan issues use labels `ocr-result,nightly-scan,file:<name>` (not `ocr-review,nightly-scan`)
- Immediate review issues use labels `ocr-result,immediate-review,file:<name>`
- GitLab Issues API uses `"description"` field (not `"body"`)
- Queue file: `/data/ocr-reviews/ocr-queue.json` (atomic write via tmp+rename, contains both `nightly_queue` and `immediate_queue`)
- Metadata file: `/data/ocr-reviews/ocr-metadata.json`
- Immediate reviews queued to `ImmediateQueue`, processed by background worker (5s polling), not nightly cron
- `OCR_TASK_CONCURRENCY` controls all review types simultaneously (prevents LLM overload)
- `OCR_SCAN_CONCURRENCY` passed as `--concurrency` flag to `ocr scan`
