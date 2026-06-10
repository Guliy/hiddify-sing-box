//go:build linux && !android

package settings

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
	M "github.com/sagernet/sing/common/metadata"
)

// SystemProxyBackupPath is where the user's original system proxy settings
// are persisted while the proxy is enabled, so they can be restored on
// Disable or after a crash. A relative path resolves against the process
// working directory (libcore chdirs to its working dir on Setup); callers
// may override it with an absolute path before Enable is called.
var SystemProxyBackupPath = filepath.Join("data", "system-proxy-backup.json")

// gnomeProxyKeys lists every "schema key" pair Enable() may modify, in
// restore order: mode goes last so partially restored values never become
// active while mode is still "manual".
var gnomeProxyKeys = []string{
	"org.gnome.system.proxy.http enabled",
	"org.gnome.system.proxy.ftp host",
	"org.gnome.system.proxy.ftp port",
	"org.gnome.system.proxy.http host",
	"org.gnome.system.proxy.http port",
	"org.gnome.system.proxy.https host",
	"org.gnome.system.proxy.https port",
	"org.gnome.system.proxy.socks host",
	"org.gnome.system.proxy.socks port",
	"org.gnome.system.proxy use-same-proxy",
	"org.gnome.system.proxy mode",
}

var kdeProxyKeys = []string{
	"Authmode",
	"ftpProxy",
	"httpProxy",
	"httpsProxy",
	"socksProxy",
	"ProxyType",
}

type proxyBackup struct {
	OursHost string            `json:"ours_host"`
	OursPort uint16            `json:"ours_port"`
	Gnome    map[string]string `json:"gnome,omitempty"`
	KDE      map[string]string `json:"kde,omitempty"`
	KDEValid bool              `json:"kde_valid"`
}

func loadProxyBackup() (*proxyBackup, error) {
	data, err := os.ReadFile(SystemProxyBackupPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var backup proxyBackup
	if err := json.Unmarshal(data, &backup); err != nil {
		_ = removeProxyBackup()
		return nil, E.Cause(err, "corrupt system proxy backup removed")
	}
	return &backup, nil
}

func (b *proxyBackup) save() error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_ = os.MkdirAll(filepath.Dir(SystemProxyBackupPath), 0o755)
	return os.WriteFile(SystemProxyBackupPath, data, 0o644)
}

func removeProxyBackup() error {
	err := os.Remove(SystemProxyBackupPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// captureBackup reads the current system proxy settings (the user's state
// before we touch anything). Read failures are tolerated per key/desktop:
// restore then simply falls back to plain disable for that desktop.
func (p *LinuxSystemProxy) captureBackup() *proxyBackup {
	backup := &proxyBackup{
		OursHost: p.serverAddr.AddrString(),
		OursPort: p.serverAddr.Port,
	}
	if p.hasGSettings {
		backup.Gnome = make(map[string]string)
		for _, schemaKey := range gnomeProxyKeys {
			parts := strings.SplitN(schemaKey, " ", 2)
			out, err := p.readAsUser("gsettings", "get", parts[0], parts[1])
			if err != nil {
				continue
			}
			backup.Gnome[schemaKey] = strings.TrimSpace(out)
		}
	}
	if p.kReadConfigCmd != "" {
		kde := make(map[string]string)
		valid := true
		for _, key := range kdeProxyKeys {
			out, err := p.readAsUser(p.kReadConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", key)
			if err != nil {
				valid = false
				break
			}
			kde[key] = strings.TrimSpace(out)
		}
		if valid {
			backup.KDE = kde
			backup.KDEValid = true
		}
	}
	return backup
}

// currentIsOurs reports whether the system proxy still points at our
// listener. If the current state cannot be read, it returns true: a backup
// file exists only because we enabled the proxy, and stranding the user on a
// dead proxy is worse than re-writing settings they may have changed.
func (p *LinuxSystemProxy) currentIsOurs(ours M.Socksaddr) bool {
	if p.hasGSettings {
		mode, err := p.readAsUser("gsettings", "get", "org.gnome.system.proxy", "mode")
		if err != nil {
			return true
		}
		if strings.Trim(strings.TrimSpace(mode), "'") != "manual" {
			return false
		}
		host, hostErr := p.readAsUser("gsettings", "get", "org.gnome.system.proxy.http", "host")
		port, portErr := p.readAsUser("gsettings", "get", "org.gnome.system.proxy.http", "port")
		if hostErr != nil || portErr != nil {
			return true
		}
		return strings.Trim(strings.TrimSpace(host), "'") == ours.AddrString() &&
			strings.TrimSpace(port) == F.ToString(ours.Port)
	}
	if p.kReadConfigCmd != "" {
		proxyType, err := p.readAsUser(p.kReadConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", "ProxyType")
		if err != nil {
			return true
		}
		if strings.TrimSpace(proxyType) != "1" {
			return false
		}
		httpProxy, err := p.readAsUser(p.kReadConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", "httpProxy")
		if err != nil {
			return true
		}
		return strings.TrimSpace(httpProxy) == "http://"+ours.String()
	}
	return true
}

// restoreBackup puts the system proxy settings back to what they were before
// Enable, but only if the current settings are still ours — if the user (or
// another tool) changed them while we were running, they own the state now
// and we must not touch it. The backup file is removed on success.
func (p *LinuxSystemProxy) restoreBackup(backup *proxyBackup) error {
	ours := M.ParseSocksaddrHostPort(backup.OursHost, backup.OursPort)
	if !p.currentIsOurs(ours) {
		return removeProxyBackup()
	}
	var errs []error
	if p.hasGSettings {
		if len(backup.Gnome) > 0 {
			for _, schemaKey := range gnomeProxyKeys {
				value, loaded := backup.Gnome[schemaKey]
				if !loaded {
					continue
				}
				parts := strings.SplitN(schemaKey, " ", 2)
				errs = append(errs, p.runAsUser("gsettings", "set", parts[0], parts[1], value))
			}
		} else {
			errs = append(errs, p.runAsUser("gsettings", "set", "org.gnome.system.proxy", "mode", "none"))
		}
	}
	if p.kWriteConfigCmd != "" {
		if backup.KDEValid {
			for _, key := range kdeProxyKeys {
				value, loaded := backup.KDE[key]
				if !loaded {
					continue
				}
				if value == "" {
					errs = append(errs, p.runAsUser(p.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", key, "--delete"))
				} else {
					errs = append(errs, p.runAsUser(p.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", key, value))
				}
			}
		} else {
			errs = append(errs, p.runAsUser(p.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", "ProxyType", "0"))
		}
		errs = append(errs, p.runAsUser("dbus-send", "--type=signal", "/KIO/Scheduler", "org.kde.KIO.Scheduler.reparseSlaveConfiguration", "string:''"))
	}
	if err := E.Errors(errs...); err != nil {
		return err
	}
	return removeProxyBackup()
}

// CleanupStaleSystemProxy restores the user's original system proxy settings
// if a previous session enabled the system proxy and never disabled it
// (crash, SIGKILL). Call on application start, before any new Enable.
func CleanupStaleSystemProxy(ctx context.Context) error {
	backup, err := loadProxyBackup()
	if err != nil || backup == nil {
		return err
	}
	proxy, err := NewSystemProxy(ctx, M.ParseSocksaddrHostPort(backup.OursHost, backup.OursPort), false)
	if err != nil {
		return err
	}
	return proxy.restoreBackup(backup)
}
