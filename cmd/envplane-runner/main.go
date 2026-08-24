package main

import (
	"log/slog"
	"os"

	runnerruntime "github.com/envplane/runner/internal/runner"
	"github.com/envplane/runner/internal/sandbox"
)

// Keep the sandbox validator reachable from the production binary. Typed
// sandbox execution is invoked by the command gateway when enabled; startup
// does not launch a sandbox or inspect customer data.
var _ = sandbox.ValidateLaunch

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "runner-connectivity-check":
			runnerruntime.ConnectivityCheck(logger)
			return
		case "runner":
			// Backward-compatible no-op argument for chart upgrades.
		default:
			logger.Error("unknown runner command", "command", os.Args[1])
			os.Exit(2)
		}
	}
	runnerruntime.Run(logger)
}
