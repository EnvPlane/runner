package main

import (
	"log/slog"
	"os"

	runnerruntime "github.com/envplane/runner/internal/runner"
	"github.com/envplane/runner/internal/sandbox"
)

// ValidateLaunch is compiled into the binary for the future sandbox execution path.
// The current runner command path does not invoke sandbox execution or this validator.
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
