package scan

import (
	"context"
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
	runStart := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	scanCancelFunc = cancel
	defer func() {
		scanCancelFunc = nil
	}()

	cfg := LoadConfig()
	windowEnd := parseWindowEnd(cfg.WindowEnd)
	hardDeadline := time.Now().AddDate(0, 0, 1).Truncate(24*time.Hour).Add(windowEnd).Add(-time.Duration(cfg.HardDeadlineBuffer) * time.Minute)

	// Compute next scheduled run
	cronExpr := cfg.CronExpr
	nextRun := time.Time{}
	if schedules, err := cron.ParseStandard(cronExpr); err == nil {
		nextRun = schedules.Next(time.Now())
	}

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

		alreadyCompleted := false
		for _, c := range queue.CompletedThisNight {
			if c.ProjectID == entry.ProjectID {
				alreadyCompleted = true
				break
			}
		}
		if alreadyCompleted {
			log.Printf("Skipping %s: already completed this night", entry.PathWithNamespace)
			queue.NightlyQueue = append(queue.NightlyQueue[:i], queue.NightlyQueue[i+1:]...)
			i--
			continue
		}

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

	// Record scan metadata
	runEnd := time.Now()
	meta, err := LoadMetadata()
	if err != nil {
		log.Printf("Failed to load metadata: %v", err)
	} else {
		meta.LastRunStart = runStart
		meta.LastRunEnd = runEnd
		meta.LastRunDurationSec = runEnd.Sub(runStart).Seconds()
		meta.LastRunSuccess = true
		meta.ProjectsScanned = len(queue.CompletedThisNight)
		meta.TotalFindings = 0
		meta.ProjectsSucceeded = 0
		meta.ProjectsFailed = 0
		for _, c := range queue.CompletedThisNight {
			meta.TotalFindings += c.Findings
			if c.IssueIID > 0 || c.Findings > 0 {
				meta.ProjectsSucceeded++
			} else {
				meta.ProjectsSucceeded++
			}
		}
		meta.NextScheduledRun = nextRun
		if err := SaveMetadata(meta); err != nil {
			log.Printf("Failed to save metadata: %v", err)
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
		// Resume: re-clone repo (temp dir doesn't survive restart)
		log.Printf("Resuming project %s: %d/%d chunks done, re-cloning...", entry.PathWithNamespace, len(queue.InProgress.CompletedChunks), len(queue.InProgress.PendingChunks)+len(queue.InProgress.CompletedChunks))
		repoDir, err = CloneRepo(ctx, entry.ProjectID, entry.PathWithNamespace, gitlabURLVal, gitlabTokenVal, "", "", "")
		if err != nil {
			return nil, fmt.Errorf("clone repo for resume: %w", err)
		}
		defer os.RemoveAll(repoDir)

		chunks = queue.InProgress.PendingChunks
		totalFiles = queue.InProgress.TotalFiles
		completedChunks = queue.InProgress.CompletedChunks
	} else {
		log.Printf("Cloning repository for %s", entry.PathWithNamespace)
		repoDir, err = CloneRepo(ctx, entry.ProjectID, entry.PathWithNamespace, gitlabURLVal, gitlabTokenVal, "", "", "")
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
	var scanSuccessful bool
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
		scanSuccessful = true

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
		}
		if err := SaveQueue(queue); err != nil {
			log.Printf("Failed to save progress: %v", err)
		}
	}

	return scanChunksAndReport(ctx, entry, cfg, allComments, allSummaries, scanSuccessful, gitlabURLVal, "Nightly Scan Report")
}

func buildIssueBody(comments []ReviewComment, summary, projectPath, defaultBranch, gitlabURL string, triggerIssueIID int) string {
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
		b.WriteString(fmt.Sprintf("### %s\n\n", FormatCommentBody(c, gitlabURL, projectPath, defaultBranch)))
	}
	b.WriteString("---\n*Generated by OpenCodeReview Bot*")
	return b.String()
}

// scanChunksAndReport handles the shared post-scan logic for both nightly and immediate scans:
// create issue, comment on trigger issue, auto-close on clean scan.
func scanChunksAndReport(ctx context.Context, entry PriorityEntry, cfg *Config, allComments []ReviewComment, allSummaries []string, scanSuccessful bool, gitlabURLVal, reportTitle string) (*scanProgressResult, error) {
	aggregatedSummary := strings.Join(allSummaries, "\n\n")

	title := fmt.Sprintf("%s%s - %s", cfg.IssueTitlePrefix, reportTitle, entry.PathWithNamespace)
	body := buildIssueBody(allComments, aggregatedSummary, entry.PathWithNamespace, "main", gitlabURLVal, entry.TriggerIssueIID)

	issueIID := 0
	if cfg.CreateIssues && len(allComments) > 0 {
		var err error
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
	} else if len(allComments) == 0 && entry.TriggerIssueIID > 0 && scanSuccessful {
		msg := "✅ Scan completed - no issues found"
		if err := AddCommentToIssue(ctx, entry.ProjectID, entry.TriggerIssueIID, msg); err != nil {
			log.Printf("Failed to add clean scan comment: %v", err)
		}
		if cfg.AutoCloseOnCleanRescan {
			log.Printf("Auto-closing issue #%d (clean scan, auto-close enabled)", entry.TriggerIssueIID)
			if err := CloseIssue(ctx, entry.ProjectID, entry.TriggerIssueIID); err != nil {
				log.Printf("Failed to auto-close issue: %v", err)
			}
		}
	}

	return &scanProgressResult{
		Comments: allComments,
		Summary:  aggregatedSummary,
		IssueIID: issueIID,
	}, nil
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
	ConfigLLM(ctx)

	includeArg := strings.Join(files, ",")
	outputFile := filepath.Join("/data/ocr-reviews", fmt.Sprintf("scan-%s-%d-%d.json",
		strings.ReplaceAll(entry.PathWithNamespace, "/", "-"), entry.ProjectID, time.Now().Unix()))

	args := []string{
		"scan",
		"--repo", repoDir,
		"--path", includeArg,
		"--format", "json",
		"--output", outputFile,
		"--model", llmModel,
		"--timeout", fmt.Sprintf("%d", cfg.ChunkTimeout),
		"--no-plan",
	}

	if cfg.Excludes != "" {
		args = append(args, "--exclude", cfg.Excludes)
	}
	args = AppendOCRArgs(args, maxTokensBudget, effort, provider)

	env := BuildOCREnv(llmURL, llmToken, llmModel, "OCR_LANGUAGE", language)
	return RunOCR(ctx, args, env, outputFile)
}

func runImmediateScan(ctx context.Context, entry PriorityEntry, cfg *Config, botToken, gitlabURLVal, gitlabTokenVal, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal string) (*scanProgressResult, error) {
	log.Printf("Starting immediate review for %s", entry.PathWithNamespace)

	repoDir, err := CloneRepo(ctx, entry.ProjectID, entry.PathWithNamespace, gitlabURLVal, gitlabTokenVal, "", "", "")
	if err != nil {
		return nil, fmt.Errorf("clone repo: %w", err)
	}
	defer os.RemoveAll(repoDir)

	chunks, err := enumerateAndChunkFiles(ctx, repoDir, strings.Split(cfg.Excludes, ","), cfg.ChunkSize)
	if err != nil {
		return nil, fmt.Errorf("enumerate files: %w", err)
	}
	totalFiles := 0
	for _, c := range chunks {
		totalFiles += c.FileCount
	}
	log.Printf("Immediate review %s: %d files in %d chunks", entry.PathWithNamespace, totalFiles, len(chunks))

	var allComments []ReviewComment
	var allSummaries []string
	var scanSuccessful bool

	for idx, chunk := range chunks {
		log.Printf("Scanning chunk %d/%d (%d files)", idx+1, len(chunks), chunk.FileCount)
		comments, summary, err := runScanChunk(ctx, entry, repoDir, chunk.Files, cfg, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal)
		if err != nil {
			log.Printf("Chunk %d failed: %v", idx, err)
			continue
		}
		scanSuccessful = true

		allComments = append(allComments, comments...)
		if summary != "" {
			allSummaries = append(allSummaries, summary)
		}
	}

	return scanChunksAndReport(ctx, entry, cfg, allComments, allSummaries, scanSuccessful, gitlabURLVal, "Immediate Review Report")
}