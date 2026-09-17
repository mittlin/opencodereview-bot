package scan

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
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

type NoteEvent struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID           int    `json:"id"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		NoteableID   int    `json:"noteable_id"`
	} `json:"object_attributes"`
	Project struct {
		ID                int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	Issue *struct {
		IID int `json:"iid"`
	} `json:"issue,omitempty"`
}

func IssueHandler(w http.ResponseWriter, r *http.Request, event IssueEvent) {
	log.Printf("Issue event received: project=%s action=%s iid=%d labels=%v",
		event.Project.PathWithNamespace,
		event.ObjectAttributes.Action,
		event.ObjectAttributes.IID,
		event.ObjectAttributes.Labels)

	if event.ObjectAttributes.Action != "open" && event.ObjectAttributes.Action != "reopen" {
		log.Printf("Skipping issue event: action=%s (only open/reopen handled)", event.ObjectAttributes.Action)
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
		log.Printf("Skipping issue event: no trigger label (labels=%v)", event.ObjectAttributes.Labels)
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

	if isProjectInQueue(queue, event.Project.ID) {
		log.Printf("Skipping issue event: project %s already in queue", event.Project.PathWithNamespace)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "skipped",
			"project": event.Project.PathWithNamespace,
			"reason":  "already in queue",
		})
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
	if event.ObjectAttributes.Action == "reopen" {
		entry.TriggerType = "manual_issue_reopen"
	}

	queue.NightlyQueue = append(queue.NightlyQueue, entry)
	if err := SaveQueue(queue); err != nil {
		log.Printf("Failed to save queue: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	if event.ObjectAttributes.Action == "reopen" {
		msg := "🔄 Issue reopened — added to nightly scan queue"
		if err := AddCommentToIssue(r.Context(), event.Project.ID, event.ObjectAttributes.IID, msg); err != nil {
			log.Printf("Failed to add reopen comment: %v", err)
		}
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
		"queued_projects":      len(queue.NightlyQueue),
		"in_progress":          queue.InProgress != nil,
		"completed_this_night": len(queue.CompletedThisNight),
		"queue":                queue.NightlyQueue,
		"in_progress_details":  queue.InProgress,
		"completed":            queue.CompletedThisNight,
	})
}

func NoteHandler(w http.ResponseWriter, r *http.Request, event NoteEvent) {
	log.Printf("Note event received: project=%s noteable_type=%s noteable_id=%d note_len=%d",
		event.Project.PathWithNamespace,
		event.ObjectAttributes.NoteableType,
		event.ObjectAttributes.NoteableID,
		len(event.ObjectAttributes.Note))

	if event.ObjectAttributes.NoteableType != "Issue" {
		log.Printf("Skipping note event: not on issue (type=%s)", event.ObjectAttributes.NoteableType)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "not on issue"})
		return
	}

	if event.Issue == nil || event.Issue.IID == 0 {
		log.Printf("Skipping note event: no issue reference")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "no issue reference"})
		return
	}

	noteLower := strings.ToLower(event.ObjectAttributes.Note)
	noteClean := strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) && r != '@' && r != '-' {
			return -1
		}
		return r
	}, noteLower)

	if !strings.Contains(noteClean, "@ocr-bot") {
		log.Printf("Skipping note event: no @ocr-bot mention")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "no @ocr-bot mention"})
		return
	}

	switch {
	case strings.Contains(noteClean, "review"):
		log.Printf("Immediate review triggered via comment on issue #%d", event.Issue.IID)
		if err := TriggerImmediateScan(r.Context(), event.Project.ID, event.Project.PathWithNamespace, event.Issue.IID); err != nil {
			log.Printf("Failed to trigger immediate scan: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "accepted",
			"project": event.Project.PathWithNamespace,
			"type":    "immediate_review",
		})

	case strings.Contains(noteClean, "rescan"):
		log.Printf("Nightly rescan queued via comment on issue #%d", event.Issue.IID)
		if err := TriggerRescanOnComment(r.Context(), event.Project.ID, event.Project.PathWithNamespace, event.Issue.IID); err != nil {
			log.Printf("Failed to trigger rescan: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "queued",
			"project": event.Project.PathWithNamespace,
			"type":    "rescan",
		})

	default:
		log.Printf("Skipping note event: no matching action word (note=%q)", event.ObjectAttributes.Note)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "no matching action word"})
	}
}

func isProjectInQueue(queue *NightlyQueue, projectID int) bool {
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
