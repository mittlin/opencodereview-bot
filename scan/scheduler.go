package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
)

var (
	schedulerMu     sync.Mutex
	schedulerRunning bool
	scanCancelFunc  context.CancelFunc
)

func StartScheduler(ctx context.Context, botToken, gitlabURLVal, gitlabTokenVal, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal string) error {
	schedulerMu.Lock()
	if schedulerRunning {
		schedulerMu.Unlock()
		return fmt.Errorf("scheduler already running")
	}
	schedulerRunning = true
	schedulerMu.Unlock()

	InitGitLabClient(gitlabURLVal, gitlabTokenVal, LoadConfig())

	c := cron.New(cron.WithSeconds())
	_, err := c.AddFunc(LoadConfig().CronExpr, func() {
		runNightlyScan(botToken, gitlabURLVal, gitlabTokenVal, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal)
	})
	if err != nil {
		schedulerMu.Lock()
		schedulerRunning = false
		schedulerMu.Unlock()
		return fmt.Errorf("add cron job: %w", err)
	}

	c.Start()
	log.Printf("Nightly scan scheduler started with cron: %s", LoadConfig().CronExpr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Println("Shutdown signal received, stopping scheduler...")
		c.Stop()
		if scanCancelFunc != nil {
			scanCancelFunc()
		}
		schedulerMu.Lock()
		schedulerRunning = false
		schedulerMu.Unlock()
	}()

	return nil
}

func runNightlyScan(botToken, gitlabURLVal, gitlabTokenVal, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal string) {
	log.Println("Starting nightly scan run")

	ctx, cancel := context.WithCancel(context.Background())
	scanCancelFunc = cancel
	defer func() {
		scanCancelFunc = nil
	}()

	cfg := LoadConfig()
	windowEnd := parseWindowEnd(cfg.WindowEnd)
	hardDeadline := time.Now().AddDate(0, 0, 1).Truncate(24*time.Hour).Add(windowEnd).Add(-time.Duration(cfg.HardDeadlineBuffer) * time.Minute)

	if time.Now().After(hardDeadline) {
		log.Printf("Past hard deadline (%v), skipping scan", hardDeadline)
		return
	}

	queue, err := LoadQueue()
	if err != nil {
		log.Printf("Failed to load queue: %v", err)
		return
	}

	if len(queue.NightlyQueue) == 0 && queue.InProgress == nil {
		projects, err := ListGroupProjects(ctx, cfg.GroupID)
		if err != nil {
			log.Printf("Failed to list group projects: %v", err)
			return
		}
		for _, p := range projects {
			queue.NightlyQueue = append(queue.NightlyQueue, PriorityEntry{
				ProjectID:         p.ID,
				PathWithNamespace: p.PathWithNamespace,
				TriggerTime:       time.Now(),
				TriggerType:       "polling",
				BasePriority:      0,
			})
		}
		log.Printf("No triggered projects, added %d polling projects", len(projects))
	}

	for i := 0; i < len(queue.NightlyQueue); i++ {
		select {
		case <-ctx.Done():
			log.Println("Scan cancelled")
			return
		default:
		}

		if time.Now().After(hardDeadline) {
			log.Printf("Hard deadline reached (%v), saving progress and stopping", hardDeadline)
			break
		}

		entry := queue.NightlyQueue[i]
		log.Printf("Processing project: %s (trigger: %s)", entry.PathWithNamespace, entry.TriggerType)

		progress, err := runProjectScan(ctx, entry, cfg, botToken, gitlabURLVal, gitlabTokenVal, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal)
		if err != nil {
			log.Printf("Project scan failed: %v", err)
			continue
		}

		queue.InProgress = nil
		queue.CompletedThisNight = append(queue.CompletedThisNight, CompletedEntry{
			ProjectID:         entry.ProjectID,
			PathWithNamespace: entry.PathWithNamespace,
			Findings:          len(progress.Comments),
			IssueIID:          progress.IssueIID,
			CompletedAt:       time.Now(),
		})
		queue.NightlyQueue = append(queue.NightlyQueue[:i], queue.NightlyQueue[i+1:]...)
		i--

		if err := SaveQueue(queue); err != nil {
			log.Printf("Failed to save queue after completion: %v", err)
		}
	}

	log.Println("Nightly scan run completed")
}

func parseWindowEnd(windowEnd string) time.Duration {
	parts := strings.Split(windowEnd, ":")
	if len(parts) != 2 {
		return 8 * time.Hour
	}
	h, _ := time.ParseDuration(parts[0] + "h")
	m, _ := time.ParseDuration(parts[1] + "m")
	return h + m
}

type scanProgressResult struct {
	Comments  []ReviewComment
	Summary   string
	IssueIID  int
}

func runProjectScan(ctx context.Context, entry PriorityEntry, cfg *Config, botToken, gitlabURLVal, gitlabTokenVal, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal string) (*scanProgressResult, error) {
	queue, err := LoadQueue()
	if err != nil {
		return nil, err
	}

	var repoDir string
	var chunks []FileChunk
	var totalFiles int
	var completedChunks []int

	if queue.InProgress != nil && queue.InProgress.ProjectID == entry.ProjectID {
		repoDir = queue.InProgress.RepoDir
		chunks = queue.InProgress.PendingChunks
		totalFiles = queue.InProgress.TotalFiles
		completedChunks = queue.InProgress.CompletedChunks
		log.Printf("Resuming project %s: %d/%d chunks done", entry.PathWithNamespace, len(completedChunks), len(chunks)+len(completedChunks))
	} else {
		log.Printf("Cloning repository for %s", entry.PathWithNamespace)
		repoDir, err = cloneRepo(ctx, entry.ProjectID, entry.PathWithNamespace, gitlabURLVal, gitlabTokenVal)
		if err != nil {
			return nil, fmt.Errorf("clone repo: %w", err)
		}
		defer os.RemoveAll(repoDir)

		chunks, err = enumerateAndChunkFiles(ctx, repoDir, strings.Split(cfg.Excludes, ","), cfg.ChunkSize)
		if err != nil {
			return nil, fmt.Errorf("enumerate files: %w", err)
		}
		totalFiles = 0
		for _, c := range chunks {
			totalFiles += c.FileCount
		}
		log.Printf("Project %s: %d files in %d chunks", entry.PathWithNamespace, totalFiles, len(chunks))
	}

	var allComments []ReviewComment
	var allSummaries []string
	windowEnd := parseWindowEnd(cfg.WindowEnd)
	hardDeadline := time.Now().AddDate(0, 0, 1).Truncate(24*time.Hour).Add(windowEnd).Add(-time.Duration(cfg.HardDeadlineBuffer) * time.Minute)

	for idx, chunk := range chunks {
		if idx < len(completedChunks) {
			continue
		}

		if time.Now().After(hardDeadline) {
			log.Printf("Hard deadline reached, saving progress at chunk %d/%d", idx, len(chunks))
			queue.InProgress = &ScanProgress{
				ProjectID:          entry.ProjectID,
				PathWithNamespace:  entry.PathWithNamespace,
				TotalFiles:         totalFiles,
				CompletedChunks:    completedChunks,
				PendingChunks:      chunks[idx:],
				CurrentChunk:       idx,
				StartedAt:          time.Now(),
				LastUpdateAt:       time.Now(),
				TriggerIssueIID:    entry.TriggerIssueIID,
				TriggerType:        entry.TriggerType,
				DefaultBranch:      "main",
				RepoDir:            repoDir,
			}
			if err := SaveQueue(queue); err != nil {
				log.Printf("Failed to save progress: %v", err)
			}
			return &scanProgressResult{Comments: allComments}, nil
		}

		log.Printf("Scanning chunk %d/%d (%d files)", idx+1, len(chunks), chunk.FileCount)
		comments, summary, err := runScanChunk(ctx, entry, repoDir, chunk.Files, cfg, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal)
		if err != nil {
			log.Printf("Chunk %d failed: %v", idx, err)
			continue
		}

		allComments = append(allComments, comments...)
		if summary != "" {
			allSummaries = append(allSummaries, summary)
		}
		completedChunks = append(completedChunks, idx)

		queue.InProgress = &ScanProgress{
			ProjectID:         entry.ProjectID,
			PathWithNamespace: entry.PathWithNamespace,
			TotalFiles:        totalFiles,
			CompletedChunks:   completedChunks,
			PendingChunks:     chunks[idx+1:],
			CurrentChunk:      idx + 1,
			StartedAt:         time.Now(),
			LastUpdateAt:      time.Now(),
			TriggerIssueIID:   entry.TriggerIssueIID,
			TriggerType:       entry.TriggerType,
			DefaultBranch:     "main",
			RepoDir:           repoDir,
		}
		if err := SaveQueue(queue); err != nil {
			log.Printf("Failed to save progress: %v", err)
		}
	}

	aggregatedSummary := strings.Join(allSummaries, "\n\n")
	title := fmt.Sprintf("%sNightly Scan Report - %s", cfg.IssueTitlePrefix, entry.PathWithNamespace)
	body := buildIssueBody(allComments, aggregatedSummary, entry.PathWithNamespace, "main", entry.TriggerIssueIID)

	issueIID := 0
	if cfg.CreateIssues && len(allComments) > 0 {
		issueIID, err = CreateOrUpdateScanIssue(ctx, entry.ProjectID, title, body, []string{}, entry.TriggerIssueIID)
		if err != nil {
			log.Printf("Failed to create issue: %v", err)
		} else {
			log.Printf("Created scan result issue #%d for %s", issueIID, entry.PathWithNamespace)
			if entry.TriggerIssueIID > 0 {
				linkMsg := fmt.Sprintf("🔗 Scan results available in issue #%d", issueIID)
				AddCommentToIssue(ctx, entry.ProjectID, entry.TriggerIssueIID, linkMsg)
			}
		}
	} else if len(allComments) == 0 && entry.TriggerIssueIID > 0 {
		msg := "✅ Nightly scan completed - no issues found"
		AddCommentToIssue(ctx, entry.ProjectID, entry.TriggerIssueIID, msg)
	}

	return &scanProgressResult{
		Comments: allComments,
		Summary:  aggregatedSummary,
		IssueIID: issueIID,
	}, nil
}

func buildIssueBody(comments []ReviewComment, summary, projectPath, defaultBranch string, triggerIssueIID int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("## Summary\n%s\n\n", summary))
	if triggerIssueIID > 0 {
		b.WriteString(fmt.Sprintf("**Triggered by:** Issue #%d\n\n", triggerIssueIID))
	}
	b.WriteString(fmt.Sprintf("**Project:** %s\n", projectPath))
	b.WriteString(fmt.Sprintf("**Branch:** %s\n", defaultBranch))
	b.WriteString(fmt.Sprintf("**Scan Time:** %s UTC\n\n", time.Now().Format("2006-01-02 15:04:05")))

	errorCount, warningCount, infoCount := 0, 0, 0
	for _, c := range comments {
		switch strings.ToLower(c.Severity) {
		case "error":
			errorCount++
		case "warning":
			warningCount++
		default:
			infoCount++
		}
	}
	b.WriteString(fmt.Sprintf("**Total Findings:** %d (Error: %d, Warning: %d, Info: %d)\n\n", len(comments), errorCount, warningCount, infoCount))
	b.WriteString("---\n\n")
	b.WriteString("## Findings\n\n")

	for _, c := range comments {
		codeLink := fmt.Sprintf("[%s:%d](%s/%s/-/blob/%s/%s#L%d)",
			c.File, c.Line, "https://gitlab.example.com", projectPath, defaultBranch, c.File, c.Line)
		b.WriteString(fmt.Sprintf("### %s [%s]\n%s\n\n", codeLink, c.Severity, c.Message))
	}
	b.WriteString("---\n*Generated by OpenCodeReview Bot*")
	return b.String()
}

func cloneRepo(ctx context.Context, projectID int, projectPath, gitlabURL, gitlabToken string) (string, error) {
	repoDir := fmt.Sprintf("/tmp/ocr-repo-%d-%d", projectID, time.Now().UnixNano())

	gitlabCloneURL := gitlabURL
	if gitlabToken != "" {
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

	gitlabCloneURL = gitlabCloneURL + "/" + projectPath + ".git"

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "50", gitlabCloneURL, repoDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone failed: %w, output: %s", err, string(output))
	}

	if err := initSubmodulesWithAuth(ctx, repoDir, gitlabToken); err != nil {
		log.Printf("Warning: submodule init failed: %v", err)
	}

	return repoDir, nil
}

func enumerateAndChunkFiles(ctx context.Context, repoDir string, excludes []string, chunkSize int) ([]FileChunk, error) {
	args := []string{"-C", repoDir, "ls-files"}
	for _, excl := range excludes {
		excl = strings.TrimSpace(excl)
		if excl != "" {
			args = append(args, fmt.Sprintf(":(exclude)%s", excl))
		}
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git ls-files failed: %w, output: %s", err, string(output))
	}

	files := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(files) == 1 && files[0] == "" {
		return []FileChunk{}, nil
	}

	var chunks []FileChunk
	for i := 0; i < len(files); i += chunkSize {
		end := i + chunkSize
		if end > len(files) {
			end = len(files)
		}
		chunks = append(chunks, FileChunk{
			Index:       len(chunks),
			Files:       files[i:end],
			FileCount:   end - i,
			EstimatedMinutes: (end - i) / 50,
		})
	}
	return chunks, nil
}

func runScanChunk(ctx context.Context, entry PriorityEntry, repoDir string, files []string, cfg *Config, llmURL, llmToken, llmModel, language, maxTokensBudget, effort, provider string) ([]ReviewComment, string, error) {
	// For chunked scanning, we use --include to specify files
	// Also pass the global excludes from config
	includeArg := strings.Join(files, ",")
	outputFile := filepath.Join("/data/ocr-reviews", fmt.Sprintf("scan-%s-%d-%d.json",
		strings.ReplaceAll(entry.PathWithNamespace, "/", "-"), entry.ProjectID, time.Now().Unix()))

	args := []string{
		"scan",
		"--repo", repoDir,
		"--include", includeArg,
		"--format", "json",
		"--output", outputFile,
		"--model", llmModel,
		"--timeout", fmt.Sprintf("%d", cfg.ChunkTimeout),
		"--no-plan", // Skip per-file PLAN_TASK pre-pass for faster scanning
	}

	// Add global excludes from config
	if cfg.Excludes != "" {
		args = append(args, "--exclude", cfg.Excludes)
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

	cmd := exec.CommandContext(ctx, "/usr/local/bin/ocr", args...)
	cmd.Env = append(os.Environ(),
		"OCR_LLM_URL="+llmURL,
		"OCR_LLM_TOKEN="+llmToken,
		"OCR_LLM_MODEL="+llmModel,
		"OCR_LANGUAGE="+language,
		"HOME=/data/ocr-home",
	)

	var stderr strings.Builder
	cmd.Stdout = nil
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		log.Printf("OCR stderr: %s", stderr.String())
		return nil, "", fmt.Errorf("ocr scan failed: %w, stderr: %s", err, stderr.String())
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		return nil, "", fmt.Errorf("read ocr output: %w", err)
	}

	var ocrResult map[string]interface{}
	if err := json.Unmarshal(data, &ocrResult); err != nil {
		return nil, "", fmt.Errorf("parse ocr output: %w", err)
	}

	comments := convertComments(ocrResult["comments"])
	summary := ""
	if m := ocrResult["message"]; m != nil {
		summary = m.(string)
	}

	os.Remove(outputFile)
	return comments, summary, nil
}

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