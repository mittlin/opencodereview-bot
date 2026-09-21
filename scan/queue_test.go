package scan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadSaveQueue(t *testing.T) {
	// Use a temp file for testing
	tmpFile := filepath.Join(t.TempDir(), "test-queue.json")

	// Temporarily override queueFilePath
	origQueueFilePath := queueFilePath
	SetQueueFilePath(tmpFile)
	defer func() { SetQueueFilePath(origQueueFilePath) }()

	// Test loading non-existent file returns empty queue
	queue, err := LoadQueue()
	if err != nil {
		t.Fatalf("LoadQueue() failed: %v", err)
	}
	if queue.NightlyQueue == nil {
		t.Fatal("NightlyQueue should not be nil")
	}
	if queue.CompletedThisNight == nil {
		t.Fatal("CompletedThisNight should not be nil")
	}

	// Test saving and loading
	entry := PriorityEntry{
		ProjectID:         123,
		PathWithNamespace: "group/project",
		TriggerTime:       time.Now(),
		TriggerIssueIID:   456,
		TriggerType:       "manual_issue",
		BasePriority:      10.5,
	}

	queue.NightlyQueue = append(queue.NightlyQueue, entry)
	queue.CompletedThisNight = append(queue.CompletedThisNight, CompletedEntry{
		ProjectID:         123,
		PathWithNamespace: "group/project",
		Findings:          5,
		IssueIID:          789,
		CompletedAt:       time.Now(),
	})

	err = SaveQueue(queue)
	if err != nil {
		t.Fatalf("SaveQueue() failed: %v", err)
	}

	// Load again and verify
	loaded, err := LoadQueue()
	if err != nil {
		t.Fatalf("LoadQueue() failed: %v", err)
	}

	if len(loaded.NightlyQueue) != 1 {
		t.Fatalf("Expected 1 queued entry, got %d", len(loaded.NightlyQueue))
	}
	if loaded.NightlyQueue[0].ProjectID != 123 {
		t.Fatalf("Expected project ID 123, got %d", loaded.NightlyQueue[0].ProjectID)
	}
	if loaded.NightlyQueue[0].TriggerType != "manual_issue" {
		t.Fatalf("Expected trigger type 'manual_issue', got %s", loaded.NightlyQueue[0].TriggerType)
	}

	if len(loaded.CompletedThisNight) != 1 {
		t.Fatalf("Expected 1 completed entry, got %d", len(loaded.CompletedThisNight))
	}
	if loaded.CompletedThisNight[0].Findings != 5 {
		t.Fatalf("Expected 5 findings, got %d", loaded.CompletedThisNight[0].Findings)
	}
}

func TestQueueJSONMarshal(t *testing.T) {
	queue := NightlyQueue{
		NightlyQueue: []PriorityEntry{
			{
				ProjectID:         1,
				PathWithNamespace: "group/project",
				TriggerTime:       time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
				TriggerIssueIID:   100,
				TriggerType:       "manual_issue",
				BasePriority:      100,
			},
		},
		CompletedThisNight: []CompletedEntry{
			{
				ProjectID:         1,
				PathWithNamespace: "group/project",
				Findings:          3,
				IssueIID:          200,
				CompletedAt:       time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC),
			},
		},
	}

	data, err := json.Marshal(queue)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	// Verify it can be unmarshaled
	var loaded NightlyQueue
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if len(loaded.NightlyQueue) != 1 {
		t.Fatalf("Expected 1 queued entry after round-trip")
	}
	if len(loaded.CompletedThisNight) != 1 {
		t.Fatalf("Expected 1 completed entry after round-trip")
	}
}

func TestConfigDefaults(t *testing.T) {
	// Test that config loads with defaults when env vars not set
	cfg := LoadConfig()

	if !cfg.Enabled {
		t.Error("Expected Enabled to be true by default")
	}
	if cfg.CronExpr != "0 21 * * *" {
		t.Errorf("Expected CronExpr '0 21 * * *', got %s", cfg.CronExpr)
	}
	if cfg.WindowEnd != "08:00" {
		t.Errorf("Expected WindowEnd '08:00', got %s", cfg.WindowEnd)
	}
	if cfg.ChunkSize != 500 {
		t.Errorf("Expected ChunkSize 500, got %d", cfg.ChunkSize)
	}
	if cfg.ChunkTimeout != 30 {
		t.Errorf("Expected ChunkTimeout 30, got %d", cfg.ChunkTimeout)
	}
	if cfg.HardDeadlineBuffer != 30 {
		t.Errorf("Expected HardDeadlineBuffer 30, got %d", cfg.HardDeadlineBuffer)
	}
	if !cfg.CreateIssues {
		t.Error("Expected CreateIssues true by default")
	}
	if cfg.IssueLabel != "ocr-review" {
		t.Errorf("Expected IssueLabel 'ocr-review', got %s", cfg.IssueLabel)
	}
	if cfg.IssueTitlePrefix != "[OCR] " {
		t.Errorf("Expected IssueTitlePrefix '[OCR] ', got %s", cfg.IssueTitlePrefix)
	}
	if cfg.SeverityThreshold != "warning" {
		t.Errorf("Expected SeverityThreshold 'warning', got %s", cfg.SeverityThreshold)
	}
	if !cfg.AutoCloseOnCleanRescan {
		t.Error("Expected AutoCloseOnCleanRescan true by default")
	}
	if cfg.RescanTriggerPhrase != "@ocr-bot" {
		t.Errorf("Expected RescanTriggerPhrase '@ocr-bot', got %s", cfg.RescanTriggerPhrase)
	}
	if len(cfg.TriggerLabels) != 2 {
		t.Errorf("Expected 2 trigger labels, got %d", len(cfg.TriggerLabels))
	}
}

func TestConfigFromEnv(t *testing.T) {
	// Set env vars
	os.Setenv("OCR_SCAN_ENABLED", "false")
	os.Setenv("OCR_SCHEDULE_CRON", "0 2 * * *")
	os.Setenv("OCR_SCAN_WINDOW_END", "06:00")
	os.Setenv("OCR_GROUP_ID", "456")
	os.Setenv("OCR_SCAN_CHUNK_SIZE", "1000")
	os.Setenv("OCR_SCAN_CHUNK_TIMEOUT", "60")
	os.Setenv("OCR_SCAN_HARD_DEADLINE_BUFFER", "60")
	os.Setenv("OCR_CREATE_ISSUES", "false")
	os.Setenv("OCR_ISSUE_LABEL", "custom-label")
	os.Setenv("OCR_ISSUE_TITLE_PREFIX", "[CUSTOM] ")
	os.Setenv("OCR_ISSUE_SEVERITY_THRESHOLD", "error")
	os.Setenv("OCR_ISSUE_TRIGGER_LABELS", "scan-me,trigger-scan")
	os.Setenv("OCR_AUTO_CLOSE_ON_CLEAN_RESCAN", "false")
	os.Setenv("OCR_RESCAN_TRIGGER_PHRASE", "@bot rescan")
	defer func() {
		os.Unsetenv("OCR_SCAN_ENABLED")
		os.Unsetenv("OCR_SCHEDULE_CRON")
		os.Unsetenv("OCR_SCAN_WINDOW_END")
		os.Unsetenv("OCR_GROUP_ID")
		os.Unsetenv("OCR_SCAN_CHUNK_SIZE")
		os.Unsetenv("OCR_SCAN_CHUNK_TIMEOUT")
		os.Unsetenv("OCR_SCAN_HARD_DEADLINE_BUFFER")
		os.Unsetenv("OCR_CREATE_ISSUES")
		os.Unsetenv("OCR_ISSUE_LABEL")
		os.Unsetenv("OCR_ISSUE_TITLE_PREFIX")
		os.Unsetenv("OCR_ISSUE_SEVERITY_THRESHOLD")
		os.Unsetenv("OCR_ISSUE_TRIGGER_LABELS")
		os.Unsetenv("OCR_AUTO_CLOSE_ON_CLEAN_RESCAN")
		os.Unsetenv("OCR_RESCAN_TRIGGER_PHRASE")
	}()

	cfg := LoadConfig()

	if cfg.Enabled {
		t.Error("Expected Enabled to be false")
	}
	if cfg.CronExpr != "0 2 * * *" {
		t.Errorf("Expected CronExpr '0 2 * * *', got %s", cfg.CronExpr)
	}
	if cfg.WindowEnd != "06:00" {
		t.Errorf("Expected WindowEnd '06:00', got %s", cfg.WindowEnd)
	}
	if cfg.GroupID != "456" {
		t.Errorf("Expected GroupID '456', got %s", cfg.GroupID)
	}
	if cfg.ChunkSize != 1000 {
		t.Errorf("Expected ChunkSize 1000, got %d", cfg.ChunkSize)
	}
	if cfg.ChunkTimeout != 60 {
		t.Errorf("Expected ChunkTimeout 60, got %d", cfg.ChunkTimeout)
	}
	if cfg.HardDeadlineBuffer != 60 {
		t.Errorf("Expected HardDeadlineBuffer 60, got %d", cfg.HardDeadlineBuffer)
	}
	if cfg.CreateIssues {
		t.Error("Expected CreateIssues false")
	}
	if cfg.IssueLabel != "custom-label" {
		t.Errorf("Expected IssueLabel 'custom-label', got %s", cfg.IssueLabel)
	}
	if cfg.IssueTitlePrefix != "[CUSTOM] " {
		t.Errorf("Expected IssueTitlePrefix '[CUSTOM] ', got %s", cfg.IssueTitlePrefix)
	}
	if cfg.SeverityThreshold != "error" {
		t.Errorf("Expected SeverityThreshold 'error', got %s", cfg.SeverityThreshold)
	}
	if len(cfg.TriggerLabels) != 2 {
		t.Errorf("Expected 2 trigger labels, got %d", len(cfg.TriggerLabels))
	}
	if cfg.TriggerLabels[0] != "scan-me" || cfg.TriggerLabels[1] != "trigger-scan" {
		t.Errorf("Expected trigger labels ['scan-me', 'trigger-scan'], got %v", cfg.TriggerLabels)
	}
	if cfg.AutoCloseOnCleanRescan {
		t.Error("Expected AutoCloseOnCleanRescan false")
	}
	if cfg.RescanTriggerPhrase != "@bot rescan" {
		t.Errorf("Expected RescanTriggerPhrase '@bot rescan', got %s", cfg.RescanTriggerPhrase)
	}
}

func TestPriorityEntryJSON(t *testing.T) {
	entry := PriorityEntry{
		ProjectID:         123,
		PathWithNamespace: "group/project",
		TriggerTime:       time.Date(2026, 9, 16, 21, 0, 0, 0, time.UTC),
		TriggerIssueIID:   456,
		TriggerType:       "manual_issue",
		BasePriority:      50.5,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var loaded PriorityEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if loaded.ProjectID != entry.ProjectID {
		t.Errorf("ProjectID mismatch")
	}
	if loaded.PathWithNamespace != entry.PathWithNamespace {
		t.Errorf("PathWithNamespace mismatch")
	}
	if loaded.TriggerIssueIID != entry.TriggerIssueIID {
		t.Errorf("TriggerIssueIID mismatch")
	}
	if loaded.TriggerType != entry.TriggerType {
		t.Errorf("TriggerType mismatch")
	}
	if loaded.BasePriority != entry.BasePriority {
		t.Errorf("BasePriority mismatch")
	}
}

func TestCompletedEntryJSON(t *testing.T) {
	entry := CompletedEntry{
		ProjectID:         123,
		PathWithNamespace: "group/project",
		Findings:          10,
		IssueIID:          789,
		CompletedAt:       time.Date(2026, 9, 17, 2, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var loaded CompletedEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if loaded.Findings != entry.Findings {
		t.Errorf("Findings mismatch")
	}
	if loaded.IssueIID != entry.IssueIID {
		t.Errorf("IssueIID mismatch")
	}
}