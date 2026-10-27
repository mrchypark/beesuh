package main

import (
	"flag"
	"fmt"
	"os"
	"testing"
)

// Re-exec isolates run's flags and signal handlers from the test runner.
func TestOAuthBinaryProcess(t *testing.T) {
	if os.Getenv("BEESUH_TEST_OAUTH_BINARY_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("beesuh", flag.ExitOnError)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
