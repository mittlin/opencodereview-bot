package scan

import (
	"fmt"
	"strings"
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
