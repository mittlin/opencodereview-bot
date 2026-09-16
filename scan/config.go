package scan

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Enabled                  bool
	CronExpr                 string
	WindowEnd                string
	GroupID                  string
	Paths                    string
	Excludes                 string
	ChunkSize                int
	ChunkTimeout             int
	HardDeadlineBuffer       int
	CreateIssues             bool
	IssueLabel               string
	IssueTitlePrefix         string
	SeverityThreshold        string
	TriggerLabels            []string
	AutoCloseOnCleanRescan   bool
	RescanTriggerPhrase      string
}

func LoadConfig() *Config {
	triggerLabelsStr := getEnvWithDefault("OCR_ISSUE_TRIGGER_LABELS", "ocr-scan,ocr-trigger")
	triggerLabels := strings.Split(triggerLabelsStr, ",")
	for i := range triggerLabels {
		triggerLabels[i] = strings.TrimSpace(triggerLabels[i])
	}

	return &Config{
		Enabled:                getEnvWithDefault("OCR_SCAN_ENABLED", "true") == "true",
		CronExpr:               getEnvWithDefault("OCR_SCHEDULE_CRON", "0 21 * * *"),
		WindowEnd:              getEnvWithDefault("OCR_SCAN_WINDOW_END", "08:00"),
		GroupID:                getEnvWithDefault("OCR_GROUP_ID", ""),
		Paths:                  getEnvWithDefault("OCR_SCAN_PATHS", ""),
		Excludes:               getEnvWithDefault("OCR_SCAN_EXCLUDES", "**/generated/*,**/testdata/*,**/vendor/*"),
		ChunkSize:              getEnvIntWithDefault("OCR_SCAN_CHUNK_SIZE", 500),
		ChunkTimeout:           getEnvIntWithDefault("OCR_SCAN_CHUNK_TIMEOUT", 30),
		HardDeadlineBuffer:     getEnvIntWithDefault("OCR_SCAN_HARD_DEADLINE_BUFFER", 30),
		CreateIssues:           getEnvWithDefault("OCR_CREATE_ISSUES", "true") == "true",
		IssueLabel:             getEnvWithDefault("OCR_ISSUE_LABEL", "ocr-review"),
		IssueTitlePrefix:       getEnvWithDefault("OCR_ISSUE_TITLE_PREFIX", "[OCR] "),
		SeverityThreshold:      getEnvWithDefault("OCR_ISSUE_SEVERITY_THRESHOLD", "warning"),
		TriggerLabels:          triggerLabels,
		AutoCloseOnCleanRescan: getEnvWithDefault("OCR_AUTO_CLOSE_ON_CLEAN_RESCAN", "true") == "true",
		RescanTriggerPhrase:    getEnvWithDefault("OCR_RESCAN_TRIGGER_PHRASE", "@ocr-bot review"),
	}
}

func getEnvWithDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvIntWithDefault(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			return parsed
		}
	}
	return defaultVal
}