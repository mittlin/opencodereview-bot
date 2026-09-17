package scan

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
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

const OCRHomeDir = "/data/ocr-home"

// ConfigLLM writes OCR LLM settings to config.json via `ocr config set`.
// Reads from environment variables (same as bot.go startup config).
func ConfigLLM(ctx context.Context) {
	llmURL := os.Getenv("LLM_URL")
	llmToken := os.Getenv("LLM_TOKEN")
	llmModel := os.Getenv("LLM_MODEL")
	language := os.Getenv("OCR_LANGUAGE")

	configs := map[string]string{
		"llm.url":           llmURL,
		"llm.auth_token":    llmToken,
		"llm.model":         llmModel,
		"llm.use_anthropic": "false",
		"llm.extra_body":    `{"thinking": {"type": "disabled"}}`,
		"language":          language,
	}
	for key, val := range configs {
		setCmd := exec.CommandContext(ctx, "/usr/local/bin/ocr", "config", "set", key, val)
		setCmd.Env = append(os.Environ(),
			"OCR_LLM_URL="+llmURL,
			"OCR_LLM_TOKEN="+llmToken,
			"OCR_LLM_MODEL="+llmModel,
			"HOME="+OCRHomeDir,
		)
		if output, err := setCmd.CombinedOutput(); err != nil {
			log.Printf("Warning: failed to set %s: %v, output: %s", key, err, string(output))
		}
	}
}

func getEnvWithDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// ParseJSONFile reads a JSON file and unmarshals it into the given target.
func ParseJSONFile(path string, target interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func getEnvIntWithDefault(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil {
			return parsed
		}
	}
	return defaultVal
}