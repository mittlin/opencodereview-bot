# Nightly Group Repository Scan & Issue Creation - Implementation Plan

> **Status: ✅ IMPLEMENTED** — All features below have been implemented and are running in production.
> Last updated: 2026-09-17

## Overview
Implement a scheduled nightly task that:
1. Enumerates all projects in a GitLab group
2. Runs full-repo scan (`ocr scan`) on each project
3. Stores results in `/data/ocr-reviews/`
4. Creates GitLab Issues with findings for each project (per-file issues)

## Interactive Priority Queue
Webhook-driven priority system where specific events during the day boost projects in the nightly scan queue:
- **Trigger events**: `issue` (with label `ocr-scan`/`ocr-trigger`) via webhook, `@ocr-bot rescan` comment
- **Priority scoring**: Projects with trigger events get higher priority (FIFO by trigger time); untouched projects use polling-based scoring
- **Fallback**: If no triggers received, scan all projects ordered by computed priority
- **Scan window**: 21:00 - 08:00 (11 hours), serial execution (1 repo at a time)
- **Long-running repo handling**: Chunked scanning with progress persisted to `/data/ocr-reviews/ocr-queue.json` for resume next night

---

## Architecture Changes

### 1. Environment Variables

#### Scheduling
```bash
OCR_SCHEDULE_CRON="0 21 * * *"        # Default: 9 PM daily (start of scan window)
OCR_SCAN_WINDOW_END="08:00"           # Hard stop time (next day)
OCR_GROUP_ID=123                      # GitLab Group ID (or full path "group/subgroup")
OCR_SCAN_ENABLED=true                 # Enable scheduled scans
```

#### Scan behavior
```bash
OCR_SCAN_PATHS=""                     # Optional: comma-separated paths to scan (default: all)
OCR_SCAN_EXCLUDES="**/generated/*,**/testdata/*,**/vendor/*"
OCR_SCAN_CHUNK_SIZE=500               # Files per chunk for large repos
OCR_SCAN_CHUNK_TIMEOUT=30             # Minutes per chunk
OCR_SCAN_HARD_DEADLINE_BUFFER=30      # Minutes before window end to stop new chunks
```

#### Issue creation
```bash
OCR_CREATE_ISSUES=true                # Enable issue creation
OCR_ISSUE_LABEL="ocr-review"          # Label for created issues
OCR_ISSUE_TITLE_PREFIX="[OCR] "       # Prefix for issue titles
OCR_ISSUE_SEVERITY_THRESHOLD="warning" # Minimum severity to create issue
```

#### Priority queue & triggers
```bash
OCR_ISSUE_TRIGGER_LABELS="ocr-scan,ocr-trigger"  # Labels that trigger priority queue
OCR_AUTO_CLOSE_ON_CLEAN_RESCAN=true   # Auto-close issue on clean re-scan
OCR_RESCAN_TRIGGER_PHRASE="@ocr-bot review"  # Comment phrase to trigger re-scan
```

### 2. Data Structures

```go
type Config struct {
    Enabled                bool
    CronExpr               string
    WindowEnd              string        // "08:00"
    GroupID                string
    Paths                  string
    Excludes               string
    ChunkSize              int           // Files per chunk
    ChunkTimeout           int           // Minutes
    HardDeadlineBuffer     int           // Minutes
    CreateIssues           bool
    IssueLabel             string
    IssueTitlePrefix       string
    SeverityThreshold      string
    TriggerLabels          []string
    AutoCloseOnCleanRescan bool
    RescanTriggerPhrase    string
}

type PriorityEntry struct {
    ProjectID         int
    PathWithNamespace string
    TriggerTime       time.Time
    TriggerIssueIID   int
    TriggerType       string  // "polling", "manual_issue", "manual_issue_reopen", "rescan_comment", "immediate_review"
    BasePriority      float64
}

type CompletedEntry struct {
    ProjectID         int
    PathWithNamespace string
    Findings          int
    IssueIID          int
    FileIssues        map[string]int `json:"file_issues,omitempty"` // file path → issue IID
    CompletedAt       time.Time
}

type ScanMetadata struct {
    LastRunStart       time.Time
    LastRunEnd         time.Time
    LastRunDurationSec float64
    LastRunSuccess     bool
    ProjectsScanned    int
    ProjectsSucceeded  int
    ProjectsFailed     int
    TotalFindings      int
    NextScheduledRun   time.Time
}
```

### 3. Functions

#### GitLab API (scan/gitlab.go)
- `ListGroupProjects(ctx, groupID)` — paginated, `include_subgroups=true`, `archived=false`
- `FindExistingOCRIssue(ctx, projectID, labels)` — search open issues by labels
- `FindExistingFileIssues(ctx, projectID)` — query all open `ocr-result,nightly-scan` issues
- `CreateOrUpdateFileIssue(ctx, projectID, filePath, body, triggerIssueIID)` — per-file issue with `file:<name>` label
- `CloseStaleFileIssues(ctx, projectID, keepLabels)` — close issues not in keepLabels map
- `AddCommentToIssue(ctx, projectID, issueIID, body)` — post issue note
- `CloseIssue(ctx, projectID, issueIID)` — close issue
- `CreateIssue(ctx, projectID, title, body, labels)` — create issue
- `TriggerRescanOnComment(ctx, projectID, path, issueIID)` — queue rescan with dedup
- `TriggerImmediateScan(ctx, projectID, path, issueIID)` — immediate scan with concurrency limit

#### Queue Persistence (scan/queue.go)
- `LoadQueue()` — read from `/data/ocr-reviews/ocr-queue.json`
- `SaveQueue(queue)` — atomic write (tmp + rename)
- `LoadMetadata()` / `SaveMetadata(meta)` — read/write `/data/ocr-reviews/ocr-metadata.json`

#### File Enumeration & Chunking (scan/scheduler.go)
- `enumerateAndChunkFiles(ctx, repoDir, excludes, chunkSize)` — `git ls-files` with excludes, split into chunks

#### Scan Execution (scan/scheduler.go)
- `runScanChunk(ctx, entry, repoDir, files, cfg, ...)` — single chunk scan via `ocr scan --path`
- `runProjectScan(ctx, entry, cfg, ...)` — full project scan with progress tracking and resume
- `runImmediateScan(ctx, entry, cfg, ...)` — immediate scan (no progress persistence)
- `scanChunksAndReport(ctx, entry, cfg, comments, summaries, ...)` — shared post-scan logic: per-file issues, stale cleanup, summary comment

#### Formatting (scan/format.go)
- `FormatIssueBody(comments, summary, ...)` — 3-level grouping: severity → category → file
- `FormatFileIssueBody(comments, filePath, ...)` — single-file issue body
- `SortCommentsBySeverity(comments)` — sort by severity (critical → high → medium → low)
- `FormatCommentBody(c, gitlabURL, projectPath, defaultBranch)` — rich markdown with code link, severity badge, collapsible thinking

#### OCR Execution (scan/ocr.go)
- `RunOCR(ctx, args, env, outputFile)` — execute OCR binary, parse JSON output
- `AppendOCRArgs(args, maxTokensBudget, effort, provider)` — optional CLI flags
- `BuildOCREnv(llmURL, llmToken, llmModel, extraKey, extraVal)` — environment variables

#### Git Operations (scan/git.go)
- `CloneRepo(ctx, projectID, projectPath, ...)` — clone with optional branch/fetch/checkout
- `InitSubmodulesWithAuth(ctx, repoDir, gitlabToken)` — submodule auth via insteadOf rules

### 4. Webhook Events

#### IssueEvent (scan/handlers.go)
```go
type IssueEvent struct {
    ObjectKind       string
    ObjectAttributes struct {
        IID    int
        Title  string
        State  string
        Action string  // "open", "reopen"
        Labels []struct{ Title string }
    }
    Project struct {
        ID                int
        PathWithNamespace string
    }
}
```

#### NoteEvent (scan/handlers.go)
```go
type NoteEvent struct {
    ObjectKind       string
    ObjectAttributes struct {
        ID           int
        Note         string
        NoteableType string  // "Issue"
        NoteableID   int
    }
    Project struct {
        ID                int
        PathWithNamespace string
    }
    Issue *struct {
        IID int
    }
}
```

---

## Concurrency Model

```
Scheduler (single goroutine, runs at cron tick)
  └── Load /data/ocr-reviews/ocr-queue.json
      ├── NightlyQueue (triggered projects, FIFO)
      ├── PollingQueue (all other projects, if no triggered)
      └── InProgress (resume if exists)
  
  └── For each project (SERIAL, max 1 at a time):
       ├── CompletedThisNight dedup check
       ├── If InProgress exists for project: re-clone, resume from pending_chunks
       ├── Else: clone repo, enumerate files, create chunks
       ├── Loop chunks:
       │    ├── Check: now > hard_deadline? → save progress, break
       │    ├── runScanChunk(chunk.Files)
       │    ├── Update progress: completed_chunks += chunk.Index
       │    ├── SaveQueue()
       │    └── Check chunk timeout
       ├── All chunks done:
       │    ├── scanChunksAndReport():
       │    │    ├── Group comments by file
       │    │    ├── CreateOrUpdateFileIssue() per file
       │    │    ├── CloseStaleFileIssues() for removed files
       │    │    └── Post summary comment on trigger issue
       │    ├── Move to CompletedThisNight
       │    ├── Clear InProgress
       │    └── SaveQueue()
       └── Next project...

Immediate Scan (goroutine, triggered by @ocr-bot review)
  ├── Concurrency limit: 1 (immediateScanMu)
  ├── If busy: queue with BasePriority=200
  └── Else: runImmediateScan() → same chunk flow but no progress persistence
```

---

## Output File Naming
```
/data/ocr-reviews/
  scan-{slug}-{projectID}-{timestamp}.json      # Per-chunk temp files (cleaned up)
  ocr-{slug}-{commitSHA}-{timestamp}.json       # Webhook review reports (persisted)
  ocr-queue.json                                # Persistent queue state
  ocr-metadata.json                             # Scan metadata (last run, next run, stats)
```

---

## Issue Format (Markdown)

### Per-File Issue (FormatFileIssueBody)
```markdown
## main.go

**File:** [main.go](https://gitlab.example.com/group/project/-/blob/main/main.go)
**Project:** group/project
**Scan Time:** 2026-09-16 21:30:00 UTC

**Findings:** 3 (Critical: 0, High: 1, Medium: 2, Low: 0)

---

### High (1)

#### Bug (1)

1. **[main.go:45-50](https://...)** [HIGH] (bug)

   Potential nil pointer dereference...

   **Existing Code:**
   ```go
   result := obj.Method()
   ```

   **Suggestion:**
   ```go
   if obj != nil {
       result := obj.Method()
   }
   ```

---

*Generated by OpenCodeReview Bot*
```

### Summary Comment (buildSummaryComment)
```markdown
🔗 **Scan complete** — 15 findings across 3 files

| File | Findings | Severity | Issue |
|------|----------|----------|-------|
| [main.go](link) | 5 | H:2 M:2 L:1 | #42 |
| [config.go](link) | 8 | H:1 M:4 L:3 | #43 |
| [utils.go](link) | 2 | H:0 M:1 L:1 | #44 |
```

### Deduplication & Auto-Close Flow

1. **New scan with findings**: Each file → `CreateOrUpdateFileIssue()` (find by `ocr-result,nightly-scan,file:<name>`)
   - If found: Add comment "Updated by new scan (triggered by #N)", close old, create new
   - If not found: Create new
2. **Stale cleanup**: `CloseStaleFileIssues()` — close all open `ocr-result` issues NOT in current scan
3. **Clean re-scan**: 0 findings + `AutoCloseOnCleanRescan=true` → close all file issues for project
4. **Summary comment**: Posted on trigger issue with markdown table

---

## Dependencies
- `github.com/robfig/cron/v3` v3.0.1 in `go.mod`
- No other new dependencies

---

## Open Questions (All Resolved)

1. **Group ID format**: Support both numeric and path ✓
2. **Subgroup handling**: `include_subgroups=true` ✓
3. **Archived projects**: Filter `archived=true` ✓
4. **Branch to scan**: Default branch only ✓
5. **Issue deduplication**: Close old, link to new, create new ✓
6. **Large groups**: 500ms delay between API calls ✓
7. **Partial failures**: Continue on individual project failure ✓
8. **LLM context size**: Use `OCR_MAX_TOKENS_BUDGET` ✓
9. **Manual trigger**: Issue event with label `ocr-scan` ✓
10. **Scan window**: 21:00-08:00, serial, chunked with progress persistence ✓
11. **Code links in issue**: Absolute URLs with line numbers ✓
12. **Auto-close**: On clean re-scan triggered by `@ocr-bot review` comment ✓

---

## Implementation Notes

### Key Bug Fixes Applied
1. **Removed invalid `--path` from `review` command**: `ocr review` uses `--include`/`--exclude` or diff-based selection (`--from`/`--to`/`--commit`); `--path` is only for `ocr scan`
2. **`body` → `description`**: GitLab Issues API requires `"description"` field (not `"body"`)
3. **Scan result labels**: Use `ocr-result,nightly-scan` (not `ocr-review`) to avoid re-triggering
4. **Auto-close on failure**: Only auto-close when `scanSuccessful=true` AND `len(allComments)==0`
5. **LLM config**: `ConfigLLM()` called in `runScanChunk()` to set `llm.use_anthropic=false`

### Per-File Issues Design
- Each file gets its own GitLab issue with 3 labels: `ocr-result`, `nightly-scan`, `file:<basename>`
- Benefits: granular tracking, targeted auto-close, easier review
- `CloseStaleFileIssues()` handles files removed from scan
- Summary comment on trigger issue links to all file issues

### Severity Grouping
- 3-level grouping: Severity (critical→high→medium→low) → Category (bug>security>performance>...) → File
- `SortCommentsBySeverity()` used by all display points (issues, MR comments, commit comments)
- `FormatIssueBody()` and `FormatFileIssueBody()` both use severity→category→file grouping
