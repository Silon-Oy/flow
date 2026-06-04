package secrets

import (
	"errors"
	"testing"
)

func TestEnvResolver(t *testing.T) {
	r := EnvResolver{}

	t.Run("empty ref", func(t *testing.T) {
		_, err := r.Resolve("")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("unset env", func(t *testing.T) {
		t.Setenv("FLOW_TEST_SECRET_UNSET", "")
		// t.Setenv sets to empty — emulate fully-unset by Unsetenv via os.
		// We rely on the variable being empty: LookupEnv returns ok=true but
		// value=="", which the resolver treats as not-found.
		_, err := r.Resolve("FLOW_TEST_SECRET_UNSET")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty value: got %v, want ErrNotFound", err)
		}
	})

	t.Run("present", func(t *testing.T) {
		t.Setenv("FLOW_TEST_SECRET_PRESENT", "hunter2")
		got, err := r.Resolve("FLOW_TEST_SECRET_PRESENT")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "hunter2" {
			t.Fatalf("got %q, want %q", got, "hunter2")
		}
	})
}
