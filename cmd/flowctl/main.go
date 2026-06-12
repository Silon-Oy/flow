// Command flowctl is the Flow developer/admin CLI; it speaks only to the
// central service (flowd). Vaihe 1 ships `status`; Vaihe 2 adds `login`
// (§7(a) GitHub OAuth device flow); init/runner/secret land later.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Silon-Oy/flow/internal/centralclient"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "status":
		if err := runStatus(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "flowctl status:", err)
			os.Exit(1)
		}
	case "login":
		if err := runLogin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "flowctl login:", err)
			os.Exit(1)
		}
	case "init":
		if err := runInit(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "flowctl init:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "flowctl: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `flowctl — Flow CLI

Usage:
  flowctl login                       sign in via GitHub OAuth device flow
  flowctl init [--config flow.yaml]   register a project (§8 wizard)
  flowctl status [--status <run-status>]

Env:
  FLOW_CENTRAL_URL   central service base URL (default http://localhost:8080)
  FLOW_TOKEN         session/runner token (overrides the credentials file
                     written by flowctl login)`)
}

func runStatus(args []string) error {
	statusFilter := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--status" && i+1 < len(args) {
			statusFilter = args[i+1]
			i++
		}
	}
	central := envOr("FLOW_CENTRAL_URL", "http://localhost:8080")
	token, err := resolveToken()
	if err != nil {
		return err
	}
	cli := centralclient.New(central, token)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runners, err := cli.ListRunners(ctx)
	if err != nil {
		return fmt.Errorf("list runners: %w", err)
	}
	runs, err := cli.ListRuns(ctx, statusFilter)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}

	fmt.Println("RUNNERS")
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  HOST\tSTATUS\tCAP\tACTIVE\tLAST HEARTBEAT")
	for _, r := range runners {
		hb := "-"
		if r.LastHeartbeat != nil {
			hb = r.LastHeartbeat.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%s\n", r.Hostname, r.Status, r.Capacity, r.ActiveLeases, hb)
	}
	tw.Flush()

	fmt.Println("\nRUNS")
	tw = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  ISSUE\tREMOTE\tSTATUS\tSTATE\tBRANCH\tPR")
	for _, r := range runs {
		fmt.Fprintf(tw, "  #%d\t%s\t%s\t%s\t%s\t%s\n",
			r.IssueNumber, r.Remote, r.Status, deref(r.CurrentState), deref(r.Branch), deref(r.PRURL))
	}
	tw.Flush()
	return nil
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// resolveToken returns the session token for central API calls: FLOW_TOKEN
// wins, otherwise the credentials file written by `flowctl login`.
func resolveToken() (string, error) {
	if t := os.Getenv("FLOW_TOKEN"); t != "" {
		return t, nil
	}
	if t, err := readCredentialsToken(); err == nil && t != "" {
		return t, nil
	}
	return "", errors.New("not signed in: run `flowctl login` or export FLOW_TOKEN")
}
