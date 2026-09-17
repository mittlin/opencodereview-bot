package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// TestReplayScanResult reads a local scan JSON file, converts comments,
// builds issue body, and optionally creates the issue on GitLab.
//
// Usage:
//   # dry-run (just print body)
//   go test -run TestReplayScanResult -v ./scan/ -count=1
//
//   # create issue on GitLab
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

	// 4. Build issue body
	gitlabURL := os.Getenv("GITLAB_URL")
	if gitlabURL == "" {
		gitlabURL = "http://10.2.2.80"
	}
	projectPath := "tda4vmeco/common"
	body := buildIssueBody(comments, summary, projectPath, "main", gitlabURL, 0)
	t.Logf("Issue body length: %d chars", len(body))
	t.Logf("Issue body preview:\n%s", truncate(body, 2000))

	// 5. Optionally create issue on GitLab
	projectID := os.Getenv("GITLAB_PROJECT_ID")
	gitlabTokenVal := os.Getenv("GITLAB_GROUP_TOKEN")
	if projectID == "" || gitlabTokenVal == "" {
		t.Log("\n--- DRY RUN: Set GITLAB_PROJECT_ID and GITLAB_GROUP_TOKEN to create issue ---")
		return
	}

	InitGitLabClient(gitlabURL, gitlabTokenVal, LoadConfig())

	title := fmt.Sprintf("%sNightly Scan Report - %s", cfg().IssueTitlePrefix, projectPath)
	issueIID, err := CreateOrUpdateScanIssue(context.Background(), atoi(projectID), title, body, []string{}, 0)
	if err != nil {
		t.Fatalf("Failed to create issue: %v", err)
	}
	t.Logf("Created GitLab issue #%d", issueIID)
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

func cfg() *Config {
	return LoadConfig()
}
