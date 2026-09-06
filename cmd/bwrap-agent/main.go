package main

import (
	"os"

	"bwrap-agent/internal/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:]))
}
