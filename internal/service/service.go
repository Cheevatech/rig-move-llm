// Package service installs OS-level supervision so that `rig-move-llm serve`
// survives reboots without a login shell. It stays true to the project's
// stdlib-only, zero-dependency identity: a "service" here is nothing but three
// text templates (a launchd LaunchAgent, a systemd --user unit, a Windows Task
// Scheduler task) driven through os/exec. The daemon itself is the ordinary
// `serve` subcommand, which already shuts down and flushes its ledger cleanly on
// SIGTERM — so an OS supervisor can start, stop, and restart it safely.
//
// Everything is USER-scoped and needs no sudo: LaunchAgents load at user login,
// systemd --user units run in the per-user manager (with lingering enabled so
// they start at boot before any login), and the scheduled task triggers on logon.
// The trade-off is honest — a fully headless machine that never logs the user in
// will not start a LaunchAgent; that is the price of avoiding root.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Label is the launchd label / systemd unit name / Windows task name. It doubles
// as the reverse-DNS identifier launchd requires.
const (
	Label    = "com.rigmovellm.rig-move-llm"
	UnitName = "rig-move-llm"
)

// Manager renders and applies the OS supervision definition for one binary. GOOS
// and Run are injectable so template generation and the exec flow are unit-testable
// on any host.
type Manager struct {
	GOOS    string                                            // target OS (defaults to runtime.GOOS)
	BinPath string                                            // absolute path to the rig-move-llm binary
	HomeDir string                                            // user home dir (where unit files live)
	DataDir string                                            // scope data dir for logs + state (RIG_STATE_DIR)
	UID     int                                               // launchd domain uid (darwin)
	Run     func(name string, args ...string) ([]byte, error) // exec hook
}

// New builds a Manager wired to the real filesystem and os/exec.
func New(binPath, homeDir, dataDir string) *Manager {
	return &Manager{
		GOOS:    runtime.GOOS,
		BinPath: binPath,
		HomeDir: homeDir,
		DataDir: dataDir,
		UID:     os.Getuid(),
		Run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
	}
}

// UnitPath is where the definition file is written for the target OS.
func (m *Manager) UnitPath() string {
	switch m.GOOS {
	case "darwin":
		return filepath.Join(m.HomeDir, "Library", "LaunchAgents", Label+".plist")
	case "windows":
		return filepath.Join(m.HomeDir, "AppData", "Local", UnitName, UnitName+".xml")
	default: // linux and other systemd-based unixes
		return filepath.Join(m.HomeDir, ".config", "systemd", "user", UnitName+".service")
	}
}

// UnitContent renders the supervision definition (pure — no I/O). It is the unit
// tested by golden tests.
func (m *Manager) UnitContent() string {
	switch m.GOOS {
	case "darwin":
		return m.launchdPlist()
	case "windows":
		return m.windowsTaskXML()
	default:
		return m.systemdUnit()
	}
}

func (m *Manager) launchdPlist() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + m.BinPath + `</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>RIG_STATE_DIR</key>
		<string>` + m.DataDir + `</string>
	</dict>
	<key>StandardOutPath</key>
	<string>` + filepath.Join(m.DataDir, "serve.out.log") + `</string>
	<key>StandardErrorPath</key>
	<string>` + filepath.Join(m.DataDir, "serve.err.log") + `</string>
</dict>
</plist>
`
}

func (m *Manager) systemdUnit() string {
	return `[Unit]
Description=rig-move-llm subscription-preserving hybrid proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + m.BinPath + ` serve
Environment=RIG_STATE_DIR=` + m.DataDir + `
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`
}

func (m *Manager) windowsTaskXML() string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + m.BinPath + `</Command>
      <Arguments>serve</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// domainTarget is the launchd gui domain for the current user, e.g. "gui/501".
func (m *Manager) domainTarget() string { return "gui/" + strconv.Itoa(m.UID) }

// tccProtected returns the offending directory ("~/Documents" etc.) when the
// binary lives in a macOS TCC-protected user folder, else "".
func (m *Manager) tccProtected() string {
	for _, d := range []string{"Documents", "Desktop", "Downloads"} {
		if strings.HasPrefix(m.BinPath, filepath.Join(m.HomeDir, d)+string(filepath.Separator)) {
			return "~/" + d
		}
	}
	return ""
}

// Install writes the unit file and asks the OS supervisor to load + start it. It
// is idempotent enough to re-run: existing definitions are overwritten and
// reloaded. Non-fatal supervisor warnings (e.g. enable-linger without polkit) are
// returned as part of the message, not as hard errors.
func (m *Manager) Install() (string, error) {
	path := m.UnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(m.UnitContent()), 0o644); err != nil {
		return "", err
	}

	var notes []string
	switch m.GOOS {
	case "darwin":
		// launchd-spawned processes cannot pass a TCC files-and-folders prompt, so a
		// binary under Documents/Desktop/Downloads hangs in dyld before main — found
		// the hard way. npm-installed binaries live elsewhere; warn dev installs.
		if p := m.tccProtected(); p != "" {
			notes = append(notes, "binary is under "+p+" (TCC-protected): launchd will hang loading it — move it outside, e.g. ~/"+filepath.Base(m.DataDir)+"/bin/")
		}
		// bootout any stale instance first so bootstrap does not fail with EEXIST.
		_, _ = m.Run("launchctl", "bootout", m.domainTarget(), path)
		if out, err := m.Run("launchctl", "bootstrap", m.domainTarget(), path); err != nil {
			return "", fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
		}
		_, _ = m.Run("launchctl", "enable", m.domainTarget()+"/"+Label)
	case "windows":
		if out, err := m.Run("schtasks", "/Create", "/TN", UnitName, "/XML", path, "/F"); err != nil {
			return "", fmt.Errorf("schtasks /Create: %v: %s", err, strings.TrimSpace(string(out)))
		}
	default:
		_, _ = m.Run("systemctl", "--user", "daemon-reload")
		if out, err := m.Run("systemctl", "--user", "enable", "--now", UnitName+".service"); err != nil {
			return "", fmt.Errorf("systemctl --user enable: %v: %s", err, strings.TrimSpace(string(out)))
		}
		// Lingering lets the user manager (and thus the proxy) start at boot before
		// any login. It may require polkit/root on locked-down systems — best effort.
		if out, err := m.Run("loginctl", "enable-linger"); err != nil {
			notes = append(notes, "could not enable-linger (service starts at login, not at boot): "+strings.TrimSpace(string(out)))
		}
	}

	msg := "installed OS service at " + path
	if len(notes) > 0 {
		msg += "\n  note: " + strings.Join(notes, "\n  note: ")
	}
	return msg, nil
}

// Uninstall stops + unloads the supervisor definition and removes the unit file.
// It is idempotent: a missing or already-unloaded service is not an error, so it
// is safe to call unconditionally from `uninstall` (roundtrip with Install).
func (m *Manager) Uninstall() (string, error) {
	path := m.UnitPath()
	switch m.GOOS {
	case "darwin":
		_, _ = m.Run("launchctl", "bootout", m.domainTarget(), path)
	case "windows":
		_, _ = m.Run("schtasks", "/Delete", "/TN", UnitName, "/F")
	default:
		_, _ = m.Run("systemctl", "--user", "disable", "--now", UnitName+".service")
	}
	removed := ""
	if err := os.Remove(path); err == nil {
		removed = " (removed " + path + ")"
	}
	return "removed OS service" + removed, nil
}

// Status reports whether the supervisor considers the service loaded/active.
func (m *Manager) Status() (string, error) {
	switch m.GOOS {
	case "darwin":
		out, err := m.Run("launchctl", "print", m.domainTarget()+"/"+Label)
		if err != nil {
			return "not loaded", nil
		}
		return summarizeLaunchctl(string(out)), nil
	case "windows":
		out, err := m.Run("schtasks", "/Query", "/TN", UnitName)
		if err != nil {
			return "not registered", nil
		}
		return strings.TrimSpace(string(out)), nil
	default:
		out, _ := m.Run("systemctl", "--user", "is-active", UnitName+".service")
		return strings.TrimSpace(string(out)), nil
	}
}

// summarizeLaunchctl pulls the `state = ...` line out of `launchctl print`.
func summarizeLaunchctl(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "state =") {
			return "loaded (" + strings.TrimSpace(strings.TrimPrefix(s, "state =")) + ")"
		}
	}
	return "loaded"
}
