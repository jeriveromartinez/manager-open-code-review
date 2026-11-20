package api

import (
	"encoding/json"
	"net/http"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
)

type settingsResponse struct {
	Values map[string]string `json:"values"`
	Fields []settings.Field  `json:"fields"`
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	stored, err := s.Store.Settings(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "read settings")
		return
	}
	s.writeSettings(w, stored, nil)
}

// writeSettings answers with the process's configuration overlaid by the stored
// rows and by the values just saved, so a save never fails on a re-read after
// committing.
func (s *server) writeSettings(w http.ResponseWriter, stored, justSaved map[string]string) {
	effective := settings.ToMap(s.Cfg)
	for key, value := range stored {
		effective[key] = value
	}
	for key, value := range justSaved {
		effective[key] = value
	}
	writeJSON(w, http.StatusOK, settingsResponse{Values: effective, Fields: settings.Fields()})
}

func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Values) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no values to save")
		return
	}

	// The baseline is the configuration the next start will load, not s.Cfg:
	// the process keeps its startup config until the announced restart, so
	// validating against it would let two saves persist an incoherent pair
	// (review_timeout 40m, then review_stale_after 30m) that aborts the next
	// boot.
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	stored, err := s.Store.Settings(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "read settings")
		return
	}
	next := config.Default()
	if err := settings.Apply(&next, stored); err != nil {
		// Only validated values are ever written through this API, so a bad row
		// is corruption that needs SQL, not a save the panel refuses to accept.
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := settings.Apply(&next, body.Values); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persist the normalised values, so the stored text matches what the
	// process will parse at the next start.
	validated := settings.ToMap(next)
	saving := make(map[string]string, len(body.Values))
	for key := range body.Values {
		saving[key] = validated[key]
	}

	user, ok := auth.UserFrom(r.Context())
	var userID *uint
	if ok {
		userID = &user.ID
	}
	if err := s.Store.SaveSettings(r.Context(), userID, saving); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save settings")
		return
	}
	// s.Cfg is not mutated: concurrent handlers read it.
	s.writeSettings(w, stored, saving)
}
