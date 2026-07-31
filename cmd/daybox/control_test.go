package main

import (
	"errors"
	"testing"
	"time"
)

// fakeExit stands in for *exec.ExitError: sshTransient keys off the exit
// code, not the concrete type, so a value with ExitCode() lets the retry
// loop be exercised without spawning real ssh processes.
type fakeExit struct{ code int }

func (f fakeExit) Error() string { return "fake exit" }
func (f fakeExit) ExitCode() int { return f.code }

func TestSSHTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"255 (transport)", fakeExit{255}, true},
		{"1 (command failure)", fakeExit{1}, false},
		{"0 (success reported as error)", fakeExit{0}, false},
		{"generic non-exit error", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := sshTransient(c.err); got != c.want {
			t.Errorf("%s: sshTransient = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSSHRetry(t *testing.T) {
	// bypass real sleeps so the suite is instant
	orig := sshRetrySleep
	sshRetrySleep = func(time.Duration) {}
	defer func() { sshRetrySleep = orig }()

	t.Run("retries a transient failure then succeeds", func(t *testing.T) {
		calls := 0
		err := sshRetry("test", func() error {
			calls++
			if calls < 3 {
				return fakeExit{255}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("expected nil after recovery, got %v", err)
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3 (2 retries + success)", calls)
		}
	})

	t.Run("gives up after max attempts", func(t *testing.T) {
		calls := 0
		err := sshRetry("test", func() error {
			calls++
			return fakeExit{255}
		})
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if calls != sshMaxAttempts {
			t.Fatalf("calls = %d, want %d", calls, sshMaxAttempts)
		}
		if !sshTransient(err) {
			t.Errorf("expected a transient (255) error after giving up, got %v", err)
		}
	})

	t.Run("does not retry a non-transient failure", func(t *testing.T) {
		calls := 0
		err := sshRetry("test", func() error {
			calls++
			return fakeExit{1} // the remote command itself failed
		})
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 — a command failure must not be retried", calls)
		}
		fe, ok := err.(fakeExit)
		if !ok || fe.code != 1 {
			t.Errorf("expected fakeExit{1} to pass through, got %v", err)
		}
	})

	t.Run("returns nil immediately on success", func(t *testing.T) {
		calls := 0
		err := sshRetry("test", func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1", calls)
		}
	})
}
