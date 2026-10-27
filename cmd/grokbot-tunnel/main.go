// Command grokbot-tunnel connects this computer to Grok Bot.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/cli"
	"github.com/exa-pub/grokbot-tunnel/internal/service"
)

func main() {
	if service.Journald() {
		// The journal stamps every line itself.
		log.SetFlags(0)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.Execute(ctx, os.Args[1:])
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(cli.ExitCode(err))
	}
}
