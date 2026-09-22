package scan

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// RunOCR executes the OCR binary with the given args and env, reads JSON output,
// and returns parsed comments and summary. This is the single source of truth
// for OCR execution used by both runReviewWithRepoDir (bot.go) and runScanChunk.
func RunOCR(ctx context.Context, args []string, env []string, outputFile string) ([]ReviewComment, string, error) {
	cmd := exec.CommandContext(ctx, "/usr/local/bin/ocr", args...)
	cmd.Env = env

	var stderr strings.Builder
	cmd.Stdout = nil
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		log.Printf("OCR stderr: %s", stderr.String())
		return nil, "", fmt.Errorf("ocr failed: %w, stderr: %s", err, stderr.String())
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		return nil, "", fmt.Errorf("read ocr output: %w", err)
	}

	var ocrResult map[string]interface{}
	if err := json.Unmarshal(data, &ocrResult); err != nil {
		return nil, "", fmt.Errorf("parse ocr output: %w", err)
	}

	comments := ConvertComments(ocrResult["comments"])
	summary := ""
	if m := ocrResult["message"]; m != nil {
		summary = m.(string)
	}

	return comments, summary, nil
}

// AppendOCRArgs appends optional OCR flags (--max-tokens-budget, --effort, --provider)
// to the args slice. Used by both runReviewWithRepoDir and runScanChunk.
func AppendOCRArgs(args []string, maxTokensBudget, effort, provider string) []string {
	if maxTokensBudget != "" {
		args = append(args, "--max-tokens-budget", maxTokensBudget)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if provider != "" {
		args = append(args, "--provider", provider)
	}
	return args
}

// BuildOCREnv builds the environment variables for OCR execution.
func BuildOCREnv(llmURL, llmToken, llmModel string, extraEnv map[string]string) []string {
	env := append(os.Environ(),
		"OCR_LLM_URL="+llmURL,
		"OCR_LLM_TOKEN="+llmToken,
		"OCR_LLM_MODEL="+llmModel,
		"HOME="+OCRHomeDir,
	)
	for k, v := range extraEnv {
		if k != "" && v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}
