package main

import (
	"fmt"
	"os"

	"github.com/ben-kuhn/tncd/v2/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("tncd", version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "tncd: not yet implemented")
	os.Exit(1)
}
