package api

import (
	"encoding/json"
	"net/http"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
)

type settingsResponse struct {
	Values map[string]string `json:"values"`
	Fields []settings.Field  `json:"fields"`
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	values, err := s.Store.Settings(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "read settings")
		return
	}
	effective := settings.ToMap(s.Cfg)
	for key, value := range values {
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

	// Validate on a copy: handlers read s.Cfg concurrently, and the running
	// process keeps its old config until the announced restart.
	next := s.Cfg
	if err := settings.Apply(&next, body.Values); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	var userID *uint
	if user, ok := auth.UserFrom(r.Context()); ok {
		userID = &user.ID
	}
	if err := s.Store.SaveSettings(r.Context(), userID, body.Values); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save settings")
		return
	}
	s.getSettings(w, r)
}
