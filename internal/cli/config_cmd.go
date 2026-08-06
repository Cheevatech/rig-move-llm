package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// cmdEnable / cmdDisable flip the ENABLED master switch in a scope's config.env
// without the user opening the hidden dir. Global by default (the install scope);
// --local targets only this project. Config is read fresh per request, so the flip
// takes effect on the next `claude` with no restart.
func cmdEnable(args []string) int  { return setEnabled("enable", args, true) }
func cmdDisable(args []string) int { return setEnabled("disable", args, false) }

func setEnabled(name string, args []string, on bool) int {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	local := fs.Bool("local", false, "target this project only (./.rig-move-llm/config.env) instead of the global scope")
	_ = fs.Parse(args)

	path := scopeConfigPath(*local)
	if !fileExists(path) {
		fmt.Fprintf(os.Stderr, "no config at %s — run `rig-move-llm setup` first\n", path)
		return 1
	}
	if err := setConfigKey(path, "ENABLED", boolStr(on)); err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		return 1
	}

	scope := "global (all projects)"
	if *local {
		scope = "local (this project)"
	}
	if on {
		fmt.Printf("ENABLED=true — the switch may route to the worker for %s\n%s\n", scope, path)
		cfg := config.Load()
		// Enabling without a worker endpoint means the switch has nothing behind it;
		// say so now rather than let every flipped turn fail mid-task.
		if cfg.WorkerAPIBase == "" {
			fmt.Println("WARNING: no worker endpoint resolves — set WORKER_API_BASE (run `rig-move-llm config --open`)")
		}
		// ENABLED is permission, not the flip itself. Saying so here is what keeps
		// the two switches from being confused for one.
		if !cfg.RouteAllToWorker {
			fmt.Println("note: turns still run on your paid model until something runs `rig-move-llm worker on` (or /worker on inside a session)")
		}
	} else {
		fmt.Printf("ENABLED=false — every request pins to your paid model for %s\n%s\n", scope, path)
	}
	return 0
}

// cmdConfig reports the EFFECTIVE configuration as resolved from the current
// directory (env > local > global), plus which scope files exist and their ENABLED
// value, so the precedence is visible at a glance. --open launches $EDITOR on the
// target scope's config.env.
func cmdConfig(args []string) int {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	open := fs.Bool("open", false, "open the target scope's config.env in your $EDITOR")
	local := fs.Bool("local", false, "target the local (project) scope for --open")
	_ = fs.Parse(args)

	if *open {
		return openConfigInEditor(*local)
	}

	cfg := config.Load()
	cwd, _ := os.Getwd()
	globalPath := filepath.Join(config.GlobalDir(), config.ConfigFile)
	localPath := filepath.Join(config.LocalDir(), config.ConfigFile)

	// Two booleans decide one thing, so print the ANSWER first and the inputs after.
	// The case worth naming is the disagreement — a switch that is on while ENABLED
	// is off reads as "I flipped it and nothing happened" unless something says why.
	brain := "your paid model"
	if cfg.Enabled && cfg.RouteAllToWorker {
		brain = "the worker"
	} else if cfg.RouteAllToWorker {
		brain = "your paid model — the switch is ON but ENABLED=false overrides it (`rig-move-llm enable`)"
	}
	fmt.Printf("effective config (as seen from %s):\n", cwd)
	fmt.Printf("  next turn runs on: %s\n", brain)
	fmt.Printf("  enabled:        %v\n", cfg.Enabled)
	fmt.Printf("  switch (worker): %v\n", cfg.RouteAllToWorker)
	if cfg.WorkerAPIBase != "" {
		fmt.Printf("  worker:         %s%s backend=%s\n", cfg.WorkerAPIBase, modelNote(cfg.WorkerModel), cfg.Backend.Name)
	} else {
		fmt.Printf("  worker:         (none set — offload cannot run)\n")
	}
	fmt.Printf("  proxy port:     %s\n", cfg.Port)
	fmt.Printf("  main upstream:  %s\n", cfg.MainUpstreamURL)

	fmt.Println()
	fmt.Println("scopes (lowest → highest precedence):")
	printScope("global", globalPath)
	printScope("local ", localPath)
	if envOv := envOverrides(); len(envOv) > 0 {
		fmt.Printf("  env      overrides in process: %s\n", strings.Join(envOv, ", "))
	}
	return 0
}

// printScope shows one config layer: whether its file exists and, if so, its ENABLED
// and worker-endpoint values (the two that most often decide behaviour).
func printScope(label, path string) {
	if !fileExists(path) {
		fmt.Printf("  %s   %s   (absent)\n", label, path)
		return
	}
	v := config.FileValues(path)
	parts := []string{}
	if e, ok := v["ENABLED"]; ok {
		parts = append(parts, "ENABLED="+e)
	}
	if b, ok := v["WORKER_API_BASE"]; ok && b != "" {
		parts = append(parts, "WORKER_API_BASE="+b)
	}
	detail := "present"
	if len(parts) > 0 {
		detail = strings.Join(parts, "  ")
	}
	fmt.Printf("  %s   %s   %s\n", label, path, detail)
}

// envOverrides lists the rig config keys currently set in the process environment
// (they override both file scopes).
func envOverrides() []string {
	var out []string
	for _, k := range []string{"ENABLED", "RIG_ROUTE_ALL_TO_WORKER", "WORKER_API_BASE", "WORKER_MODEL", "WORKER_BACKEND", "WORKER_API_KEY", "PORT", "MAIN_UPSTREAM_URL", "LOG_BODIES", "LOG_MAX_MB"} {
		if _, ok := os.LookupEnv(k); ok {
			out = append(out, k)
		}
	}
	return out
}

// openConfigInEditor launches $EDITOR (falling back to a sensible default) on the
// target scope's config.env, creating nothing: the file must already exist.
func openConfigInEditor(local bool) int {
	path := scopeConfigPath(local)
	if !fileExists(path) {
		fmt.Fprintf(os.Stderr, "no config at %s — run `rig-move-llm setup` first\n", path)
		return 1
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	c := exec.Command(editor, path)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "config --open:", err)
		return 1
	}
	return 0
}

// scopeConfigPath returns the config.env path for the selected scope.
func scopeConfigPath(local bool) string {
	if local {
		return filepath.Join(config.LocalDir(), config.ConfigFile)
	}
	return filepath.Join(config.GlobalDir(), config.ConfigFile)
}

// keyLineRE matches a KEY assignment line, commented or not, with an optional
// `export` prefix — so the toggle rewrites the existing line in place rather than
// appending a duplicate.
func keyLineRE(key string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*#?\s*(export\s+)?` + regexp.QuoteMeta(key) + `\s*=`)
}

// setConfigKey rewrites (or appends) a KEY=value line in an env file, preserving
// every other line and comment. An existing commented-out `# KEY=` is un-commented.
func setConfigKey(path, key, val string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	re := keyLineRE(key)
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, ln := range lines {
		if re.MatchString(ln) {
			lines[i] = key + "=" + val
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, key+"="+val)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
