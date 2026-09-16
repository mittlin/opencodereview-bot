# Nightly Group Repository Scan & Issue Creation - Implementation Plan

## Overview
Implement a scheduled nightly task that:
1. Enumerates all projects in a GitLab group
2. Runs full-repo scan (`ocr scan`) on each project
3. Stores results in `/data/ocr-reviews/`
4. Creates GitLab Issues with findings for each project

## Interactive Priority Queue
Add webhook-driven priority system where specific events during the day boost projects in the nightly scan queue:
- **Trigger events**: `issue` (with label `ocr-scan`) via Test button in Project Hooks
- **Priority scoring**: Projects with trigger events get higher priority (FIFO by trigger time); untouched projects use polling-based scoring (last scan age, commit frequency, issue count)
- **Fallback**: If no triggers received, scan all projects ordered by computed priority
- **Scan window**: 21:00 - 08:00 (11 hours), serial execution (1 repo at a time)
- **Long-running repo handling**: Chunked scanning with progress persisted to `/data/ocr-queue.json` for resume next night

---

## Architecture Changes

### 1. New Environment Variables
```bash
# Scheduling
OCR_SCHEDULE_CRON="0 21 * * *"        # Default: 9 PM daily (start of scan window)
OCR_SCAN_WINDOW_END="08:00"           # Hard stop time (next day)
OCR_GROUP_ID=123                      # GitLab Group ID (or full path "group/subgroup")
OCR_SCAN_ENABLED=true                 # Enable scheduled scans

# Scan behavior
OCR_SCAN_PATHS=""                     # Optional: comma-separated paths to scan (default: all)
OCR_SCAN_EXCLUDES="**/generated/*,**/testdata/*,**/vendor/*"
OCR_SCAN_CHUNK_SIZE=500               # Files per chunk for large repos
OCR_SCAN_CHUNK_TIMEOUT=30             # Minutes per chunk
OCR_SCAN_HARD_DEADLINE_BUFFER=30      # Minutes before window end to stop new chunks

# Issue creation
OCR_CREATE_ISSUES=true                # Enable issue creation
OCR_ISSUE_LABEL="ocr-review"          # Label for created issues
OCR_ISSUE_TITLE_PREFIX="[OCR] "       # Prefix for issue titles
OCR_ISSUE_SEVERITY_THRESHOLD="warning" # Minimum severity to create issue (info/warning/error)

# Priority queue & triggers
OCR_ISSUE_TRIGGER_LABELS="ocr-scan,ocr-trigger"  # Labels that trigger priority queue
OCR_AUTO_CLOSE_ON_CLEAN_RESCAN=true   # Auto-close issue on clean re-scan
OCR_RESCAN_TRIGGER_PHRASE="@ocr-bot review"  # Comment phrase to trigger re-scan
```

### 2. New Data Structures (bot.go)
```go
type ScanConfig struct {
    Enabled              bool
    CronExpr             string
    WindowEnd            string        // "08:00"
    GroupID              string
    Paths                []string
    Excludes             []string
    ChunkSize            int           // Files per chunk
    ChunkTimeout         int           // Minutes
    HardDeadlineBuffer   int           // Minutes
    CreateIssues         bool
    IssueLabel           string
    IssueTitlePrefix     string
    SeverityThreshold    string
    TriggerLabels        []string
    AutoCloseOnCleanRescan bool
    RescanTriggerPhrase  string
}

type ProjectInfo struct {
    ID                int
    PathWithNamespace string
    HTTPURLToRepo     string
    DefaultBranch     string
}

type FileChunk struct {
    Index            int
    Files            []string
    FileCount        int
    EstimatedMinutes int
}

type ScanProgress struct {
    ProjectID          int
    PathWithNamespace  string
    TotalFiles         int
    CompletedChunks    []int
    PendingChunks      []FileChunk
    CurrentChunk       int
    StartedAt          time.Time
    LastUpdateAt       time.Time
    TriggerIssueIID    int
    TriggerType        string
    DefaultBranch      string
    RepoDir            string
}

type NightlyQueue struct {
    NightlyQueue       []PriorityEntry   `json:"nightly_queue"`
    InProgress         *ScanProgress     `json:"in_progress,omitempty"`
    CompletedThisNight []CompletedEntry  `json:"completed_this_night"`
}

type PriorityEntry struct {
    ProjectID           int
    PathWithNamespace   string
    TriggerTime         time.Time
    TriggerIssueIID     int
    TriggerType         string
    BasePriority        float64
}

type CompletedEntry struct {
    ProjectID      int
    PathWithNamespace string
    Findings       int
    IssueIID       int
    CompletedAt    time.Time
}

type ScanResult struct {
    Project       ProjectInfo
    OutputFile    string
    Comments      []ReviewComment
    Summary       string
    Error         error
}
```

### 3. New Functions (bot.go)

#### GitLab API: List Group Projects
```go
func listGroupProjects(ctx context.Context, groupID string) ([]ProjectInfo, error) {
    // GET /api/v4/groups/:id/projects?per_page=100&page=1&include_subgroups=true
    // Handle pagination, filter archived/disabled projects
}
```

#### GitLab API: Create/Update Issue with Deduplication
```go
func createOrUpdateScanIssue(ctx context.Context, projectID int, title, body string, labels []string, triggerIssueIID int) (int, error) {
    // 1. Search existing open issues with labels: ocr-review,nightly-scan
    //    GET /api/v4/projects/:id/issues?labels=ocr-review,nightly-scan&state=opened&per_page=10
    // 2. If found: add comment linking to new results, close old issue, create new
    // 3. If not found: create new issue
    // Returns new issue IID
}
```

#### GitLab API: Find Existing OCR Issue
```go
func findExistingOCRIssue(ctx context.Context, projectID int, labels []string) (*Issue, error) {
    // GET /api/v4/projects/:id/issues?labels=ocr-review,nightly-scan&state=opened
}
```

#### GitLab API: Add Comment to Issue
```go
func addCommentToIssue(ctx context.Context, projectID, issueIID int, body string) error {
    // POST /api/v4/projects/:id/issues/:issue_iid/notes
}
```

#### GitLab API: Close Issue
```go
func closeIssue(ctx context.Context, projectID, issueIID int) error {
    // PUT /api/v4/projects/:id/issues/:issue_iid with state_event=close
}
```

#### Queue Persistence
```go
func loadNightlyQueue() (*NightlyQueue, error) {
    // Read /data/ocr-queue.json
}

func saveNightlyQueue(queue *NightlyQueue) error {
    // Write /data/ocr-queue.json atomically
}
```

#### File Enumeration & Chunking
```go
func enumerateAndChunkFiles(ctx context.Context, repoDir string, excludes []string, chunkSize int) ([]FileChunk, error) {
    // git ls-files with excludes, split into chunks of chunkSize
}
```

#### Chunked Scan Execution
```go
func runScanChunk(ctx context.Context, project ProjectInfo, repoDir string, files []string, outputDir string) (string, []ReviewComment, string, error) {
    // 1. Run: /root/ocr-bot scan --repo <dir> --include "file1,file2,..." --format json --output <outputFile>
    // 2. Parse JSON output
    // 3. Return output file path, comments, summary, error
}
```

#### Scan with Progress Tracking
```go
func runScanWithProgress(ctx context.Context, project ProjectInfo, progress *ScanProgress, outputDir string) ([]ReviewComment, string, error) {
    // Loop through pending chunks
    // For each chunk: runScanChunk, update progress, save queue
    // Check hard deadline (08:00 - buffer)
    // Return aggregated comments & summary
}
```

#### Result to Issue Converter
```go
func commentsToIssueBody(comments []ReviewComment, summary string, projectPath, defaultBranch, triggerIssueIID int) string {
    // Format as Markdown with GitLab code links:
    // ## Summary
    // {summary}
    // **Triggered by:** Issue #123
    // ## Findings
    // ### [path/file.go:45](https://gitlab.com/group/proj/-/blob/main/path/file.go#L45) [ERROR]
    // {message}
}
```

#### Scheduled Runner
```go
func startScheduler(ctx context.Context) {
    // Parse cron expression
    // At cron tick: load queue, build priority list, run serial scans until window end
    // Save progress on each chunk completion
}
```

#### Trigger Handler (Issue Webhook)
```go
func issueHandler(w http.ResponseWriter, r *http.Request, event IssueEvent) {
    // On issue open with trigger label:
    //   - Add to nightly_queue with trigger timestamp (FIFO)
    //   - Return queue position
}
```

---

## Implementation Steps

### Phase 1: Core Infrastructure (bot.go)
1. Add new env var parsing in `main()`
2. Add `ScanConfig`, `ProjectInfo`, `FileChunk`, `ScanProgress`, `NightlyQueue`, `PriorityEntry`, `CompletedEntry` structs and load from env
3. Implement `listGroupProjects()` with pagination
4. Implement `findExistingOCRIssue()`, `createOrUpdateScanIssue()`, `addCommentToIssue()`, `closeIssue()`
5. Implement `loadNightlyQueue()`, `saveNightlyQueue()`
6. Add `IssueEvent` struct and `issueHandler()` for webhook dispatch

### Phase 2: Scan Execution with Chunking
5. Add `enumerateAndChunkFiles()` - git ls-files with excludes, split into chunks
6. Add `runScanChunk()` - single chunk scan with `--include` flag
7. Add `runScanWithProgress()` - loop chunks, update progress, save queue, check deadline
8. Handle scan-specific flags: `--path`, `--exclude`, `--no-plan`, `--max-tokens-budget`
9. Ensure submodule auth works for scan (reuse `initSubmodulesWithAuth`)

### Phase 3: Scheduler with Priority Queue
6. Add cron library (recommend: `github.com/robfig/cron/v3`)
7. Implement `startScheduler()`:
   - At cron tick: load queue, merge triggered + polling projects
   - Sort: triggered (FIFO by trigger_time) first, then polling (by base priority)
   - Serial execution: one project at a time
   - For each project: clone once, run chunked scan, persist progress
   - At 08:00 - buffer: stop new chunks, save progress, exit
   - On complete: aggregate results, create/update issue, mark completed
8. Add graceful shutdown handling (save progress on SIGTERM)

### Phase 4: Integration
7. Wire scheduler in `main()` after HTTP server starts
8. Add `/scan-status` endpoint for monitoring (queue, in-progress, completed)
9. Add health check for scheduler status
10. Register `issue` event in webhook dispatcher

### Phase 5: Testing & Validation
8. Unit tests for GitLab API functions
9. Integration test with local GitLab
10. Validate issue format, code links, deduplication, auto-close
11. Test chunked scan resume across nights

---

## GitLab API Endpoints Needed

| Operation | Endpoint | Method |
|-----------|----------|--------|
| List group projects | `/api/v4/groups/:id/projects` | GET |
| Create issue | `/api/v4/projects/:id/issues` | POST |
| Update issue | `/api/v4/projects/:id/issues/:issue_iid` | PUT |
| Close issue | `/api/v4/projects/:id/issues/:issue_iid` (state_event=close) | PUT |
| List issues (search) | `/api/v4/projects/:id/issues` | GET |
| Add issue comment | `/api/v4/projects/:id/issues/:issue_iid/notes` | POST |
| (Existing) Post commit comment | `/api/v4/projects/:id/repository/commits/:sha/discussions` | POST |
| (Existing) Post MR comment | `/api/v4/projects/:id/merge_requests/:iid/discussions` | POST |
| Get project info | `/api/v4/projects/:id` | GET |
| List releases | `/api/v4/projects/:id/releases` | GET |
| Get commit for tag | `/api/v4/projects/:id/repository/commits/:sha` | GET |

---

## New Webhook Event Structs (bot.go)

```go
type IssueEvent struct {
    ObjectKind       string `json:"object_kind"`
    ObjectAttributes struct {
        IID          int    `json:"iid"`
        Title        string `json:"title"`
        State        string `json:"state"`   // opened, closed
        Action       string `json:"action"`  // open, close, reopen, update
        Labels       []struct {
            Title string `json:"title"`
        } `json:"labels"`
        Description  string `json:"description"`
        UpdatedAt    string `json:"updated_at"`
    } `json:"object_attributes"`
    Project struct {
        ID                int    `json:"id"`
        PathWithNamespace string `json:"path_with_namespace"`
    } `json:"project"`
}

type NoteEvent struct {
    ObjectKind       string `json:"object_kind"`
    ObjectAttributes struct {
        ID          int    `json:"id"`
        Note        string `json:"note"`
        NoteableType string `json:"noteable_type"` // Issue, MergeRequest, Commit, Snippet
        NoteableID  int    `json:"noteable_id"`
    } `json:"object_attributes"`
    Project struct {
        ID                int    `json:"id"`
        PathWithNamespace string `json:"path_with_namespace"`
    } `json:"project"`
    Issue *struct {
        IID int `json:"iid"`
    } `json:"issue,omitempty"`
    MergeRequest *struct {
        IID int `json:"iid"`
    } `json:"merge_request,omitempty"`
}
```

---

## Concurrency Model

```
Scheduler (single goroutine, runs at cron tick)
  └── Load /data/ocr-queue.json
      ├── NightlyQueue (triggered projects, FIFO)
      ├── PollingQueue (all other projects, scored)
      └── InProgress (resume if exists)
  
  └── For each project (SERIAL, max 1 at a time):
       ├── If InProgress exists for project: resume from pending_chunks
       ├── Else: clone repo, enumerate files, create chunks
       ├── Loop chunks:
       │    ├── Check: now > (08:00 - buffer)? → save progress, break
       │    ├── runScanChunk(chunk.Files)
       │    ├── Update progress: completed_chunks += chunk.Index
       │    ├── saveNightlyQueue()
       │    └── Check chunk timeout
       ├── All chunks done:
       │    ├── Aggregate all chunk results
       │    ├── createOrUpdateScanIssue() with deduplication
       │    ├── Add comment to trigger issue linking to result issue
       │    ├── Move to CompletedThisNight
       │    ├── Clear InProgress
       │    └── saveNightlyQueue()
       └── Next project...
```

## Output File Naming
```
/data/ocr-reviews/
  scan-{group-slug}-{project-slug}-{timestamp}.json      # Per-chunk temp files (cleaned up)
  scan-{group-slug}-{project-slug}-{timestamp}-full.json # Aggregated final result

/data/ocr-queue.json                                     # Persistent queue state
```

---

## Issue Format (Markdown)

```markdown
## OCR Nightly Scan Report
**Project:** group/project
**Branch:** main
**Scan Time:** 2026-09-10 02:00:00 UTC
**Total Findings:** 42 (Error: 5, Warning: 20, Info: 17)
**Triggered by:** Issue #456

---

### Findings by File

#### [internal/auth/token.go:45](https://gitlab.example.com/group/project/-/blob/main/internal/auth/token.go#L45) [ERROR]
Hardcoded secret detected in source code.
```go
apiKey := "sk-1234567890abcdef"
```
**Recommendation:** Move to environment variable or secret manager.

#### [internal/db/query.go:120](https://gitlab.example.com/group/project/-/blob/main/internal/db/query.go#L120) [WARNING]
SQL query uses string concatenation, potential injection risk.
```go
query := "SELECT * FROM users WHERE name = '" + name + "'"
```
**Recommendation:** Use parameterized queries.

---

*Generated by OpenCodeReview Bot v1.11.7*
```

### Deduplication & Auto-Close Flow

1. **New scan with findings**: Search existing open issue with labels `ocr-review,nightly-scan`
   - If found: Add comment "Superseded by new scan: #<new_issue_iid>", close old issue, create new issue
   - If not found: Create new issue

2. **Re-scan triggered by comment `@ocr-bot review`**:
   - Run scan on same project
   - If 0 findings: Add comment "✅ Re-scan clean - auto-closing", close issue
   - If findings remain: Add comment "⚠️ Re-scan still shows issues", keep issue open

---

## Dependencies
- Add `github.com/robfig/cron/v3` to `go.mod`
- No other new dependencies

---

## Rollback Plan
- Feature flag `OCR_SCAN_ENABLED=false` disables entirely
- Individual repos can be skipped via `OCR_SCAN_EXCLUDES` patterns
- Issues creation can be disabled independently with `OCR_CREATE_ISSUES=false`
- Queue persistence: delete `/data/ocr-queue.json` to reset state
- Trigger labels: set `OCR_ISSUE_TRIGGER_LABELS=""` to disable priority queue

---

## Monitoring
- Add `/scan-status` endpoint returning:
  - Last run time, duration, projects scanned
  - Success/failure counts
  - Next scheduled run
  - Current queue: triggered count, polling count, in-progress project
- Log structured JSON for each project scan result (chunk level)
- Prometheus metrics (optional): `ocr_scan_duration_seconds`, `ocr_scan_findings_total`, `ocr_issues_created_total`, `ocr_queue_size`, `ocr_chunk_duration_seconds`
- `/data/ocr-queue.json` as source of truth for progress

---

## Open Questions (Resolved)

1. **Group ID format**: Numeric ID (`123`) or full path (`group/subgroup`)? → **Support both**
2. **Subgroup handling**: Include subgroups by default? → **Yes, `include_subgroups=true`**
3. **Archived projects**: Skip by default? → **Yes, filter `archived=true`**
4. **Branch to scan**: Default branch only, or all branches? → **Default branch only**
5. **Issue deduplication**: Skip if identical issue exists? → **Close old, link to new, create new**
6. **Large groups**: Rate limiting - need delay between API calls? → **Add 500ms delay between API calls**
7. **Partial failures**: Continue on individual project failure? → **Yes, log error, continue next**
8. **LLM context size**: 55k tokens → **Use `OCR_MAX_TOKENS_BUDGET` for both review & scan**
9. **Manual trigger**: Use `issue` event with label `ocr-scan` via Test button
10. **Scan window**: 21:00-08:00, serial, chunked with progress persistence
11. **Code links in issue**: Absolute URLs with line numbers
12. **Auto-close**: On clean re-scan triggered by `@ocr-bot review` comment