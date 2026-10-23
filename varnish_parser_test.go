package main

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestScrapeVarnishRejectsInvalidEnvelopes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", "decode varnishstat JSON"},
		{"truncated", `{"version":1`, "decode varnishstat JSON"},
		{"null", "null", "must be an object"},
		{"array", "[]", "decode varnishstat JSON"},
		{"number", "1", "decode varnishstat JSON"},
		{"string", `"stats"`, "decode varnishstat JSON"},
		{"version string", `{"version":"1","counters":{}}`, "version type"},
		{"version null", `{"version":null,"counters":{}}`, "version type"},
		{"version fractional", `{"version":1.5,"counters":{}}`, "invalid JSON stats version"},
		{"version overflow", `{"version":18446744073709551616,"counters":{}}`, "invalid JSON stats version"},
		{"unsupported version", `{"version":2,"counters":{}}`, "unimplemented JSON stats version 2"},
		{"missing counters", `{"version":1}`, "counters must be an object"},
		{"null counters", `{"version":1,"counters":null}`, "counters must be an object"},
		{"array counters", `{"version":1,"counters":[]}`, "counters must be an object"},
		{"string counters", `{"version":1,"counters":"invalid"}`, "counters must be an object"},
		{"trailing value", `{} {}`, "multiple values"},
		{"trailing garbage", `{} invalid`, "decode trailing varnishstat JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(test.input)
			metrics := make(chan prometheus.Metric, 1)
			output, err := ScrapeVarnishFrom(input, metrics)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
			if !bytes.Equal(output, input) {
				t.Fatal("original varnishstat output was not preserved for diagnostics")
			}
			if len(metrics) != 0 {
				t.Fatal("invalid envelope emitted metrics")
			}
		})
	}
}

func TestScrapeVarnishSupportsJSONFormats(t *testing.T) {
	for _, input := range []string{
		`{"timestamp":"now","MAIN.format_test":{"value":42,"flag":"c"}}`,
		"{\"version\":1,\"counters\":{\"MAIN.format_test\":{\"value\":42,\"flag\":\"c\"}}} \n\t",
	} {
		metrics, err := scrapeMetricsForTest([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if len(metrics) != 1 {
			t.Fatalf("got %d metrics, want 1", len(metrics))
		}
		metric := &dto.Metric{}
		if err := metrics[0].Write(metric); err != nil {
			t.Fatal(err)
		}
		if metric.Counter == nil || metric.GetCounter().GetValue() != 42 {
			t.Errorf("metric = %s, want counter with value 42", metric)
		}
	}
}

func TestScrapeVarnishSkipsMalformedCounters(t *testing.T) {
	previousParams, previousLogger := *StartParams, logger
	var logs bytes.Buffer
	StartParams.Verbose, StartParams.Raw = true, false
	logger = log.New(&logs, "", 0)
	t.Cleanup(func() {
		*StartParams, logger = previousParams, previousLogger
	})

	tests := []struct {
		name string
		raw  string
	}{
		{"null", "null"},
		{"scalar", "7"},
		{"array", "[]"},
		{"missing value", `{}`},
		{"null value", `{"value":null}`},
		{"string value", `{"value":"10"}`},
		{"object value", `{"value":{}}`},
		{"overflow value", `{"value":1e400}`},
		{"invalid description", `{"value":1,"description":true}`},
		{"invalid ident", `{"value":1,"ident":3}`},
		{"invalid flag", `{"value":1,"flag":true}`},
		{"negative bitmap", `{"value":-1,"flag":"b"}`},
		{"fractional bitmap", `{"value":1.5,"flag":"b"}`},
		{"overflow bitmap", `{"value":18446744073709551616,"flag":"b"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs.Reset()
			input := fmt.Sprintf(`{"MAIN.malformed":%s,"MAIN.healthy":{"flag":"g","value":7}}`, test.raw)
			metrics, err := scrapeMetricsForTest([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if len(metrics) != 1 || !strings.Contains(metrics[0].Desc().String(), "varnish_main_healthy") {
				t.Fatalf("got %d metrics, want only the healthy metric", len(metrics))
			}
			if logs.Len() == 0 {
				t.Error("malformed counter was not logged in verbose mode")
			}
		})
	}
}

func TestScrapeVarnishFailsWhenEveryCounterIsMalformed(t *testing.T) {
	// A format change that breaks every counter must not look like a healthy, empty scrape.
	input := []byte(`{"version":1,"counters":{"MAIN.uptime":{"flag":"c","val":42},"MAIN.sess_conn":{"flag":"c","val":7}}}`)
	metrics, err := scrapeMetricsForTest(input)
	if err == nil || !strings.Contains(err.Error(), "skipped 2 malformed counters") {
		t.Fatalf("error = %v, want skipped counters to be reported", err)
	}
	if len(metrics) != 0 {
		t.Fatalf("got %d metrics, want none", len(metrics))
	}

	// No counters at all is not a parse failure.
	if _, err := scrapeMetricsForTest([]byte(`{"version":1,"counters":{}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestScrapeVarnishSkipsInvalidDescriptor(t *testing.T) {
	// A bad descriptor must not take down a scrape that contains healthy counters.
	key := "varnish_main_invalid_descriptor_"
	DescCache.Set(key, prometheus.NewDesc("varnish_main_invalid_descriptor", "Invalid descriptor", []string{"duplicate", "duplicate"}, nil))
	t.Cleanup(func() {
		DescCache.Lock()
		delete(DescCache.descs, key)
		DescCache.Unlock()
	})
	metrics, err := scrapeMetricsForTest([]byte(`{"MAIN.invalid_descriptor":{"value":1},"MAIN.healthy":{"value":7}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || !strings.Contains(metrics[0].Desc().String(), "varnish_main_healthy") {
		t.Fatalf("got %d metrics, want only the healthy metric", len(metrics))
	}
}

func TestScrapeVarnishMetricTypes(t *testing.T) {
	for _, flag := range []string{"a", "c", "g", "b", "", "unknown"} {
		t.Run(flag, func(t *testing.T) {
			input := fmt.Sprintf(`{"MAIN.type_test":{"flag":%q,"value":5}}`, flag)
			metrics, err := scrapeMetricsForTest([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			if len(metrics) != 1 {
				t.Fatalf("got %d metrics, want 1", len(metrics))
			}
			metric := &dto.Metric{}
			if err := metrics[0].Write(metric); err != nil {
				t.Fatal(err)
			}
			if flag == "a" || flag == "c" {
				if metric.Counter == nil || metric.GetCounter().GetValue() != 5 {
					t.Errorf("metric = %s, want counter with value 5", metric)
				}
			} else if metric.Gauge == nil || metric.GetGauge().GetValue() != 5 {
				t.Errorf("metric = %s, want gauge with value 5", metric)
			}
		})
	}
}

func TestBackendHealthUsesLatestBitmapBit(t *testing.T) {
	for _, test := range []struct {
		bitmap string
		want   float64
	}{
		{"0", 0},
		{"1", 1},
		{"2", 0},
		{"3", 1},
		{"18446744073709551614", 0},
		{"18446744073709551615", 1},
	} {
		t.Run(test.bitmap, func(t *testing.T) {
			input := fmt.Sprintf(`{"VBE.boot.backend_health.happy":{"value":%s,"flag":"b"}}`, test.bitmap)
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(&fixtureCollector{data: []byte(input)})
			families, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			if len(families) != 2 {
				t.Fatalf("got %d families, want happy and up", len(families))
			}
			for _, family := range families {
				if family.GetName() == "varnish_backend_up" && family.Metric[0].GetGauge().GetValue() != test.want {
					t.Errorf("backend up = %v, want %v", family.Metric[0].GetGauge().GetValue(), test.want)
				}
			}
		})
	}
}

func TestBackendReloadPrefixRequiresBoundary(t *testing.T) {
	prefix := "VBE.reload_20200101"
	for _, test := range []struct {
		name string
		want bool
	}{
		{prefix + ".default.happy", false},
		{prefix + "0.default.happy", true},
		{prefix, true},
		{"VBE.boot.default.happy", true},
		{"MAIN.uptime", false},
	} {
		if got := isOutdatedVbe(test.name, prefix); got != test.want {
			t.Errorf("isOutdatedVbe(%q, %q) = %v, want %v", test.name, prefix, got, test.want)
		}
	}
}

func TestVersionParsingErrorsPreserveState(t *testing.T) {
	for _, input := range []string{"", "varnishstat", "not a version", strings.Repeat("9", 100)} {
		t.Run(input, func(t *testing.T) {
			version := varnishVersion{Major: 6, Minor: 5, Patch: 1, Revision: "abcdef"}
			before := version
			if err := version.parseVersion(input); err == nil {
				t.Fatalf("parseVersion(%q) succeeded", input)
			}
			if version != before {
				t.Errorf("failed parse mutated version: got %v, want %v", version, before)
			}
		})
	}
}

func TestVersionParsingResetsMissingParts(t *testing.T) {
	version := varnishVersion{Major: 6, Minor: 5, Patch: 1, Revision: "abcdef"}
	if err := version.parseVersion("varnish 7"); err != nil {
		t.Fatal(err)
	}
	if want := (varnishVersion{Major: 7, Minor: -1, Patch: -1}); version != want {
		t.Errorf("got %+v, want %+v", version, want)
	}
}

func TestVersionLabelsAndComparison(t *testing.T) {
	unknown := NewVarnishVersion()
	if unknown.Valid() || unknown.EqualsOrGreater(0, 0) || len(unknown.Labels()) != 1 {
		t.Errorf("unknown version has unexpected state: %+v", unknown)
	}
	version := varnishVersion{Major: 7, Minor: 6, Patch: 2, Revision: "abcdef"}
	labels := version.Labels()
	for key, want := range map[string]string{"version": "7.6.2", "major": "7", "minor": "6", "patch": "2", "revision": "abcdef"} {
		if labels[key] != want {
			t.Errorf("label %q = %q, want %q", key, labels[key], want)
		}
	}
	if version.String() != "7.6.2 abcdef" || !version.Valid() || !version.EqualsOrGreater(7, 6) || !version.EqualsOrGreater(6, 100) || version.EqualsOrGreater(8, 0) {
		t.Errorf("unexpected version string or comparison: %+v", version)
	}
}

func FuzzScrapeVarnishFrom(f *testing.F) {
	for _, input := range []string{
		`null`,
		`{"version":1}`,
		`{"version":1,"counters":{}}`,
		`{"MAIN.uptime":{"value":42,"flag":"c"}}`,
		`{"MAIN.invalid":null,"MAIN.valid":{"value":1}}`,
		`{"VBE.reload_20200101.default.happy":{"value":18446744073709551615,"flag":"b"}}`,
		`{} {}`,
	} {
		f.Add([]byte(input))
	}
	for _, version := range testFileVersions {
		f.Add(readScrapeFixture(f, version))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		// Each fuzz input can invent metric names; keep cache growth bounded.
		DescCache.Lock()
		DescCache.descs = make(map[string]*prometheus.Desc)
		DescCache.Unlock()
		metrics, err := scrapeMetricsForTest(input)
		if err != nil {
			if len(metrics) != 0 {
				t.Fatal("invalid envelope emitted metrics")
			}
			return
		}
		for _, metric := range metrics {
			value := &dto.Metric{}
			if err := metric.Write(value); err != nil {
				t.Fatal(err)
			}
			for _, number := range []float64{value.GetCounter().GetValue(), value.GetGauge().GetValue()} {
				if math.IsInf(number, 0) || math.IsNaN(number) {
					t.Errorf("non-finite metric value %v", number)
				}
			}
		}
	})
}

func FuzzVarnishVersion(f *testing.F) {
	for _, input := range []string{"", "not a version", "varnish 1", "7.6.2", "varnishstat (varnish-6.5.1 revision abcdef)", strings.Repeat("9", 100)} {
		f.Add(input)
	}
	f.Fuzz(func(t *testing.T, input string) {
		version := NewVarnishVersion()
		if err := version.parseVersion(input); err != nil {
			if version.Valid() {
				t.Fatal("failed parse left a partial version")
			}
			return
		}
		if !version.Valid() || version.Labels()["version"] != version.VersionString() {
			t.Fatalf("inconsistent version: %+v", version)
		}
		var roundTrip varnishVersion
		if err := roundTrip.parseVersion(version.VersionString()); err != nil {
			t.Fatal(err)
		}
		if roundTrip.VersionString() != version.VersionString() {
			t.Errorf("version did not survive round trip: %q => %q", version.VersionString(), roundTrip.VersionString())
		}
	})
}
