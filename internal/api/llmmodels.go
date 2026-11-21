package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/llmmodels"
)

const maxAPIKeyBytes = 8192

// LLMModelLister resolves the models a platform offers for one API key.
type LLMModelLister interface {
	Models(ctx context.Context, provider, apiKey string) ([]llmmodels.Model, error)
}

func (s *server) llmModels(w http.ResponseWriter, r *http.Request) {
	raw, ok := decodeRawBody(w, r)
	if !ok {
		return
	}
	var body struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	provider := strings.TrimSpace(body.Provider)
	apiKey := strings.TrimSpace(body.APIKey)
	if provider == "" || apiKey == "" {
		writeAPIError(w, http.StatusBadRequest, "provider and api_key are required")
		return
	}
	if len(apiKey) > maxAPIKeyBytes {
		writeAPIError(w, http.StatusBadRequest, "api_key is too long")
		return
	}
	if s.Models == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "model catalog unavailable")
		return
	}

	models, err := s.Models.Models(r.Context(), provider, apiKey)
	if errors.Is(err, llmmodels.ErrUnknownProvider) {
		writeAPIError(w, http.StatusBadRequest, "unknown provider")
		return
	}
	if err != nil {
		var upstream *llmmodels.UpstreamError
		if errors.As(err, &upstream) && upstream.Status > 0 {
			writeAPIError(w, http.StatusBadGateway, fmt.Sprintf("provider returned HTTP %d", upstream.Status))
			return
		}
		writeAPIError(w, http.StatusBadGateway, "provider did not answer")
		return
	}
	if models == nil {
		models = []llmmodels.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
