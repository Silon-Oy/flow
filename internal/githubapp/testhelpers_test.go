package githubapp

import (
	"crypto/rand"
	"io"
)

// testRand returns the package crypto/rand reader. It's wrapped so tests can
// swap it for a deterministic source later if needed without touching call
// sites.
func testRand() io.Reader { return rand.Reader }
