// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package build_test

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

const (
	lodiaAPI      = "go.lodia.io/lodia/api"
	clusterAPIAPI = "sigs.k8s.io/cluster-api/api"
)

var lodiaCommit = regexp.MustCompile(`(?m)^LODIA_COMMIT\s*[:?]?=\s*(\S+)\s*$`)

func requiredVersion(name string, gomod []byte, path string) (string, error) {
	file, err := modfile.Parse(name, gomod, nil)
	if err != nil {
		return "", err
	}
	for _, require := range file.Require {
		if require.Mod.Path == path {
			return require.Mod.Version, nil
		}
	}
	return "", fmt.Errorf("%s requires no %s", name, path)
}

func refuseReplaces(name string, gomod []byte) error {
	file, err := modfile.Parse(name, gomod, nil)
	if err != nil {
		return err
	}
	pinned := []string{lodiaAPI, clusterAPIAPI, "sigs.k8s.io/cluster-api"}
	var replaced []string
	for _, replace := range file.Replace {
		if slices.Contains(pinned, replace.Old.Path) {
			replaced = append(replaced, replace.Old.Path)
		}
	}
	if len(replaced) > 0 {
		return fmt.Errorf("%s replaces %s, which the pins of both go.mod files must name", name, strings.Join(replaced, ", "))
	}
	return nil
}

func lodiaPin(gomod, makefile []byte) error {
	if err := refuseReplaces("go.mod", gomod); err != nil {
		return err
	}
	version, err := requiredVersion("go.mod", gomod, lodiaAPI)
	if err != nil {
		return err
	}
	rev, err := module.PseudoVersionRev(version)
	if err != nil {
		return fmt.Errorf("%s %s: %w", lodiaAPI, version, err)
	}
	found := lodiaCommit.FindSubmatch(makefile)
	if found == nil {
		return errors.New("the Makefile sets no LODIA_COMMIT")
	}
	if commit := string(found[1]); len(commit) != 40 || !strings.HasPrefix(commit, rev) {
		return fmt.Errorf("LODIA_COMMIT %q is not the full commit that go.mod pins, %s", commit, rev)
	}
	return nil
}

func apiPins(gomod, apiGomod []byte) error {
	if err := refuseReplaces("go.mod", gomod); err != nil {
		return err
	}
	if err := refuseReplaces("api/go.mod", apiGomod); err != nil {
		return err
	}
	var differing []string
	for _, path := range []string{lodiaAPI, clusterAPIAPI} {
		want, err := requiredVersion("go.mod", gomod, path)
		if err != nil {
			return err
		}
		got, err := requiredVersion("api/go.mod", apiGomod, path)
		if err != nil {
			return err
		}
		if got != want {
			differing = append(differing, fmt.Sprintf("%s is %s in api/go.mod and %s in go.mod", path, got, want))
		}
	}
	if len(differing) > 0 {
		return fmt.Errorf("the API module pins what the main module does not: %s", strings.Join(differing, "; "))
	}
	return nil
}

func TestLodiaCommitIsThePinnedAPI(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if err := lodiaPin(gomod, makefile); err != nil {
		t.Fatal(err)
	}
}

func TestAWrongLodiaPinIsRefused(t *testing.T) {
	const pinned = "require go.lodia.io/lodia/api v0.0.0-20261114202616-f91168b1248d\n"
	cases := []struct {
		name, gomod, makefile, want string
	}{
		{"no requirement", "module m\n", "LODIA_COMMIT = f91168b1248dc18151a0c2d442fe2050593a0866\n", "requires no"},
		{"not a pseudo-version", "module m\nrequire go.lodia.io/lodia/api v1.0.0\n", "LODIA_COMMIT = f91168b1248dc18151a0c2d442fe2050593a0866\n", "pseudo-version"},
		{"no commit", "module m\n" + pinned, "E2E_DIR = x\n", "sets no LODIA_COMMIT"},
		{"another commit", "module m\n" + pinned, "LODIA_COMMIT = f91168baaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", "not the full commit"},
		{"a short commit", "module m\n" + pinned, "LODIA_COMMIT = f91168b1248d\n", "not the full commit"},
		{"a replace of Lodia", "module m\n" + pinned + "replace go.lodia.io/lodia/api => go.lodia.io/lodia/api v0.0.0-20261112004109-bf2882642645\n", "LODIA_COMMIT = f91168b1248dc18151a0c2d442fe2050593a0866\n", "go.mod replaces go.lodia.io/lodia/api"},
		{"a replace of the API module", "module m\n" + pinned + "replace go.lodia.io/cluster-api-provider-lodia/api => ./api\n", "LODIA_COMMIT = f91168b1248dc18151a0c2d442fe2050593a0866\n", ""},
		{"the pinned commit", "module m\n" + pinned, "LODIA_COMMIT = f91168b1248dc18151a0c2d442fe2050593a0866\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := lodiaPin([]byte(c.gomod), []byte(c.makefile))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestTheAPIModulePinsWhatTheMainModuleDoes(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	apiGomod, err := os.ReadFile("../../api/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if err := apiPins(gomod, apiGomod); err != nil {
		t.Fatal(err)
	}
}

func TestPinsThatDifferBetweenTheModulesAreRefused(t *testing.T) {
	const (
		lodia     = "go.lodia.io/lodia/api v0.0.0-20261114202616-f91168b1248d\n"
		lodiaNew  = "go.lodia.io/lodia/api v0.0.0-20261112004109-bf2882642645\n"
		cluster   = "sigs.k8s.io/cluster-api/api v1.15.0-beta.0\n"
		clusterGA = "sigs.k8s.io/cluster-api/api v1.15.0\n"
	)
	gomod := func(requires ...string) string {
		return "module m\nrequire (\n" + strings.Join(requires, "") + ")\n"
	}
	replacing := func(gomod, replace string) string {
		return gomod + "replace " + replace + "\n"
	}
	cases := []struct {
		name, root, api, want string
	}{
		{"the same pins", gomod(lodia, cluster), gomod(cluster, lodia), ""},
		{"another Lodia", gomod(lodia, cluster), gomod(lodiaNew, cluster), "go.lodia.io/lodia/api is v0.0.0-20261112004109-bf2882642645 in api/go.mod and v0.0.0-20261114202616-f91168b1248d in go.mod"},
		{"another Cluster API", gomod(lodia, cluster), gomod(lodia, clusterGA), "sigs.k8s.io/cluster-api/api is v1.15.0 in api/go.mod and v1.15.0-beta.0 in go.mod"},
		{"both another", gomod(lodia, cluster), gomod(lodiaNew, clusterGA), "go.lodia.io/lodia/api is v0.0.0-20261112004109-bf2882642645 in api/go.mod and v0.0.0-20261114202616-f91168b1248d in go.mod; sigs.k8s.io/cluster-api/api is v1.15.0"},
		{"no Lodia in the API module", gomod(lodia, cluster), gomod(cluster), "api/go.mod requires no go.lodia.io/lodia/api"},
		{"no Cluster API in the API module", gomod(lodia, cluster), gomod(lodia), "api/go.mod requires no sigs.k8s.io/cluster-api/api"},
		{"no Lodia in the main module", gomod(cluster), gomod(lodia, cluster), "go.mod requires no go.lodia.io/lodia/api"},
		{"the main module's replace of the API module", replacing(gomod(lodia, cluster), "go.lodia.io/cluster-api-provider-lodia/api => ./api"), gomod(lodia, cluster), ""},
		{"a replace of Lodia by a version in the API module", gomod(lodia, cluster), replacing(gomod(lodia, cluster), "go.lodia.io/lodia/api => go.lodia.io/lodia/api v0.0.0-20261112004109-bf2882642645"), "api/go.mod replaces go.lodia.io/lodia/api"},
		{"a replace of Lodia by a version in the main module", replacing(gomod(lodia, cluster), "go.lodia.io/lodia/api => go.lodia.io/lodia/api v0.0.0-20261112004109-bf2882642645"), gomod(lodia, cluster), "go.mod replaces go.lodia.io/lodia/api"},
		{"a replace of Lodia by a directory in the main module", replacing(gomod(lodia, cluster), "go.lodia.io/lodia/api => ../lodia/api"), gomod(lodia, cluster), "go.mod replaces go.lodia.io/lodia/api"},
		{"a replace of Lodia at a version in the API module", gomod(lodia, cluster), replacing(gomod(lodia, cluster), "go.lodia.io/lodia/api v0.0.0-20261114202616-f91168b1248d => ../lodia/api"), "api/go.mod replaces go.lodia.io/lodia/api"},
		{"a replace of Cluster API's API in the API module", gomod(lodia, cluster), replacing(gomod(lodia, cluster), "sigs.k8s.io/cluster-api/api => sigs.k8s.io/cluster-api/api v1.15.0"), "api/go.mod replaces sigs.k8s.io/cluster-api/api"},
		{"a replace of Cluster API's API in the main module", replacing(gomod(lodia, cluster), "sigs.k8s.io/cluster-api/api => sigs.k8s.io/cluster-api/api v1.15.0"), gomod(lodia, cluster), "go.mod replaces sigs.k8s.io/cluster-api/api"},
		{"a replace of Cluster API in the main module", replacing(gomod(lodia, cluster), "sigs.k8s.io/cluster-api => sigs.k8s.io/cluster-api v1.15.0"), gomod(lodia, cluster), "go.mod replaces sigs.k8s.io/cluster-api, which"},
		{"a replace of Cluster API in the API module", gomod(lodia, cluster), replacing(gomod(lodia, cluster), "sigs.k8s.io/cluster-api => sigs.k8s.io/cluster-api v1.15.0"), "api/go.mod replaces sigs.k8s.io/cluster-api, which"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := apiPins([]byte(c.root), []byte(c.api))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}
