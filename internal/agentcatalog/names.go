package agentcatalog

import (
	"strings"

	"github.com/repogo/host/internal/agent"
)

// Namer is a Provider that can read a name off its own model ids
// (`claude-opus-5-5` → "Opus 5.5"), for the ids a transcript records and the
// catalog does not list. Empty means the id is not in a shape it knows.
type Namer interface {
	ModelName(id string) string
}

// ModelLabel is what a phone shows for a model id as a chat or a transcript
// records it: the catalog's name when an entry or its alias matches, else the
// name read off the id. Only the cached catalog is read, never the CLI.
func (s *Service) ModelLabel(kind agent.Kind, id string) string {
	if id == "" {
		return ""
	}
	namer, _ := s.namer(kind)
	if c, ok := s.cachedValue(kind); ok {
		if m, ok := c.model(id); ok && m.Label != "" {
			return m.Label
		}
	}
	return nameFromID(namer, id, id)
}

// label is a catalog model's name: read off its id when the agent knows the
// id's shape, so every model of one agent reads alike, else the CLI's name.
func label(namer Namer, m Model) string {
	return nameFromID(namer, m.ID, m.Name)
}

func nameFromID(namer Namer, id, fallback string) string {
	if namer != nil {
		if name := namer.ModelName(id); name != "" {
			return name
		}
	}
	return fallback
}

func (s *Service) namer(kind agent.Kind) (Namer, bool) {
	provider, ok := agent.Find(s.providers, kind, Provider.Kind)
	if !ok {
		return nil, false
	}
	namer, ok := provider.(Namer)
	return namer, ok
}

// model is the entry for id: an exact id, hidden ones included, else the
// listed alias it resolves to. Hidden entries are skipped there, since Claude's
// hidden `default` resolves to the same model as `opus`.
func (c Catalog) model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	var listed []Model
	for _, m := range c.Models {
		if !m.Hidden {
			listed = append(listed, m)
		}
	}
	match := CatalogID(id, listed)
	for _, m := range listed {
		if match != "" && m.ID == match {
			return m, true
		}
	}
	return Model{}, false
}

// CatalogID joins a transcript's model id (`claude-opus-5-5[1m]`) to the
// catalog's alias (`opus[1m]`): an exact id, else the entry whose resolved
// name matches with the same context suffix, else without it. Empty when none.
func CatalogID(transcript string, models []Model) string {
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return ""
	}
	for _, m := range models {
		if m.ID == transcript {
			return transcript
		}
	}
	base, suffix := splitContext(transcript)
	var sameSuffix, anySuffix string
	for _, m := range models {
		if m.Resolved == "" {
			continue
		}
		if m.Resolved == transcript {
			return m.ID
		}
		resolvedBase, resolvedSuffix := splitContext(m.Resolved)
		idBase, idSuffix := splitContext(m.ID)
		if resolvedBase != base && idBase != base {
			continue
		}
		candidateSuffix := resolvedSuffix
		if candidateSuffix == "" {
			candidateSuffix = idSuffix
		}
		if candidateSuffix == suffix && sameSuffix == "" {
			sameSuffix = m.ID
		}
		if anySuffix == "" {
			anySuffix = m.ID
		}
	}
	if sameSuffix != "" {
		return sameSuffix
	}
	return anySuffix
}

// splitContext parts `claude-opus-5-5[1m]` into `claude-opus-5-5` and `[1m]`.
func splitContext(id string) (base, suffix string) {
	open := strings.IndexByte(id, '[')
	if open < 0 || !strings.HasSuffix(id, "]") {
		return id, ""
	}
	return id[:open], id[open:]
}

// WordName reads an id's words as a name: title-cased and joined with spaces.
// Shared by the agents' Namers.
func WordName(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(out, " ")
}
