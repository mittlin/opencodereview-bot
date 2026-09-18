package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alibaba/open-code-review/internal/llm"
	"github.com/alibaba/open-code-review/scan"
)

// ── Review request/response (existing) ──────────────────────────────────────

type ReviewRequest struct {
	MRIID        int      `json:"mr_iid"`
	ProjectID    int      `json:"project_id"`
	ProjectPath  string   `json:"project_path,omitempty"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	CommitSHA    string   `json:"commit_sha"`
	FromSHA      string   `json:"from_sha,omitempty"`
	IncludePaths []string `json:"include_paths,omitempty"`
	ExcludePaths []string `json:"exclude_paths,omitempty"`
	LocalRepo    string   `json:"local_repo,omitempty"`
}

type ReviewResponse struct {
	Status   string          `json:"status"`
	MRIID    int             `json:"mr_iid,omitempty"`
	Comments []ReviewComment `json:"comments,omitempty"`
	Summary  string          `json:"summary,omitempty"`
	ReviewID string          `json:"review_id,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// ReviewComment is an alias for scan.ReviewComment (single source of truth).
type ReviewComment = scan.ReviewComment

// ── GitLab Webhook event structs ────────────────────────────────────────────

type PushEvent struct {
	ObjectKind string `json:"object_kind"`
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	UserName   string `json:"user_name"`
	Project    struct {
		ID              int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	Commits []struct {
		ID string `json:"id"`
	} `json:"commits"`
}

type MergeRequestEvent struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		IID           int    `json:"iid"`
		Title         string `json:"title"`
		State         string `json:"state"`
		SourceBranch  string `json:"source_branch"`
		TargetBranch  string `json:"target_branch"`
		LastCommitSHA string `json:"last_commit_sha"`
		Action        string `json:"action"`
	} `json:"object_attributes"`
	Project struct {
		ID                int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
}

type ReleaseEvent struct {
	ObjectKind string `json:"object_kind"`
	UserName   string `json:"user_name"`
	Project    struct {
		ID                int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	Release struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"release"`
}

// ── Global config ───────────────────────────────────────────────────────────

var (
	botToken        string
	gitlabURL       string
	gitlabToken     string // Group Access Token (clone + API)
	webhookSecret   string // GitLab Webhook Secret Token (X-Gitlab-Token header)
	llmURL          string
	llmToken        string
	llmModel        string
	language        string
	llmTimeout      string
	reviewTimeout   string
	perFileTimeout  string
	maxTokensBudget string
	effort          string
	provider        string

	// Queue control
	reviewSemaphore = make(chan struct{}, 1) // max 1 concurrent review
	activeReviews   int32                    // current running review count
)

func main() {
	llm.AppVersion = "bot-1.11.7"
	llm.InitEmbeddedLoader()

	botToken = os.Getenv("BOT_TOKEN")
	gitlabURL = os.Getenv("GITLAB_URL")
	gitlabToken = os.Getenv("GITLAB_GROUP_TOKEN")
	webhookSecret = os.Getenv("WEBHOOK_SECRET")
	llmURL = os.Getenv("LLM_URL")
	llmToken = os.Getenv("LLM_TOKEN")
	llmModel = os.Getenv("LLM_MODEL")
	language = scan.GetEnvWithDefault("OCR_LANGUAGE", "Chinese")
	llmTimeout = scan.GetEnvWithDefault("OCR_LLM_TIMEOUT", "900")
	reviewTimeout = scan.GetEnvWithDefault("OCR_REVIEW_TIMEOUT", "60")
	perFileTimeout = scan.GetEnvWithDefault("OCR_PER_FILE_TIMEOUT", "30")
	maxTokensBudget = scan.GetEnvWithDefault("OCR_MAX_TOKENS_BUDGET", "")
	effort = scan.GetEnvWithDefault("OCR_EFFORT", "")
	provider = scan.GetEnvWithDefault("OCR_PROVIDER", "")

	if botToken == "" {
		log.Fatal("BOT_TOKEN is required")
	}
	if webhookSecret == "" {
		log.Fatal("WEBHOOK_SECRET is required (GitLab Webhook Secret Token)")
	}
	if gitlabURL == "" {
		log.Fatal("GITLAB_URL is required")
	}
	if llmURL == "" {
		log.Fatal("LLM_URL is required")
	}
	if llmToken == "" {
		log.Fatal("LLM_TOKEN is required")
	}
	if llmModel == "" {
		log.Fatal("LLM_MODEL is required")
	}
	if gitlabToken == "" {
		log.Println("WARNING: GITLAB_GROUP_TOKEN is empty, clone and API calls will fail for private repos")
	}

	log.Printf("Bot starting - Language: %s, LLM: %s, GitLab: %s", language, llmModel, gitlabURL)

	port := os.Getenv("PORT")
	if port == "" {
		port = "9999"
	}

	// Manual review API
	http.HandleFunc("/review", authMiddleware(reviewHandler))
	// Review report download (requires auth)
	http.HandleFunc("/review/", authMiddleware(reportHandler))
	// Health check
	http.HandleFunc("/health", healthHandler)
	// Status endpoint for queue monitoring
	http.HandleFunc("/status", statusHandler)
	// Scan status endpoint
	http.HandleFunc("/scan-status", scan.ScanStatusHandler)
	// Single webhook endpoint - dispatches by object_kind
	http.HandleFunc("/webhook", webhookMiddleware(webhookHandler))

	// Start nightly scan scheduler if enabled
	scanCfg := scan.LoadConfig()
	if scanCfg.Enabled && scanCfg.GroupID != "" {
		if err := scan.StartScheduler(context.Background(), botToken, gitlabURL, gitlabToken, llmURL, llmToken, llmModel, language, maxTokensBudget, effort, provider); err != nil {
			log.Printf("Failed to start scan scheduler: %v", err)
		}
	} else if scanCfg.Enabled {
		log.Println("Scan enabled but OCR_GROUP_ID not set, scheduler not started")
	}

	log.Printf("OpenCodeReview Bot starting on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// statusHandler returns current review queue status
func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "ok",
		"active_reviews": atomic.LoadInt32(&activeReviews),
		"queue_capacity": 1,
	})
}

// reportHandler serves the stored review report by review ID
// GET /review/<review-id>
func reportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract review ID from path: /review/<review-id>
	path := strings.TrimPrefix(r.URL.Path, "/review/")
	if path == "" || path == r.URL.Path {
		http.Error(w, "Review ID required", http.StatusBadRequest)
		return
	}

	// Sanitize review ID to prevent path traversal
	reviewID := filepath.Base(path)
	if reviewID == "." || reviewID == ".." || strings.Contains(reviewID, "/") {
		http.Error(w, "Invalid review ID", http.StatusBadRequest)
		return
	}

	reportPath := filepath.Join(outputDir, fmt.Sprintf("%s.json", reviewID))
	
	// Check if file exists
	if _, err := os.Stat(reportPath); os.IsNotExist(err) {
		http.Error(w, "Review report not found", http.StatusNotFound)
		return
	}

	// Serve the file
	w.Header().Set("Content-Type", "application/json")
	http.ServeFile(w, r, reportPath)
}

// ── Handlers ────────────────────────────────────────────────────────────────

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// authMiddleware: Bearer token for manual /review API
func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" || !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != botToken {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// webhookMiddleware: verifies X-Gitlab-Token header from GitLab webhooks
func webhookMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Gitlab-Token")
		if token == "" || token != webhookSecret {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// ── Unified webhook dispatcher ──────────────────────────────────────────────

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read body to determine event type
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Peek at object_kind
	var peek struct {
		ObjectKind string `json:"object_kind"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("Webhook received: object_kind=%s", peek.ObjectKind)

	switch peek.ObjectKind {
	case "push":
		var event PushEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "Invalid push event", http.StatusBadRequest)
			return
		}
		pushHandler(w, r, event)
	case "merge_request":
		var event MergeRequestEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "Invalid merge_request event", http.StatusBadRequest)
			return
		}
		mrHandler(w, r, event)
	case "release":
		var event ReleaseEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "Invalid release event", http.StatusBadRequest)
			return
		}
		releaseHandler(w, r, event)
	case "issue":
		var event scan.IssueEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "Invalid issue event", http.StatusBadRequest)
			return
		}
		log.Printf("Webhook received: object_kind=issue project=%s action=%s iid=%d labels=%v",
			event.Project.PathWithNamespace,
			event.ObjectAttributes.Action,
			event.ObjectAttributes.IID,
			event.ObjectAttributes.Labels)
		scan.IssueHandler(w, r, event)
	case "note":
		var event scan.NoteEvent
		if err := json.Unmarshal(body, &event); err != nil {
			http.Error(w, "Invalid note event", http.StatusBadRequest)
			return
		}
		log.Printf("Webhook received: object_kind=note project=%s noteable_type=%s noteable_id=%d",
			event.Project.PathWithNamespace,
			event.ObjectAttributes.NoteableType,
			event.ObjectAttributes.NoteableID)
		scan.NoteHandler(w, r, event)
	default:
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "unsupported event: " + peek.ObjectKind})
	}
}

// ── Manual /review endpoint ─────────────────────────────────────────────────

func reviewHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if req.ProjectID == 0 {
		http.Error(w, "Missing required field: project_id", http.StatusBadRequest)
		return
	}

	reviewTimeoutMin, _ := strconv.Atoi(reviewTimeout)
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(reviewTimeoutMin)*time.Minute)
	defer cancel()

	result, err := runReview(ctx, req)
	if err != nil {
		log.Printf("Review error: %v", err)
		json.NewEncoder(w).Encode(ReviewResponse{
			Status: "error",
			Error:  err.Error(),
		})
		return
	}

	result.Comments = scan.SortCommentsBySeverity(result.Comments)
	json.NewEncoder(w).Encode(result)
}

// ── Webhook: Push events ────────────────────────────────────────────────────

func pushHandler(w http.ResponseWriter, r *http.Request, event PushEvent) {
	// Skip branch deletions
	if event.After == "0000000000000000000000000000000000000000" {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "branch deletion"})
		return
	}

	branch := strings.TrimPrefix(event.Ref, "refs/heads/")
	log.Printf("Push event: project=%s branch=%s commits=%d", event.Project.PathWithNamespace, branch, len(event.Commits))

	// Immediately return 202 Accepted
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "accepted", "event": "push",
		"commit": event.After, "from": event.Before,
	})

	// Async: review from before to after (entire push range)
	go runReviewAsync(event.Project.ID, event.Before, event.After,
		event.Project.PathWithNamespace, "push", branch, branch,
		func(pid int, sha string, comments []ReviewComment, repoDir string) {
			// Build file -> commit map for distributing comments
			ctx := context.Background()
			fileToCommit, err := buildFileToCommitMap(ctx, repoDir, event.Before, event.After)
			if err != nil {
				log.Printf("Warning: failed to build file-commit map: %v, falling back to after SHA", err)
				postCommentsToCommit(pid, sha, comments, event.Project.PathWithNamespace, branch)
				return
			}

			// Group comments by target commit
			commentsByCommit := make(map[string][]ReviewComment)
			for _, c := range comments {
				targetSHA := fileToCommit[c.File]
				if targetSHA == "" {
					targetSHA = sha // fallback to after SHA
					log.Printf("Warning: no commit mapping for file %s, using after SHA", c.File)
				}
				commentsByCommit[targetSHA] = append(commentsByCommit[targetSHA], c)
			}

			// Post to each commit
			for commitSHA, cs := range commentsByCommit {
				postCommentsToCommit(pid, commitSHA, cs, event.Project.PathWithNamespace, branch)
			}
		})
}

// ── Webhook: Merge Request events ───────────────────────────────────────────

func mrHandler(w http.ResponseWriter, r *http.Request, event MergeRequestEvent) {
	// Only review on open/reopen/sync events
	action := event.ObjectAttributes.Action
	if action != "open" && action != "reopen" && action != "update" {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "action=" + action})
		return
	}

	log.Printf("MR event: project=%s MR=!%d action=%s", event.Project.PathWithNamespace, event.ObjectAttributes.IID, action)

	// Immediately return 202 Accepted
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "accepted", "event": "merge_request",
		"mr_iid": event.ObjectAttributes.IID,
	})

	mrIID := event.ObjectAttributes.IID
	// MR: review last commit (commit mode)
	go runReviewAsync(event.Project.ID, "", event.ObjectAttributes.LastCommitSHA,
		event.Project.PathWithNamespace, "merge_request",
		event.ObjectAttributes.SourceBranch, event.ObjectAttributes.TargetBranch,
		func(pid int, sha string, comments []ReviewComment, repoDir string) {
			postCommentsToMR(pid, mrIID, comments, event.Project.PathWithNamespace, event.ObjectAttributes.TargetBranch)
		})
}

// ── Webhook: Release events ─────────────────────────────────────────────────

func releaseHandler(w http.ResponseWriter, r *http.Request, event ReleaseEvent) {
	log.Printf("Release event: project=%s tag=%s", event.Project.PathWithNamespace, event.Release.TagName)

	// Get previous release tag (fast API call, not in async)
	prevTag, err := getPrevReleaseTag(event.Project.ID, event.Release.TagName)
	if err != nil {
		log.Printf("Failed to get previous release tag: %v", err)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": fmt.Sprintf("get prev release: %v", err)})
		return
	}

	// Get commit SHA for the new release tag
	tagCommitSHA, err := getTagCommitSHA(event.Project.ID, event.Release.TagName)
	if err != nil {
		log.Printf("Failed to get tag commit SHA: %v", err)
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": fmt.Sprintf("get tag commit: %v", err)})
		return
	}

	log.Printf("Release diff: %s -> %s", prevTag, event.Release.TagName)

	// Immediately return 202 Accepted
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "accepted", "event": "release",
		"tag": event.Release.TagName, "prev_tag": prevTag,
	})

	// Async: review from prevTag to tagCommitSHA
	go runReviewAsync(event.Project.ID, prevTag, tagCommitSHA,
		event.Project.PathWithNamespace, "release",
		event.Release.TagName, event.Release.TagName,
		func(pid int, sha string, comments []ReviewComment, repoDir string) {
			postCommentsToCommit(pid, sha, comments, event.Project.PathWithNamespace, event.Release.TagName)
		})
}

// ── Core review logic ───────────────────────────────────────────────────────

const (
	outputDir = "/data/ocr-reviews"
)

func runReview(ctx context.Context, req ReviewRequest) (*ReviewResponse, error) {
	var repoDir string
	var err error
	var cleanup func()

	if req.LocalRepo != "" {
		repoDir = req.LocalRepo
		cleanup = func() {}
	} else {
		repoDir, err = scan.CloneRepo(ctx, req.ProjectID, req.ProjectPath, gitlabURL, gitlabToken, req.SourceBranch, req.TargetBranch, req.CommitSHA)
		if err != nil {
			return nil, fmt.Errorf("clone repo: %w", err)
		}
		cleanup = func() { os.RemoveAll(repoDir) }
	}
	defer cleanup()

	return runReviewWithRepoDir(ctx, req, repoDir)
}

// runReviewWithRepoDir executes review using an already-prepared repoDir.
// Caller is responsible for repoDir lifecycle (clone/cleanup).
func runReviewWithRepoDir(ctx context.Context, req ReviewRequest, repoDir string) (*ReviewResponse, error) {
	// Ensure safe.directory for local repos
	if req.LocalRepo != "" {
		safeCmd := exec.CommandContext(ctx, "git", "config", "--global", "--add", "safe.directory", repoDir)
		safeCmd.Env = append(os.Environ(), "HOME="+scan.OCRHomeDir)
		if out, err := safeCmd.CombinedOutput(); err != nil {
			log.Printf("Warning: git config safe.directory failed: %v, output: %s", err, string(out))
		}
	}

	scan.ConfigLLM(ctx)

	// Generate review ID with project path and commit SHA for readability
	projectPath := req.ProjectPath
	if projectPath == "" {
		projectPath = fmt.Sprintf("project-%d", req.ProjectID)
	}
	// Sanitize path for filename (replace / with -)
	safeProjectPath := strings.ReplaceAll(projectPath, "/", "-")
	// Include commit SHA (short) in filename
	shortCommit := req.CommitSHA
	if len(shortCommit) > 8 {
		shortCommit = shortCommit[:8]
	}
	reviewID := fmt.Sprintf("ocr-%s-%s-%d", safeProjectPath, shortCommit, time.Now().Unix())
	outputPath := filepath.Join(outputDir, fmt.Sprintf("%s.json", reviewID))

	args := []string{
		"review",
		"--format", "json",
		"--audience", "agent",
		"--repo", repoDir,
		"--model", llmModel,
		"--timeout", perFileTimeout,
		"--output", outputPath,
	}
	// Priority: FromSHA+CommitSHA > CommitSHA > from target branch
	if req.FromSHA != "" && req.CommitSHA != "" {
		args = append(args, "--from", req.FromSHA, "--to", req.CommitSHA)
	} else if req.CommitSHA != "" {
		args = append(args, "--commit", req.CommitSHA)
	} else {
		args = append(args, "--from", "origin/"+req.TargetBranch, "--to", req.CommitSHA)
	}

	if len(req.ExcludePaths) > 0 {
		args = append(args, "--exclude", strings.Join(req.ExcludePaths, ","))
	}

	args = scan.AppendOCRArgs(args, maxTokensBudget, effort, provider)

	env := scan.BuildOCREnv(llmURL, llmToken, llmModel, "OCR_LLM_TIMEOUT", llmTimeout)
	comments, summary, err := scan.RunOCR(ctx, args, env, outputPath)
	if err != nil {
		return nil, err
	}

	return &ReviewResponse{
		Status:   "success",
		MRIID:    req.MRIID,
		Comments: comments,
		Summary:  summary,
		ReviewID: reviewID,
	}, nil
}

// runReviewAsync executes review in background with concurrency control.
// postFunc receives: projectID, commitSHA, comments, repoDir
func runReviewAsync(projectID int, fromSHA, toSHA, projectPath, eventType, sourceBranch, targetBranch string,
	postFunc func(int, string, []ReviewComment, string)) {

	displayProject := projectPath
	if displayProject == "" {
		displayProject = fmt.Sprintf("%d", projectID)
	}

	log.Printf("Queued review: project=%s from=%s to=%s event=%s branch=%s (active=%d)",
		displayProject, fromSHA, toSHA, eventType, sourceBranch, atomic.LoadInt32(&activeReviews))

	reviewSemaphore <- struct{}{}
	atomic.AddInt32(&activeReviews, 1)
	defer func() {
		atomic.AddInt32(&activeReviews, -1)
		<-reviewSemaphore
	}()

	log.Printf("Starting review: project=%s from=%s to=%s event=%s branch=%s",
		displayProject, fromSHA, toSHA, eventType, sourceBranch)

	reviewTimeoutMin, _ := strconv.Atoi(reviewTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(reviewTimeoutMin)*time.Minute)
	defer cancel()

	req := ReviewRequest{
		ProjectID:    projectID,
		ProjectPath:  projectPath,
		CommitSHA:    toSHA,
		FromSHA:      fromSHA,
		SourceBranch: sourceBranch,
		TargetBranch: targetBranch,
	}

	// Determine repoDir before calling runReview so we can pass it to callback
	var repoDir string
	var err error
	var cleanup func()
	if req.LocalRepo != "" {
		repoDir = req.LocalRepo
		cleanup = func() {}
	} else {
		repoDir, err = scan.CloneRepo(ctx, projectID, projectPath, gitlabURL, gitlabToken, sourceBranch, targetBranch, toSHA)
		if err != nil {
			log.Printf("Review error: project=%s from=%s to=%s clone failed: %v",
				displayProject, fromSHA, toSHA, err)
			return
		}
		cleanup = func() { os.RemoveAll(repoDir) }
	}
	defer cleanup()

	result, err := runReviewWithRepoDir(ctx, req, repoDir)
	if err != nil {
		log.Printf("Review error: project=%s from=%s to=%s error=%v",
			displayProject, fromSHA, toSHA, err)
		return
	}

	postFunc(projectID, toSHA, result.Comments, repoDir)
	log.Printf("Review completed: project=%s from=%s to=%s comments=%d",
		displayProject, fromSHA, toSHA, len(result.Comments))
}

// ── Git operations ──────────────────────────────────────────────────────────

// buildFileToCommitMap runs git log --name-only from..to and returns
// a map of filePath -> lastCommitSHA that modified that file in the range.
// Only considers Added, Modified, Deleted, Renamed files (--diff-filter=AMDR).
func buildFileToCommitMap(ctx context.Context, repoDir, fromSHA, toSHA string) (map[string]string, error) {
	// Handle first push (fromSHA is zero SHA) - use all commits up to toSHA
	var logRange string
	zeroSHA := "0000000000000000000000000000000000000000"
	if fromSHA == zeroSHA {
		logRange = toSHA
	} else {
		logRange = fromSHA + ".." + toSHA
	}

	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "log", "--name-only", "--pretty=format:%H", "--diff-filter=AMDR", "--", logRange)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git log failed: %w, output: %s", err, string(output))
	}

	fileToCommit := make(map[string]string)
	var currentCommit string

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Commit SHA is 40 hex chars
		if len(line) == 40 && isHexString(line) {
			currentCommit = line
			continue
		}
		// File path
		if currentCommit != "" {
			fileToCommit[line] = currentCommit
		}
	}

	return fileToCommit, nil
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func getTagCommitSHA(projectID int, tagName string) (string, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%d/repository/commits/%s", gitlabURL, projectID, tagName)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("PRIVATE-TOKEN", gitlabToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("get commit for tag %s: %w", tagName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get commit for tag %s: status %d: %s", tagName, resp.StatusCode, string(body))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode commit response: %w", err)
	}

	return result.ID, nil
}

func getPrevReleaseTag(projectID int, currentTag string) (string, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%d/releases", gitlabURL, projectID)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("PRIVATE-TOKEN", gitlabToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("list releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("list releases: status %d: %s", resp.StatusCode, string(body))
	}

	var releases []struct {
		TagName    string `json:"tag_name"`
		ReleasedAt string `json:"released_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", fmt.Errorf("decode releases: %w", err)
	}

	// Sort by released_at descending
	sort.Slice(releases, func(i, j int) bool {
		return releases[i].ReleasedAt > releases[j].ReleasedAt
	})

	// Find current tag and return the one before it
	for i, rel := range releases {
		if rel.TagName == currentTag && i+1 < len(releases) {
			return releases[i+1].TagName, nil
		}
	}

	// Fallback: if only one release, use the tag before currentTag
	// Try git-based fallback
	return fmt.Sprintf("%s~1", currentTag), nil
}

// ── GitLab API: post comments ──────────────────────────────────────────────

func postCommentsToMR(projectID, mrIID int, comments []ReviewComment, projectPath, defaultBranch string) {
	if gitlabToken == "" {
		return
	}

	comments = scan.SortCommentsBySeverity(comments)
	for _, comment := range comments {
		url := fmt.Sprintf("%s/api/v4/projects/%d/merge_requests/%d/discussions", gitlabURL, projectID, mrIID)

		body := map[string]interface{}{
			"body": scan.FormatCommentBody(comment, gitlabURL, projectPath, defaultBranch),
			"position": map[string]interface{}{
				"position_type": "text",
				"new_path":      comment.File,
				"old_path":      comment.File,
				"new_line":      comment.Line,
			},
		}

		jsonBody, _ := json.Marshal(body)
		httpReq, _ := http.NewRequest("POST", url, strings.NewReader(string(jsonBody)))
		httpReq.Header.Set("PRIVATE-TOKEN", gitlabToken)
		httpReq.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(httpReq)
		if err != nil {
			log.Printf("Failed to post MR comment: %v", err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func postCommentsToCommit(projectID int, commitSHA string, comments []ReviewComment, projectPath, defaultBranch string) {
	if gitlabToken == "" {
		return
	}

	comments = scan.SortCommentsBySeverity(comments)
	for _, comment := range comments {
		url := fmt.Sprintf("%s/api/v4/projects/%d/repository/commits/%s/discussions", gitlabURL, projectID, commitSHA)

		body := map[string]interface{}{
			"body": scan.FormatCommentBody(comment, gitlabURL, projectPath, defaultBranch),
		}

		jsonBody, _ := json.Marshal(body)
		httpReq, _ := http.NewRequest("POST", url, strings.NewReader(string(jsonBody)))
		httpReq.Header.Set("PRIVATE-TOKEN", gitlabToken)
		httpReq.Header.Set("Content-Type", "application/json")

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(httpReq)
		if err != nil {
			log.Printf("Failed to post commit comment: %v", err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// ── Utilities ───────────────────────────────────────────────────────────────
