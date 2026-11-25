package store

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/datatypes"
)

// Finding scopes. A finding is anchored to one file, to several files or to
// the whole pull request. An empty stored value means file, the scope every
// row written before #162 has.
const (
	FindingScopeFile  = "file"
	FindingScopeFiles = "files"
	FindingScopePR    = "pr"
)

// Citation sides. RIGHT cites the head of the comparison; LEFT cites the code
// the change removed, which only exists in the base.
const (
	FindingCitationHead = "RIGHT"
	FindingCitationBase = "LEFT"
)

// FindingCitation is one verified path:line reference that anchors a finding
// to the compared code. A deletion cites the base side.
type FindingCitation struct {
	Path         string `json:"path"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	Side         string `json:"side,omitempty"`
	ExistingCode string `json:"existing_code,omitempty"`
}

// NormalizeFindingCitations canonicalizes the citations the review and the
// publisher share: paths collapse to one spelling, ranges are clamped, the
// side defaults to the head and the list is sorted and deduplicated. Invalid
// citations without a path or a line are dropped instead of anchoring the
// finding to nowhere.
func NormalizeFindingCitations(citations []FindingCitation) []FindingCitation {
	out := make([]FindingCitation, 0, len(citations))
	index := make(map[string]int, len(citations))
	for _, citation := range citations {
		citation.Path = normalizeFindingPath(citation.Path)
		if citation.Path == "" || citation.StartLine <= 0 {
			continue
		}
		if citation.EndLine < citation.StartLine {
			citation.EndLine = citation.StartLine
		}
		citation.Side = normalizeFindingCitationSide(citation.Side)
		citation.ExistingCode = normalizeFindingCitationCode(citation.ExistingCode)

		key := fmt.Sprintf("%s\x00%d\x00%d\x00%s", citation.Path, citation.StartLine, citation.EndLine, citation.Side)
		if i, ok := index[key]; ok {
			if out[i].ExistingCode == "" {
				out[i].ExistingCode = citation.ExistingCode
			}
			continue
		}
		index[key] = len(out)
		out = append(out, citation)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		if out[i].EndLine != out[j].EndLine {
			return out[i].EndLine < out[j].EndLine
		}
		if out[i].Side != out[j].Side {
			return out[i].Side < out[j].Side
		}
		return out[i].ExistingCode < out[j].ExistingCode
	})
	return out
}

// FindingCitationIdentity is the line-independent identity of a citation set.
// A push that shifts every cited line keeps the finding's identity and its
// GitHub references; different files or different cited code do not.
func FindingCitationIdentity(citations []FindingCitation) string {
	normalized := NormalizeFindingCitations(citations)
	parts := make([]string, 0, len(normalized))
	seen := make(map[string]bool, len(normalized))
	for _, citation := range normalized {
		part := citation.Path + "\x00" + citation.Side + "\x00" + citation.ExistingCode
		if seen[part] {
			continue
		}
		seen[part] = true
		parts = append(parts, part)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}

// NormalizeFindingScope keeps the scope coherent with the citations: two
// different paths are always "files", and an unknown or empty scope becomes
// "file" unless the citations already span files.
func NormalizeFindingScope(scope string, citations []FindingCitation) string {
	paths := make(map[string]bool)
	for _, citation := range NormalizeFindingCitations(citations) {
		paths[citation.Path] = true
	}
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case FindingScopePR:
		return FindingScopePR
	case FindingScopeFiles:
		if len(paths) > 1 {
			return FindingScopeFiles
		}
		return FindingScopeFile
	}
	if len(paths) > 1 {
		return FindingScopeFiles
	}
	return FindingScopeFile
}

// DecodeFindingCitations reads the stored JSON column. A malformed or empty
// value yields no citations, which keeps the finding on the legacy single
// path behavior.
func DecodeFindingCitations(raw []byte) []FindingCitation {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	var citations []FindingCitation
	if err := json.Unmarshal(raw, &citations); err != nil {
		return nil
	}
	return NormalizeFindingCitations(citations)
}

// EncodeFindingCitations serializes the canonical citation set. An empty list
// stores NULL so legacy rows and findings without citations stay identical.
func EncodeFindingCitations(citations []FindingCitation) datatypes.JSON {
	normalized := NormalizeFindingCitations(citations)
	if len(normalized) == 0 {
		return nil
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return nil
	}
	return datatypes.JSON(raw)
}

func normalizeFindingCitationSide(side string) string {
	if strings.EqualFold(strings.TrimSpace(side), FindingCitationBase) {
		return FindingCitationBase
	}
	return FindingCitationHead
}

func normalizeFindingCitationCode(code string) string {
	return strings.Join(strings.Fields(code), " ")
}

func normalizeFindingPath(value string) string {
	cleaned := path.Clean(filepath.ToSlash(strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")))
	if cleaned == "." {
		return ""
	}
	return cleaned
}
