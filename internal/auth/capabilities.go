package auth

// Capability is a string-typed permission key. The full set is the canonical
// codification of the §7 role-limits table; RoleAllows is the single source of
// truth for "role X can do Y". Endpoints gate on these via RequireRole.
//
// Adding a row to the §7 table = adding a constant here + a row to the
// roleCapabilities map. Removing or relaxing a row is a security-sensitive
// change — guard it with the table-driven tests in capabilities_test.go.
type Capability string

// §7 role-limits capabilities. Order mirrors the architecture table for
// reviewability.
const (
	// CapProjectRegister — "Rekisteröi projekti (wizard)".
	// Wire-up lands with the wizard endpoint (#11 / Vaihe 3 — `flowctl init`).
	CapProjectRegister Capability = "project.register"

	// CapRunsViewOwn — "Näkee omat ajot". Implicit baseline: any
	// authenticated user can list their own runs. Wired in /v1/runs.
	CapRunsViewOwn Capability = "runs.view.own"

	// CapRunsViewTenant — "Näkee koko tenantin ajot". Wired in /v1/runs:
	// admin gets every row in the tenant, developer only their own.
	CapRunsViewTenant Capability = "runs.view.tenant"

	// CapRunnerRegisterSelf — "Rekisteröi oman koneen runneriksi
	// (henkilökohtainen pool)". Wire-up lands with the runner-register
	// auth refactor (#6 / Vaihe 2 — per-user runner pool).
	CapRunnerRegisterSelf Capability = "runner.register.self"

	// CapRunnersManageShared — "Hallitsee jaettuja runnereita". Admin-only.
	// Wired in /v1/runners (list) now; future delete/drain endpoints reuse
	// the same capability.
	CapRunnersManageShared Capability = "runners.manage.shared"

	// CapSecretsManage — "Asettaa/muokkaa secretsejä". Admin-only.
	// Wire-up lands with the secrets-broker CRUD endpoint (#10 / Vaihe 3).
	CapSecretsManage Capability = "secrets.manage"

	// CapMergePolicyManage — "Muokkaa merge-policya". Admin-only.
	// Wire-up lands with the project-edit endpoint (#11 / Vaihe 3 wizard).
	CapMergePolicyManage Capability = "merge_policy.manage"

	// CapGitHubAppManage — "Asettaa GitHub App -installaation". Admin-only.
	// Wire-up lands with the App-install management endpoints (#9 / Vaihe 2 —
	// admin CLI on top of github_app_install).
	CapGitHubAppManage Capability = "github_app.manage"
)

// roleCapabilities is the canonical §7 role-limits table.
//
// "Lukee toisen tenantin dataa" is intentionally absent: that row is enforced
// by the tenant-isolation middleware ([[WithTenant]]) at the request boundary,
// not by a capability — it is absolute, even for admins (no cross-tenant
// super-admin; §7 last row + architecture invariant #4).
//
// Capability keys not listed for a role default to "deny" via [[RoleAllows]].
var roleCapabilities = map[Role]map[Capability]bool{
	RoleAdmin: {
		CapProjectRegister:     true,
		CapRunsViewOwn:         true,
		CapRunsViewTenant:      true,
		CapRunnerRegisterSelf:  true,
		CapRunnersManageShared: true,
		CapSecretsManage:       true,
		CapMergePolicyManage:   true,
		CapGitHubAppManage:     true,
	},
	RoleDeveloper: {
		CapProjectRegister:    true,
		CapRunsViewOwn:        true,
		CapRunnerRegisterSelf: true,
	},
}

// RoleAllows reports whether role is permitted to exercise cap. Unknown roles
// and unknown capabilities both return false (fail-closed). The cross-tenant
// row of the §7 table is NOT covered here; see the [[roleCapabilities]] doc.
func RoleAllows(role Role, cap Capability) bool {
	caps, ok := roleCapabilities[role]
	if !ok {
		return false
	}
	return caps[cap]
}

// allCapabilities lists every defined capability in the §7 row order. The
// dashboard's /v1/me endpoint reflects this list back to the browser so the UI
// can render role-aware controls without hard-coding role→capability mappings
// client-side; adding a row to the §7 table = appending to this list and to
// roleCapabilities.
var allCapabilities = []Capability{
	CapProjectRegister,
	CapRunsViewOwn,
	CapRunsViewTenant,
	CapRunnerRegisterSelf,
	CapRunnersManageShared,
	CapSecretsManage,
	CapMergePolicyManage,
	CapGitHubAppManage,
}

// CapabilitiesFor returns every capability granted to role, in the §7 table
// order. Used by /v1/me to project the role-allows table onto the wire so the
// dashboard can branch on capabilities, not on role strings. Unknown roles
// return nil.
func CapabilitiesFor(role Role) []Capability {
	if _, ok := roleCapabilities[role]; !ok {
		return nil
	}
	out := make([]Capability, 0, len(allCapabilities))
	for _, c := range allCapabilities {
		if RoleAllows(role, c) {
			out = append(out, c)
		}
	}
	return out
}
