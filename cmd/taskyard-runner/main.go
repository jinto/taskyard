package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jinto/taskyard/internal/buildinfo"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("taskyard-runner %s (protocol v%d)\n", buildinfo.Version(), buildinfo.ProtocolVersion())
		return
	}

	fmt.Fprintln(os.Stderr, "taskyard-runner: not implemented yet")
	os.Exit(1)
}
