package main

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestHTTPHandlerRoutes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		healthPath string
	}{
		{name: "default", path: "/metrics"},
		{name: "custom", path: "/stats", healthPath: "/ready"},
		{name: "metrics at root", path: "/", healthPath: "/ready"},
		{name: "health at root", path: "/metrics", healthPath: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metric := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_exporter_value", Help: "Test value."})
			metric.Set(42)
			handler, err := newHTTPHandler(&startParams{Path: tc.path, HealthPath: tc.healthPath}, metric)
			if err != nil {
				t.Fatal(err)
			}

			metrics := httptest.NewRecorder()
			handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), "test_exporter_value 42\n") {
				t.Fatalf("metrics response = %d %q", metrics.Code, metrics.Body.String())
			}
			if strings.Contains(metrics.Body.String(), "go_goroutines") || strings.Contains(metrics.Body.String(), "promhttp_metric_handler_") {
				t.Fatal("runtime/handler metrics exposed without -with-go-metrics")
			}

			if tc.healthPath != "" {
				health := httptest.NewRecorder()
				handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, tc.healthPath, nil))
				if health.Code != http.StatusOK || health.Body.String() != "Ok\n" {
					t.Fatalf("health response = %d %q", health.Code, health.Body.String())
				}
			}
			if tc.path != "/" && tc.healthPath != "/" {
				home := httptest.NewRecorder()
				handler.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "/", nil))
				if home.Code != http.StatusOK || !strings.Contains(home.Body.String(), `href="`+tc.path+`"`) {
					t.Fatalf("home response = %d %q", home.Code, home.Body.String())
				}
				if got := home.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
					t.Fatalf("home Content-Type = %q", got)
				}
			}
		})
	}
}

func TestHTTPHandlerWithGoMetrics(t *testing.T) {
	metric := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_exporter_value", Help: "Test value."})
	handler, err := newHTTPHandler(&startParams{Path: "/metrics", WithGoMetrics: true}, metric)
	if err != nil {
		t.Fatal(err)
	}
	// The second scrape observes the request count recorded for the first.
	for scrape := range 2 {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("metrics response = %d %q", response.Code, response.Body.String())
		}
		for _, name := range []string{"test_exporter_value", "go_goroutines", "promhttp_metric_handler_requests_in_flight", "promhttp_metric_handler_requests_total"} {
			if !strings.Contains(response.Body.String(), name) {
				t.Errorf("missing %s", name)
			}
		}
		if runtime.GOOS == "linux" && !strings.Contains(response.Body.String(), "process_cpu_seconds_total") {
			t.Error("missing process metrics")
		}
		if scrape == 1 && !strings.Contains(response.Body.String(), `promhttp_metric_handler_requests_total{code="200"} 1`) {
			t.Error("handler instrumentation did not count the first scrape")
		}
	}
}

func TestHTTPHandlerPatternedRoutes(t *testing.T) {
	metric := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_exporter_value", Help: "Test value."})
	metric.Set(42)
	handler, err := newHTTPHandler(&startParams{Path: "/metrics/{instance}", HealthPath: "/health/{$}"}, metric)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/metrics/default": "test_exporter_value 42\n",
		"/health/":         "Ok\n",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Errorf("GET %s = %d %q, want status 200 and %q", path, response.Code, response.Body.String(), want)
		}
	}
}

func TestHTTPHandlersHaveIndependentRegistries(t *testing.T) {
	for _, value := range []float64{1, 2} {
		metric := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_independent_value", Help: "Test value."})
		metric.Set(value)
		handler, err := newHTTPHandler(&startParams{Path: "/independent", WithGoMetrics: true}, metric)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/independent", nil))
		want := "test_independent_value " + strconv.FormatFloat(value, 'g', -1, 64) + "\n"
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("independent handler response = %d %q, want %q", response.Code, response.Body.String(), want)
		}
	}
	if _, pattern := http.DefaultServeMux.Handler(httptest.NewRequest(http.MethodGet, "/independent", nil)); pattern != "" {
		t.Fatalf("handler registered with the default mux: %q", pattern)
	}
}

func TestHTTPHandlerRejectsInvalidConfiguration(t *testing.T) {
	metric := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_exporter_value", Help: "Test value."})
	for _, params := range []*startParams{
		{},
		{Path: "metrics"},
		{Path: "/metrics", HealthPath: "health"},
		{Path: "/metrics", HealthPath: "/metrics"},
		{Path: "/metrics/{"},
		{Path: "/metrics", HealthPath: "/health/{"},
		{Path: "/{a}/metrics", HealthPath: "/health/{b}"},
		{Path: "/{rest...}"},
	} {
		if handler, err := newHTTPHandler(params, metric); err == nil || handler != nil {
			t.Fatalf("newHTTPHandler(%+v) = %v, %v; want nil handler and an error", params, handler, err)
		}
	}
}
