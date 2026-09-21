package scan

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

var queueMutex sync.Mutex

var queueFilePath = "/data/ocr-reviews/ocr-queue.json"

var metadataFilePath = "/data/ocr-reviews/ocr-metadata.json"

func SetQueueFilePath(path string) {
	queueFilePath = path
}

func SetMetadataFilePath(path string) {
	metadataFilePath = path
}

type ProjectInfo struct {
	ID                int
	PathWithNamespace string
	HTTPURLToRepo     string
	DefaultBranch     string
}

type FileChunk struct {
	Index            int
	Files            []string
	FileCount        int
	EstimatedMinutes int
}

type ScanProgress struct {
	ProjectID          int
	PathWithNamespace  string
	TotalFiles         int
	CompletedChunks    []int
	PendingChunks      []FileChunk
	CurrentChunk       int
	StartedAt          time.Time
	LastUpdateAt       time.Time
	TriggerIssueIID    int
	TriggerType        string
	DefaultBranch      string
	SourceBranch       string
	// RepoDir is NOT persisted - temp dir won't survive restart
	// Re-clone on resume using ProjectID + PathWithNamespace
}

type NightlyQueue struct {
	NightlyQueue       []PriorityEntry   `json:"nightly_queue"`
	InProgress         *ScanProgress     `json:"in_progress,omitempty"`
	CompletedThisNight []CompletedEntry  `json:"completed_this_night"`
}

type PriorityEntry struct {
	ProjectID           int
	PathWithNamespace   string
	TriggerTime         time.Time
	TriggerIssueIID     int
	TriggerType         string
	BasePriority        float64
	SourceBranch        string
}

type CompletedEntry struct {
	ProjectID           int
	PathWithNamespace   string
	Findings            int
	IssueIID            int
	FileIssues          map[string]int `json:"file_issues,omitempty"`
	CompletedAt         time.Time
}

type ScanResult struct {
	Project       ProjectInfo
	OutputFile    string
	Comments      []ReviewComment
	Summary       string
	Error         error
}

type GitLabIssue struct {
	IID    int      `json:"iid"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
	WebURL string   `json:"web_url"`
}

type GitLabProject struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	DefaultBranch     string `json:"default_branch"`
	Archived          bool   `json:"archived"`
}

func LoadQueue() (*NightlyQueue, error) {
	queueMutex.Lock()
	defer queueMutex.Unlock()

	data, err := os.ReadFile(queueFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &NightlyQueue{
				NightlyQueue:       []PriorityEntry{},
				CompletedThisNight: []CompletedEntry{},
			}, nil
		}
		return nil, fmt.Errorf("read queue file: %w", err)
	}

	var queue NightlyQueue
	if err := json.Unmarshal(data, &queue); err != nil {
		return nil, fmt.Errorf("unmarshal queue: %w", err)
	}
	if queue.NightlyQueue == nil {
		queue.NightlyQueue = []PriorityEntry{}
	}
	if queue.CompletedThisNight == nil {
		queue.CompletedThisNight = []CompletedEntry{}
	}
	return &queue, nil
}

func SaveQueue(queue *NightlyQueue) error {
	queueMutex.Lock()
	defer queueMutex.Unlock()

	tmpPath := queueFilePath + ".tmp"
	data, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal queue: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp queue file: %w", err)
	}
	if err := os.Rename(tmpPath, queueFilePath); err != nil {
		return fmt.Errorf("rename queue file: %w", err)
	}
	return nil
}

type ScanMetadata struct {
	LastRunStart       time.Time `json:"last_run_start"`
	LastRunEnd         time.Time `json:"last_run_end"`
	LastRunDurationSec float64   `json:"last_run_duration_sec"`
	LastRunSuccess     bool      `json:"last_run_success"`
	ProjectsScanned    int       `json:"projects_scanned"`
	ProjectsSucceeded  int       `json:"projects_succeeded"`
	ProjectsFailed     int       `json:"projects_failed"`
	TotalFindings      int       `json:"total_findings"`
	NextScheduledRun   time.Time `json:"next_run"`
}

func LoadMetadata() (*ScanMetadata, error) {
	data, err := os.ReadFile(metadataFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &ScanMetadata{}, nil
		}
		return nil, fmt.Errorf("read metadata file: %w", err)
	}

	var meta ScanMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	return &meta, nil
}

func SaveMetadata(meta *ScanMetadata) error {
	tmpPath := metadataFilePath + ".tmp"
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp metadata file: %w", err)
	}
	if err := os.Rename(tmpPath, metadataFilePath); err != nil {
		return fmt.Errorf("rename metadata file: %w", err)
	}
	return nil
}

// IsProjectInQueue checks if a project is in the queue (thread-safe).
// Must be called while holding queueMutex, or use LoadQueue/SaveQueue which handle locking.
func IsProjectInQueue(queue *NightlyQueue, projectID int) bool {
	for _, e := range queue.NightlyQueue {
		if e.ProjectID == projectID {
			return true
		}
	}
	if queue.InProgress != nil && queue.InProgress.ProjectID == projectID {
		return true
	}
	for _, c := range queue.CompletedThisNight {
		if c.ProjectID == projectID {
			return true
		}
	}
	return false
}
