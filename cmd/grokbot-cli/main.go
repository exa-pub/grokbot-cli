// Command grokbot-cli connects this computer to Grok Bot.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/exa-pub/grokbot-cli/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cli.Execute(ctx, os.Args[1:])
	stop()
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
