// Package review runs the isolated OpenCode agent and checks provider health.
package review

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/llmmodels"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	reviewerAgent = "qa-reviewer"
	apiKeyEnvVar  = "QA_LLM_API_KEY"
	// maxAgentErrorDetail bounds the provider message stored as a health error.
	maxAgentErrorDetail = 500
	// defaultReviewerSteps caps one `opencode run` at 40 agentic iterations.
	// A pass carries at most MaxFilesPerPass files whose diff is attached, so
	// 40 read/grep rounds are plenty; without a cap a stuck agent loops until
	// the wall-clock timeout and the run ends with no useful diagnosis.
	defaultReviewerSteps = 40
)

// Agent runs one isolated `opencode run` invocation. Home is the temporary HOME
// used by the child; when empty, a temporary directory is created and removed
// automatically.
type Agent struct {
	Bin     string
	Dir     string
	Home    string
	Profile store.LLMProfile
	APIKey  string
	Timeout time.Duration
	// Steps is the agentic iteration cap written to the generated config.
	// Zero means defaultReviewerSteps.
	Steps int
}

// Run executes OpenCode with a private HOME/XDG environment and a locked-down
// configuration, then extracts the last contiguous text block and the token
// usage from the JSONL event stream.
func (a *Agent) Run(ctx context.Context, prompt string, files []string) (text string, usage []byte, err error) {
	home := a.Home
	if home == "" {
		tmp, mkErr := os.MkdirTemp("", "qa-opencode-home-")
		if mkErr != nil {
			return "", nil, fmt.Errorf("review: create temporary home: %w", mkErr)
		}
		home = tmp
		defer os.RemoveAll(tmp)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", nil, fmt.Errorf("review: create home %q: %w", home, err)
	}
	if err := writeOpenCodeConfig(home, a.Profile, a.Steps); err != nil {
		return "", nil, err
	}

	dir := a.Dir
	if dir == "" {
		dir = "."
	}
	bin := a.Bin
	if bin == "" {
		bin = "opencode"
	}

	runCtx := ctx
	if a.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, a.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, bin, buildArgs(a.Profile, dir, prompt, files)...)
	cmd.Dir = dir
	cmd.Env = childEnv(a.APIKey, home)
	isolateProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if cause := runCtx.Err(); cause != nil {
			return "", nil, fmt.Errorf("review: opencode: %w", cause)
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = agentErrorDetail(stdout.Bytes())
		}
		if detail == "" {
			return "", nil, fmt.Errorf("review: opencode: %w", err)
		}
		return "", nil, fmt.Errorf("review: opencode: %w: %s", err, detail)
	}

	text, usage = parseEvents(&stdout)
	return text, usage, nil
}

func buildArgs(profile store.LLMProfile, dir, prompt string, files []string) []string {
	args := []string{
		"run",
		"--pure",
		"--format", "json",
		"--agent", reviewerAgent,
		"--model", profile.Provider + "/" + profile.Model,
	}
	if profile.Variant != nil && *profile.Variant != "" {
		args = append(args, "--variant", *profile.Variant)
	}
	args = append(args, "--dir", dir, prompt)
	if len(files) > 0 {
		args = append(args, "-f")
		args = append(args, files...)
	}
	return args
}

// childEnv copies the parent environment without mutating it, replaces the
// credential and points all home-like variables at the isolated directory.
func childEnv(key, home string) []string {
	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch name {
		case apiKeyEnvVar, "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		apiKeyEnvVar+"="+key,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
	)
}

// writeOpenCodeConfig writes the global config inside the isolated HOME.
//
// OpenCode 1.19 drops the V2 `providers` block when it downgrades a config to
// V1 ("Omitted native setting that cannot be represented in V1"), leaving every
// provider unauthenticated. The deprecated-but-supported `provider` object with
// `options.apiKey: "{env:...}"` works in both shapes and keeps the key out of
// the file.
//
// The permissions are declared twice on purpose. Agent rules are resolved
// correctly (verified with `opencode debug agent qa-reviewer`), but a spawned
// subagent uses its own agent policy instead of inheriting the parent's, so the
// reviewer must be unable to spawn one: `task` is the V1 name for that action.
// The global block keeps every other tool denied even if a future version
// changes the inheritance rules.
func writeOpenCodeConfig(home string, profile store.LLMProfile, steps int) error {
	if steps <= 0 {
		steps = defaultReviewerSteps
	}
	options := map[string]string{
		"apiKey": "{env:" + apiKeyEnvVar + "}",
	}
	// OpenCode 1.19 only knows a fixed set of providers by name. For the rest
	// (CommandCode, for example) it builds "<undefined>/chat/completions"
	// unless the endpoint is written here. The table is closed and shared with
	// the model-listing API, so the client still cannot choose the URL.
	if profile.BaseURL != nil && *profile.BaseURL != "" {
		options["baseURL"] = *profile.BaseURL
	} else if baseURL, ok := llmmodels.ProviderBaseURL(profile.Provider); ok {
		options["baseURL"] = baseURL
	}
	provider := map[string]any{
		"options": options,
		// OpenCode 1.19 only runs model ids present in its catalog; declaring
		// the selected one keeps provider-api models usable before models.dev
		// catches up.
		"models": map[string]any{
			profile.Model: map[string]string{"name": profile.Model},
		},
	}

	permission := map[string]string{
		"read":               "allow",
		"edit":               "deny",
		"bash":               "deny",
		"task":               "deny",
		"webfetch":           "deny",
		"websearch":          "deny",
		"external_directory": "deny",
	}

	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"provider":   map[string]any{profile.Provider: provider},
		"permission": permission,
		"agent": map[string]any{
			reviewerAgent: map[string]any{
				"description": "Solo lectura: revisa el diff sin modificar el repo",
				"mode":        "primary",
				"permission":  permission,
				"steps":       steps,
			},
		},
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("review: encode opencode config: %w", err)
	}
	configDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("review: create opencode config dir: %w", err)
	}
	path := filepath.Join(configDir, "opencode.jsonc")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("review: write opencode config: %w", err)
	}
	return nil
}

// agentErrorData is the subset of OpenCode's provider error payload that helps
// an operator diagnose a failed health check. Some 1.19 errors only carry a
// generic message, so every field is optional.
type agentErrorData struct {
	Message      string          `json:"message"`
	Ref          string          `json:"ref"`
	ProviderID   string          `json:"providerID"`
	ModelID      string          `json:"modelID"`
	Reason       string          `json:"reason"`
	Status       int             `json:"status"`
	StatusCode   int             `json:"statusCode"`
	RetryAfterMs int64           `json:"retryAfterMs"`
	ResponseBody json.RawMessage `json:"responseBody"`
	URL          string          `json:"url"`
}

// agentErrorDetail extracts the message of the last error event in an OpenCode
// JSONL stream. OpenCode reports provider failures there rather than on stderr,
// so without this the caller only sees "exit status 1".
func agentErrorDetail(raw []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	detail := ""
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Error struct {
				Name string          `json:"name"`
				Data json.RawMessage `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "error" {
			continue
		}
		var data agentErrorData
		if len(ev.Error.Data) > 0 {
			// A malformed payload must not hide the error name; keep whatever
			// fields could be decoded and fall back to the plain format.
			_ = json.Unmarshal(ev.Error.Data, &data)
		}
		detail = describeAgentError(strings.TrimSpace(ev.Error.Name), data)
	}
	if len(detail) > maxAgentErrorDetail {
		detail = detail[:maxAgentErrorDetail]
	}
	return detail
}

// describeAgentError turns a provider error into the actionable sentence
// persisted as llm_profiles.health_error. The plain "name: message" format is
// kept as the fallback so unknown payloads stay readable.
func describeAgentError(name string, data agentErrorData) string {
	message := strings.TrimSpace(data.Message)
	reason := strings.TrimSpace(data.Reason)
	status := data.Status
	if status == 0 {
		status = data.StatusCode
	}
	statusLabel := ""
	if status > 0 {
		statusLabel = fmt.Sprintf(" (HTTP %d)", status)
	}

	var detail string
	switch {
	case name == "ModelNotFoundError":
		detail = fmt.Sprintf("el modelo %q no existe en el proveedor; revisa el modelo configurado en el perfil", agentModelLabel(data))
	case name == "UnsupportedModelError":
		detail = fmt.Sprintf("el modelo %q no está soportado por el proveedor; revisa el modelo o la capacidad solicitada", agentModelLabel(data))
	case name == "ProviderNotFoundError":
		detail = fmt.Sprintf("proveedor %q desconocido; providerID debe ser uno de los IDs de proveedor soportados por OpenCode", agentProviderLabel(data))
	case isNetworkFailure(message):
		detail = "fallo de red/DNS al conectar con el proveedor: " + message
	case reason == "authentication" || status == 401 || status == 403:
		detail = "credencial rechazada por el proveedor" + statusLabel + "; revisa la API key del perfil"
	case reason == "rate_limit" || status == 429:
		detail = "cuota o límite de peticiones agotado" + statusLabel + "; reintenta más tarde"
		if data.RetryAfterMs > 0 {
			detail += fmt.Sprintf(" (retryAfterMs=%d)", data.RetryAfterMs)
		}
	case reason == "overloaded" || status >= 500:
		detail = "proveedor no disponible" + statusLabel + "; reintenta más tarde"
	case reason == "invalid_request" || status == 400:
		detail = "el proveedor rechazó la petición" + statusLabel + "; revisa el modelo o los parámetros del perfil"
	case name == "UnknownError":
		detail = "OpenCode devolvió un error genérico (UnknownError)"
		if message != "" {
			detail += ": " + message
		}
		detail += "; revisa el proveedor, el modelo y la API key del perfil"
		if ref := strings.TrimSpace(data.Ref); ref != "" {
			detail += "; ref: " + ref
		}
	}

	if detail == "" {
		switch {
		case name != "" && message != "":
			detail = name + ": " + message
		case message != "":
			detail = message
		case name != "":
			detail = name
		}
	}

	extras := ""
	if body := responseBodyText(data.ResponseBody); body != "" && !strings.Contains(detail, body) {
		extras += "; responseBody: " + truncateDetailText(body, 200)
	}
	if url := strings.TrimSpace(data.URL); url != "" {
		extras += "; url: " + url
	}
	if extras != "" {
		if detail == "" {
			detail = strings.TrimPrefix(extras, "; ")
		} else {
			detail += extras
		}
	}
	return detail
}

func agentProviderLabel(data agentErrorData) string {
	if provider := strings.TrimSpace(data.ProviderID); provider != "" {
		return provider
	}
	return "desconocido"
}

func agentModelLabel(data agentErrorData) string {
	provider := strings.TrimSpace(data.ProviderID)
	model := strings.TrimSpace(data.ModelID)
	switch {
	case provider != "" && model != "":
		return provider + "/" + model
	case model != "":
		return model
	case provider != "":
		return provider + "/desconocido"
	default:
		return "desconocido"
	}
}

// isNetworkFailure spots transport failures that OpenCode sometimes reports as
// a generic error, where the message is the only useful diagnostic.
func isNetworkFailure(message string) bool {
	lower := strings.ToLower(message)
	for _, pattern := range []string{
		"no such host",
		"dial tcp",
		"connection refused",
		"i/o timeout",
		"temporary failure in name resolution",
	} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// responseBodyText renders responseBody whether OpenCode sent a JSON string or
// an embedded JSON object.
func responseBodyText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return strings.TrimSpace(string(raw))
}

func truncateDetailText(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// parseEvents keeps the last contiguous block of text events and the most
// recent token-usage object from an OpenCode JSONL stream.
func parseEvents(r io.Reader) (string, []byte) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var current strings.Builder
	var last string
	var usage []byte
	flush := func() {
		if block := strings.TrimSpace(current.String()); block != "" {
			last = block
		}
		current.Reset()
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Part struct {
				Type   string          `json:"type"`
				Text   string          `json:"text"`
				Tokens json.RawMessage `json:"tokens"`
				Usage  json.RawMessage `json:"usage"`
			} `json:"part"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		block := ev.Part.Text
		if block == "" {
			block = ev.Text
		}
		if (ev.Type == "text" || ev.Part.Type == "text") && block != "" {
			current.WriteString(block)
			continue
		}

		flush()
		switch {
		case len(ev.Part.Tokens) > 0:
			usage = append([]byte(nil), ev.Part.Tokens...)
		case len(ev.Part.Usage) > 0:
			usage = append([]byte(nil), ev.Part.Usage...)
		case len(ev.Usage) > 0:
			usage = append([]byte(nil), ev.Usage...)
		}
	}
	flush()
	return last, usage
}
