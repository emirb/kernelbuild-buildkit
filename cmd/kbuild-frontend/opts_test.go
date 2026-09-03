package main

import (
	"testing"

	kbuild "github.com/emirb/kernelbuild-buildkit"
)

// TestApplyOptsProxies: docker build forwards proxy build-args under both
// spellings; all three (http, https, no_proxy) must reach the Spec: apt's
// http:// mirrors need http_proxy behind a plain corporate proxy, and
// no_proxy is how an internal mirror is exempted.
func TestApplyOptsProxies(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]string
	}{
		{"upper", map[string]string{
			"build-arg:HTTP_PROXY": "http://p:1", "build-arg:HTTPS_PROXY": "http://p:2", "build-arg:NO_PROXY": "mirror.corp",
		}},
		{"lower", map[string]string{
			"build-arg:http_proxy": "http://p:1", "build-arg:https_proxy": "http://p:2", "build-arg:no_proxy": "mirror.corp",
		}},
	} {
		spec := kbuild.DefaultSpec()
		if err := applyOpts(tc.opts, &spec); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if spec.HTTPProxy != "http://p:1" || spec.HTTPSProxy != "http://p:2" || spec.NoProxy != "mirror.corp" {
			t.Errorf("%s: proxies = %q %q %q", tc.name, spec.HTTPProxy, spec.HTTPSProxy, spec.NoProxy)
		}
	}
	// The mapper still owns the platform error path.
	spec := kbuild.DefaultSpec()
	if err := applyOpts(map[string]string{"platform": "linux/amd64,linux/arm64"}, &spec); err == nil {
		t.Error("multi-platform accepted by applyOpts")
	}
}
