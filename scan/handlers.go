package scan

import (
	"encoding/json"
	"fmt"
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

	// Parse labels: find trigger label and branch candidates
	triggerLabel, candidates, err := parseLabels(event.ObjectAttributes.Labels, scanConfig.TriggerLabels)
	if err != nil {
		log.Printf("Skipping issue event: %v", err)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": err.Error()})
		return
	}

	// Validate each candidate branch via git ls-remote
	var validBranches []string
	for _, candidate := range candidates {
		if err := ValidateBranch(r.Context(), gitlabURL, gitlabToken, event.Project.PathWithNamespace, candidate); err == nil {
			validBranches = append(validBranches, candidate)
		}
	}

	// Resolve branch
	var sourceBranch string
	switch len(validBranches) {
	case 0:
		sourceBranch = "" // default branch
	case 1:
		sourceBranch = validBranches[0]
	default:
		msg := fmt.Sprintf("⚠️ Multiple valid branch labels found: %v. Only one allowed.", validBranches)
		AddCommentToIssue(r.Context(), event.Project.ID, event.ObjectAttributes.IID, msg)
		log.Printf("Multiple valid branch labels: %v", validBranches)
		http.Error(w, "Multiple valid branch labels", http.StatusBadRequest)
		return
	}

	log.Printf("Issue %d: trigger_label=%s, candidates=%v, valid_branches=%v, selected_branch=%s",
		event.ObjectAttributes.IID, triggerLabel, candidates, validBranches, sourceBranch)

	queue, err := LoadQueue()
	if err != nil {
		log.Printf("Failed to load queue: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	if IsProjectInQueue(queue, event.Project.ID) {
		// Update existing queue entry with new branch
		updated := false

		// Update NightlyQueue entry
		for i := range queue.NightlyQueue {
			if queue.NightlyQueue[i].ProjectID == event.Project.ID {
				queue.NightlyQueue[i].SourceBranch = sourceBranch
				queue.NightlyQueue[i].TriggerTime = time.Now()
				queue.NightlyQueue[i].TriggerIssueIID = event.ObjectAttributes.IID
				queue.NightlyQueue[i].TriggerType = "manual_issue_reopen"
				updated = true
				log.Printf("Updated NightlyQueue entry for %s with branch=%s", event.Project.PathWithNamespace, sourceBranch)
				break
			}
		}

		// Update InProgress if present (affects next resume)
		if !updated && queue.InProgress != nil && queue.InProgress.ProjectID == event.Project.ID {
			queue.InProgress.SourceBranch = sourceBranch
			queue.InProgress.TriggerIssueIID = event.ObjectAttributes.IID
			queue.InProgress.TriggerType = "manual_issue_reopen"
			updated = true
			log.Printf("Updated InProgress scan for %s with branch=%s (will use on next resume)", event.Project.PathWithNamespace, sourceBranch)
		}

		if updated {
			if err := SaveQueue(queue); err != nil {
				log.Printf("Failed to save queue after branch update: %v", err)
			}
		}

		// Post reopen comment with branch info
		msg := "🔄 Issue reopened — added to nightly scan queue"
		if sourceBranch != "" {
			msg += fmt.Sprintf(" (branch: %s)", sourceBranch)
		}
		if err := AddCommentToIssue(r.Context(), event.Project.ID, event.ObjectAttributes.IID, msg); err != nil {
			log.Printf("Failed to add reopen comment: %v", err)
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "updated",
			"project":  event.Project.PathWithNamespace,
			"branch":   sourceBranch,
			"reason":   "updated existing queue entry",
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
		SourceBranch:      sourceBranch,
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
		if sourceBranch != "" {
			msg += fmt.Sprintf(" (branch: %s)", sourceBranch)
		}
		if err := AddCommentToIssue(r.Context(), event.Project.ID, event.ObjectAttributes.IID, msg); err != nil {
			log.Printf("Failed to add reopen comment: %v", err)
		}
	}

	log.Printf("Queued project for nightly scan: %s (trigger issue #%d, branch=%s)", event.Project.PathWithNamespace, event.ObjectAttributes.IID, sourceBranch)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "queued",
		"project":  event.Project.PathWithNamespace,
		"branch":   sourceBranch,
		"position": len(queue.NightlyQueue),
	})
}

func ScanStatusHandler(w http.ResponseWriter, r *http.Request) {
	queue, err := LoadQueue()
	if err != nil {
		http.Error(w, "Failed to load queue", http.StatusInternalServerError)
		return
	}

	meta, err := LoadMetadata()
	if err != nil {
		log.Printf("Failed to load metadata: %v", err)
		meta = &ScanMetadata{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"queued_projects":      len(queue.NightlyQueue),
		"in_progress":          queue.InProgress != nil,
		"completed_this_night": len(queue.CompletedThisNight),
		"queue":                queue.NightlyQueue,
		"in_progress_details":  queue.InProgress,
		"completed":            queue.CompletedThisNight,
		"last_run_start":       meta.LastRunStart,
		"last_run_end":         meta.LastRunEnd,
		"last_run_duration_s":  meta.LastRunDurationSec,
		"last_run_success":     meta.LastRunSuccess,
		"projects_scanned":     meta.ProjectsScanned,
		"projects_succeeded":   meta.ProjectsSucceeded,
		"projects_failed":      meta.ProjectsFailed,
		"total_findings":       meta.TotalFindings,
		"next_run":             meta.NextScheduledRun,
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

	log.Printf("Note text: raw=%q clean=%q", event.ObjectAttributes.Note, noteClean)

	// Skip bot-generated comments to avoid infinite loops
	if isBotGeneratedComment(event.ObjectAttributes.Note) {
		log.Printf("Skipping note event: bot-generated comment")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "bot comment"})
		return
	}

	triggerPhrase := strings.ToLower(scanConfig.RescanTriggerPhrase)
	if !strings.Contains(noteClean, triggerPhrase) {
		log.Printf("Skipping note event: trigger phrase %q not found (clean=%q)", triggerPhrase, noteClean)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped", "reason": "trigger phrase not found"})
		return
	}

	// Parse branch from suffix (everything after action word)
	parts := strings.Fields(noteClean)
	sourceBranch := ""
	if len(parts) >= 3 {
		sourceBranch = strings.Join(parts[2:], " ")
	}

	// Validate branch if specified
	if sourceBranch != "" {
		if err := ValidateBranch(r.Context(), gitlabURL, gitlabToken, event.Project.PathWithNamespace, sourceBranch); err != nil {
			msg := fmt.Sprintf("⚠️ Branch validation failed: %v", err)
			AddCommentToIssue(r.Context(), event.Project.ID, event.Issue.IID, msg)
			log.Printf("Branch validation failed: %v", err)
			http.Error(w, "Branch validation failed", http.StatusBadRequest)
			return
		}
	}

	switch {
	case strings.Contains(noteClean, "review"):
		log.Printf("Immediate review triggered via comment on issue #%d (branch=%s)", event.Issue.IID, sourceBranch)
		if err := TriggerImmediateScan(r.Context(), event.Project.ID, event.Project.PathWithNamespace, event.Issue.IID, sourceBranch); err != nil {
			log.Printf("Failed to trigger immediate scan: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "accepted",
			"project": event.Project.PathWithNamespace,
			"branch":  sourceBranch,
			"type":    "immediate_review",
		})

	case strings.Contains(noteClean, "rescan"):
		log.Printf("Nightly rescan queued via comment on issue #%d (branch=%s)", event.Issue.IID, sourceBranch)
		if err := TriggerRescanOnComment(r.Context(), event.Project.ID, event.Project.PathWithNamespace, event.Issue.IID, sourceBranch); err != nil {
			log.Printf("Failed to trigger rescan: %v", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "queued",
			"project": event.Project.PathWithNamespace,
			"branch":  sourceBranch,
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

// parseLabels extracts trigger label and all non-trigger labels as branch candidates.
// Returns trigger label, candidate labels, and error if no trigger label found.
func parseLabels(labels []struct{ Title string `json:"title"` }, triggerLabels []string) (string, []string, error) {
	var triggerLabel string
	var candidates []string

	for _, label := range labels {
		isTrigger := false
		for _, tl := range triggerLabels {
			if strings.EqualFold(label.Title, tl) {
				isTrigger = true
				triggerLabel = label.Title
				break
			}
		}
		if isTrigger {
			continue
		}
		// All non-trigger labels are branch candidates (no heuristic filtering)
		candidates = append(candidates, label.Title)
	}

	if triggerLabel == "" {
		return "", nil, fmt.Errorf("no trigger label found")
	}
	return triggerLabel, candidates, nil
}

// isBotGeneratedComment checks if a comment was generated by the bot.
// Returns true if the comment matches known bot-generated patterns.
func isBotGeneratedComment(note string) bool {
	noteLower := strings.ToLower(note)
	botPatterns := []string{
		"issue reopened — added to nightly scan queue",
		"scan completed - no issues found",
		"scan complete —",
		"immediate review triggered",
		"nightly rescan queued",
		"branch validation failed",
		"multiple branch labels found",
		"superseded by new scan",
	}
	for _, pattern := range botPatterns {
		if strings.Contains(noteLower, pattern) {
			return true
		}
	}
	return false
}
