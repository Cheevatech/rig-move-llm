package cli

import (
	"flag"
	"fmt"
	"os"

	"github.com/Cheevatech/rig-move-llm/internal/thin"
)

// cmdWatch is the third pillar of the map: a way to see what the worker is
// doing while it does it. It answers one question — working, or stuck? — from a
// second window, which is the question a 20-to-50-minute run actually raises.
func cmdWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	list := fs.Bool("list", false, "list recent runs instead of following one")
	noFollow := fs.Bool("no-follow", false, "print what has happened so far and exit")
	limit := fs.Int("n", 10, "with --list: how many runs to show")
	_ = fs.Parse(args)

	if *list {
		runs, err := thin.ListRuns(*limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, "watch:", err)
			return 1
		}
		for _, r := range runs {
			fmt.Println(thin.RunSummary(r))
		}
		return 0
	}

	dir := ""
	if rest := fs.Args(); len(rest) > 0 {
		dir = rest[0]
	}
	if err := thin.Watch(os.Stdout, dir, !*noFollow); err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		return 1
	}
	return 0
}
