package scan

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReviewComment holds a single code review finding.
// Used by both bot.go (webhook reviews) and scan (nightly/immediate scans).
type ReviewComment struct {
	File           string `json:"file"`
	Line           int    `json:"line"`
	EndLine        int    `json:"end_line"`
	Message        string `json:"message"`
	SuggestionCode string `json:"suggestion_code"`
	ExistingCode   string `json:"existing_code"`
	Severity       string `json:"severity"`
	Category       string `json:"category"`
	Thinking       string `json:"thinking"`
	Tool           string `json:"tool"`
}

// ConvertComments parses raw OCR JSON output into ReviewComment structs.
func ConvertComments(raw interface{}) []ReviewComment {
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
			File:           getString(cm, "path"),
			Line:           getInt(cm, "start_line"),
			EndLine:        getInt(cm, "end_line"),
			Message:        getString(cm, "content"),
			SuggestionCode: getString(cm, "suggestion_code"),
			ExistingCode:   getString(cm, "existing_code"),
			Severity:       getString(cm, "severity"),
			Category:       getString(cm, "category"),
			Thinking:       getString(cm, "thinking"),
			Tool:           "code_review",
		})
	}
	return comments
}

// FormatCommentBody creates a rich markdown body for GitLab comments.
func FormatCommentBody(c ReviewComment, gitlabURL, projectPath, defaultBranch string) string {
	var b strings.Builder

	// File link with line range
	codeLink := fmt.Sprintf("[%s:%d-%d](%s/%s/-/blob/%s/%s#L%d-L%d)",
		c.File, c.Line, c.EndLine, gitlabURL, projectPath, defaultBranch, c.File, c.Line, c.EndLine)

	// Header with severity and category badges
	sev := strings.ToUpper(c.Severity)
	cat := c.Category
	if cat == "" {
		cat = "general"
	}
	b.WriteString(fmt.Sprintf("**%s** [%s] (%s)\n\n", codeLink, sev, cat))

	// Main message
	if c.Message != "" {
		b.WriteString(fmt.Sprintf("%s\n\n", c.Message))
	}

	// Thinking (collapsible)
	if c.Thinking != "" {
		b.WriteString("<details>\n<summary><strong>Analysis</strong></summary>\n\n")
		b.WriteString(fmt.Sprintf("%s\n\n", c.Thinking))
		b.WriteString("</details>\n\n")
	}

	// Existing code
	if c.ExistingCode != "" {
		b.WriteString("**Existing Code:**\n")
		ext := GetFileExt(c.File)
		b.WriteString(fmt.Sprintf("```%s\n%s\n```\n\n", ext, c.ExistingCode))
	}

	// Suggestion code
	if c.SuggestionCode != "" {
		b.WriteString("**Suggestion:**\n")
		ext := GetFileExt(c.File)
		b.WriteString(fmt.Sprintf("```%s\n%s\n```\n\n", ext, c.SuggestionCode))
	}

	return b.String()
}

// severityOrder defines sort priority: lower number = higher priority.
var severityOrder = map[string]int{
	"critical": 0,
	"high":     1,
	"medium":   2,
	"low":      3,
}

// categoryOrder defines category sort priority within each severity group.
var categoryOrder = map[string]int{
	"bug":             0,
	"security":        1,
	"performance":     2,
	"maintainability": 3,
	"test":            4,
	"style":           5,
	"documentation":   6,
	"other":           7,
}

// SortCommentsBySeverity returns a copy of comments sorted by severity
// (critical → high → medium → low). Unknown severities sort last.
func SortCommentsBySeverity(comments []ReviewComment) []ReviewComment {
	sorted := make([]ReviewComment, len(comments))
	copy(sorted, comments)
	sort.SliceStable(sorted, func(i, j int) bool {
		si, okI := severityOrder[strings.ToLower(sorted[i].Severity)]
		sj, okJ := severityOrder[strings.ToLower(sorted[j].Severity)]
		if !okI {
			si = 99
		}
		if !okJ {
			sj = 99
		}
		return si < sj
	})
	return sorted
}

// FormatIssueBody creates a full GitLab issue body with findings grouped
// by severity (critical → high → medium → low), then by category, then
// by file within each category group.
func FormatIssueBody(comments []ReviewComment, summary, projectPath, defaultBranch, gitlabURL string, triggerIssueIID int) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("## Summary\n%s\n\n", summary))
	if triggerIssueIID > 0 {
		b.WriteString(fmt.Sprintf("**Triggered by:** Issue #%d\n\n", triggerIssueIID))
	}
	b.WriteString(fmt.Sprintf("**Project:** %s\n", projectPath))
	b.WriteString(fmt.Sprintf("**Branch:** %s\n", defaultBranch))
	b.WriteString(fmt.Sprintf("**Scan Time:** %s UTC\n\n", time.Now().Format("2006-01-02 15:04:05")))

	// Count by severity
	var sevCounts [4]int
	for _, c := range comments {
		switch strings.ToLower(c.Severity) {
		case "critical":
			sevCounts[0]++
		case "high":
			sevCounts[1]++
		case "medium":
			sevCounts[2]++
		case "low":
			sevCounts[3]++
		}
	}
	b.WriteString(fmt.Sprintf("**Total Findings:** %d (Critical: %d, High: %d, Medium: %d, Low: %d)\n\n",
		len(comments), sevCounts[0], sevCounts[1], sevCounts[2], sevCounts[3]))
	b.WriteString("---\n\n")
	b.WriteString("## Findings\n\n")

	if len(comments) == 0 {
		b.WriteString("No findings.\n")
		b.WriteString("---\n*Generated by OpenCodeReview Bot*")
		return b.String()
	}

	// Group by severity
	sorted := SortCommentsBySeverity(comments)
	bySeverity := map[string][]ReviewComment{}
	for _, c := range sorted {
		s := strings.ToLower(c.Severity)
		if s == "" {
			s = "low"
		}
		bySeverity[s] = append(bySeverity[s], c)
	}

	sevOrder := []string{"critical", "high", "medium", "low"}
	sevLabels := map[string]string{
		"critical": "Critical",
		"high":     "High",
		"medium":   "Medium",
		"low":      "Low",
	}

	for _, sev := range sevOrder {
		group := bySeverity[sev]
		if len(group) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("### %s (%d)\n\n", sevLabels[sev], len(group)))

		// Group by category within severity
		byCategory := map[string][]ReviewComment{}
		for _, c := range group {
			cat := strings.ToLower(c.Category)
			if cat == "" {
				cat = "other"
			}
			byCategory[cat] = append(byCategory[cat], c)
		}

		// Sort categories by priority
		cats := make([]string, 0, len(byCategory))
		for cat := range byCategory {
			cats = append(cats, cat)
		}
		sort.SliceStable(cats, func(i, j int) bool {
			ci, okI := categoryOrder[cats[i]]
			cj, okJ := categoryOrder[cats[j]]
			if !okI {
				ci = 99
			}
			if !okJ {
				cj = 99
			}
			return ci < cj
		})

		for _, cat := range cats {
			catGroup := byCategory[cat]
			b.WriteString(fmt.Sprintf("#### %s (%d)\n\n", capitalize(cat), len(catGroup)))

			// Group by file within category
			byFile := map[string][]ReviewComment{}
			for _, c := range catGroup {
				byFile[c.File] = append(byFile[c.File], c)
			}
			files := make([]string, 0, len(byFile))
			for f := range byFile {
				files = append(files, f)
			}
			sort.Strings(files)

			for _, file := range files {
				fileComments := byFile[file]
				fileLink := fmt.Sprintf("[%s](%s/%s/-/blob/%s/%s)",
					file, gitlabURL, projectPath, defaultBranch, file)
				b.WriteString(fmt.Sprintf("##### %s\n\n", fileLink))
				for i, c := range fileComments {
					b.WriteString(fmt.Sprintf("%d. %s\n\n", i+1,
						FormatCommentBody(c, gitlabURL, projectPath, defaultBranch)))
				}
			}
		}
	}

	b.WriteString("---\n*Generated by OpenCodeReview Bot*")
	return b.String()
}

// GetFileExt extracts the file extension from a path.
func GetFileExt(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return path[i+1:]
		}
		if path[i] == '/' {
			break
		}
	}
	return ""
}

// capitalize returns the string with the first character uppercased.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
