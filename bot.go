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
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/open-code-review/open-code-review/internal/llm"
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

	// Queue control
	reviewSemaphore = make(chan struct{}, 1) // max 1 concurrent review
	activeReviews   int32                    // current running review count
)

func main() {
	llm.AppVersion = "bot-1.0"
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

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
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
	log.Printf("Push event: project=%d branch=%s commits=%d", event.Project.ID, branch, len(event.Commits))

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

	log.Printf("MR event: project=%d MR=!%d action=%s", event.Project.ID, event.ObjectAttributes.IID, action)

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
	log.Printf("Release event: project=%d tag=%s", event.Project.ID, event.Release.TagName)

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

func runReview(ctx context.Context, req ReviewRequest) (*ReviewResponse, error) {
	var repoDir string
	var err error
	cleanup := func() {}

	if req.LocalRepo != "" {
		repoDir = req.LocalRepo
		log.Printf("Using local repo: %s", repoDir)
		safeCmd := exec.CommandContext(ctx, "git", "config", "--global", "--add", "safe.directory", repoDir)
		safeCmd.Env = append(os.Environ(), "HOME=/root")
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

	args := []string{
		"review",
		"--format", "json",
		"--audience", "agent",
		"--repo", repoDir,
		"--model", llmModel,
		"--timeout", "30",
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

	cmd := exec.CommandContext(ctx, "/root/ocr-bot", args...)
	cmd.Env = append(os.Environ(),
		"OCR_LLM_URL="+llmURL,
		"OCR_LLM_AUTH_TOKEN="+llmToken,
		"OCR_LLM_MODEL="+llmModel,
		"OCR_LLM_TIMEOUT="+llmTimeout,
		"HOME=/root",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err != nil {
		log.Printf("OCR stderr: %s", stderr.String())
		return nil, fmt.Errorf("ocr review failed: %w, stdout: %s, stderr: %s", err, stdout.String(), stderr.String())
	}

	var ocrResult map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &ocrResult); err != nil {
		// Try to extract JSON from stdout if it contains extra text
		stdoutStr := stdout.String()
		if idx := strings.Index(stdoutStr, "{"); idx >= 0 {
			if err := json.Unmarshal([]byte(stdoutStr[idx:]), &ocrResult); err == nil {
				log.Printf("Warning: extracted JSON from stdout (offset %d)", idx)
			} else {
				return nil, fmt.Errorf("parse ocr output: %w, stdout: %s, stderr: %s", err, stdoutStr, stderr.String())
			}
		} else {
			return nil, fmt.Errorf("parse ocr output: %w, stdout: %s, stderr: %s", err, stdoutStr, stderr.String())
		}
	}

	comments := convertComments(ocrResult["comments"])
	summary := ""
	if m := ocrResult["message"]; m != nil {
		summary = m.(string)
	}

	reviewID := fmt.Sprintf("ocr-%d-%d", req.ProjectID, time.Now().Unix())

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

	log.Printf("Queued review: project=%d from=%s to=%s event=%s branch=%s (active=%d)",
		projectID, fromSHA, toSHA, eventType, sourceBranch, atomic.LoadInt32(&activeReviews))

	reviewSemaphore <- struct{}{}
	atomic.AddInt32(&activeReviews, 1)
	defer func() {
		atomic.AddInt32(&activeReviews, -1)
		<-reviewSemaphore
	}()

	log.Printf("Starting review: project=%d from=%s to=%s event=%s branch=%s",
		projectID, fromSHA, toSHA, eventType, sourceBranch)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
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
		log.Printf("Review error: project=%d from=%s to=%s error=%v",
			projectID, fromSHA, toSHA, err)
		return
	}

	postFunc(projectID, toSHA, result.Comments)
	log.Printf("Review completed: project=%d from=%s to=%s comments=%d",
		projectID, fromSHA, toSHA, len(result.Comments))
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
			"OCR_LLM_AUTH_TOKEN="+llmToken,
			"OCR_LLM_MODEL="+llmModel,
			"HOME=/root",
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

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "50", "--branch", req.SourceBranch, gitlabCloneURL, repoDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone failed: %w, output: %s", err, string(output))
	}

	cmd = exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "origin", req.TargetBranch)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git fetch target failed: %w, output: %s", err, string(output))
	}

	return repoDir, nil
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
