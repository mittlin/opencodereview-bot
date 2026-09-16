package scan

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// initSubmodulesWithAuth reads .gitmodules, builds insteadOf rules to inject
// the GitLab token into submodule URLs, and runs submodule update.
// Uses GIT_CONFIG_GLOBAL with a temp file so child processes (git clone for
// submodules) inherit the insteadOf rules.
func initSubmodulesWithAuth(ctx context.Context, repoDir, gitlabToken string) error {
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