package workflow

import "regexp"

var templateRefPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_-]+)\s*\}\}`)

// TemplateRefs returns the deduplicated node ids referenced by a template.
func TemplateRefs(tpl string) []string {
	matches := templateRefPattern.FindAllStringSubmatch(tpl, -1)
	refs := make([]string, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		refs = append(refs, m[1])
	}
	return refs
}

// RenderTemplate replaces {{node_id}} references with the given values;
// unknown references render as an empty string.
func RenderTemplate(tpl string, values map[string]string) string {
	return templateRefPattern.ReplaceAllStringFunc(tpl, func(m string) string {
		sub := templateRefPattern.FindStringSubmatch(m)
		return values[sub[1]]
	})
}
