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
