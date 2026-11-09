package models

import "github.com/router-for-me/CLIProxyAPI/v8/internal/codexaccountmodels"

// devyreWithAccountTemplates adds, for each model OpenAI lists for a Codex
// account (internal/codexaccountmodels) that the client catalog has no template
// for, OpenAI's own entry as its template: Codex then gets the metadata OpenAI
// gives it for the model instead of a generic stand-in. The catalog's templates
// win; the cached map is never written, a copy is returned when one is added.
func devyreWithAccountTemplates(templates map[string]map[string]any) map[string]map[string]any {
	learned := codexaccountmodels.Default().Templates()
	if len(learned) == 0 || templates == nil {
		return templates
	}
	var out map[string]map[string]any
	for slug, entry := range learned {
		if _, ok := templates[slug]; ok {
			continue
		}
		if out == nil {
			out = make(map[string]map[string]any, len(templates)+len(learned))
			for key, value := range templates {
				out[key] = value
			}
		}
		out[slug] = cloneCodexClientModelMap(entry)
	}
	if out == nil {
		return templates
	}
	return out
}
