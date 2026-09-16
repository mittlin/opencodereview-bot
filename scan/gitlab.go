package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

var (
	gitlabURL   string
	gitlabToken string
	scanConfig  *Config
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
		"title":  title,
		"body":   body,
		"labels": labels,
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
	ocrLabels := append([]string{scanConfig.IssueLabel, "nightly-scan"}, labels...)

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