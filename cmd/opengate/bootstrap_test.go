package main

import (
	"strings"
	"testing"
)

// mapGetenv adapts a map into a getenv func; absent keys yield "" like os.Getenv.
func mapGetenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// fullInputs returns a complete, valid set of bootstrap env vars.
func fullInputs() map[string]string {
	return map[string]string{
		envBootstrapTenantName:    "Acme Climbing",
		envBootstrapOwnerEmail:    "owner@acme.test",
		envBootstrapOwnerPassword: "s3cret-passw0rd",
	}
}

// TestReadBootstrapInputsAllPresent covers AC2: with all three vars set, the
// inputs are returned with no error.
func TestReadBootstrapInputsAllPresent(t *testing.T) {
	name, email, password, err := readBootstrapInputs(mapGetenv(fullInputs()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "Acme Climbing" || email != "owner@acme.test" || password != "s3cret-passw0rd" {
		t.Errorf("got (%q, %q, %q); want (Acme Climbing, owner@acme.test, s3cret-passw0rd)", name, email, password)
	}
}

// TestReadBootstrapInputsMissing covers AC2: with any one var absent, the error
// names the missing variable.
func TestReadBootstrapInputsMissing(t *testing.T) {
	for _, missing := range []string{envBootstrapTenantName, envBootstrapOwnerEmail, envBootstrapOwnerPassword} {
		t.Run(missing, func(t *testing.T) {
			env := fullInputs()
			delete(env, missing)

			_, _, _, err := readBootstrapInputs(mapGetenv(env))
			if err == nil {
				t.Fatalf("expected error when %s is missing", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error %q does not name the missing variable %q", err, missing)
			}
		})
	}
}

// TestResolveTenantSlugExplicitValid proves an explicit, well-formed slug is
// accepted and returned verbatim regardless of the tenant name.
func TestResolveTenantSlugExplicitValid(t *testing.T) {
	got, err := resolveTenantSlug("acme-gym", "Totally Different Name")
	if err != nil {
		t.Fatalf("resolveTenantSlug: %v", err)
	}
	if got != "acme-gym" {
		t.Errorf("slug = %q, want acme-gym", got)
	}
}

// TestResolveTenantSlugExplicitInvalid proves a malformed explicit slug fails
// fast, and the error names the variable the operator must fix.
func TestResolveTenantSlugExplicitInvalid(t *testing.T) {
	_, err := resolveTenantSlug("Bad Slug", "Acme Gym")
	if err == nil {
		t.Fatal("expected error for invalid explicit slug")
	}
	if !strings.Contains(err.Error(), envBootstrapTenantSlug) {
		t.Errorf("error %q does not name %s", err, envBootstrapTenantSlug)
	}
}

// TestResolveTenantSlugDerivesFromName proves that when no explicit slug is set,
// the slug is derived from the name with the migration's normalization rule.
func TestResolveTenantSlugDerivesFromName(t *testing.T) {
	cases := map[string]string{
		"Acme Gym":       "acme-gym",
		"  Acme  Gym!! ": "acme-gym",
		"CrossFit 101":   "crossfit-101",
	}
	for name, want := range cases {
		got, err := resolveTenantSlug("", name)
		if err != nil {
			t.Fatalf("resolveTenantSlug(%q): %v", name, err)
		}
		if got != want {
			t.Errorf("resolveTenantSlug(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestResolveTenantSlugUnderivable proves a name with no alphanumeric characters
// fails fast and instructs the operator to set the slug explicitly.
func TestResolveTenantSlugUnderivable(t *testing.T) {
	_, err := resolveTenantSlug("", "!@#$%")
	if err == nil {
		t.Fatal("expected error for underivable name")
	}
	if !strings.Contains(err.Error(), envBootstrapTenantSlug) {
		t.Errorf("error %q does not tell the operator to set %s", err, envBootstrapTenantSlug)
	}
}
