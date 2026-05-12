package main

import (
	"embed"
	"io/fs"
	"log"

	"ekbn/internal/serve"
)

//go:embed dist
var distFS embed.FS

func main() {
	root, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("failed to access embedded dist: %v", err)
	}
	serve.Run(root)
}
