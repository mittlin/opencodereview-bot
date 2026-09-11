package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alibaba/open-code-review/internal/llm"
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

type ReviewComment struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
	Tool     string `json:"tool"`
}

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
	language = getEnvWithDefault("OCR_LANGUAGE", "Chinese")
	llmTimeout = getEnvWithDefault("OCR_LLM_TIMEOUT", "900")
	reviewTimeout = getEnvWithDefault("OCR_REVIEW_TIMEOUT", "60")
	perFileTimeout = getEnvWithDefault("OCR_PER_FILE_TIMEOUT", "30")
	maxTokensBudget = getEnvWithDefault("OCR_MAX_TOKENS_BUDGET", "")
	effort = getEnvWithDefault("OCR_EFFORT", "")
	provider = getEnvWithDefault("OCR_PROVIDER", "")

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
	// Single webhook endpoint - dispatches by object_kind
	http.HandleFunc("/webhook", webhookMiddleware(webhookHandler))

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
		func(pid int, sha string, comments []ReviewComment) {
			postCommentsToCommit(pid, sha, comments)
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
		func(pid int, sha string, comments []ReviewComment) {
			postCommentsToMR(pid, mrIID, comments)
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
		func(pid int, sha string, comments []ReviewComment) {
			postCommentsToCommit(pid, sha, comments)
		})
}

// ── Core review logic ───────────────────────────────────────────────────────

const (
	outputDir = "/data/ocr-reviews"
	homeDir   = "/data/ocr-home"
)

func runReview(ctx context.Context, req ReviewRequest) (*ReviewResponse, error) {
	var repoDir string
	var err error
	cleanup := func() {}

	if req.LocalRepo != "" {
		repoDir = req.LocalRepo
		log.Printf("Using local repo: %s", repoDir)
		safeCmd := exec.CommandContext(ctx, "git", "config", "--global", "--add", "safe.directory", repoDir)
		safeCmd.Env = append(os.Environ(), "HOME="+homeDir)
		if out, err := safeCmd.CombinedOutput(); err != nil {
			log.Printf("Warning: git config safe.directory failed: %v, output: %s", err, string(out))
		}
	} else {
		repoDir, err = cloneRepo(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("clone repo: %w", err)
		}
		cleanup = func() { os.RemoveAll(repoDir) }
	}
	defer cleanup()

	configLLM(ctx)

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

	if len(req.IncludePaths) > 0 {
		args = append(args, "--include", strings.Join(req.IncludePaths, ","))
	}
	if len(req.ExcludePaths) > 0 {
		args = append(args, "--exclude", strings.Join(req.ExcludePaths, ","))
	}

	if maxTokensBudget != "" {
		args = append(args, "--max-tokens-budget", maxTokensBudget)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if provider != "" {
		args = append(args, "--provider", provider)
	}

	cmd := exec.CommandContext(ctx, "/root/ocr-bot", args...)
	cmd.Env = append(os.Environ(),
		"OCR_LLM_URL="+llmURL,
		"OCR_LLM_TOKEN="+llmToken,
		"OCR_LLM_MODEL="+llmModel,
		"OCR_LLM_TIMEOUT="+llmTimeout,
		"HOME="+homeDir,
	)

	var stderr bytes.Buffer
	cmd.Stdout = nil
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		log.Printf("OCR stderr: %s", stderr.String())
		return nil, fmt.Errorf("ocr review failed: %w, stderr: %s", err, stderr.String())
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("read ocr output file: %w", err)
	}

	var ocrResult map[string]interface{}
	if err := json.Unmarshal(data, &ocrResult); err != nil {
		return nil, fmt.Errorf("parse ocr output file: %w, content: %s", err, string(data))
	}

	comments := convertComments(ocrResult["comments"])
	summary := ""
	if m := ocrResult["message"]; m != nil {
		summary = m.(string)
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
func runReviewAsync(projectID int, fromSHA, toSHA, projectPath, eventType, sourceBranch, targetBranch string,
	postFunc func(int, string, []ReviewComment)) {

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

	result, err := runReview(ctx, req)
	if err != nil {
		log.Printf("Review error: project=%s from=%s to=%s error=%v",
			displayProject, fromSHA, toSHA, err)
		return
	}

	postFunc(projectID, toSHA, result.Comments)
	log.Printf("Review completed: project=%s from=%s to=%s comments=%d",
		displayProject, fromSHA, toSHA, len(result.Comments))
}

func configLLM(ctx context.Context) {
	configs := map[string]string{
		"llm.url":          llmURL,
		"llm.auth_token":   llmToken,
		"llm.model":        llmModel,
		"llm.use_anthropic": "false",
		"llm.extra_body":   `{"thinking": {"type": "disabled"}}`,
		"language":         language,
	}
	for key, val := range configs {
		setCmd := exec.CommandContext(ctx, "/root/ocr-bot", "config", "set", key, val)
		setCmd.Env = append(os.Environ(),
			"OCR_LLM_URL="+llmURL,
			"OCR_LLM_TOKEN="+llmToken,
			"OCR_LLM_MODEL="+llmModel,
			"HOME="+homeDir,
		)
		if output, err := setCmd.CombinedOutput(); err != nil {
			log.Printf("Warning: failed to set %s: %v, output: %s", key, err, string(output))
		}
	}
}

// ── Git operations ──────────────────────────────────────────────────────────

func cloneRepo(ctx context.Context, req ReviewRequest) (string, error) {
	repoDir := fmt.Sprintf("/tmp/ocr-repo-%d-%d", req.ProjectID, time.Now().UnixNano())

	// Build clone URL with group token
	gitlabCloneURL := gitlabURL
	if gitlabToken != "" {
		// http://oauth2:<token>@host/group/project.git
		prefix := "http://"
		suffix := gitlabURL
		if strings.HasPrefix(gitlabURL, "https://") {
			prefix = "https://"
			suffix = strings.TrimPrefix(gitlabURL, "https://")
		} else {
			suffix = strings.TrimPrefix(gitlabURL, "http://")
		}
		gitlabCloneURL = prefix + "oauth2:" + gitlabToken + "@" + suffix
	}

	if req.ProjectPath != "" {
		gitlabCloneURL = gitlabCloneURL + "/" + req.ProjectPath + ".git"
	} else {
		gitlabCloneURL = gitlabCloneURL + "/" + fmt.Sprintf("%d.git", req.ProjectID)
	}

	// Clone main repo without --recurse-submodules to avoid submodule auth failures
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "50", "--branch", req.SourceBranch, gitlabCloneURL, repoDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone failed: %w, output: %s", err, string(output))
	}

	cmd = exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "origin", req.TargetBranch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git fetch target failed: %w, output: %s", err, string(output))
	}

	// Checkout target commit so submodules update to the correct versions
	if req.CommitSHA != "" {
		cmd = exec.CommandContext(ctx, "git", "-C", repoDir, "checkout", req.CommitSHA)
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git checkout target commit failed: %w, output: %s", err, string(output))
		}
	}

	// Init submodules with insteadOf config to inject token into submodule URLs
	if err := initSubmodulesWithAuth(ctx, repoDir); err != nil {
		log.Printf("Warning: submodule init failed: %v", err)
	}

	return repoDir, nil
}

// initSubmodulesWithAuth reads .gitmodules, builds insteadOf rules to inject
// the GitLab token into submodule URLs, and runs submodule update.
// Uses GIT_CONFIG_GLOBAL with a temp file so child processes (git clone for
// submodules) inherit the insteadOf rules.
func initSubmodulesWithAuth(ctx context.Context, repoDir string) error {
	if gitlabToken == "" {
		// No token — try plain submodule update
		cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "submodule", "update", "--init", "--recursive", "--depth", "50")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git submodule update failed: %w, output: %s", err, string(output))
		}
		logSubmoduleStatus(ctx, repoDir)
		return nil
	}

	// Parse .gitmodules to find unique (scheme, host) pairs needing auth
	hosts := parseGitmodulesHosts(repoDir)
	if len(hosts) == 0 {
		return nil
	}

	// Build a temporary git config file with insteadOf rules
	var cfg strings.Builder
	for _, h := range hosts {
		// scheme://HOST/ → scheme://oauth2:TOKEN@HOST/
		src := h.scheme + "://" + h.host + "/"
		dst := h.scheme + "://oauth2:" + gitlabToken + "@" + h.host + "/"
		fmt.Fprintf(&cfg, "[url \"%s\"]\n\tinsteadOf = %s\n", dst, src)
	}

	tmpFile, err := os.CreateTemp("", "ocr-gitconfig-*")
	if err != nil {
		return fmt.Errorf("create temp git config: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(cfg.String()); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp git config: %w", err)
	}
	tmpFile.Close()

	// GIT_CONFIG_GLOBAL makes all child processes (including git clone for
	// submodules) inherit the insteadOf rules.
	env := os.Environ()
	env = append(env, "GIT_CONFIG_GLOBAL="+tmpPath)

	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "submodule", "update", "--init", "--recursive", "--depth", "50")
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git submodule update failed: %w, output: %s", err, string(output))
	}
	logSubmoduleStatus(ctx, repoDir)
	return nil
}

// logSubmoduleStatus logs the status of all submodules after update.
func logSubmoduleStatus(ctx context.Context, repoDir string) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "submodule", "status", "--recursive")
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[submodule] status check failed: %v, output: %s", err, string(output))
	} else {
		log.Printf("[submodule] status:\n%s", string(output))
	}
}

// submoduleHost holds scheme and host for a submodule URL.
type submoduleHost struct {
	scheme string // "http" or "https"
	host   string // hostname without port/path
}

// parseGitmodulesHosts extracts unique (scheme, host) pairs from .gitmodules URLs.
func parseGitmodulesHosts(repoDir string) []submoduleHost {
	data, err := os.ReadFile(filepath.Join(repoDir, ".gitmodules"))
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var hosts []submoduleHost
	// Match: url = <scheme>://<host>[:port]/<path> or url = <scheme>://<user>@<host>[:port]/<path>
	re := regexp.MustCompile(`(?i)^\s*url\s*=\s*(\w+)://(?:[^/@]+@)?([^/:]+)`)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "url") {
			continue
		}
		matches := re.FindStringSubmatch(line)
		if len(matches) != 3 {
			continue
		}
		scheme := strings.ToLower(matches[1])
		host := matches[2]
		key := scheme + "://" + host
		if host != "" && !seen[key] {
			seen[key] = true
			hosts = append(hosts, submoduleHost{scheme: scheme, host: host})
		}
	}
	return hosts
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

func postCommentsToMR(projectID, mrIID int, comments []ReviewComment) {
	if gitlabToken == "" {
		return
	}

	for _, comment := range comments {
		url := fmt.Sprintf("%s/api/v4/projects/%d/merge_requests/%d/discussions", gitlabURL, projectID, mrIID)

		body := map[string]interface{}{
			"body": comment.Message,
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

func postCommentsToCommit(projectID int, commitSHA string, comments []ReviewComment) {
	if gitlabToken == "" {
		return
	}

	for _, comment := range comments {
		url := fmt.Sprintf("%s/api/v4/projects/%d/repository/commits/%s/discussions", gitlabURL, projectID, commitSHA)

		body := map[string]interface{}{
			"body": comment.Message,
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

func convertComments(raw interface{}) []ReviewComment {
	comments := []ReviewComment{}
	commentsList, ok := raw.([]interface{})
	if !ok {
		return comments
	}

	for _, c := range commentsList {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}

		comments = append(comments, ReviewComment{
			File:     getString(cm, "path"),
			Line:     getInt(cm, "end_line"),
			Message:  getString(cm, "content"),
			Severity: getString(cm, "severity"),
			Tool:     "code_review",
		})
	}
	return comments
}

func getEnvWithDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt(m map[string]interface{}, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	if v, ok := m[key].(int); ok {
		return v
	}
	return 0
}
