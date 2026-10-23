package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestVarnishstatParams(t *testing.T) {
	for _, test := range []struct {
		name   string
		major  int
		params varnishstatParams
		want   []string
		empty  bool
	}{
		{name: "defaults", major: 7, empty: true},
		{name: "instance", major: 3, params: varnishstatParams{Instance: "cache"}, want: []string{"-n", "cache"}},
		{name: "VSM unsupported on Varnish 3", major: 3, params: varnishstatParams{VSM: "/var/lib/varnish/cache"}},
		{name: "VSM supported on Varnish 4", major: 4, params: varnishstatParams{VSM: "/var/lib/varnish/cache"}, want: []string{"-N", "/var/lib/varnish/cache"}},
		{name: "combined", major: 9, params: varnishstatParams{Instance: "cache", VSM: "/var/lib/varnish/cache"}, want: []string{"-n", "cache", "-N", "/var/lib/varnish/cache"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			version := &varnishVersion{Major: test.major, Minor: 0, Patch: 0}
			if got := test.params.isEmpty(); got != test.empty {
				t.Errorf("isEmpty() = %v, want %v", got, test.empty)
			}
			if got := test.params.make(version); !slices.Equal(got, test.want) {
				t.Errorf("make() = %v, want %v", got, test.want)
			}
		})
	}
}

// fakeVarnishstat points StartParams at a shell script standing in for varnishstat.
func fakeVarnishstat(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake varnishstat needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "varnishstat")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	previousParams := *StartParams
	t.Cleanup(func() { *StartParams = previousParams })
	StartParams.VarnishstatExe = path
	StartParams.VarnishDockerContainer = ""
	StartParams.Params = &varnishstatParams{}
}

func TestScrapeVarnishIgnoresStderr(t *testing.T) {
	fakeVarnishstat(t, `case "$1" in
-V) echo "varnishstat (varnish-9.1.0 revision ffd36e9)" >&2; echo "Copyright" >&2 ;;
*) echo "warning: before" >&2
   echo '{"version":1,"counters":{"MAIN.uptime":{"flag":"c","value":42}}}'
   echo "warning: after" >&2 ;;
esac
`)
	// varnishstat -V prints the version to stderr.
	version, err := queryVarnishVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version.String() != "9.1.0 ffd36e9" {
		t.Fatalf("version = %q, want 9.1.0 ffd36e9", version)
	}

	metrics := make(chan prometheus.Metric, 10)
	if _, err := ScrapeVarnish(version, metrics); err != nil {
		t.Fatalf("stderr output broke the scrape: %s", err)
	}
	if len(metrics) != 1 {
		t.Fatalf("got %d metrics, want 1", len(metrics))
	}
}

func TestScrapeVarnishReportsStderrOnFailure(t *testing.T) {
	fakeVarnishstat(t, `echo "Could not get hold of varnishd" >&2; exit 1
`)
	output, err := ScrapeVarnish(NewVarnishVersion(), make(chan prometheus.Metric))
	if err == nil {
		t.Fatal("failing varnishstat did not fail the scrape")
	}
	if !strings.Contains(string(output), "Could not get hold of varnishd") {
		t.Fatalf("output = %q, want stderr for diagnostics", output)
	}
}

func TestExecuteVarnishstatTimesOut(t *testing.T) {
	// The background sleep keeps the output pipes open after the shell is killed.
	fakeVarnishstat(t, `sleep 30 &
sleep 30
`)
	previousTimeout := varnishstatTimeout
	varnishstatTimeout = 100 * time.Millisecond
	t.Cleanup(func() { varnishstatTimeout = previousTimeout })

	start := time.Now()
	_, _, err := executeVarnishstat(StartParams.VarnishstatExe, "-j")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("hung varnishstat blocked for %s", elapsed)
	}
}
