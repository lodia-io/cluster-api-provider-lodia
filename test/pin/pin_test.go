// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package pin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
)

const (
	sumA = "3f4032fd612a3bd47b71e896eab4d5d28e187cec0433485b50522aef405d58dd"
	sumB = "acd502a025bd5dd6427c4a5184c36a27661f1ef18f95e6521e042aca8dea12d6"
)

func TestThePinIsTheFileThatAReaderOfAnotherCommitReads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(File)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, previous) {
		t.Errorf("%s is not the file that the package embeds", File)
	}
	release, err := Previous()
	if err != nil {
		t.Fatal(err)
	}
	if release.Version == "" || release.ComponentsURL() == release.MetadataURL() {
		t.Errorf("the pin names %+v", release)
	}
}

func TestTheNewestSeriesIsTheHighestMajorAndOfThatTheHighestMinorWhereverItStands(t *testing.T) {
	series := func(versions ...[2]int32) []clusterctlv1.ReleaseSeries {
		var out []clusterctlv1.ReleaseSeries
		for _, v := range versions {
			out = append(out, clusterctlv1.ReleaseSeries{Major: v[0], Minor: v[1]})
		}
		return out
	}
	for name, c := range map[string]struct {
		series []clusterctlv1.ReleaseSeries
		want   [2]int32
	}{
		"one":                           {series([2]int32{0, 1}), [2]int32{0, 1}},
		"the last":                      {series([2]int32{0, 1}, [2]int32{0, 2}), [2]int32{0, 2}},
		"the first":                     {series([2]int32{0, 2}, [2]int32{0, 1}), [2]int32{0, 2}},
		"a higher major, a lower minor": {series([2]int32{0, 9}, [2]int32{1, 0}), [2]int32{1, 0}},
		"a higher minor, a lower major": {series([2]int32{1, 0}, [2]int32{0, 9}), [2]int32{1, 0}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := NewestSeries(c.series); [2]int32{got.Major, got.Minor} != c.want {
				t.Errorf("the newest of %v is %d.%d, want %d.%d", c.series, got.Major, got.Minor, c.want[0], c.want[1])
			}
		})
	}
}

func TestAPinThatIsNotWrittenAsAReleaseIsRefused(t *testing.T) {
	const pin = "version: v0.1.0-alpha.2\ncomponents: " + sumA + "\nmetadata: " + sumB + "\n"
	cases := []struct {
		name, pin, want string
	}{
		{"a release", pin, ""},
		{"a stable release", strings.Replace(pin, "v0.1.0-alpha.2", "v0.1.0", 1), ""},
		{"no version", "components: " + sumA + "\nmetadata: " + sumB + "\n", "the pin's version"},
		{"a version without the v", strings.Replace(pin, "v0.1.0-alpha.2", "0.1.0-alpha.2", 1), "not written vMAJOR"},
		{"a version of two components", strings.Replace(pin, "v0.1.0-alpha.2", "v0.1", 1), "the pin's version"},
		{"a version with build metadata", strings.Replace(pin, "v0.1.0-alpha.2", "v0.1.0+build.1", 1), "not written vMAJOR"},
		{"a branch", strings.Replace(pin, "v0.1.0-alpha.2", "main", 1), "the pin's version"},
		{"no sum of the components", "version: v0.1.0\nmetadata: " + sumB + "\n", "sum of components"},
		{"no sum of the metadata", "version: v0.1.0\ncomponents: " + sumA + "\n", "sum of metadata"},
		{"a short sum", strings.Replace(pin, sumA, sumA[:63], 1), "sum of components"},
		{"an upper case sum", strings.Replace(pin, sumA, strings.ToUpper(sumA), 1), "sum of components"},
		{"a sum with another digest", strings.Replace(pin, sumA, "sha256:"+sumA, 1), "sum of components"},
		{"a field it does not know", pin + "image: x\n", "unknown field"},
		{"text that is not YAML", "a: [\n", "the pin of the previous release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.pin))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestTheNextReleaseUpgradesFromThePinUnlessItSetsUpgradeToFalse(t *testing.T) {
	const pin = "version: v0.1.0-alpha.2\ncomponents: " + sumA + "\nmetadata: " + sumB + "\n"
	cases := []struct {
		name, pin string
		want      bool
		failure   string
	}{
		{"no field", pin, true, ""},
		{"upgrade false", pin + "upgrade: false\n", false, ""},
		{"upgrade true", pin + "upgrade: true\n", true, ""},
		{"upgrade that is not a boolean", pin + "upgrade: \"no\"\n", false, "the pin of the previous release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			release, err := Parse([]byte(c.pin))
			switch {
			case c.failure != "":
				if err == nil || !strings.Contains(err.Error(), c.failure) {
					t.Fatalf("want an error with %q, got %v", c.failure, err)
				}
			case err != nil:
				t.Fatal(err)
			case release.Upgrades() != c.want:
				t.Errorf("Upgrades is %t, want %t", release.Upgrades(), c.want)
			}
		})
	}
}

func TestTheAssetsAreTheReleasesOwn(t *testing.T) {
	release := Release{Version: "v0.1.0-alpha.2"}
	for got, want := range map[string]string{
		release.ComponentsURL(): "https://github.com/lodia-io/cluster-api-provider-lodia/releases/download/v0.1.0-alpha.2/control-plane-components.yaml",
		release.MetadataURL():   "https://github.com/lodia-io/cluster-api-provider-lodia/releases/download/v0.1.0-alpha.2/metadata.yaml",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

type downloads struct {
	url  string
	hits atomic.Int32
}

const body = "kind: Namespace\n"

func serve(t *testing.T, status int) *downloads {
	t.Helper()
	d := &downloads{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		d.hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	d.url = server.URL + "/releases/download/v1/components.yaml"
	return d
}

func TestAFileIsDownloadedOnceAndReadFromTheCacheAfter(t *testing.T) {
	d := serve(t, http.StatusOK)
	dir := filepath.Join(t.TempDir(), "not", "there", "yet")
	for range 2 {
		got, err := Fetch(t.Context(), dir, d.url, checksum([]byte(body)))
		if err != nil || string(got) != body {
			t.Fatalf("got %q and %v", got, err)
		}
	}
	if hits := d.hits.Load(); hits != 1 {
		t.Errorf("the server was asked %d times, want 1", hits)
	}
	if cached, err := os.ReadFile(filepath.Join(dir, "components.yaml")); err != nil || string(cached) != body {
		t.Errorf("the cache holds %q and %v", cached, err)
	}
}

func TestACachedFileWithAnotherSumIsReplacedByTheDownload(t *testing.T) {
	d := serve(t, http.StatusOK)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "components.yaml"), []byte("kind: Other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(t.Context(), dir, d.url, checksum([]byte(body)))
	if err != nil || string(got) != body {
		t.Fatalf("got %q and %v", got, err)
	}
	if d.hits.Load() != 1 {
		t.Errorf("the server was asked %d times, want 1", d.hits.Load())
	}
	if cached, _ := os.ReadFile(filepath.Join(dir, "components.yaml")); string(cached) != body {
		t.Errorf("the cache holds %q after the download", cached)
	}
}

func TestADownloadWithAnotherSumIsRefusedAndNotCached(t *testing.T) {
	d := serve(t, http.StatusOK)
	dir := t.TempDir()
	_, err := Fetch(t.Context(), dir, d.url, sumA)
	if err == nil || !strings.Contains(err.Error(), "the pin is "+sumA) {
		t.Fatalf("want the refusal of the sum, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "components.yaml")); !os.IsNotExist(err) {
		t.Errorf("a file with another sum is cached: %v", err)
	}
}

func TestADownloadThatIsNotAnsweredWithOKIsRefused(t *testing.T) {
	d := serve(t, http.StatusNotFound)
	_, err := Fetch(t.Context(), t.TempDir(), d.url, checksum([]byte(body)))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want the refusal of the status, got %v", err)
	}
}
