// Command gofer starts the Gofer super-agent runtime.
package main

import (
	"context"
	"os"

	"github.com/Rememorio/gofer/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
