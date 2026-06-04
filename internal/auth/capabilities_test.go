package auth

import (
	"testing"
)

// TestRoleLimitsTable is the §7 invariant test: every row of the architecture
// "Roolirajat" table is asserted here. Adding a row in the doc without adding
// it here (or vice versa) is the failure mode this guards against.
//
// "Lukee toisen tenantin dataa" is intentionally absent — that row is enforced
// by the tenant-isolation middleware (WithTenant), not by a capability flag.
func TestRoleLimitsTable(t *testing.T) {
	type row struct {
		name             string
		cap              Capability
		devAllowed       bool
		adminAllowed     bool
	}
	rows := []row{
		{"Rekisteröi projekti (wizard)", CapProjectRegister, true, true},
		{"Näkee omat ajot", CapRunsViewOwn, true, true},
		{"Näkee koko tenantin ajot", CapRunsViewTenant, false, true},
		{"Rekisteröi oman koneen runneriksi (henkilökohtainen pool)", CapRunnerRegisterSelf, true, true},
		{"Hallitsee jaettuja runnereita", CapRunnersManageShared, false, true},
		{"Asettaa/muokkaa secretsejä", CapSecretsManage, false, true},
		{"Muokkaa merge-policya", CapMergePolicyManage, false, true},
		{"Asettaa GitHub App -installaation", CapGitHubAppManage, false, true},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if got := RoleAllows(RoleDeveloper, r.cap); got != r.devAllowed {
				t.Errorf("RoleAllows(developer, %s) = %v, want %v", r.cap, got, r.devAllowed)
			}
			if got := RoleAllows(RoleAdmin, r.cap); got != r.adminAllowed {
				t.Errorf("RoleAllows(admin, %s) = %v, want %v", r.cap, got, r.adminAllowed)
			}
		})
	}
}

// TestRoleAllows_UnknownRole proves the fail-closed contract: a role string
// that is not in the canonical enum (typo, future role added in DB before the
// code knows about it, attacker-supplied principal) is denied for every
// capability.
func TestRoleAllows_UnknownRole(t *testing.T) {
	caps := []Capability{
		CapProjectRegister, CapRunsViewOwn, CapRunsViewTenant,
		CapRunnerRegisterSelf, CapRunnersManageShared, CapSecretsManage,
		CapMergePolicyManage, CapGitHubAppManage,
	}
	for _, c := range caps {
		if RoleAllows(Role("ghost"), c) {
			t.Errorf("unknown role allowed %s", c)
		}
		if RoleAllows(Role(""), c) {
			t.Errorf("empty role allowed %s", c)
		}
	}
}

// TestRoleAllows_UnknownCapability proves the same fail-closed contract for
// capabilities the registry has never heard of.
func TestRoleAllows_UnknownCapability(t *testing.T) {
	if RoleAllows(RoleAdmin, Capability("nonexistent.thing")) {
		t.Errorf("admin allowed unknown capability — should be fail-closed")
	}
	if RoleAllows(RoleDeveloper, Capability("nonexistent.thing")) {
		t.Errorf("developer allowed unknown capability — should be fail-closed")
	}
}
