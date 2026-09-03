package main

import (
	"encoding/json"
	"testing"

	"github.com/moby/buildkit/frontend/subrequests"
)

func TestParseCacheImports(t *testing.T) {
	// legacy cache-from: comma-separated registry refs
	got, err := parseCacheImports(map[string]string{"cache-from": "ghcr.io/o/c:main, ghcr.io/o/c:pr"})
	if err != nil || len(got) != 2 || got[0].Type != "registry" || got[0].Attrs["ref"] != "ghcr.io/o/c:main" {
		t.Fatalf("cache-from: %+v %v", got, err)
	}
	// cache-imports: JSON array
	got, err = parseCacheImports(map[string]string{
		"cache-imports": `[{"type":"s3","attrs":{"bucket":"b","region":"auto"}}]`,
	})
	if err != nil || len(got) != 1 || got[0].Type != "s3" || got[0].Attrs["bucket"] != "b" {
		t.Fatalf("cache-imports: %+v %v", got, err)
	}
	if got, err := parseCacheImports(map[string]string{}); err != nil || got != nil {
		t.Fatalf("empty: %+v %v", got, err)
	}
	if _, err := parseCacheImports(map[string]string{"cache-imports": "not json"}); err == nil {
		t.Error("malformed cache-imports accepted")
	}
}

func TestArchForPlatform(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"linux/amd64", "x86_64"},
		{"linux/x86_64", "x86_64"},
		{"linux/arm64", "arm64"},
		{"linux/arm64/v8", "arm64"},
		{"arm64", "arm64"},
	} {
		got, err := archForPlatform(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("archForPlatform(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"linux/amd64,linux/arm64", "windows/amd64", "linux/riscv64", "not a platform"} {
		if got, err := archForPlatform(bad); err == nil {
			t.Errorf("archForPlatform(%q) = %q, want an error", bad, got)
		}
	}
}

func TestDescribeSubrequests(t *testing.T) {
	res, err := describeSubrequests()
	if err != nil {
		t.Fatal(err)
	}
	var got []subrequests.Request
	if err := json.Unmarshal(res.Metadata["result.json"], &got); err != nil {
		t.Fatalf("result.json is not a request list: %v", err)
	}
	if len(got) != 1 || got[0].Name != subrequests.RequestSubrequestsDescribe {
		t.Errorf("describe advertised %+v", got)
	}
	if len(res.Metadata["result.txt"]) == 0 || len(res.Metadata["version"]) == 0 {
		t.Error("describe result is missing the text rendering or version")
	}
}
