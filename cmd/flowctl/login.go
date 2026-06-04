// `flowctl login` — §7(a) human → central authentication via GitHub OAuth
// device flow. Designed for headless boxes (Studio over SSH): the only thing
// the user does locally is type a short code into a browser on a machine that
// already has a GitHub session. The session token is written to
// ~/.config/flow/credentials with mode 0600.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Silon-Oy/flow/internal/centralclient"
)

// credentialsRelPath / credentialsDirPerm / credentialsFilePerm — XDG-style
// $HOME-local credentials store. 0600 on the file and 0700 on the directory
// keeps it private; the acceptance check is explicit in the README.
const (
	credentialsRelPath   = ".config/flow/credentials"
	credentialsDirPerm   = 0o700
	credentialsFilePerm  = 0o600
	maxPollTotalDuration = 15 * time.Minute // hard ceiling regardless of GitHub's expires_in
)

func runLogin(args []string) error {
	// No flags yet; reserved so future --no-browser / --json don't break callers.
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(`flowctl login — sign in via GitHub OAuth device flow

The CLI prints a short code; open the URL it prints, paste the code, and the
central exchanges a flow session token. The token is written to
~/.config/flow/credentials (chmod 600); export FLOW_TOKEN=$(cat <path>) when
calling other flowctl subcommands.`)
		return nil
	}

	central := envOr("FLOW_CENTRAL_URL", "http://localhost:8080")
	cli := centralclient.New(central, "")

	// Bound the total time we'll wait; GitHub device codes expire after ~15
	// minutes and we want a clear failure mode if the user wanders off.
	ctx, cancel := context.WithTimeout(context.Background(), maxPollTotalDuration)
	defer cancel()

	start, err := cli.StartDeviceLogin(ctx)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}

	fmt.Println()
	fmt.Println("Sign in to Flow with GitHub:")
	fmt.Println()
	fmt.Printf("  1. Open this URL in a browser:   %s\n", start.VerificationURI)
	fmt.Printf("  2. Enter this code:              %s\n", start.UserCode)
	fmt.Println()
	fmt.Println("Waiting for authorization (Ctrl+C to cancel)...")

	interval := time.Duration(start.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for authorization: %w", ctx.Err())
		case <-time.After(interval):
		}

		poll, err := cli.PollDeviceLogin(ctx, start.DeviceCode)
		if err != nil {
			// Honour the 429 slow_down signal by doubling the interval and
			// retrying; other HTTP errors are terminal.
			if strings.Contains(err.Error(), ": 429:") {
				interval *= 2
				fmt.Fprintf(os.Stderr, "  (slow down — backing off to %s)\n", interval)
				continue
			}
			return fmt.Errorf("poll: %w", err)
		}
		if poll.Pending {
			continue
		}
		if poll.SessionToken == "" {
			return errors.New("poll: empty session_token in non-pending response")
		}
		path, err := writeCredentials(poll.SessionToken)
		if err != nil {
			return fmt.Errorf("write credentials: %w", err)
		}
		fmt.Println()
		fmt.Printf("Signed in as %s.\n", poll.GitHubLogin)
		fmt.Printf("Session token written to %s (chmod 600).\n", path)
		if !poll.ExpiresAt.IsZero() {
			fmt.Printf("Expires %s.\n", poll.ExpiresAt.Format(time.RFC3339))
		}
		return nil
	}
}

// writeCredentials persists the session token to $HOME/.config/flow/credentials
// (mode 0600). Honours XDG_CONFIG_HOME / FLOW_CREDENTIALS_PATH for test/CI
// overrides; the acceptance criterion (chmod 600) is the file-mode side, not
// the path itself.
func writeCredentials(token string) (string, error) {
	path, err := credentialsPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, credentialsDirPerm); err != nil {
		return "", err
	}
	// Write via O_CREATE|O_TRUNC|O_WRONLY with the file-creation mode set
	// directly so a pre-existing file is overwritten with the right mode in one
	// syscall (an existing 0644 file would survive a naive WriteFile).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, credentialsFilePerm)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(token + "\n"); err != nil {
		return "", err
	}
	// Re-chmod defensively: if the file pre-existed, OpenFile's perm arg is
	// ignored and the old mode (possibly 0644) sticks. The acceptance criterion
	// is an invariant of the file on disk.
	if err := os.Chmod(path, credentialsFilePerm); err != nil {
		return "", err
	}
	return path, nil
}

func credentialsPath() (string, error) {
	if v := os.Getenv("FLOW_CREDENTIALS_PATH"); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "flow", "credentials"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, credentialsRelPath), nil
}
