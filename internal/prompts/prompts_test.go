package prompts

import (
	"strings"
	"testing"
)

// TestEmbeddedPromptsNonEmpty guards the embed wiring: each template must be
// embedded and non-empty. A broken //go:embed path fails compilation; this
// catches an accidentally-emptied file.
func TestEmbeddedPromptsNonEmpty(t *testing.T) {
	for name, body := range ByStep {
		if strings.TrimSpace(body) == "" {
			t.Errorf("prompt %q is empty", name)
		}
	}
	if len(ByStep) != 4 {
		t.Errorf("expected 4 prompts, got %d", len(ByStep))
	}
}
