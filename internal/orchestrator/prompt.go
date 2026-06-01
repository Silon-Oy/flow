package orchestrator

import "strings"

// RenderPrompt substitutes {{KEY}} placeholders in template in a SINGLE pass
// (REWRITE of lib/claude-call.sh:render_prompt). Values are never re-scanned, so
// a literal "{{OTHER}}" inside a substituted value is preserved — this prevents
// placeholder injection from untrusted issue content. An unknown placeholder is
// left verbatim.
func RenderPrompt(template string, values map[string]string) string {
	var b strings.Builder
	b.Grow(len(template))
	i := 0
	for i < len(template) {
		if i+1 < len(template) && template[i] == '{' && template[i+1] == '{' {
			rest := template[i+2:]
			if end := strings.Index(rest, "}}"); end >= 0 {
				key := rest[:end]
				if val, ok := values[key]; ok {
					b.WriteString(val)
					i += 2 + end + 2
					continue
				}
			}
		}
		b.WriteByte(template[i])
		i++
	}
	return b.String()
}
