package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	vbeReload = "VBE.reload_"
)

var (
	// Bounds a hung varnishstat (or docker exec), which would otherwise hold
	// the exporter lock and block every following scrape.
	varnishstatTimeout = 10 * time.Second

	DescCache = &descCache{
		descs: make(map[string]*prometheus.Desc),
	}
	versionPattern = regexp.MustCompile(`(?P<major>\d+)(\.(?P<minor>\d+))?(\.(?P<patch>\d+))?(.*revision\s(?P<revision>[0-9a-f]*)\))?`)
)

type descCache struct {
	sync.RWMutex

	descs map[string]*prometheus.Desc
}

func (dc *descCache) Desc(key string) *prometheus.Desc {
	dc.RLock()
	desc := dc.descs[key]
	dc.RUnlock()
	return desc
}

func (dc *descCache) Set(key string, desc *prometheus.Desc) *prometheus.Desc {
	dc.Lock()
	dc.descs[key] = desc
	dc.Unlock()
	return desc
}

func ScrapeVarnish(version *varnishVersion, ch chan<- prometheus.Metric) ([]byte, error) {
	params := []string{"-j"}
	if version.EqualsOrGreater(4, 1) {
		// 4.1 started to support timeout to exit immediately on connection errors.
		// Before that varnishstat exits immediately on faulty params or connection errors.
		params = append(params, "-t", "0")
	}
	if !StartParams.Params.isEmpty() {
		params = append(params, StartParams.Params.make(version)...)
	}
	stdout, stderr, errExec := executeVarnishstat(StartParams.VarnishstatExe, params...)
	if errExec != nil {
		return append(stdout, stderr...), fmt.Errorf("%s scrape failed: %s", StartParams.VarnishstatExe, errExec)
	}
	if len(stderr) > 0 && StartParams.Verbose {
		logWarn("%s stderr: %s", StartParams.VarnishstatExe, bytes.TrimSpace(stderr))
	}
	return ScrapeVarnishFrom(stdout, ch)
}

func ScrapeVarnishFrom(buf []byte, ch chan<- prometheus.Metric) ([]byte, error) {
	var metricsJSON map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.UseNumber()
	if err := dec.Decode(&metricsJSON); err != nil {
		return buf, fmt.Errorf("decode varnishstat JSON: %w", err)
	}
	if metricsJSON == nil {
		return buf, fmt.Errorf("varnishstat JSON must be an object")
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return buf, fmt.Errorf("varnishstat JSON contains multiple values")
		}
		return buf, fmt.Errorf("decode trailing varnishstat JSON: %w", err)
	}
	countersJSON := metricsJSON
	// From Varnish 6.5 https://varnish-cache.org/docs/6.5/whats-new/upgrading-6.5.html#varnishstat
	if rawVersion, present := metricsJSON["version"]; present {
		versionNumber, ok := rawVersion.(json.Number)
		if !ok {
			return buf, fmt.Errorf("unhandled JSON stats version type: %T", rawVersion)
		}
		version, err := versionNumber.Int64()
		if err != nil {
			return buf, fmt.Errorf("invalid JSON stats version: %w", err)
		}
		if version != 1 {
			return buf, fmt.Errorf("unimplemented JSON stats version %d", version)
		}
		countersJSON, ok = metricsJSON["counters"].(map[string]interface{})
		if !ok {
			return buf, fmt.Errorf("JSON stats version 1 counters must be an object")
		}
	}

	mostRecentVbeReloadPrefix := findMostRecentVbeReloadPrefix(countersJSON)

	exported, skipped := 0, 0
	for vName, raw := range countersJSON {

		if isOutdatedVbe(vName, mostRecentVbeReloadPrefix) {
			continue
		}
		if vName == "timestamp" {
			continue
		}
		data, ok := raw.(map[string]interface{})
		if !ok {
			if StartParams.Verbose {
				logWarn("Found unexpected data from JSON: %s: %#v", vName, raw)
			}
			skipped++
			continue
		}
		var (
			vGroup       = prometheusGroup(vName)
			vDescription string
			vIdentifier  string
			vValue       float64
			iValue       uint64
			vErr         error
		)
		flag, vErr := stringProperty(data, "flag")

		if value, ok := data["description"]; ok && vErr == nil {
			if vDescription, ok = value.(string); !ok {
				vErr = fmt.Errorf("%s description is not a string", vName)
			}
		}
		if value, ok := data["ident"]; ok && vErr == nil {
			if vIdentifier, ok = value.(string); !ok {
				vErr = fmt.Errorf("%s ident is not a string", vName)
			}
		}
		if vErr == nil {
			value, present := data["value"]
			if !present {
				vErr = fmt.Errorf("%s value is missing", vName)
			} else if number, ok := value.(json.Number); ok {
				if vValue, vErr = number.Float64(); vErr != nil {
					vErr = fmt.Errorf("%s value float64 error: %s", vName, vErr)
				}
				if flag == "b" {
					if iValue, vErr = strconv.ParseUint(number.String(), 10, 64); vErr != nil {
						vErr = fmt.Errorf("%s value uint64 error: %s", vName, vErr)
					}
				}
			} else {
				vErr = fmt.Errorf("%s value is not a number", vName)
			}
		}
		if vErr != nil {
			if StartParams.Verbose {
				logWarn("%s", vErr)
			}
			skipped++
			continue
		}

		pName, pDescription, pLabelKeys, pLabelValues := computePrometheusInfo(vName, vGroup, vIdentifier, vDescription)

		var metricType prometheus.ValueType
		switch flag {
		case "c", "a":
			metricType = prometheus.CounterValue
		case "g":
			metricType = prometheus.GaugeValue
		default:
			metricType = prometheus.GaugeValue
		}

		if !sendConstMetric(ch, pName, pDescription, pLabelKeys, metricType, vValue, pLabelValues) {
			skipped++
			continue
		}
		exported++

		// augment varnish_backend_up from _happy varnish bitmap value
		// we are only interested in the latest happy value (up or down) on each scrape
		// see draw_line_bitmap function from https://github.com/varnishcache/varnish-cache/blob/master/bin/varnishstat/varnishstat_curses.c
		if pName == "varnish_backend_happy" {
			upValue := 0.0
			if iValue > 0 && (iValue&uint64(1)) > 0 {
				upValue = 1.0
			}
			sendConstMetric(ch, "varnish_backend_up", "Backend up as per the latest health probe", pLabelKeys, prometheus.GaugeValue, upValue, pLabelValues)
		}
	}
	// Report a format change that breaks every counter instead of silently
	// exporting nothing with varnish_up=1.
	if exported == 0 && skipped > 0 {
		return buf, fmt.Errorf("no usable counters in varnishstat JSON, skipped %d malformed counters", skipped)
	}
	return buf, nil
}

// Sends a constant metric, returning false if Prometheus rejects it.
func sendConstMetric(ch chan<- prometheus.Metric, name, help string, labelKeys []string, valueType prometheus.ValueType, value float64, labelValues []string) bool {
	descKey := name + "_" + strings.Join(labelKeys, "_")
	desc := DescCache.Desc(descKey)
	if desc == nil {
		desc = DescCache.Set(descKey, prometheus.NewDesc(name, help, labelKeys, nil))
	}
	metric, err := prometheus.NewConstMetric(desc, valueType, value, labelValues...)
	if err != nil {
		if StartParams.Verbose {
			logWarn("Failed to create metric %s: %s", name, err)
		}
		return false
	}
	ch <- metric
	return true
}

// Returns the stdout and stderr of 'varnishstat' with optional command line params.
// They are kept apart so that warnings on stderr cannot corrupt the JSON on stdout.
func executeVarnishstat(varnishstatExe string, params ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), varnishstatTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if len(StartParams.VarnishDockerContainer) == 0 {
		cmd = exec.CommandContext(ctx, varnishstatExe, params...)
	} else {
		// No -t: a TTY would merge stderr into stdout.
		cmd = exec.CommandContext(ctx, "docker", append([]string{"exec", StartParams.VarnishDockerContainer, varnishstatExe}, params...)...)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	// Don't wait for output pipes held open by children of a killed command.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s: %w", varnishstatTimeout, err)
	}
	return outBuf.Bytes(), errBuf.Bytes(), err
}

// Returns the most recent prefix for 'VBE.reload_' stats. Empty until first reload.
// 'VBE.reload_2019-08-29T100458' as by varnish_reload_vcl in 4.1+
// 'VBE.reload_20191014_091124_78599' as by varnishreload in 6+
func findMostRecentVbeReloadPrefix(countersJSON map[string]interface{}) string {
	var mostRecentVbeReloadPrefix string
	for vName := range countersJSON {
		// Checking only the required ".happy" stat
		if strings.HasPrefix(vName, vbeReload) && strings.HasSuffix(vName, ".happy") {
			dotAfterPrefixIndex := len(vbeReload) + strings.Index(vName[len(vbeReload):], ".")
			vbeReloadPrefix := vName[:dotAfterPrefixIndex]
			if strings.Compare(vbeReloadPrefix, mostRecentVbeReloadPrefix) > 0 {
				mostRecentVbeReloadPrefix = vbeReloadPrefix
			}
		}
	}
	return mostRecentVbeReloadPrefix
}

// Returns true if the given 'VBE.' statistic refers to an outdated VCL version
func isOutdatedVbe(vName string, mostRecentVbeReloadPrefix string) bool {
	return len(mostRecentVbeReloadPrefix) > 0 &&
		strings.HasPrefix(vName, "VBE.") &&
		!strings.HasPrefix(vName, mostRecentVbeReloadPrefix+".")
}

// varnishVersion

type varnishVersion struct {
	Major    int
	Minor    int
	Patch    int
	Revision string
}

func NewVarnishVersion() *varnishVersion {
	return &varnishVersion{
		Major: -1, Minor: -1, Patch: -1,
	}
}

func (v *varnishVersion) EqualsOrGreater(major, minor int) bool {
	if v.Major > major {
		return true
	} else if v.Major == major && v.Minor >= minor {
		return true
	}
	return false
}

func (v *varnishVersion) Valid() bool {
	return v.Major != -1
}

func (v *varnishVersion) Initialize() error {
	return v.queryVersion()
}

// Queries a fresh varnishVersion from varnishstat.
func queryVarnishVersion() (*varnishVersion, error) {
	version := NewVarnishVersion()
	if err := version.Initialize(); err != nil {
		return nil, err
	}
	return version, nil
}

func (v *varnishVersion) queryVersion() error {
	stdout, stderr, err := executeVarnishstat(StartParams.VarnishstatExe, "-V")
	if err != nil {
		return err
	}
	// varnishstat -V prints to stderr, stdout is checked first for wrappers.
	scanner := bufio.NewScanner(io.MultiReader(bytes.NewReader(stdout), bytes.NewReader(stderr)))
	if scanner.Scan() {
		return v.parseVersion(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read varnishstat -V output: %w", err)
	}
	return fmt.Errorf("failed to get varnishstat -V output")
}

func (v *varnishVersion) parseVersion(version string) error {
	match := versionPattern.FindStringSubmatch(version)
	if match == nil {
		return fmt.Errorf("failed to resolve version from %q", version)
	}

	parts := make(map[string]string)
	for i, name := range versionPattern.SubexpNames() {
		if i != 0 && name != "" {
			parts[name] = match[i]
		}
	}

	parsed := NewVarnishVersion()
	if err := parsed.set(parts); err != nil {
		return fmt.Errorf("failed to resolve version from %q: %w", version, err)
	}
	*v = *parsed
	return nil
}

func (v *varnishVersion) Labels() map[string]string {
	labels := make(map[string]string)
	if v.Major != -1 {
		labels["major"] = strconv.Itoa(v.Major)
	}
	if v.Minor != -1 {
		labels["minor"] = strconv.Itoa(v.Minor)
	}
	if v.Patch != -1 {
		labels["patch"] = strconv.Itoa(v.Patch)
	}
	if v.Revision != "" {
		labels["revision"] = v.Revision
	}
	labels["version"] = v.VersionString()
	return labels
}

func (v *varnishVersion) set(parts map[string]string) error {
	for name, value := range parts {
		// skip empty value
		if len(value) == 0 {
			continue
		}

		// save revision as-is (string)
		if name == "revision" {
			v.Revision = value
			continue
		}

		// convert semver parts to integer and save it
		num, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		switch name {
		case "major":
			v.Major = num
		case "minor":
			v.Minor = num
		case "patch":
			v.Patch = num
		}
	}
	return nil
}

// Version string with numbers only, no revision.
func (v *varnishVersion) VersionString() string {
	parts := []string{}
	for _, num := range []int{v.Major, v.Minor, v.Patch} {
		if num != -1 {
			parts = append(parts, strconv.Itoa(num))
		}
	}
	return strings.Join(parts, ".")
}

// Full version string, including revision.
func (v *varnishVersion) String() string {
	version := v.VersionString()
	if v.Revision != "" {
		version += " " + v.Revision
	}
	return version
}
