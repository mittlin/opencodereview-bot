package scan

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CloneRepo clones a repository with optional branch/fetch/checkout.
// sourceBranch: if non-empty, adds --branch to clone.
// targetBranch: if non-empty, runs git fetch origin <targetBranch>.
// commitSHA: if non-empty, runs git checkout <commitSHA>.
// Calls InitSubmodulesWithAuth at the end.
func CloneRepo(ctx context.Context, projectID int, projectPath, gitlabURLVal, gitlabTokenVal,
	sourceBranch, targetBranch, commitSHA string) (string, error) {

	repoDir := fmt.Sprintf("/tmp/ocr-repo-%d-%d", projectID, time.Now().UnixNano())

	// Build clone URL with group token
	gitlabCloneURL := gitlabURLVal
	if gitlabTokenVal != "" {
		prefix := "http://"
		suffix := gitlabURLVal
		if strings.HasPrefix(gitlabURLVal, "https://") {
			prefix = "https://"
			suffix = strings.TrimPrefix(gitlabURLVal, "https://")
		} else {
			suffix = strings.TrimPrefix(gitlabURLVal, "http://")
		}
		gitlabCloneURL = prefix + "oauth2:" + gitlabTokenVal + "@" + suffix
	}

	gitlabCloneURL = gitlabCloneURL + "/" + projectPath + ".git"

	// Clone
	args := []string{"clone", "--depth", "50"}
	if sourceBranch != "" {
		args = append(args, "--branch", sourceBranch)
	}
	args = append(args, gitlabCloneURL, repoDir)

	cmd := exec.CommandContext(ctx, "git", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone failed: %w, output: %s", err, string(output))
	}

	// Fetch target branch if specified
	if targetBranch != "" {
		cmd = exec.CommandContext(ctx, "git", "-C", repoDir, "fetch", "origin", targetBranch)
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git fetch target failed: %w, output: %s", err, string(output))
		}
	}

	// Checkout commit if specified
	if commitSHA != "" {
		cmd = exec.CommandContext(ctx, "git", "-C", repoDir, "checkout", commitSHA)
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git checkout target commit failed: %w, output: %s", err, string(output))
		}
	}

	// Init submodules with insteadOf config to inject token into submodule URLs
	if err := InitSubmodulesWithAuth(ctx, repoDir, gitlabTokenVal); err != nil {
		log.Printf("Warning: submodule init failed: %v", err)
	}

	return repoDir, nil
}

// InitSubmodulesWithAuth reads .gitmodules, builds insteadOf rules to inject
// the GitLab token into submodule URLs, and runs submodule update.
// Uses GIT_CONFIG_GLOBAL with a temp file so child processes (git clone for
// submodules) inherit the insteadOf rules.
func InitSubmodulesWithAuth(ctx context.Context, repoDir, gitlabToken string) error {
	if gitlabToken == "" {
		// No token — try plain submodule update
		cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "submodule", "update", "--init", "--recursive", "--depth", "50")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git submodule update failed: %w, output: %s", err, string(output))
		}
		logSubmoduleStatus(ctx, repoDir)
		return nil
	}

	// Parse .gitmodules to find unique (scheme, host) pairs needing auth
	hosts := parseGitmodulesHosts(repoDir)
	if len(hosts) == 0 {
		return nil
	}

	// Build a temporary git config file with insteadOf rules
	var cfg strings.Builder
	for _, h := range hosts {
		// scheme://HOST/ → scheme://oauth2:TOKEN@HOST/
		src := h.scheme + "://" + h.host + "/"
		dst := h.scheme + "://oauth2:" + gitlabToken + "@" + h.host + "/"
		fmt.Fprintf(&cfg, "[url \"%s\"]\n\tinsteadOf = %s\n", dst, src)
	}

	tmpFile, err := os.CreateTemp("", "ocr-gitconfig-*")
	if err != nil {
		return fmt.Errorf("create temp git config: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.WriteString(cfg.String()); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp git config: %w", err)
	}
	tmpFile.Close()

	// GIT_CONFIG_GLOBAL makes all child processes (including git clone for
	// submodules) inherit the insteadOf rules.
	env := os.Environ()
	env = append(env, "GIT_CONFIG_GLOBAL="+tmpPath)

	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "submodule", "update", "--init", "--recursive", "--depth", "50")
	cmd.Env = env
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git submodule update failed: %w, output: %s", err, string(output))
	}
	logSubmoduleStatus(ctx, repoDir)
	return nil
}

// logSubmoduleStatus logs the status of all submodules after update.
func logSubmoduleStatus(ctx context.Context, repoDir string) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "submodule", "status", "--recursive")
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[submodule] status check failed: %v, output: %s", err, string(output))
	} else {
		log.Printf("[submodule] status:\n%s", string(output))
	}
}

// submoduleHost holds scheme and host for a submodule URL.
type submoduleHost struct {
	scheme string // "http" or "https"
	host   string // hostname without port/path
}

// parseGitmodulesHosts extracts unique (scheme, host) pairs from .gitmodules URLs.
func parseGitmodulesHosts(repoDir string) []submoduleHost {
	data, err := os.ReadFile(filepath.Join(repoDir, ".gitmodules"))
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var hosts []submoduleHost
	// Match: url = <scheme>://<host>[:port]/<path> or url = <scheme>://<user>@<host>[:port]/<path>
	re := regexp.MustCompile(`(?i)^\s*url\s*=\s*(\w+)://(?:[^/@]+@)?([^/:]+)`)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "url") {
			continue
		}
		matches := re.FindStringSubmatch(line)
		if len(matches) != 3 {
			continue
		}
		scheme := strings.ToLower(matches[1])
		host := matches[2]
		key := scheme + "://" + host
		if host != "" && !seen[key] {
			seen[key] = true
			hosts = append(hosts, submoduleHost{scheme: scheme, host: host})
		}
	}
	return hosts
}

// buildGitURL builds an authenticated Git URL for git ls-remote.
// Format: scheme://oauth2:TOKEN@host/group/repo.git
func buildGitURL(gitlabURL, gitlabToken, projectPath string) string {
	u, _ := url.Parse(gitlabURL)
	return fmt.Sprintf("%s://oauth2:%s@%s/%s.git", u.Scheme, gitlabToken, u.Host, projectPath)
}

// ValidateBranch checks if a branch exists in the remote repository using git ls-remote.
// Returns error if branch not found or git command fails.
func ValidateBranch(ctx context.Context, gitlabURL, gitlabToken, projectPath, branch string) error {
	if branch == "" {
		return nil // default branch, no validation needed
	}
	url := buildGitURL(gitlabURL, gitlabToken, projectPath)
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", url, branch)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git ls-remote failed: %w, output: %s", err, string(output))
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return fmt.Errorf("branch %q not found in repository", branch)
	}
	return nil
}

// ExtractBranchFromNote extracts branch from note preserving original case.
// triggerPhrase: e.g. "@ocr-bot review" from OCR_RESCAN_TRIGGER_PHRASE config.
// Uses case-insensitive match for trigger phrase but preserves original case of branch.
func ExtractBranchFromNote(note, triggerPhrase string) string {
	lowerNote := strings.ToLower(note)
	lowerTrigger := strings.ToLower(triggerPhrase)
	idx := strings.Index(lowerNote, lowerTrigger)
	if idx == -1 {
		return ""
	}
	branchPart := strings.TrimSpace(note[idx+len(triggerPhrase):])
	return branchPart
}

// ResolveBranch validates candidates via git ls-remote.
// Returns: (branch, error) where:
//   - branch="" + nil error = default branch (0 valid)
//   - branch="X" + nil error = use branch X (1 valid)
//   - branch="" + error = multiple valid branches (>1 valid)
func ResolveBranch(ctx context.Context, gitlabURL, gitlabToken, projectPath string, candidates []string) (string, error) {
	var validBranches []string
	for _, candidate := range candidates {
		if err := ValidateBranch(ctx, gitlabURL, gitlabToken, projectPath, candidate); err == nil {
			validBranches = append(validBranches, candidate)
		}
	}
	switch len(validBranches) {
	case 0:
		return "", nil // default branch
	case 1:
		return validBranches[0], nil
	default:
		return "", fmt.Errorf("multiple valid branch labels found: %v. Only one allowed.", validBranches)
	}
}
