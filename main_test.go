package main

import (
	"bytes"
	"errors"
	"flag"
	"log"
	"strings"
	"testing"
)

func TestRegisterFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  bool
		test bool
	}{
		{name: "test mode preserves timestamped logging", test: true},
		{name: "raw logging stays enabled", raw: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := &startParams{Raw: tc.raw, Test: tc.test, Params: &varnishstatParams{}}
			flags := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			version := registerFlags(flags, params)
			if err := flags.Parse(nil); err != nil {
				t.Fatal(err)
			}
			if params.Raw != tc.raw || params.Test != tc.test || *version {
				t.Fatalf("flags changed defaults: raw=%t, test=%t, version=%t", params.Raw, params.Test, *version)
			}
		})
	}

	t.Run("explicit options", func(t *testing.T) {
		params := &startParams{Params: &varnishstatParams{}}
		flags := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
		version := registerFlags(flags, params)
		err := flags.Parse([]string{"-version", "-raw", "-test", "-with-go-metrics", "-web.telemetry-path=/stats", "-web.health-path=/health", "-n=cache"})
		if err != nil {
			t.Fatal(err)
		}
		if !*version || !params.Raw || !params.Test || !params.WithGoMetrics {
			t.Fatalf("boolean options were not applied: %+v, version=%t", params, *version)
		}
		if params.Path != "/stats" || params.HealthPath != "/health" || params.Params.Instance != "cache" {
			t.Fatalf("path and instance options were not applied: %+v", params)
		}
	})
}

func TestStartParamsValidate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		healthPath string
		wantErr    string
	}{
		{name: "default metrics", path: "/metrics"},
		{name: "custom health", path: "/stats", healthPath: "/health"},
		{name: "metrics at root", path: "/", healthPath: "/health"},
		{name: "health at root", path: "/metrics", healthPath: "/"},
		{name: "patterned paths", path: "/metrics/{instance}", healthPath: "/health/{$}"},
		{name: "empty metrics", wantErr: "telemetry-path"},
		{name: "relative metrics", path: "metrics", wantErr: "telemetry-path"},
		{name: "relative health", path: "/metrics", healthPath: "health", wantErr: "health-path"},
		{name: "conflicting paths", path: "/metrics", healthPath: "/metrics", wantErr: "same value"},
		{name: "malformed metrics pattern", path: "/metrics/{", wantErr: "invalid HTTP path configuration"},
		{name: "malformed health pattern", path: "/metrics", healthPath: "/health/{", wantErr: "invalid HTTP path configuration"},
		{name: "conflicting wildcard patterns", path: "/{a}/metrics", healthPath: "/health/{b}", wantErr: "conflicts with pattern"},
		{name: "conflicting landing pattern", path: "/{rest...}", wantErr: "conflicts with pattern"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&startParams{Path: tc.path, HealthPath: tc.healthPath}).validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestGetVersion(t *testing.T) {
	oldVersion, oldHash, oldDate := Version, VersionHash, VersionDate
	t.Cleanup(func() { Version, VersionHash, VersionDate = oldVersion, oldHash, oldDate })
	for _, tc := range []struct {
		name    string
		version string
		date    bool
		want    string
	}{
		{name: "development", want: "dev"},
		{name: "development with date", date: true, want: "dev"},
		{name: "release", version: "1.7.0", want: "v1.7.0 (abc123)"},
		{name: "release with date", version: "1.7.0", date: true, want: "v1.7.0 (abc123) 2026-10-22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Version, VersionHash, VersionDate = tc.version, "abc123", "2026-10-22"
			if got := getVersion(tc.date); got != tc.want {
				t.Fatalf("getVersion(%t) = %q, want %q", tc.date, got, tc.want)
			}
		})
	}
}

func TestExitHandlerDeduplicatesAndRecovers(t *testing.T) {
	oldLogger, oldRaw := logger, StartParams.Raw
	var output bytes.Buffer
	logger = log.New(&output, "", 0)
	StartParams.Raw = false
	t.Cleanup(func() { logger, StartParams.Raw = oldLogger, oldRaw })

	handler := &exitHandler{}
	if handler.HasError() {
		t.Fatal("new handler reports an error")
	}
	first := errors.New("scrape failed")
	if got := handler.Set(first); got != first || !handler.HasError() {
		t.Fatalf("Set() = %v, HasError() = %t", got, handler.HasError())
	}
	if err := handler.Errorf("scrape %s", "failed"); err == nil {
		t.Fatal("Errorf() did not return an error")
	}
	if got := strings.Count(output.String(), "[ERROR] scrape failed\n"); got != 1 {
		t.Fatalf("same error logged %d times, want 1: %q", got, output.String())
	}
	if err := handler.Set(nil); err != nil || handler.HasError() {
		t.Fatalf("Set(nil) = %v, HasError() = %t", err, handler.HasError())
	}
	if err := handler.Set(first); err != first {
		t.Fatalf("Set() after recovery = %v, want %v", err, first)
	}
	if got := strings.Count(output.String(), "[ERROR] scrape failed\n"); got != 2 {
		t.Fatalf("error after recovery was not logged: %q", output.String())
	}
}
