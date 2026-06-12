// `flowctl init` — §8 wizard. Two modes: interactive prompts for a human
// operator, or `--config flow.yaml` for CI. Both build the same wire shape
// the central validates (§8 acceptance criterion: validation in central, not
// just in CLI). The CLI's job is collection + a friendly UI; the truth check
// happens server-side.

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Silon-Oy/flow/internal/centralclient"
)

// runInit dispatches to either interactive mode or YAML mode. Flags kept
// minimal so the help screen reads cleanly: -config <path> for CI; bare for
// the human wizard.
func runInit(args []string) error {
	var configPath string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Print(initHelp)
			return nil
		case "--config":
			if i+1 >= len(args) {
				return errors.New("--config requires a path")
			}
			configPath = args[i+1]
			i++
		default:
			return fmt.Errorf("unknown argument: %s", args[i])
		}
	}

	central := envOr("FLOW_CENTRAL_URL", "http://localhost:8080")
	token, err := resolveToken()
	if err != nil {
		return err
	}
	cli := centralclient.New(central, token)

	var req centralclient.CreateProjectRequest
	if configPath != "" {
		req, err = loadProjectConfig(configPath)
		if err != nil {
			return fmt.Errorf("load %s: %w", configPath, err)
		}
	} else {
		req, err = runInitInteractive(os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	id, err := cli.CreateProject(ctx, req)
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	fmt.Printf("Project created: %s (%s)\n", req.Name, id)
	return nil
}

const initHelp = `flowctl init — register a project with the central service (§8 wizard)

Two modes:

  flowctl init                     interactive prompts
  flowctl init --config flow.yaml  read fields from a YAML file (CI)

Required fields:
  name                  unique project name within your tenant
  owner_repo            "owner/repo" (matches the GitHub url segment)

Optional fields (server-side defaults applied if omitted):
  remotes[]             [{remote, owner_repo, base_branch?}]
                        default: [{remote:"origin", owner_repo:<owner_repo>}]
  labels[]              default: ["auto-run"]
  base_branch           default: "main"
  runner_pool           uuid; default: unset
  claude_timeout_seconds positive int; default: unset
  merge_policy          JSON object
  secret_refs           {alias: secret_ref_key} — references to secrets,
                        NEVER plaintext values (architecture invariant)

Env:
  FLOW_CENTRAL_URL  central service base URL (default http://localhost:8080)
  FLOW_TOKEN        session token (default: read from ~/.config/flow/credentials)
`

// projectYAML is what --config flow.yaml deserialises to. Field names mirror
// the wire shape exactly so the YAML is a 1:1 description of POST /v1/projects.
type projectYAML struct {
	Name                 string             `yaml:"name"`
	OwnerRepo            string             `yaml:"owner_repo"`
	Remotes              []projectYAMLRemote `yaml:"remotes"`
	Labels               []string           `yaml:"labels"`
	BaseBranch           string             `yaml:"base_branch"`
	RunnerPool           string             `yaml:"runner_pool"`
	ClaudeTimeoutSeconds int                `yaml:"claude_timeout_seconds"`
	MergePolicy          map[string]any     `yaml:"merge_policy"`
	SecretRefs           map[string]string  `yaml:"secret_refs"`
}

type projectYAMLRemote struct {
	Remote     string `yaml:"remote"`
	OwnerRepo  string `yaml:"owner_repo"`
	BaseBranch string `yaml:"base_branch,omitempty"`
}

// loadProjectConfig reads + parses a YAML config and converts it to the wire
// shape. Validation (regex, branch existence) is the central's job.
func loadProjectConfig(path string) (centralclient.CreateProjectRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}
	var y projectYAML
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // unknown YAML keys are typos — fail fast
	if err := dec.Decode(&y); err != nil {
		return centralclient.CreateProjectRequest{}, fmt.Errorf("parse yaml: %w", err)
	}
	remotes := make([]centralclient.ProjectRemote, 0, len(y.Remotes))
	for _, r := range y.Remotes {
		remotes = append(remotes, centralclient.ProjectRemote{
			Remote:     r.Remote,
			OwnerRepo:  r.OwnerRepo,
			BaseBranch: r.BaseBranch,
		})
	}
	return centralclient.CreateProjectRequest{
		Name:                 y.Name,
		OwnerRepo:            y.OwnerRepo,
		Remotes:              remotes,
		Labels:               y.Labels,
		BaseBranch:           y.BaseBranch,
		RunnerPool:           y.RunnerPool,
		ClaudeTimeoutSeconds: y.ClaudeTimeoutSeconds,
		MergePolicy:          y.MergePolicy,
		SecretRefs:           y.SecretRefs,
	}, nil
}

// runInitInteractive prompts the operator for each field. Defaults pulled
// from the current working directory's `gh` remote when possible so the
// happy path is "press enter a lot, type a name". Sequential prompts are
// the simplest UX; a TUI is out of scope for #9.
func runInitInteractive(in io.Reader, out io.Writer) (centralclient.CreateProjectRequest, error) {
	br := bufio.NewReader(in)
	fmt.Fprintln(out, "flowctl init — registering a new project with the central service")
	fmt.Fprintln(out)

	suggestedOwnerRepo := detectOwnerRepoFromGit()
	suggestedRemote := "origin"
	if suggestedOwnerRepo == "" {
		suggestedRemote = ""
	}

	name, err := prompt(br, out, "Project name", "")
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}
	ownerRepo, err := prompt(br, out, "owner/repo", suggestedOwnerRepo)
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}
	baseBranch, err := prompt(br, out, "base_branch (project default)", "main")
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}
	labelsStr, err := prompt(br, out, "labels (comma-separated)", "auto-run")
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}
	timeoutStr, err := prompt(br, out, "claude_timeout_seconds (0 = server default)", "0")
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}
	timeoutSeconds, _ := strconv.Atoi(strings.TrimSpace(timeoutStr))

	remotes, err := promptRemotes(br, out, suggestedRemote, ownerRepo)
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Secrets: enter SECRET_REF keys (NEVER plaintext values).")
	fmt.Fprintln(out, "  Format: ALIAS=secret_ref_key  (blank line to finish)")
	secretRefs, err := promptSecretRefs(br, out)
	if err != nil {
		return centralclient.CreateProjectRequest{}, err
	}

	return centralclient.CreateProjectRequest{
		Name:                 strings.TrimSpace(name),
		OwnerRepo:            strings.TrimSpace(ownerRepo),
		Remotes:              remotes,
		Labels:               splitCSV(labelsStr),
		BaseBranch:           strings.TrimSpace(baseBranch),
		ClaudeTimeoutSeconds: timeoutSeconds,
		SecretRefs:           secretRefs,
	}, nil
}

// promptRemotes accepts the "origin -> owner/repo[, base_branch override]"
// shape one line at a time. Empty first answer keeps the implicit default
// (single origin pointing at owner_repo).
func promptRemotes(br *bufio.Reader, out io.Writer, defaultRemote, defaultOwnerRepo string) ([]centralclient.ProjectRemote, error) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Remotes (press Enter to accept the default single-remote setup):")
	fmt.Fprintln(out, "  Format: <remote>=<owner/repo>[@<base_branch>]")
	fmt.Fprintln(out, "  Example: upstream=Silon-Oy/flow@develop")
	var out_ []centralclient.ProjectRemote
	first := true
	for {
		def := ""
		if first && defaultRemote != "" && defaultOwnerRepo != "" {
			def = defaultRemote + "=" + defaultOwnerRepo
		}
		line, err := prompt(br, out, "remote", def)
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		rem, err := parseRemoteLine(line)
		if err != nil {
			fmt.Fprintf(out, "  invalid: %v\n", err)
			continue
		}
		out_ = append(out_, rem)
		first = false
	}
	return out_, nil
}

// parseRemoteLine accepts "<remote>=<owner/repo>[@<base_branch>]" and returns
// a wire ProjectRemote. Empty / malformed input is an error so the prompt
// loop can re-ask.
func parseRemoteLine(line string) (centralclient.ProjectRemote, error) {
	eq := strings.Index(line, "=")
	if eq <= 0 || eq == len(line)-1 {
		return centralclient.ProjectRemote{}, fmt.Errorf("expected <remote>=<owner/repo>[@<branch>]")
	}
	name := strings.TrimSpace(line[:eq])
	rest := strings.TrimSpace(line[eq+1:])
	branch := ""
	if at := strings.Index(rest, "@"); at >= 0 {
		branch = strings.TrimSpace(rest[at+1:])
		rest = strings.TrimSpace(rest[:at])
	}
	if !strings.Contains(rest, "/") {
		return centralclient.ProjectRemote{}, fmt.Errorf("owner/repo must contain '/'")
	}
	return centralclient.ProjectRemote{
		Remote:     name,
		OwnerRepo:  rest,
		BaseBranch: branch,
	}, nil
}

// promptSecretRefs accumulates secret references until a blank line. The CLI
// refuses obvious token-prefix shapes so the wizard hard-fails the
// architecture invariant ("references, not values") without waiting for the
// central to return an error.
func promptSecretRefs(br *bufio.Reader, out io.Writer) (map[string]string, error) {
	refs := map[string]string{}
	for {
		line, err := prompt(br, out, "secret_ref", "")
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return refs, nil
		}
		eq := strings.Index(line, "=")
		if eq <= 0 || eq == len(line)-1 {
			fmt.Fprintln(out, "  invalid: expected ALIAS=secret_ref_key")
			continue
		}
		alias := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if val == "" {
			fmt.Fprintln(out, "  invalid: secret_ref_key must not be empty")
			continue
		}
		if looksLikeSecretValue(val) {
			fmt.Fprintln(out, "  refused: that looks like a token value, not a SECRET_REF key.")
			fmt.Fprintln(out, "  architecture: §8 secrets are references, not values.")
			continue
		}
		refs[alias] = val
	}
}

// looksLikeSecretValue mirrors the central's hasSecretLikePrefix check so the
// CLI fails fast on the same shapes the server would reject. Kept in sync
// deliberately: see internal/api/project_handlers.go.
func looksLikeSecretValue(v string) bool {
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "github_pat_", "sk-", "xoxb-", "xoxp-"} {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// prompt asks one question. An empty answer means "use the default" if one
// was offered; otherwise the prompt re-asks (no silent empty submissions for
// required fields).
func prompt(br *bufio.Reader, out io.Writer, label, def string) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", label, def)
		} else {
			fmt.Fprintf(out, "%s: ", label)
		}
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" && def != "" {
			return def, nil
		}
		return line, nil
	}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// detectOwnerRepoFromGit asks the local git remote for origin's URL and
// extracts owner/repo. Best effort — failure returns "" and the prompt
// shows no default. Supports both git@github.com: and https://github.com/
// forms.
func detectOwnerRepoFromGit() string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	return parseOwnerRepoFromURL(url)
}

func parseOwnerRepoFromURL(url string) string {
	// Strip protocol / git@host: prefix.
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	if i := strings.Index(url, "@"); i >= 0 {
		url = url[i+1:]
	}
	// Now "host:owner/repo" or "host/owner/repo"; collapse the host delimiter.
	for _, sep := range []string{":", "/"} {
		if i := strings.Index(url, sep); i >= 0 {
			url = url[i+1:]
			break
		}
	}
	url = strings.TrimSuffix(url, ".git")
	if strings.Count(url, "/") != 1 {
		return ""
	}
	return url
}

// readCredentialsToken returns the bearer in ~/.config/flow/credentials (or
// XDG_CONFIG_HOME / FLOW_CREDENTIALS_PATH override). Mirrors login.go's
// writer side. Trims trailing newline.
func readCredentialsToken() (string, error) {
	path, err := credentialsPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
