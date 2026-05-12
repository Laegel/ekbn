package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"ekbn/internal/serve"
)

func runServe() {
	cmd := flag.NewFlagSet("serve", flag.ExitOnError)
	cmd.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ekbn serve [options]\n\nOptions:\n")
		cmd.PrintDefaults()
	}
	cmd.Parse(os.Args[2:])

	root := os.DirFS("./dist")
	serve.Run(root)

	log.Println("Server stopped")
}
