package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLogFatalErrorPreservesMessage(t *testing.T) {
	const childEnv = "VARNISH_EXPORTER_TEST_FATAL_LOG"
	if os.Getenv(childEnv) == "1" {
		logger = log.New(os.Stdout, "", 0)
		StartParams.Raw = false
		logFatalError(errors.New("scrape failed: 100% used %s"))
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestLogFatalErrorPreservesMessage$")
	command.Env = append(os.Environ(), childEnv+"=1")
	output, err := command.CombinedOutput()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
		t.Fatalf("fatal log subprocess exit = %v, output = %q", err, output)
	}
	if got, want := strings.TrimSpace(string(output)), "[FATAL] scrape failed: 100% used %s"; got != want {
		t.Fatalf("fatal log = %q, want %q", got, want)
	}
}
