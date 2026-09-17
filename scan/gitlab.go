package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	gitlabURL   string
	gitlabToken string
	scanConfig  *Config

	immediateScanMu      sync.Mutex
	immediateScanRunning bool
)

func InitGitLabClient(url, token string, cfg *Config) {
	gitlabURL = url
	gitlabToken = token
	scanConfig = cfg
}

func ListGroupProjects(ctx context.Context, groupID string) ([]ProjectInfo, error) {
	var allProjects []ProjectInfo
	page := 1
	perPage := 100

	for {
		url := fmt.Sprintf("%s/api/v4/groups/%s/projects?per_page=%d&page=%d&include_subgroups=true&archived=false", gitlabURL, groupID, perPage, page)

		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("PRIVATE-TOKEN", gitlabToken)

		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list group projects: %w", err)
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("list group projects: status %d: %s", resp.StatusCode, string(body))
		}

		var projects []GitLabProject
		if err := json.Unmarshal(body, &projects); err != nil {
			return nil, fmt.Errorf("unmarshal projects: %w", err)
		}

		if len(projects) == 0 {
			break
		}

		for _, p := range projects {
			allProjects = append(allProjects, ProjectInfo{
				ID:                p.ID,
				PathWithNamespace: p.PathWithNamespace,
				HTTPURLToRepo:     p.HTTPURLToRepo,
				DefaultBranch:     p.DefaultBranch,
			})
		}

		if len(projects) < perPage {
			break
		}
		page++
		time.Sleep(500 * time.Millisecond)
	}

	return allProjects, nil
}

func FindExistingOCRIssue(ctx context.Context, projectID int, labels []string) (*GitLabIssue, error) {
	labelStr := strings.Join(labels, ",")
	url := fmt.Sprintf("%s/api/v4/projects/%d/issues?labels=%s&state=opened&per_page=10", gitlabURL, projectID, labelStr)

	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("PRIVATE-TOKEN", gitlabToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("find existing issue: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("find existing issue: status %d: %s", resp.StatusCode, string(body))
	}

	var issues []GitLabIssue
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return nil, fmt.Errorf("decode issues: %w", err)
	}

	if len(issues) > 0 {
		return &issues[0], nil
	}
	return nil, nil
}

func AddCommentToIssue(ctx context.Context, projectID, issueIID int, body string) error {
	url := fmt.Sprintf("%s/api/v4/projects/%d/issues/%d/notes", gitlabURL, projectID, issueIID)

	bodyMap := map[string]string{"body": body}
	jsonBody, _ := json.Marshal(bodyMap)

	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(jsonBody)))
	req.Header.Set("PRIVATE-TOKEN", gitlabToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("add comment to issue: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add comment to issue: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func CloseIssue(ctx context.Context, projectID, issueIID int) error {
	url := fmt.Sprintf("%s/api/v4/projects/%d/issues/%d", gitlabURL, projectID, issueIID)

	bodyMap := map[string]string{"state_event": "close"}
	jsonBody, _ := json.Marshal(bodyMap)

	req, _ := http.NewRequestWithContext(ctx, "PUT", url, strings.NewReader(string(jsonBody)))
	req.Header.Set("PRIVATE-TOKEN", gitlabToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("close issue: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("close issue: status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func CreateIssue(ctx context.Context, projectID int, title, body string, labels []string) (int, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%d/issues", gitlabURL, projectID)

	bodyMap := map[string]interface{}{
		"title":       title,
		"description": body,
		"labels":      labels,
	}
	jsonBody, _ := json.Marshal(bodyMap)

	req, _ := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(jsonBody)))
	req.Header.Set("PRIVATE-TOKEN", gitlabToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("create issue: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("create issue: status %d: %s", resp.StatusCode, string(b))
	}

	var issue GitLabIssue
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		return 0, fmt.Errorf("decode created issue: %w", err)
	}

	return issue.IID, nil
}

func CreateOrUpdateScanIssue(ctx context.Context, projectID int, title, body string, labels []string, triggerIssueIID int) (int, error) {
	ocrLabels := append([]string{"ocr-result", "nightly-scan"}, labels...)

	existing, err := FindExistingOCRIssue(ctx, projectID, ocrLabels)
	if err != nil {
		return 0, err
	}

	if existing != nil {
		linkMsg := fmt.Sprintf("🔄 Superseded by new scan (triggered by #%d)", triggerIssueIID)
		if err := AddCommentToIssue(ctx, projectID, existing.IID, linkMsg); err != nil {
			log.Printf("Failed to add supersede comment: %v", err)
		}
		if err := CloseIssue(ctx, projectID, existing.IID); err != nil {
			log.Printf("Failed to close old issue: %v", err)
		}
	}

	return CreateIssue(ctx, projectID, title, body, ocrLabels)
}

// GetFileBaseName returns the last component of a file path.
func GetFileBaseName(filePath string) string {
	if i := strings.LastIndex(filePath, "/"); i >= 0 {
		return filePath[i+1:]
	}
	return filePath
}

// FindExistingFileIssues returns all open issues with ocr-result + nightly-scan labels.
func FindExistingFileIssues(ctx context.Context, projectID int) ([]GitLabIssue, error) {
	labels := "ocr-result,nightly-scan"
	url := fmt.Sprintf("%s/api/v4/projects/%d/issues?labels=%s&state=opened&per_page=100", gitlabURL, projectID, labels)

	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("PRIVATE-TOKEN", gitlabToken)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("find existing file issues: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("find existing file issues: status %d: %s", resp.StatusCode, string(body))
	}

	var issues []GitLabIssue
	if err := json.NewDecoder(resp.Body).Decode(&issues); err != nil {
		return nil, fmt.Errorf("decode file issues: %w", err)
	}
	return issues, nil
}

// CreateOrUpdateFileIssue creates or updates a per-file scan issue.
// Labels: ocr-result, nightly-scan, file:<basename>
func CreateOrUpdateFileIssue(ctx context.Context, projectID int, filePath, body string, triggerIssueIID int) (int, error) {
	baseName := GetFileBaseName(filePath)
	fileLabel := "file:" + baseName
	ocrLabels := []string{"ocr-result", "nightly-scan", fileLabel}

	existing, err := FindExistingOCRIssue(ctx, projectID, ocrLabels)
	if err != nil {
		return 0, err
	}

	if existing != nil {
		linkMsg := fmt.Sprintf("🔄 Updated by new scan (triggered by #%d)", triggerIssueIID)
		if err := AddCommentToIssue(ctx, projectID, existing.IID, linkMsg); err != nil {
			log.Printf("Failed to add update comment: %v", err)
		}
		if err := CloseIssue(ctx, projectID, existing.IID); err != nil {
			log.Printf("Failed to close old issue: %v", err)
		}
	}

	title := fmt.Sprintf("[OCR] %s - findings", baseName)
	return CreateIssue(ctx, projectID, title, body, ocrLabels)
}

// CloseStaleFileIssues closes open ocr-result issues that are NOT in keepLabels.
func CloseStaleFileIssues(ctx context.Context, projectID int, keepLabels map[string]bool) error {
	existing, err := FindExistingFileIssues(ctx, projectID)
	if err != nil {
		return err
	}

	closed := 0
	for _, issue := range existing {
		shouldKeep := false
		for _, label := range issue.Labels {
			if strings.HasPrefix(label, "file:") && keepLabels[label] {
				shouldKeep = true
				break
			}
		}
		if !shouldKeep {
			if err := CloseIssue(ctx, projectID, issue.IID); err != nil {
				log.Printf("Failed to close stale issue #%d: %v", issue.IID, err)
			} else {
				log.Printf("Closed stale file issue #%d (%s)", issue.IID, issue.Title)
				closed++
			}
		}
	}
	if closed > 0 {
		log.Printf("Closed %d stale file issues for project %d", closed, projectID)
	}
	return nil
}

func TriggerRescanOnComment(ctx context.Context, projectID int, pathWithNamespace string, issueIID int) error {
	log.Printf("Queuing rescan for %s (trigger issue #%d)", pathWithNamespace, issueIID)

	queue, err := LoadQueue()
	if err != nil {
		return fmt.Errorf("load queue: %w", err)
	}

	if isProjectInQueue(queue, projectID) {
		log.Printf("Project %s already in queue, skipping", pathWithNamespace)
		return nil
	}

	entry := PriorityEntry{
		ProjectID:         projectID,
		PathWithNamespace: pathWithNamespace,
		TriggerTime:       time.Now(),
		TriggerIssueIID:   issueIID,
		TriggerType:       "rescan_comment",
		BasePriority:      100,
	}

	queue.NightlyQueue = append(queue.NightlyQueue, entry)
	if err := SaveQueue(queue); err != nil {
		return fmt.Errorf("save queue: %w", err)
	}

	log.Printf("Queued project for rescan: %s (position %d)", pathWithNamespace, len(queue.NightlyQueue))
	return nil
}

func TriggerImmediateScan(ctx context.Context, projectID int, pathWithNamespace string, issueIID int) error {
	log.Printf("Triggering immediate review for %s (trigger issue #%d)", pathWithNamespace, issueIID)

	immediateScanMu.Lock()
	if immediateScanRunning {
		immediateScanMu.Unlock()
		log.Printf("Another immediate scan is running, queueing %s for later", pathWithNamespace)
		queue, err := LoadQueue()
		if err != nil {
			return fmt.Errorf("load queue: %w", err)
		}
		if isProjectInQueue(queue, projectID) {
			return nil
		}
		entry := PriorityEntry{
			ProjectID:         projectID,
			PathWithNamespace: pathWithNamespace,
			TriggerTime:       time.Now(),
			TriggerIssueIID:   issueIID,
			TriggerType:       "immediate_review",
			BasePriority:      200,
		}
		queue.NightlyQueue = append(queue.NightlyQueue, entry)
		return SaveQueue(queue)
	}
	immediateScanRunning = true
	immediateScanMu.Unlock()

	go func() {
		defer func() {
			immediateScanMu.Lock()
			immediateScanRunning = false
			immediateScanMu.Unlock()
			log.Printf("Immediate review completed for %s", pathWithNamespace)
		}()

		cfg := LoadConfig()
		botToken := os.Getenv("BOT_TOKEN")
		gitlabURLVal := gitlabURL
		gitlabTokenVal := gitlabToken
		llmURLVal := os.Getenv("LLM_URL")
		llmTokenVal := os.Getenv("LLM_TOKEN")
		llmModelVal := os.Getenv("LLM_MODEL")
		languageVal := os.Getenv("OCR_LANGUAGE")
		if languageVal == "" {
			languageVal = "Chinese"
		}
		maxTokensBudgetVal := os.Getenv("OCR_MAX_TOKENS_BUDGET")
		effortVal := os.Getenv("OCR_EFFORT")
		providerVal := os.Getenv("OCR_PROVIDER")

		scanCtx := context.Background()
		entry := PriorityEntry{
			ProjectID:         projectID,
			PathWithNamespace: pathWithNamespace,
			TriggerTime:       time.Now(),
			TriggerIssueIID:   issueIID,
			TriggerType:       "immediate_review",
			BasePriority:      200,
		}

		result, err := runImmediateScan(scanCtx, entry, cfg, botToken, gitlabURLVal, gitlabTokenVal, llmURLVal, llmTokenVal, llmModelVal, languageVal, maxTokensBudgetVal, effortVal, providerVal)
		if err != nil {
			log.Printf("Immediate review failed for %s: %v", pathWithNamespace, err)
			return
		}

		if result != nil && len(result.Comments) > 0 {
			log.Printf("Immediate review found %d issues in %s", len(result.Comments), pathWithNamespace)
		} else if result != nil {
			log.Printf("Immediate review completed for %s (no issues found or scan failed)", pathWithNamespace)
		}
	}()

	return nil
}
