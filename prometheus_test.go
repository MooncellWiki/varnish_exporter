package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func isolateExporterErrors(t *testing.T) {
	t.Helper()
	previous := ExitHandler
	ExitHandler = &exitHandler{}
	t.Cleanup(func() { ExitHandler = previous })
}

func gatherExporter(t *testing.T, registry *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		result[family.GetName()] = family
	}
	return result
}

func TestExporterRecoversAfterVarnishBecomesAvailable(t *testing.T) {
	isolateExporterErrors(t)
	unavailable := errors.New("varnishstat unavailable")
	versionCalls, scrapeCalls := 0, 0
	exporter := newPrometheusExporter(func(version *varnishVersion, ch chan<- prometheus.Metric) ([]byte, error) {
		scrapeCalls++
		if scrapeCalls == 1 {
			if version.Valid() {
				t.Error("scrape received a version before discovery succeeded")
			}
			return nil, unavailable
		}
		if version.VersionString() != "7.5.0" {
			t.Errorf("scrape received version %q, want the discovered 7.5.0", version)
		}
		ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("varnish_main_uptime", "Uptime", nil, nil), prometheus.GaugeValue, 42)
		return nil, nil
	}, func() (*varnishVersion, error) {
		versionCalls++
		if versionCalls == 1 {
			return nil, unavailable
		}
		return &varnishVersion{Major: 7, Minor: 5, Patch: 0, Revision: "abc123"}, nil
	})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(exporter)

	first := gatherExporter(t, registry)
	if first["varnish_up"].GetMetric()[0].GetGauge().GetValue() != 0 || !ExitHandler.HasError() {
		t.Fatal("failed scrape must report varnish_up=0 and record the error")
	}
	if first["varnish_version"] != nil {
		t.Fatal("unresolved version must not be exported")
	}

	for range 2 {
		families := gatherExporter(t, registry)
		if families["varnish_up"].GetMetric()[0].GetGauge().GetValue() != 1 || ExitHandler.HasError() {
			t.Fatal("successful scrape must report varnish_up=1 and clear the error")
		}
		version := families["varnish_version"].GetMetric()
		if len(version) != 1 || version[0].GetGauge().GetValue() != 1 {
			t.Fatalf("recovered version must have value 1: %v", version)
		}
		labels := make(map[string]string)
		for _, label := range version[0].GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["version"] != "7.5.0" || labels["revision"] != "abc123" {
			t.Fatalf("unexpected version labels: %v", labels)
		}
		if families["varnish_main_uptime"].GetMetric()[0].GetGauge().GetValue() != 42 {
			t.Fatal("successful scrape must export Varnish metrics")
		}
	}
	if versionCalls != 2 {
		t.Fatalf("version queried %d times, want 2", versionCalls)
	}
}

func TestExporterInitialize(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%v", fail), func(t *testing.T) {
			wantErr := errors.New("version unavailable")
			exporter := newPrometheusExporter(func(*varnishVersion, chan<- prometheus.Metric) ([]byte, error) {
				return nil, nil
			}, func() (*varnishVersion, error) {
				if fail {
					return nil, wantErr
				}
				return &varnishVersion{Major: 6, Minor: 5, Patch: 1}, nil
			})
			err := exporter.Initialize()
			if fail {
				if !errors.Is(err, wantErr) || exporter.version != nil || exporter.VarnishVersion().Valid() {
					t.Fatalf("failed initialization: err=%v, version=%v", err, exporter.version)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := exporter.VarnishVersion().VersionString(); got != "6.5.1" {
				t.Fatalf("VarnishVersion() = %q, want 6.5.1", got)
			}
			metric := &dto.Metric{}
			if err := exporter.version.Write(metric); err != nil {
				t.Fatal(err)
			}
			if metric.GetGauge().GetValue() != 1 {
				t.Fatal("initialized version must have value 1")
			}
		})
	}
}

func TestExporterRemainsUncheckedAfterInitialization(t *testing.T) {
	exporter := newPrometheusExporter(nil, func() (*varnishVersion, error) {
		return &varnishVersion{Major: 9, Minor: 1, Patch: 0}, nil
	})
	for _, initialize := range []bool{false, true} {
		if initialize {
			if err := exporter.Initialize(); err != nil {
				t.Fatal(err)
			}
		}
		descriptors := make(chan *prometheus.Desc, 2)
		exporter.Describe(descriptors)
		if len(descriptors) != 0 {
			t.Fatal("dynamic collector must not advertise a partial descriptor set")
		}
	}
}

func TestExporterLogsVersionDiscoveryErrorsOnce(t *testing.T) {
	isolateExporterErrors(t)
	oldLogger, oldRaw := logger, StartParams.Raw
	var output bytes.Buffer
	logger = log.New(&output, "", 0)
	StartParams.Raw = false
	t.Cleanup(func() { logger, StartParams.Raw = oldLogger, oldRaw })

	versionCalls := 0
	exporter := newPrometheusExporter(func(*varnishVersion, chan<- prometheus.Metric) ([]byte, error) {
		return nil, nil
	}, func() (*varnishVersion, error) {
		versionCalls++
		return nil, errors.New("unparseable -V output")
	})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(exporter)
	for range 3 {
		gatherExporter(t, registry)
	}
	if versionCalls != 3 {
		t.Fatalf("version queried %d times, want a retry on every scrape", versionCalls)
	}
	if got := strings.Count(output.String(), "unparseable -V output"); got != 1 {
		t.Fatalf("version error logged %d times, want 1: %q", got, output.String())
	}
}

func TestExporterConcurrentGatherAndInitialize(t *testing.T) {
	isolateExporterErrors(t)
	calls := 0
	exporter := newPrometheusExporter(func(*varnishVersion, chan<- prometheus.Metric) ([]byte, error) {
		calls++ // Collect must serialize access to the scraper.
		return nil, nil
	}, func() (*varnishVersion, error) {
		return &varnishVersion{Major: 7, Minor: 5, Patch: 0}, nil
	})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(exporter)
	const count = 16
	var workers sync.WaitGroup
	errs := make(chan error, 2*count)
	for range count {
		workers.Go(func() {
			if _, err := registry.Gather(); err != nil {
				errs <- err
			}
		})
		// Initialize replaces the version state that Collect reads.
		workers.Go(func() {
			if err := exporter.Initialize(); err != nil {
				errs <- err
			}
			_ = exporter.VarnishVersion().String()
		})
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if calls != count {
		t.Fatalf("scraper called %d times, want %d", calls, count)
	}
}

func TestMetricGrouping(t *testing.T) {
	tests := []struct {
		input, identifier, name, description string
		keys, values                         []string
	}{
		{"MAIN.uptime", "", "varnish_main_uptime", "original", nil, nil},
		{"uptime", "", "varnish_main_uptime", "original", nil, nil},
		{"MAIN.fetch_failed", "", "varnish_main_fetch", "Number of fetches", []string{"type"}, []string{"failed"}},
		{"MAIN.s_fetch", "", "varnish_main_fetch_total", "Number of fetches", nil, nil},
		{"MAIN.sess_conn", "", "varnish_main_sessions", "Number of sessions", []string{"type"}, []string{"conn"}},
		{"MAIN.s_sess", "", "varnish_main_sessions_total", "Number of sessions", nil, nil},
		{"MAIN.n_wrk_create", "", "varnish_main_worker_threads", "Number of worker threads", []string{"type"}, []string{"create"}},
		{"MAIN.n_wrk", "", "varnish_main_worker_threads_total", "Number of worker threads", nil, nil},
		{"LCK.backend.creat", "backend", "varnish_lock_created", "original", []string{"target"}, []string{"backend"}},
		{"SMA.Transient.g_bytes", "Transient", "varnish_sma_g_bytes", "original", []string{"type"}, []string{"Transient"}},
		{"SMF.disk.g_space", "disk", "varnish_smf_g_space", "original", []string{"type"}, []string{"disk"}},
		{"MEMPOOL.busyobj.live", "busyobj", "varnish_mempool_live", "original", []string{"id"}, []string{"busyobj"}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			name, desc, keys, values := computePrometheusInfo(tt.input, prometheusGroup(tt.input), tt.identifier, "original")
			if name != tt.name || desc != tt.description || !slices.Equal(keys, tt.keys) || !slices.Equal(values, tt.values) {
				t.Fatalf("got (%q, %q, %v, %v), want (%q, %q, %v, %v)", name, desc, keys, values, tt.name, tt.description, tt.keys, tt.values)
			}
		})
	}
}
