package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// NativeVerifier runs one focused `ocr scan` over the files that already have
// findings. It answers a verdict per finding instead of looking for new ones.
type NativeVerifier struct {
	Bin         string
	RepoDir     string
	Home        string
	Profile     store.LLMProfile
	APIKey      string
	Paths       []string
	Background  string
	MaxTokens   int
	TokenBudget int64
	Timeout     time.Duration
	OnLog       func(line string)
}

// verifyInvocation is the flag set of one `ocr scan`. It has no effort: scan
// does not accept --effort, and the finding context travels inline because scan
// has no --background-file.
type verifyInvocation struct {
	RepoDir     string
	Paths       []string
	RulePath    string
	Background  string
	Profile     store.LLMProfile
	MaxTokens   int
	TokenBudget int64
	TaskTimeout time.Duration
}

func buildScanArgs(in verifyInvocation) []string {
	args := []string{
		"scan",
		"--repo", in.RepoDir,
		"--path", strings.Join(in.Paths, ","),
		"--rule", in.RulePath,
		"--background", in.Background,
		"--format", "json",
		"--audience", "human",
		"--provider", nativeProviderName,
		"--model", in.Profile.Model,
		"--max-tokens", fmt.Sprint(in.MaxTokens),
	}
	if in.TokenBudget > 0 {
		args = append(args, "--max-tokens-budget", fmt.Sprint(in.TokenBudget))
	}
	if in.TaskTimeout > 0 {
		minutes := int(in.TaskTimeout / time.Minute)
		if minutes < 1 {
			minutes = 1
		}
		args = append(args, "--timeout", fmt.Sprint(minutes))
	}
	return args
}

// verifyRuleJSON is the system rule every scanned file receives. The contract
// it pins is what ParseVerdicts reads.
func verifyRuleJSON() []byte {
	rule := struct {
		SchemaVersion string `json:"schema_version"`
		Groups        []struct {
			GroupID int    `json:"group_id"`
			Source  string `json:"source"`
			Pattern string `json:"pattern"`
			Rule    string `json:"rule"`
		} `json:"groups"`
	}{SchemaVersion: "1"}
	rule.Groups = append(rule.Groups, struct {
		GroupID int    `json:"group_id"`
		Source  string `json:"source"`
		Pattern string `json:"pattern"`
		Rule    string `json:"rule"`
	}{
		GroupID: 1,
		Source:  "system",
		Pattern: "**/*",
		Rule: `You are verifying findings already reported. For each finding in the context,
decide CONFIRM, REFUTE, or UNCERTAIN and emit exactly one comment placed at its
lines. The first non-empty line MUST be:
QA-VERIFY <finding_id> <CONFIRM|REFUTE|UNCERTAIN>
The second line MUST be:
EVIDENCE: <one line copied verbatim from the file at the reported revision>
If no such line proves the verdict, answer UNCERTAIN. Do not report new
findings. Do not report style, naming, or test suggestions. Never REFUTE
without evidence that contradicts the claim.`,
	})
	encoded, err := json.Marshal(rule)
	if err != nil {
		// The literal above is static; a marshal failure is a programming error.
		panic(fmt.Sprintf("review: encode verify rule: %v", err))
	}
	return encoded
}

// buildVerifyContext renders the findings as the verifier's background. Every
// model-generated value is fenced: OCR reads this text as instructions.
func buildVerifyContext(findings []store.Finding) string {
	var b strings.Builder
	b.WriteString("Verify these findings against the code at the reported revision. ")
	b.WriteString("Emit exactly one QA-VERIFY comment per finding, with an EVIDENCE line copied literally from the file.\n")
	for _, f := range findings {
		fmt.Fprintf(&b, "\n## %s\n", f.ID)
		fmt.Fprintf(&b, "- id: %s\n", f.ID)
		fmt.Fprintf(&b, "- severity: %s\n", fence(f.Severity))
		fmt.Fprintf(&b, "- category: %s\n", fence(f.Category))
		fmt.Fprintf(&b, "- path: %s:%d-%d\n", fence(f.Path), f.StartLine, f.EndLine)
		fmt.Fprintf(&b, "- title: %s\n", fence(f.Title))
		if body := strings.TrimSpace(f.Body); body != "" {
			fmt.Fprintf(&b, "- body: %s\n", fence(body))
		}
		if suggestion := strings.TrimSpace(f.Suggestion); suggestion != "" {
			fmt.Fprintf(&b, "- suggestion: %s\n", fence(suggestion))
		}
	}
	return b.String()
}

// Review executes the scan and parses its JSON with the same path as a review.
func (v *NativeVerifier) Review(ctx context.Context) (nativeReview, []byte, error) {
	home := v.Home
	if home == "" {
		tmp, err := os.MkdirTemp("", "qa-ocr-verify-home-")
		if err != nil {
			return nativeReview{}, nil, fmt.Errorf("verify: create temporary home: %w", err)
		}
		home = tmp
		defer os.RemoveAll(tmp)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nativeReview{}, nil, fmt.Errorf("verify: create home %q: %w", home, err)
	}

	keyPath, err := writeNativeKeyHelper(home)
	if err != nil {
		return nativeReview{}, nil, err
	}
	remaining := v.Timeout
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); remaining <= 0 || left < remaining {
			remaining = left
		}
	}
	taskTimeout, requestTimeout := ocrBudget(remaining, store.ReviewEffortLow)
	if err := writeNativeConfig(home, v.Profile, keyPath, requestTimeout); err != nil {
		return nativeReview{}, nil, err
	}
	rulePath := filepath.Join(home, "verify-rule.json")
	if err := os.WriteFile(rulePath, verifyRuleJSON(), 0o600); err != nil {
		return nativeReview{}, nil, fmt.Errorf("verify: write rule: %w", err)
	}

	bin := v.Bin
	if bin == "" {
		bin = "ocr"
	}
	args := buildScanArgs(verifyInvocation{
		RepoDir:     v.RepoDir,
		Paths:       v.Paths,
		RulePath:    rulePath,
		Background:  v.Background,
		Profile:     v.Profile,
		MaxTokens:   v.MaxTokens,
		TokenBudget: v.TokenBudget,
		TaskTimeout: taskTimeout,
	})

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = v.RepoDir
	cmd.Env = reviewEnv(os.Environ(), v.APIKey, home)
	isolateProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout

	var runErr error
	if v.OnLog != nil {
		runErr = runNativeStreaming(cmd, &stderr, v.OnLog)
	} else {
		cmd.Stderr = &stderr
		runErr = cmd.Run()
	}
	if cause := ctx.Err(); cause != nil {
		return nativeReview{}, nil, fmt.Errorf("ocr scan: %w", cause)
	}
	if len(bytes.TrimSpace(stdout.Bytes())) == 0 {
		return nativeReview{}, nil, nativeExecError(runErr, stderr.String())
	}
	res, err := parseNativeReview(stdout.Bytes())
	if err != nil {
		return nativeReview{}, stdout.Bytes(), nativeExecError(runErr, stderr.String())
	}
	return res, stdout.Bytes(), nil
}
