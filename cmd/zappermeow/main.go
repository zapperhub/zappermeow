// Command zappermeow is the single binary of the platform. It dispatches to the
// subcommands described in the constitution: serve (stateless API), session-worker
// (stateful WhatsApp sessions) and jobs (asynq consumers).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "zappermeow: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("missing subcommand")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "serve":
		return runServe(ctx)
	case "session-worker":
		return runSessionWorker(ctx)
	case "jobs":
		return errors.New("jobs: not implemented in this release")
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `zappermeow — WhatsApp platform

Usage:
  zappermeow <command>

Commands:
  serve            Run the stateless REST API
  session-worker   Run the stateful WhatsApp session worker
  jobs             Run the asynq job consumers (not implemented)
  help             Show this message
`)
}
