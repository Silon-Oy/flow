package runnerexec

import (
	"strings"
	"testing"
)

// TestDockerArgsHardening asserts every §11.1 isolation flag is present and the
// forbidden ones (docker.sock, raw token env) are absent. This is the durable
// guard that the isolation posture cannot silently regress.
func TestDockerArgsHardening(t *testing.T) {
	spec := Spec{
		Image:              "flow-orchestrator:1",
		RunID:              "20260602-rid-1",
		WorktreePath:       "/srv/flow/work/run-1",
		RunnerName:         "studio",
		CentralURL:         "http://flowd:8080",
		CentralToken:       "runner-token-abc",
		ClaudeCredHostPath: "/srv/flow/creds/claude.json",
		MemoryLimit:        "2g",
		CPULimit:           "2",
		PidsLimit:          512,
	}
	args := spec.DockerArgs()
	joined := strings.Join(args, " ")

	mustContain := []string{
		"--user 65532:65532",
		"--read-only",
		"--tmpfs /tmp",
		"--cap-drop=ALL",
		"--security-opt no-new-privileges",
		"--network flow-egress-studio",
		"--memory 2g",
		"--cpus 2",
		"--pids-limit=512",
		"-v /srv/flow/work/run-1:/work",
		"HTTPS_PROXY=http://egress-proxy:3128",
		// Central-service callback envs (NOT GitHub credentials — §11.3 OK).
		"FLOW_CENTRAL_URL=http://flowd:8080",
		"FLOW_RUNNER_TOKEN=runner-token-abc",
		// Claude credential as a read-only FILE mount (Model B), not env.
		"-v /srv/flow/creds/claude.json:/run/claude-credentials.json:ro",
	}
	for _, want := range mustContain {
		if !strings.Contains(joined, want) {
			t.Errorf("hardened args missing %q\nfull: %s", want, joined)
		}
	}

	// Forbidden: the docker socket must NEVER be mounted (§11.1 invariant).
	if HasForbiddenMounts(args) {
		t.Errorf("docker.sock mounted into the untrusted container — §11.1 violation")
	}
	if strings.Contains(joined, "docker.sock") {
		t.Errorf("docker.sock referenced anywhere in args")
	}

	// The worktree is the ONLY host mount: exactly two -v (worktree + creds),
	// neither of which is the socket.
	vCount := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" {
			vCount++
		}
	}
	if vCount != 2 {
		t.Errorf("expected exactly 2 mounts (worktree + creds), got %d", vCount)
	}

	// The raw GitHub token must never appear as a container env (§11.3): the
	// only network env is the proxy. Assert no GITHUB_TOKEN / GH_TOKEN env flag.
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-e" {
			v := args[i+1]
			if strings.HasPrefix(v, "GITHUB_TOKEN=") || strings.HasPrefix(v, "GH_TOKEN=") {
				t.Errorf("raw token injected into container env: %q (§11.3 violation)", v)
			}
		}
	}
}

// TestDefaultsApplied: empty resource fields fall back to safe defaults.
func TestDefaultsApplied(t *testing.T) {
	spec := Spec{Image: "img", RunID: "r", WorktreePath: "/w"}
	joined := strings.Join(spec.DockerArgs(), " ")
	for _, want := range []string{"--memory 2g", "--cpus 2", "--pids-limit=512", "--network flow-egress "} {
		if !strings.Contains(joined+" ", want) {
			t.Errorf("default missing %q in %s", want, joined)
		}
	}
}
