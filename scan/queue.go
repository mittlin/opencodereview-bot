package scan

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

var queueFilePath = "/data/ocr-reviews/ocr-queue.json"

func SetQueueFilePath(path string) {
	queueFilePath = path
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
}

type CompletedEntry struct {
	ProjectID           int
	PathWithNamespace   string
	Findings            int
	IssueIID            int
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
