package scan

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

type IssueEvent struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		IID          int    `json:"iid"`
		Title        string `json:"title"`
		State        string `json:"state"`
		Action       string `json:"action"`
		Labels       []struct {
			Title string `json:"title"`
		} `json:"labels"`
		Description string `json:"description"`
		UpdatedAt   string `json:"updated_at"`
	} `json:"object_attributes"`
	Project struct {
		ID                int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
}

func IssueHandler(w http.ResponseWriter, r *http.Request, event IssueEvent) {
	if event.ObjectAttributes.Action != "open" {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "action=" + event.ObjectAttributes.Action})
		return
	}

	hasTriggerLabel := false
	for _, label := range event.ObjectAttributes.Labels {
		for _, tl := range scanConfig.TriggerLabels {
			if strings.EqualFold(label.Title, tl) {
				hasTriggerLabel = true
				break
			}
		}
		if hasTriggerLabel {
			break
		}
	}

	if !hasTriggerLabel {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "no trigger label"})
		return
	}

	queue, err := LoadQueue()
	if err != nil {
		log.Printf("Failed to load queue: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	entry := PriorityEntry{
		ProjectID:         event.Project.ID,
		PathWithNamespace: event.Project.PathWithNamespace,
		TriggerTime:       time.Now(),
		TriggerIssueIID:   event.ObjectAttributes.IID,
		TriggerType:       "manual_issue",
		BasePriority:      0,
	}

	queue.NightlyQueue = append(queue.NightlyQueue, entry)
	if err := SaveQueue(queue); err != nil {
		log.Printf("Failed to save queue: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("Queued project for nightly scan: %s (trigger issue #%d)", event.Project.PathWithNamespace, event.ObjectAttributes.IID)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "queued",
		"project":  event.Project.PathWithNamespace,
		"position": len(queue.NightlyQueue),
	})
}

func ScanStatusHandler(w http.ResponseWriter, r *http.Request) {
	queue, err := LoadQueue()
	if err != nil {
		http.Error(w, "Failed to load queue", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"queued_projects":     len(queue.NightlyQueue),
		"in_progress":         queue.InProgress != nil,
		"completed_this_night": len(queue.CompletedThisNight),
		"queue":               queue.NightlyQueue,
		"in_progress_details": queue.InProgress,
		"completed":           queue.CompletedThisNight,
	})
}