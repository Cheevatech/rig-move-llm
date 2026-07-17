package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mgr returns a Manager for a target OS with a recording, always-succeeding Run.
func mgr(goos string, t *testing.T) (*Manager, *[][]string) {
	t.Helper()
	var calls [][]string
	m := &Manager{
		GOOS:    goos,
		BinPath: "/opt/rml/rig-move-llm",
		HomeDir: t.TempDir(),
		DataDir: "/home/u/.rig-move-llm",
		UID:     501,
		Run: func(name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			return nil, nil
		},
	}
	return m, &calls
}

func TestUnitPathPerOS(t *testing.T) {
	for goos, want := range map[string]string{
		"darwin":  filepath.Join("Library", "LaunchAgents", Label+".plist"),
		"linux":   filepath.Join(".config", "systemd", "user", UnitName+".service"),
		"windows": filepath.Join("AppData", "Local", UnitName, UnitName+".xml"),
	} {
		m, _ := mgr(goos, t)
		if got := m.UnitPath(); !strings.HasSuffix(got, want) {
			t.Errorf("%s: UnitPath = %q, want suffix %q", goos, got, want)
		}
	}
}

func TestLaunchdPlistContent(t *testing.T) {
	m, _ := mgr("darwin", t)
	c := m.UnitContent()
	for _, want := range []string{
		"<string>" + Label + "</string>",
		"<string>/opt/rml/rig-move-llm</string>",
		"<string>serve</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"RIG_STATE_DIR",
		filepath.Join(m.DataDir, "serve.err.log"),
	} {
		if !strings.Contains(c, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestSystemdUnitContent(t *testing.T) {
	m, _ := mgr("linux", t)
	c := m.UnitContent()
	for _, want := range []string{
		"ExecStart=/opt/rml/rig-move-llm serve",
		"Restart=on-failure",
		"Environment=RIG_STATE_DIR=/home/u/.rig-move-llm",
		"WantedBy=default.target",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("unit missing %q", want)
		}
	}
}

func TestWindowsTaskContent(t *testing.T) {
	m, _ := mgr("windows", t)
	c := m.UnitContent()
	for _, want := range []string{
		"<Command>/opt/rml/rig-move-llm</Command>",
		"<Arguments>serve</Arguments>",
		"<LogonTrigger>",
		"<RunLevel>LeastPrivilege</RunLevel>",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("task xml missing %q", want)
		}
	}
}

func TestInstallDarwinFlow(t *testing.T) {
	m, calls := mgr("darwin", t)
	msg, err := m.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, m.UnitPath()) {
		t.Errorf("msg %q missing unit path", msg)
	}
	// Unit file written to disk.
	if _, err := os.Stat(m.UnitPath()); err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	// bootout (stale) then bootstrap into gui/501.
	if len(*calls) < 2 {
		t.Fatalf("expected >=2 launchctl calls, got %v", *calls)
	}
	if got := (*calls)[0]; got[0] != "launchctl" || got[1] != "bootout" || got[2] != "gui/501" {
		t.Errorf("first call = %v, want launchctl bootout gui/501", got)
	}
	if got := (*calls)[1]; got[1] != "bootstrap" || got[2] != "gui/501" || got[3] != m.UnitPath() {
		t.Errorf("second call = %v, want launchctl bootstrap gui/501 <plist>", got)
	}
}

func TestInstallLinuxFlow(t *testing.T) {
	m, calls := mgr("linux", t)
	if _, err := m.Install(); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range *calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + UnitName + ".service",
		"loginctl enable-linger",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("calls missing %q in:\n%s", want, joined)
		}
	}
}

func TestInstallLinuxLingerFailureIsNote(t *testing.T) {
	m, _ := mgr("linux", t)
	m.Run = func(name string, args ...string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("access denied"), errors.New("exit 1")
		}
		return nil, nil
	}
	msg, err := m.Install()
	if err != nil {
		t.Fatalf("linger failure must not fail install: %v", err)
	}
	if !strings.Contains(msg, "enable-linger") {
		t.Errorf("msg %q missing linger note", msg)
	}
}

func TestInstallBootstrapFailure(t *testing.T) {
	m, _ := mgr("darwin", t)
	m.Run = func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "bootstrap" {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit 5")
		}
		return nil, nil
	}
	if _, err := m.Install(); err == nil {
		t.Fatal("bootstrap failure must surface as error")
	}
}

func TestUninstallRoundtrip(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		m, calls := mgr(goos, t)
		if _, err := m.Install(); err != nil {
			t.Fatalf("%s install: %v", goos, err)
		}
		*calls = nil
		msg, err := m.Uninstall()
		if err != nil {
			t.Fatalf("%s uninstall: %v", goos, err)
		}
		if _, err := os.Stat(m.UnitPath()); !os.IsNotExist(err) {
			t.Errorf("%s: unit file still present after uninstall", goos)
		}
		if !strings.Contains(msg, "removed") {
			t.Errorf("%s: msg %q", goos, msg)
		}
		if len(*calls) == 0 {
			t.Errorf("%s: uninstall issued no supervisor calls", goos)
		}
	}
}

func TestUninstallIdempotentWhenNeverInstalled(t *testing.T) {
	m, _ := mgr("linux", t)
	if _, err := m.Uninstall(); err != nil {
		t.Fatalf("uninstall without install must be a no-op, got %v", err)
	}
}

func TestStatusDarwinParsesState(t *testing.T) {
	m, _ := mgr("darwin", t)
	m.Run = func(name string, args ...string) ([]byte, error) {
		return []byte("com.rigmovellm.rig-move-llm = {\n\tstate = running\n}\n"), nil
	}
	got, _ := m.Status()
	if got != "loaded (running)" {
		t.Errorf("Status = %q, want loaded (running)", got)
	}
}

func TestStatusDarwinNotLoaded(t *testing.T) {
	m, _ := mgr("darwin", t)
	m.Run = func(name string, args ...string) ([]byte, error) {
		return []byte("Could not find service"), errors.New("exit 113")
	}
	got, _ := m.Status()
	if got != "not loaded" {
		t.Errorf("Status = %q, want not loaded", got)
	}
}

func TestInstallWarnsOnTCCProtectedBinary(t *testing.T) {
	m, _ := mgr("darwin", t)
	m.BinPath = filepath.Join(m.HomeDir, "Documents", "proj", "rig-move-llm")
	msg, err := m.Install()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "TCC-protected") {
		t.Errorf("msg %q missing TCC warning", msg)
	}
	// Outside a protected folder: no warning.
	m2, _ := mgr("darwin", t)
	m2.BinPath = filepath.Join(m2.HomeDir, ".rig-move-llm", "bin", "rig-move-llm")
	msg2, _ := m2.Install()
	if strings.Contains(msg2, "TCC-protected") {
		t.Errorf("unexpected TCC warning for %q", m2.BinPath)
	}
}
