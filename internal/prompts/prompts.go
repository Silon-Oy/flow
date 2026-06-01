// Package prompts embeds the run-issues agent prompt templates (copied verbatim
// from the dotfiles run-issues orchestrator) so the runner binary is fully
// self-contained — no external prompt files to ship or path to resolve.
//
// The canonical, embeddable copies live in internal/prompts/files/. The
// repo-root prompts/ directory holds an identical copy for human reference and
// to satisfy the §14 repo layout; internal/prompts/files/ is what the binary
// actually ships, because //go:embed cannot reach a parent directory.
package prompts

import (
	_ "embed"
)

// The four orchestrator prompt templates. Placeholders ({{KEY}}) are
// substituted at render time by internal/orchestrator.
var (
	//go:embed files/01-cycle-review.md
	CycleReview string

	//go:embed files/02-implementer.md
	Implementer string

	//go:embed files/03-evolution.md
	Evolution string

	//go:embed files/04-conflict-resolution.md
	ConflictResolution string
)

// ByStep maps an orchestrator step name to its prompt template.
var ByStep = map[string]string{
	"cycle-review":        CycleReview,
	"implementer":         Implementer,
	"evolution":           Evolution,
	"conflict-resolution": ConflictResolution,
}
