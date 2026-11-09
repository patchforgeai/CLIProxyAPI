package registry

import "fmt"

// ValidateCodexClientModel checks one Codex client catalog entry the way
// ValidateCodexClientModelsJSON checks each entry of a catalog (devyre): a slug
// and every field the catalog builder requires of a template. The catalog-wide
// rules (no duplicate slug, the default template present) are the catalog's.
func ValidateCodexClientModel(model map[string]any) error {
	slug, err := requiredCodexClientModelString(model, "slug")
	if err != nil {
		return err
	}
	if err = validateCodexClientModel(model); err != nil {
		return fmt.Errorf("Codex client model %q: %w", slug, err)
	}
	return nil
}
