//go:build linux && !android

package settings

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/shell"
)

type LinuxSystemProxy struct {
	hasGSettings    bool
	kWriteConfigCmd string
	kReadConfigCmd  string
	sudoUser        string
	serverAddr      M.Socksaddr
	supportSOCKS    bool
	isEnabled       bool
}

func NewSystemProxy(ctx context.Context, serverAddr M.Socksaddr, supportSOCKS bool) (*LinuxSystemProxy, error) {
	hasGSettings := common.Error(exec.LookPath("gsettings")) == nil
	kWriteConfigCmds := []string{
		"kwriteconfig5",
		"kwriteconfig6",
	}
	var kWriteConfigCmd string
	for _, cmd := range kWriteConfigCmds {
		if common.Error(exec.LookPath(cmd)) == nil {
			kWriteConfigCmd = cmd
			break
		}
	}
	var kReadConfigCmd string
	if kWriteConfigCmd != "" {
		readCmd := strings.Replace(kWriteConfigCmd, "kwriteconfig", "kreadconfig", 1)
		if common.Error(exec.LookPath(readCmd)) == nil {
			kReadConfigCmd = readCmd
		}
	}
	var sudoUser string
	if os.Getuid() == 0 {
		sudoUser = os.Getenv("SUDO_USER")
	}
	if !hasGSettings && kWriteConfigCmd == "" {
		return nil, E.New("unsupported desktop environment")
	}
	return &LinuxSystemProxy{
		hasGSettings:    hasGSettings,
		kWriteConfigCmd: kWriteConfigCmd,
		kReadConfigCmd:  kReadConfigCmd,
		sudoUser:        sudoUser,
		serverAddr:      serverAddr,
		supportSOCKS:    supportSOCKS,
	}, nil
}

func (p *LinuxSystemProxy) IsEnabled() bool {
	return p.isEnabled
}

func (p *LinuxSystemProxy) Enable() error {
	// Snapshot the user's current proxy settings before overwriting them, so
	// Disable (or crash recovery on next start) can restore them instead of
	// blindly switching the proxy off. Best effort: a failed snapshot must
	// not block enabling the proxy.
	if existing, _ := loadProxyBackup(); existing != nil &&
		p.currentIsOurs(M.ParseSocksaddrHostPort(existing.OursHost, existing.OursPort)) {
		// Current settings were written by a previous session of ours — the
		// user's original settings live in the existing backup. Keep it and
		// only refresh our address.
		existing.OursHost = p.serverAddr.AddrString()
		existing.OursPort = p.serverAddr.Port
		_ = existing.save()
	} else {
		_ = p.captureBackup().save()
	}
	if p.hasGSettings {
		err := p.runAsUser("gsettings", "set", "org.gnome.system.proxy.http", "enabled", "true")
		if err != nil {
			return err
		}
		if p.supportSOCKS {
			err = p.setGnomeProxy("ftp", "http", "https", "socks")
		} else {
			err = p.setGnomeProxy("http", "https")
		}
		if err != nil {
			return err
		}
		err = p.runAsUser("gsettings", "set", "org.gnome.system.proxy", "use-same-proxy", F.ToString(p.supportSOCKS))
		if err != nil {
			return err
		}
		err = p.runAsUser("gsettings", "set", "org.gnome.system.proxy", "mode", "manual")
		if err != nil {
			return err
		}
	}
	if p.kWriteConfigCmd != "" {
		err := p.runAsUser(p.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", "ProxyType", "1")
		if err != nil {
			return err
		}
		if p.supportSOCKS {
			err = p.setKDEProxy("ftp", "http", "https", "socks")
		} else {
			err = p.setKDEProxy("http", "https")
		}
		if err != nil {
			return err
		}
		err = p.runAsUser(p.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", "Authmode", "0")
		if err != nil {
			return err
		}
		err = p.runAsUser("dbus-send", "--type=signal", "/KIO/Scheduler", "org.kde.KIO.Scheduler.reparseSlaveConfiguration", "string:''")
		if err != nil {
			return err
		}
	}
	p.isEnabled = true
	return nil
}

func (p *LinuxSystemProxy) Disable() error {
	// Restore the user's pre-Enable settings when we have them. Falls through
	// to the plain "switch off" path only if there is no backup (snapshot
	// failed) or restoring it failed.
	if backup, _ := loadProxyBackup(); backup != nil {
		err := p.restoreBackup(backup)
		if err == nil {
			p.isEnabled = false
			return nil
		}
	}
	if p.hasGSettings {
		err := p.runAsUser("gsettings", "set", "org.gnome.system.proxy", "mode", "none")
		if err != nil {
			return err
		}
	}
	if p.kWriteConfigCmd != "" {
		err := p.runAsUser(p.kWriteConfigCmd, "--file", "kioslaverc", "--group", "Proxy Settings", "--key", "ProxyType", "0")
		if err != nil {
			return err
		}
		err = p.runAsUser("dbus-send", "--type=signal", "/KIO/Scheduler", "org.kde.KIO.Scheduler.reparseSlaveConfiguration", "string:''")
		if err != nil {
			return err
		}
	}
	p.isEnabled = false
	return nil
}

func (p *LinuxSystemProxy) userShell(name string, args ...string) (*shell.Shell, error) {
	if os.Getuid() != 0 {
		return shell.Exec(name, args...), nil
	} else if p.sudoUser != "" {
		cmd := F.ToString(name, " ", strings.Join(args, " "))
		// Look up the target user's UID for D-Bus socket path
		u, err := user.Lookup(p.sudoUser)
		if err != nil {
			// Fallback: run without D-Bus env (original behavior)
			return shell.Exec("su", "-", p.sudoUser, "-c", cmd), nil
		}
		dbusAddr := fmt.Sprintf("unix:path=/run/user/%s/bus", u.Uid)
		display := os.Getenv("DISPLAY")
		if display == "" {
			display = ":0"
		}
		envCmd := fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=%s DISPLAY=%s %s", dbusAddr, display, cmd)
		return shell.Exec("su", "-", p.sudoUser, "-c", envCmd), nil
	} else {
		return nil, E.New("set system proxy: unable to set as root")
	}
}

func (p *LinuxSystemProxy) runAsUser(name string, args ...string) error {
	s, err := p.userShell(name, args...)
	if err != nil {
		return err
	}
	return s.Attach().Run()
}

func (p *LinuxSystemProxy) readAsUser(name string, args ...string) (string, error) {
	s, err := p.userShell(name, args...)
	if err != nil {
		return "", err
	}
	return s.ReadOutput()
}

func (p *LinuxSystemProxy) setGnomeProxy(proxyTypes ...string) error {
	for _, proxyType := range proxyTypes {
		err := p.runAsUser("gsettings", "set", "org.gnome.system.proxy."+proxyType, "host", p.serverAddr.AddrString())
		if err != nil {
			return err
		}
		err = p.runAsUser("gsettings", "set", "org.gnome.system.proxy."+proxyType, "port", F.ToString(p.serverAddr.Port))
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *LinuxSystemProxy) setKDEProxy(proxyTypes ...string) error {
	for _, proxyType := range proxyTypes {
		var proxyUrl string
		if proxyType == "socks" {
			proxyUrl = "socks://" + p.serverAddr.String()
		} else {
			proxyUrl = "http://" + p.serverAddr.String()
		}
		err := p.runAsUser(
			p.kWriteConfigCmd,
			"--file",
			"kioslaverc",
			"--group",
			"Proxy Settings",
			"--key", proxyType+"Proxy",
			proxyUrl,
		)
		if err != nil {
			return err
		}
	}
	return nil
}
