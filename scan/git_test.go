package scan

import (
	"context"
	"os"
	"testing"
)

func TestExtractBranchFromNote(t *testing.T) {
	testCases := []struct {
		name         string
		note         string
		triggerPhrase string
		expected     string
	}{
		{
			name:         "review with branch - case sensitive",
			note:         "@ocr-bot review VmEco14",
			triggerPhrase: "@ocr-bot review",
			expected:     "VmEco14",
		},
		{
			name:         "rescan with branch",
			note:         "@ocr-bot review feature/new-ui",
			triggerPhrase: "@ocr-bot review",
			expected:     "feature/new-ui",
		},
		{
			name:         "trigger phrase case insensitive",
			note:         "@OCR-BOT REVIEW develop",
			triggerPhrase: "@ocr-bot review",
			expected:     "develop",
		},
		{
			name:         "branch with multiple words",
			note:         "@ocr-bot review feature/A feature/B",
			triggerPhrase: "@ocr-bot review",
			expected:     "feature/A feature/B",
		},
		{
			name:         "no branch specified",
			note:         "@ocr-bot review",
			triggerPhrase: "@ocr-bot review",
			expected:     "",
		},
		{
			name:         "trigger phrase not found",
			note:         "some other text",
			triggerPhrase: "@ocr-bot review",
			expected:     "",
		},
		{
			name:         "with punctuation",
			note:         "@ocr-bot review VmEco14!",
			triggerPhrase: "@ocr-bot review",
			expected:     "VmEco14!",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := ExtractBranchFromNote(tc.note, tc.triggerPhrase)
			if result != tc.expected {
				t.Errorf("ExtractBranchFromNote(%q, %q) = %q, want %q",
					tc.note, tc.triggerPhrase, result, tc.expected)
			}
		})
	}
}

func TestResolveBranch(t *testing.T) {
	// Skip if no GitLab credentials
	gitlabURL := os.Getenv("GITLAB_URL")
	gitlabToken := os.Getenv("GITLAB_GROUP_TOKEN")
	if gitlabURL == "" || gitlabToken == "" {
		t.Skip("Skipping: GITLAB_URL and GITLAB_GROUP_TOKEN required")
	}

	projectPath := os.Getenv("TEST_PROJECT_PATH")
	if projectPath == "" {
		t.Skip("Skipping: TEST_PROJECT_PATH required")
	}

	ctx := context.Background()

	testCases := []struct {
		name          string
		candidates    []string
		expectError   bool
		expectedValid int // 0=default, 1=branch found
	}{
		{
			name:          "valid existing branch",
			candidates:    []string{"main"},
			expectedValid: 1,
			expectError:   false,
		},
		{
			name:          "non-existent branch",
			candidates:    []string{"this-branch-definitely-does-not-exist-12345"},
			expectedValid: 0,
			expectError:   false,
		},
		{
			name:          "multiple valid branches",
			candidates:    []string{"main", "develop"},
			expectedValid: 2,
			expectError:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			branch, err := ResolveBranch(ctx, gitlabURL, gitlabToken, projectPath, tc.candidates)

			if tc.expectError {
				if err == nil {
					t.Errorf("Expected error for multiple valid branches, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if tc.expectedValid == 0 && branch != "" {
				t.Errorf("Expected empty branch (default), got %q", branch)
			}
			if tc.expectedValid == 1 && branch == "" {
				t.Errorf("Expected valid branch, got empty")
			}
		})
	}
}

func TestResolveBranch_CaseSensitivity(t *testing.T) {
	gitlabURL := os.Getenv("GITLAB_URL")
	gitlabToken := os.Getenv("GITLAB_GROUP_TOKEN")
	projectPath := os.Getenv("TEST_PROJECT_PATH")
	if gitlabURL == "" || gitlabToken == "" || projectPath == "" {
		t.Skip("Skipping: requires GitLab credentials")
	}

	ctx := context.Background()

	// First, find a branch with mixed case to test
	// Try common branches that might have mixed case
	testBranches := []string{"VmEco14", "Feature/Test", "Release/v1.0"}
	foundBranch := ""

	for _, b := range testBranches {
		err := ValidateBranch(ctx, gitlabURL, gitlabToken, projectPath, b)
		if err == nil {
			foundBranch = b
			break
		}
	}

	if foundBranch == "" {
		t.Skip("No mixed-case branch found to test case sensitivity")
	}

	t.Logf("Testing case sensitivity with branch: %s", foundBranch)

	// Test that exact case works
	branch, err := ResolveBranch(ctx, gitlabURL, gitlabToken, projectPath, []string{foundBranch})
	if err != nil {
		t.Fatalf("ResolveBranch(%q) failed: %v", foundBranch, err)
	}
	if branch != foundBranch {
		t.Errorf("Expected %q, got %q", foundBranch, branch)
	}

	// Test that lowercase fails
	lowerBranch := ""
	for _, r := range foundBranch {
		if r >= 'A' && r <= 'Z' {
			lowerBranch += string(r + 32)
		} else {
			lowerBranch += string(r)
		}
	}

	branch, err = ResolveBranch(ctx, gitlabURL, gitlabToken, projectPath, []string{lowerBranch})
	if err == nil {
		t.Errorf("Expected error for lowercase %q, got branch=%q", lowerBranch, branch)
	}
}