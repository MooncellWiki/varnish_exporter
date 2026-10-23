package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	ApplicationName = "prometheus_varnish_exporter"
	Version         string
	VersionHash     string
	VersionDate     string

	PrometheusExporter = NewPrometheusExporter()
	ExitHandler        = &exitHandler{}

	StartParams = &startParams{
		ListenAddress:  ":9131", // Reserved and publicly announced at https://github.com/prometheus/prometheus/wiki/Default-port-allocations
		Path:           "/metrics",
		VarnishstatExe: "varnishstat",
		Params:         &varnishstatParams{},
	}
	logger = log.New(os.Stdout, "", log.Ldate|log.Ltime)
)

type startParams struct {
	ListenAddress          string
	Path                   string
	HealthPath             string
	VarnishstatExe         string
	VarnishDockerContainer string
	Params                 *varnishstatParams

	Verbose       bool
	ExitOnErrors  bool
	Test          bool
	Raw           bool
	WithGoMetrics bool

	noExit bool // deprecated
}

type varnishstatParams struct {
	Instance string
	VSM      string
}

func (p *varnishstatParams) isEmpty() bool {
	return p.Instance == "" && p.VSM == ""
}

func (p *varnishstatParams) make(version *varnishVersion) (params []string) {
	// -n
	if p.Instance != "" {
		params = append(params, "-n", p.Instance)
	}
	// -N is not supported by 3.x
	if p.VSM != "" && version.EqualsOrGreater(4, 0) {
		params = append(params, "-N", p.VSM)
	}
	return params
}

func registerFlags(flags *flag.FlagSet, params *startParams) *bool {
	// prometheus conventions
	flags.StringVar(&params.ListenAddress, "web.listen-address", params.ListenAddress, "Address on which to expose metrics and web interface.")
	flags.StringVar(&params.Path, "web.telemetry-path", params.Path, "Path under which to expose metrics.")
	flags.StringVar(&params.HealthPath, "web.health-path", params.HealthPath, "Path under which to expose healthcheck. Disabled unless configured.")

	// varnish
	flags.StringVar(&params.VarnishstatExe, "varnishstat-path", params.VarnishstatExe, "Path to varnishstat.")
	flags.StringVar(&params.Params.Instance, "n", params.Params.Instance, "varnishstat -n value.")
	flags.StringVar(&params.Params.VSM, "N", params.Params.VSM, "varnishstat -N value.")

	// docker
	flags.StringVar(&params.VarnishDockerContainer, "docker-container-name", params.VarnishDockerContainer, "Docker container name to exec varnishstat in.")

	// modes
	version := false
	flags.BoolVar(&version, "version", version, "Print version and exit")
	flags.BoolVar(&params.ExitOnErrors, "exit-on-errors", params.ExitOnErrors, "Exit process on scrape errors.")
	flags.BoolVar(&params.Verbose, "verbose", params.Verbose, "Verbose logging.")
	flags.BoolVar(&params.Test, "test", params.Test, "Test varnishstat availability, prints available metrics and exits.")
	flags.BoolVar(&params.Raw, "raw", params.Raw, "Raw stdout logging without timestamps.")
	flags.BoolVar(&params.WithGoMetrics, "with-go-metrics", params.WithGoMetrics, "Export go runtime and http handler metrics")

	// deprecated
	flags.BoolVar(&params.noExit, "no-exit", params.noExit, "Deprecated: see -exit-on-errors")

	return &version
}

func main() {
	version := registerFlags(flag.CommandLine, StartParams)

	flag.Parse()

	if *version {
		fmt.Printf("%s %s\n", ApplicationName, getVersion(true))
		os.Exit(0)
	}

	// Building the handler validates the HTTP paths before querying Varnish.
	handler, err := newHTTPHandler(StartParams, PrometheusExporter)
	if err != nil {
		logFatal("HTTP handler initialize failed: %s", err)
	}

	// Don't log warning on !noExit as that would spam for the formed default value.
	if StartParams.noExit {
		logWarn("-no-exit is deprecated. As of v1.5 it is the default behavior not to exit process on scrape errors. You can remove this parameter.")
	}

	// Test run or user explicitly wants to exit on any scrape errors during runtime.
	ExitHandler.exitOnError = StartParams.Test || StartParams.ExitOnErrors

	if b, err := json.MarshalIndent(StartParams, "", "  "); err == nil {
		logInfo("%s %s %s", ApplicationName, getVersion(false), b)
	} else {
		logFatal("%s", err)
	}

	// Initialize
	if err := PrometheusExporter.Initialize(); err != nil {
		// nolint: errcheck
		ExitHandler.Errorf("Varnish version initialize failed: %s", err.Error())
	} else {
		logInfo("Found varnishstat %s", PrometheusExporter.VarnishVersion())
	}

	// Test to verify everything is ok before starting the server
	{
		done := make(chan bool)
		metrics := make(chan prometheus.Metric)
		go func() {
			for m := range metrics {
				if StartParams.Test {
					logInfo("%s", m.Desc())
				}
			}
			done <- true
		}()
		tStart := time.Now()
		buf, err := ScrapeVarnish(PrometheusExporter.VarnishVersion(), metrics)
		close(metrics)
		<-done

		if err == nil {
			logInfo("Test scrape done in %s", time.Since(tStart))
			logRaw("")
		} else {
			if len(buf) > 0 {
				logRaw("\n%s", buf)
			}
			// nolint: errcheck
			ExitHandler.Errorf("Startup test: %s", err.Error())
		}
	}
	if StartParams.Test {
		return
	}

	// Start serving
	logInfo("Server starting on %s with metrics path %s", StartParams.ListenAddress, StartParams.Path)

	server := &http.Server{
		Addr:              StartParams.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logFatalError(server.ListenAndServe())
}

func (p *startParams) validate() error {
	if len(p.Path) == 0 || p.Path[0] != '/' {
		return fmt.Errorf("-web.telemetry-path cannot be empty and must start with a slash '/', given %q", p.Path)
	}
	if len(p.HealthPath) != 0 && p.HealthPath[0] != '/' {
		return fmt.Errorf("-web.health-path must start with a slash '/' if configured, given %q", p.HealthPath)
	}
	// ServeMux wildcards would match many URLs and break the landing page link.
	if strings.ContainsAny(p.Path, "{}") {
		return fmt.Errorf("-web.telemetry-path must be a literal path without wildcards, given %q", p.Path)
	}
	if strings.ContainsAny(p.HealthPath, "{}") {
		return fmt.Errorf("-web.health-path must be a literal path without wildcards, given %q", p.HealthPath)
	}
	if p.Path == p.HealthPath {
		return fmt.Errorf("-web.telemetry-path and -web.health-path cannot have same value")
	}
	return nil
}

// newHTTPHandler keeps metrics and routes local to this exporter instance.
func newHTTPHandler(params *startParams, collector prometheus.Collector) (handler http.Handler, err error) {
	if err := params.validate(); err != nil {
		return nil, err
	}
	// ServeMux panics on malformed patterns, report them as configuration errors.
	defer func() {
		if reason := recover(); reason != nil {
			handler, err = nil, fmt.Errorf("invalid HTTP path configuration: %v", reason)
		}
	}()

	registry := prometheus.NewRegistry()
	if err := registry.Register(collector); err != nil {
		return nil, fmt.Errorf("register exporter: %w", err)
	}
	if params.WithGoMetrics {
		if err := registry.Register(collectors.NewGoCollector()); err != nil {
			return nil, fmt.Errorf("register Go metrics: %w", err)
		}
		if err := registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
			return nil, fmt.Errorf("register process metrics: %w", err)
		}
	}

	metricsHandler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog: logger,
		// Serve the valid metrics when some are inconsistent rather than a 500
		// that also hides varnish_up.
		ErrorHandling: promhttp.ContinueOnError,
	})
	if params.WithGoMetrics {
		metricsHandler = promhttp.InstrumentMetricHandler(registry, metricsHandler)
	}

	mux := http.NewServeMux()
	mux.Handle(params.Path, metricsHandler)
	if params.Path != "/" && params.HealthPath != "/" {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// nolint: errcheck
			fmt.Fprint(w, `<html>
    <head><title>Varnish Exporter</title></head>
    <body>
        <h1>Varnish Exporter</h1>
        <p><a href="`+html.EscapeString(params.Path)+`">Metrics</a></p>
    </body>
</html>`)
		})
	}
	if params.HealthPath != "" {
		mux.HandleFunc(params.HealthPath, func(w http.ResponseWriter, r *http.Request) {
			// This endpoint checks whether the exporter accepts connections.
			w.WriteHeader(http.StatusOK)
			// nolint: errcheck
			fmt.Fprintln(w, "Ok")
		})
	}
	return mux, nil
}

type exitHandler struct {
	sync.RWMutex
	exitOnError bool
	err         error
}

func (ex *exitHandler) Errorf(format string, a ...interface{}) error {
	return ex.Set(fmt.Errorf(format, a...))
}

func (ex *exitHandler) HasError() bool {
	ex.RLock()
	hasError := ex.err != nil
	ex.RUnlock()
	return hasError
}

func (ex *exitHandler) Set(err error) error {
	ex.Lock()
	defer ex.Unlock()

	if err == nil {
		ex.err = nil
		return nil
	}

	errDiffers := ex.err == nil || ex.err.Error() != err.Error()
	ex.err = err

	if ex.exitOnError {
		logFatal("%s", err.Error())
	} else if errDiffers {
		logError("%s", err.Error())
	}
	return err
}

func getVersion(date bool) (version string) {
	if Version == "" {
		return "dev"
	}
	version = fmt.Sprintf("v%s (%s)", Version, VersionHash)
	if date {
		version += " " + VersionDate
	}
	return version
}
