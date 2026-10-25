package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

const pollInterval = 5 * time.Second

// watch runs w until the test ends, collecting what it applies and rejects.
type watch struct {
	applied  chan *Config
	rejected chan error
}

func startWatch(t *testing.T, w *Watcher, trigger <-chan os.Signal) *watch {
	t.Helper()
	ws := &watch{applied: make(chan *Config, 10), rejected: make(chan error, 10)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, pollInterval, trigger,
			func(c *Config) { ws.applied <- c },
			func(err error) { ws.rejected <- err })
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ws
}

// settle lets d of fake time pass and waits for the watcher to go idle.
func settle(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func writeChecks(t *testing.T, path string, names ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("checks:\n")
	for _, n := range names {
		b.WriteString("  - name: " + n + "\n    tcp: {address: \"" + n + ".example:443\"}\n")
	}
	writeFile(t, path, b.String())
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newWatcher(t *testing.T, path string) *Watcher {
	t.Helper()
	w, _, err := NewWatcher(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func checkNames(c *Config) []string {
	var names []string
	for _, ch := range c.Checks {
		names = append(names, ch.Name)
	}
	return names
}

func TestNewWatcherLoadsInitialConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checks.yaml")
	writeChecks(t, path, "a", "b")

	_, cfg, err := NewWatcher(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(checkNames(cfg), ","); got != "a,b" {
		t.Fatalf("checks = %s, want a,b", got)
	}
}

func TestNewWatcherRejectsMissingOrInvalidFile(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := NewWatcher(filepath.Join(dir, "missing.yaml"), noEnv); err == nil {
		t.Error("NewWatcher succeeded on a missing file")
	}
	invalid := filepath.Join(dir, "invalid.yaml")
	writeFile(t, invalid, "checks:\n  - name: a\n")
	if _, _, err := NewWatcher(invalid, noEnv); err == nil {
		t.Error("NewWatcher succeeded on an invalid config")
	}
}

func TestWatcherAppliesChangedContents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checks.yaml")
		writeChecks(t, path, "a")
		ws := startWatch(t, newWatcher(t, path), nil)

		settle(4 * pollInterval)
		if len(ws.applied) != 0 {
			t.Fatalf("applied %d configs while the file was unchanged", len(ws.applied))
		}

		writeChecks(t, path, "a", "b")
		settle(pollInterval)
		select {
		case cfg := <-ws.applied:
			if got := strings.Join(checkNames(cfg), ","); got != "a,b" {
				t.Fatalf("applied checks = %s, want a,b", got)
			}
		default:
			t.Fatal("changed config was not applied within one poll interval")
		}
	})
}

func TestWatcherRejectsInvalidConfigOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checks.yaml")
		writeChecks(t, path, "a")
		ws := startWatch(t, newWatcher(t, path), nil)

		writeFile(t, path, "checks:\n  - name: a\n    timout: 5s\n")
		settle(5 * pollInterval)
		if len(ws.rejected) != 1 || len(ws.applied) != 0 {
			t.Fatalf("after an invalid edit: %d rejections, %d applied; want 1 and 0", len(ws.rejected), len(ws.applied))
		}
		if err := <-ws.rejected; !strings.Contains(err.Error(), "timout") {
			t.Fatalf("rejection %q does not explain the problem", err)
		}

		writeChecks(t, path, "a", "fixed")
		settle(pollInterval)
		if len(ws.applied) != 1 {
			t.Fatalf("applied %d configs after the fix, want 1", len(ws.applied))
		}
	})
}

func TestWatcherReportsMissingFileOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checks.yaml")
		writeChecks(t, path, "a")
		ws := startWatch(t, newWatcher(t, path), nil)

		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		settle(5 * pollInterval)
		if len(ws.rejected) != 1 {
			t.Fatalf("missing file reported %d times, want once", len(ws.rejected))
		}

		// Restoring the same contents applies them again, which ends the
		// failure without changing what runs.
		writeChecks(t, path, "a")
		settle(5 * pollInterval)
		if len(ws.applied) != 1 || len(ws.rejected) != 1 {
			t.Fatalf("after restoring unchanged contents: %d applied, %d rejected; want 1 and 1",
				len(ws.applied), len(ws.rejected))
		}
		if got := strings.Join(checkNames(<-ws.applied), ","); got != "a" {
			t.Fatalf("applied checks = %s, want a", got)
		}
	})
}

func TestWatcherReloadsImmediatelyOnTrigger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "checks.yaml")
		writeChecks(t, path, "a")
		trigger := make(chan os.Signal, 1)
		ws := startWatch(t, newWatcher(t, path), trigger)

		writeChecks(t, path, "b")
		trigger <- syscall.SIGHUP
		synctest.Wait() // no time passes
		if len(ws.applied) != 1 {
			t.Fatalf("applied %d configs right after the trigger, want 1", len(ws.applied))
		}
	})
}

func TestVersionFollowsContents(t *testing.T) {
	parse := func(s string) string {
		t.Helper()
		cfg, err := Parse([]byte(s), noEnv)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Version
	}
	a := parse("checks: [{name: a, tcp: {address: 'a:1'}}]")
	if again := parse("checks: [{name: a, tcp: {address: 'a:1'}}]"); again != a {
		t.Errorf("same contents gave versions %q and %q", a, again)
	}
	if b := parse("checks: [{name: b, tcp: {address: 'a:1'}}]"); b == a {
		t.Errorf("different contents share version %q", a)
	}
	if len(a) != 12 {
		t.Errorf("version %q has %d characters, want 12", a, len(a))
	}
}
