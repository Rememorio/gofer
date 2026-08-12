package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Rememorio/gofer/internal/app"
	"github.com/Rememorio/gofer/internal/buildinfo"
)

const usage = `Gofer is a Go-native super agent harness.

Usage:
  gofer [command]

Commands:
  serve          Start the Gofer HTTP service
  version        Print build information
  help           Show this help

Options:
  -h, --help     Show this help
  -v, --version  Print the version
`

// Run executes the command-line interface and returns a process exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWithServices(ctx, args, stdout, stderr, Services{Serve: app.Serve})
}

// Services supplies process-level operations for testable command dispatch.
type Services struct {
	Serve func(context.Context, string, io.Writer) error
}

// RunWithServices executes the CLI with explicit process services.
func RunWithServices(ctx context.Context, args []string, stdout, stderr io.Writer, services Services) int {
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(stderr, "gofer: %v\n", err)
		return 1
	}

	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "-v", "--version":
		return runVersion(args[1:], stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr, services.Serve)
	default:
		fmt.Fprintf(stderr, "gofer: unknown command %q\n", args[0])
		return 2
	}
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer, serve func(context.Context, string, io.Writer) error) int {
	path := "config.yaml"
	if len(args) == 2 && args[0] == "--config" && args[1] != "" {
		path = args[1]
	} else if len(args) != 0 {
		fmt.Fprintln(stderr, "gofer: usage: gofer serve [--config FILE]")
		return 2
	}
	if serve == nil {
		fmt.Fprintln(stderr, "gofer: serve service is unavailable")
		return 1
	}
	if err := serve(ctx, path, stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "gofer: serve: %v\n", err)
		return 1
	}
	return 0
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	info := buildinfo.Current()
	if len(args) == 0 {
		fmt.Fprintf(stdout, "gofer %s (%s, %s)\n", info.Version, info.Commit, info.Date)
		return 0
	}
	if len(args) != 1 || args[0] != "--json" {
		fmt.Fprintln(stderr, "gofer: usage: gofer version [--json]")
		return 2
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(info); err != nil {
		fmt.Fprintf(stderr, "gofer: encode version: %v\n", err)
		return 1
	}
	return 0
}

// IsUsageError reports whether code represents invalid command-line input.
func IsUsageError(code int) bool {
	return errors.Is(exitCode(code), exitCode(2))
}

type exitCode int

func (code exitCode) Error() string {
	return fmt.Sprintf("exit code %d", code)
}

func (code exitCode) Is(target error) bool {
	want, ok := target.(exitCode)
	return ok && code == want
}
