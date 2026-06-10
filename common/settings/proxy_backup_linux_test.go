//go:build linux && !android

package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	M "github.com/sagernet/sing/common/metadata"
)

// TestSystemProxyBackupRestore mutates the REAL desktop proxy settings of the
// machine running it. It restores them afterwards, but is still gated behind
// an env var so it never runs in CI or by accident.
func TestSystemProxyBackupRestore(t *testing.T) {
	if os.Getenv("TUNLOK_PROXY_E2E") != "1" {
		t.Skip("set TUNLOK_PROXY_E2E=1 to run (mutates real desktop proxy settings)")
	}

	SystemProxyBackupPath = filepath.Join(t.TempDir(), "system-proxy-backup.json")
	ctx := context.Background()
	ours := M.ParseSocksaddrHostPort("127.0.0.1", 2080)

	proxy, err := NewSystemProxy(ctx, ours, false)
	if err != nil {
		t.Fatalf("NewSystemProxy: %v", err)
	}

	// Save the machine's real settings and restore them when the test ends,
	// whatever happens in between. Applied unconditionally — restoreBackup's
	// is-ours guard would (correctly) refuse to touch a non-ours state, but
	// the teardown must always win.
	machineState := proxy.captureBackup()
	defer func() {
		for _, schemaKey := range gnomeProxyKeys {
			value, loaded := machineState.Gnome[schemaKey]
			if !loaded {
				continue
			}
			parts := strings.SplitN(schemaKey, " ", 2)
			if err := proxy.runAsUser("gsettings", "set", parts[0], parts[1], value); err != nil {
				t.Errorf("teardown gsettings set %s: %v", schemaKey, err)
			}
		}
		if machineState.KDEValid {
			for _, key := range kdeProxyKeys {
				value, loaded := machineState.KDE[key]
				if !loaded {
					continue
				}
				var err error
				if value == "" {
					err = proxy.runAsUser(proxy.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", key, "--delete")
				} else {
					err = proxy.runAsUser(proxy.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", key, value)
				}
				if err != nil {
					t.Errorf("teardown kwriteconfig %s: %v", key, err)
				}
			}
		}
	}()

	gnomeGet := func(schema, key string) string {
		out, err := proxy.readAsUser("gsettings", "get", schema, key)
		if err != nil {
			t.Fatalf("gsettings get %s %s: %v", schema, key, err)
		}
		return strings.Trim(strings.TrimSpace(out), "'")
	}
	mustRun := func(args ...string) {
		if err := proxy.runAsUser(args[0], args[1:]...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	// 1. Simulate a user-configured manual proxy.
	mustRun("gsettings", "set", "org.gnome.system.proxy.http", "host", "userproxy.example")
	mustRun("gsettings", "set", "org.gnome.system.proxy.http", "port", "3128")
	mustRun("gsettings", "set", "org.gnome.system.proxy", "mode", "manual")

	// 2. Enable must snapshot the user's settings and switch the proxy to ours.
	if err := proxy.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if _, err := os.Stat(SystemProxyBackupPath); err != nil {
		t.Fatalf("backup file not written: %v", err)
	}
	if got := gnomeGet("org.gnome.system.proxy.http", "host"); got != "127.0.0.1" {
		t.Fatalf("after Enable host = %q, want 127.0.0.1", got)
	}
	if got := gnomeGet("org.gnome.system.proxy", "mode"); got != "manual" {
		t.Fatalf("after Enable mode = %q, want manual", got)
	}

	// 3. Disable must restore the user's settings, not blindly switch off.
	if err := proxy.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if got := gnomeGet("org.gnome.system.proxy", "mode"); got != "manual" {
		t.Fatalf("after Disable mode = %q, want manual (user's setting)", got)
	}
	if got := gnomeGet("org.gnome.system.proxy.http", "host"); got != "userproxy.example" {
		t.Fatalf("after Disable host = %q, want userproxy.example", got)
	}
	if got := gnomeGet("org.gnome.system.proxy.http", "port"); got != "3128" {
		t.Fatalf("after Disable port = %q, want 3128", got)
	}
	if _, err := os.Stat(SystemProxyBackupPath); !os.IsNotExist(err) {
		t.Fatalf("backup file should be removed after Disable")
	}

	// 4. Crash recovery: Enable, "crash" (no Disable), then CleanupStaleSystemProxy
	// on next start must restore the user's settings.
	if err := proxy.Enable(); err != nil {
		t.Fatalf("Enable (crash sim): %v", err)
	}
	if err := CleanupStaleSystemProxy(ctx); err != nil {
		t.Fatalf("CleanupStaleSystemProxy: %v", err)
	}
	if got := gnomeGet("org.gnome.system.proxy.http", "host"); got != "userproxy.example" {
		t.Fatalf("after stale cleanup host = %q, want userproxy.example", got)
	}
	if got := gnomeGet("org.gnome.system.proxy", "mode"); got != "manual" {
		t.Fatalf("after stale cleanup mode = %q, want manual", got)
	}
	if _, err := os.Stat(SystemProxyBackupPath); !os.IsNotExist(err) {
		t.Fatalf("backup file should be removed after stale cleanup")
	}

	// 5. If the user changed the proxy while we were running, Disable must
	// leave their settings alone.
	if err := proxy.Enable(); err != nil {
		t.Fatalf("Enable (user-change sim): %v", err)
	}
	mustRun("gsettings", "set", "org.gnome.system.proxy.http", "host", "other.example")
	if err := proxy.Disable(); err != nil {
		t.Fatalf("Disable (user-change sim): %v", err)
	}
	if got := gnomeGet("org.gnome.system.proxy.http", "host"); got != "other.example" {
		t.Fatalf("after Disable with user change host = %q, want other.example (untouched)", got)
	}
	if _, err := os.Stat(SystemProxyBackupPath); !os.IsNotExist(err) {
		t.Fatalf("backup file should be removed after not-ours Disable")
	}

	// Cleanup for step 5's leftovers: reset to user's simulated state so the
	// deferred machine restore starts from a known point.
	mustRun("gsettings", "set", "org.gnome.system.proxy", "mode", "none")
}
