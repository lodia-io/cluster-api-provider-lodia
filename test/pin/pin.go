// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Package pin names the release of the provider that the upgrade starts from,
// by the sha256 of the two files of it that clusterctl reads, and downloads a
// file by the sha256 it must have.
package pin

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/yaml"
)

// File is the pin's path in the repository, for a reader of another commit's.
const File = "test/pin/previous-release.yaml"

const (
	repository       = "https://github.com/lodia-io/cluster-api-provider-lodia"
	componentsAsset  = "control-plane-components.yaml"
	metadataAsset    = "metadata.yaml"
	downloadDeadline = time.Minute
)

//go:embed previous-release.yaml
var previous []byte

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

type Release struct {
	Version    string `json:"version"`
	Components string `json:"components"`
	Metadata   string `json:"metadata"`
}

func Previous() (Release, error) {
	return Parse(previous)
}

// Parse reads a pin, and refuses a field it does not know, a version that is
// not written vMAJOR.MINOR.PATCH[-PRERELEASE] and a sum that is not a lower
// case sha256.
func Parse(raw []byte) (Release, error) {
	var r Release
	if err := yaml.UnmarshalStrict(raw, &r); err != nil {
		return Release{}, fmt.Errorf("the pin of the previous release: %w", err)
	}
	v, err := version.ParseSemantic(r.Version)
	if err != nil {
		return Release{}, fmt.Errorf("the pin's version %q: %w", r.Version, err)
	}
	if r.Version != "v"+v.String() || v.BuildMetadata() != "" {
		return Release{}, fmt.Errorf("the pin's version %q is not written vMAJOR.MINOR.PATCH[-PRERELEASE]", r.Version)
	}
	for name, sum := range map[string]string{"components": r.Components, "metadata": r.Metadata} {
		if !sha256Hex.MatchString(sum) {
			return Release{}, fmt.Errorf("the pin's sum of %s is %q, want 64 lower case hexadecimal digits", name, sum)
		}
	}
	return r, nil
}

func (r Release) ComponentsURL() string {
	return r.url(componentsAsset)
}

func (r Release) MetadataURL() string {
	return r.url(metadataAsset)
}

func (r Release) url(asset string) string {
	return repository + "/releases/download/" + r.Version + "/" + asset
}

// Fetch returns the file at url, which must have the sha256 sum. A file of that
// name in dir that has it is read instead, and a download is written there.
func Fetch(ctx context.Context, dir, url, sum string) ([]byte, error) {
	cached := filepath.Join(dir, path.Base(url))
	raw, err := os.ReadFile(cached)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if Checksum(raw) == sum {
		return raw, nil
	}
	if raw, err = download(ctx, url); err != nil {
		return nil, err
	}
	if got := Checksum(raw); got != sum {
		return nil, fmt.Errorf("%s has sha256 %s, the pin is %s", url, got, sum)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return raw, os.WriteFile(cached, raw, 0o600)
}

func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: downloadDeadline}).Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s answers %s", url, resp.Status)
	}
	return body, nil
}
