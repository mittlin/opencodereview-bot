package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
)

// TestReplayScanResult reads a local scan JSON file, converts comments,
// and optionally creates per-file issues on GitLab.
//
// Usage:
//   # dry-run (just print body)
//   go test -run TestReplayScanResult -v ./scan/ -count=1
//
//   # create per-file issues on GitLab
//   GITLAB_URL=http://10.2.2.80 GITLAB_GROUP_TOKEN=xxx \
//   GITLAB_PROJECT_ID=318 go test -run TestReplayScanResult -v ./scan/ -count=1

func TestReplayScanResult(t *testing.T) {
	jsonPath := os.Getenv("SCAN_JSON_PATH")
	if jsonPath == "" {
		jsonPath = "/data/scan.json"
	}

	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("Failed to read %s: %v\nHint: mount JSON file to /data/scan.json", jsonPath, err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}

	// 1. Convert comments
	comments := ConvertComments(raw["comments"])
	t.Logf("Converted %d comments from JSON", len(comments))

	if len(comments) == 0 {
		t.Fatal("No comments found — check JSON structure (expected 'comments' array)")
	}

	// 2. Print comment summary
	for i, c := range comments {
		sev := c.Severity
		if sev == "" {
			sev = "info"
		}
		t.Logf("  [%d] %s:%d-%d [%s/%s] %s",
			i+1, c.File, c.Line, c.EndLine, sev, c.Category,
			truncate(c.Message, 80))
	}

	// 3. Extract summary
	summary := ""
	if msg, ok := raw["message"].(string); ok {
		summary = msg
	} else if s := raw["summary"]; s != nil {
		if sm, ok := s.(map[string]interface{}); ok {
			if c, ok := sm["comments"].(float64); ok {
				summary = fmt.Sprintf("Files reviewed, %d findings", int(c))
			}
		}
	}
	t.Logf("Summary: %s", truncate(summary, 200))

	// 4. Group by file
	gitlabURL := os.Getenv("GITLAB_URL")
	if gitlabURL == "" {
		gitlabURL = "http://10.2.2.80"
	}
	projectPath := "tda4vmeco/common"

	byFile := map[string][]ReviewComment{}
	for _, c := range comments {
		byFile[c.File] = append(byFile[c.File], c)
	}

	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	t.Logf("\n=== Per-file issues (%d files) ===", len(files))
	for _, file := range files {
		cs := byFile[file]
		body := FormatFileIssueBody(cs, file, projectPath, "main", gitlabURL)
		baseName := GetFileBaseName(file)
		t.Logf("  %s: %d findings, body length: %d chars", baseName, len(cs), len(body))
	}

	// 5. Optionally create per-file issues on GitLab
	projectID := os.Getenv("GITLAB_PROJECT_ID")
	gitlabTokenVal := os.Getenv("GITLAB_GROUP_TOKEN")
	if projectID == "" || gitlabTokenVal == "" {
		t.Log("\n--- DRY RUN: Set GITLAB_PROJECT_ID and GITLAB_GROUP_TOKEN to create issues ---")
		return
	}

	InitGitLabClient(gitlabURL, gitlabTokenVal, LoadConfig())
	pid := atoi(projectID)

	// Create trigger issue
	triggerTitle := "[OCR] Trigger - replay test"
	triggerBody := "Test trigger issue for replay verification. Summary comment should appear here."
	triggerIID, err := CreateIssue(context.Background(), pid, triggerTitle, triggerBody, []string{"ocr-trigger"})
	if err != nil {
		t.Fatalf("Failed to create trigger issue: %v", err)
	}
	t.Logf("Created trigger issue #%d", triggerIID)

	// Create per-file issues (with triggerIssueIID)
	fileIssues := map[string]int{}
	for _, file := range files {
		cs := byFile[file]
		body := FormatFileIssueBody(cs, file, projectPath, "main", gitlabURL)
		fiid, err := CreateOrUpdateFileIssue(context.Background(), pid, file, body, triggerIID)
		if err != nil {
			t.Logf("Failed to create issue for %s: %v", file, err)
			continue
		}
		fileIssues[file] = fiid
		t.Logf("Created file issue #%d for %s (%d findings)", fiid, GetFileBaseName(file), len(cs))
	}

	// Post summary comment on trigger issue
	summaryComment := buildSummaryComment(byFile, fileIssues, gitlabURL, projectPath, "main")
	if err := AddCommentToIssue(context.Background(), pid, triggerIID, summaryComment); err != nil {
		t.Fatalf("Failed to post summary comment: %v", err)
	}
	t.Logf("Posted summary comment on trigger issue #%d", triggerIID)

	// Print summary
	t.Logf("\n=== Summary ===")
	for _, file := range files {
		baseName := GetFileBaseName(file)
		fiid := fileIssues[file]
		t.Logf("  %s: %d findings -> #%d", baseName, len(byFile[file]), fiid)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
