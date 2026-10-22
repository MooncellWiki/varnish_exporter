package main

import (
	"slices"
	"testing"
)

func TestVarnishstatParams(t *testing.T) {
	previousVersion := *VarnishVersion
	t.Cleanup(func() { *VarnishVersion = previousVersion })
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
			*VarnishVersion = varnishVersion{Major: test.major, Minor: 0, Patch: 0}
			if got := test.params.isEmpty(); got != test.empty {
				t.Errorf("isEmpty() = %v, want %v", got, test.empty)
			}
			if got := test.params.make(); !slices.Equal(got, test.want) {
				t.Errorf("make() = %v, want %v", got, test.want)
			}
		})
	}
}
