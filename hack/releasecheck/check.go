// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/version"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/yaml"
)

const (
	metadataFile = "metadata.yaml"
	crdsDir      = "config/crd/bases"
	originMain   = "refs/remotes/origin/main"

	completed = "completed"
	success   = "success"
)

var contractLabel = regexp.MustCompile(`^cluster\.x-k8s\.io/(v[0-9]+(?:(?:alpha|beta)[0-9]+)?)$`)

type Run struct {
	URL        string
	Status     string
	Conclusion string
}

// CI lists the runs of ci.yml for the push of a commit to main.
type CI interface {
	Runs(ctx context.Context, sha string) ([]Run, error)
}

func checkTag(ctx context.Context, dir string, ci CI, tag, commit string) (string, error) {
	v, err := parseTag(tag)
	if err != nil {
		return "", err
	}
	out, err := git(ctx, dir, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("commit %q: %w", commit, err)
	}
	sha := strings.TrimSpace(string(out))

	md, err := metadataAt(ctx, dir, sha)
	if err != nil {
		return "", err
	}
	if md.GetReleaseSeriesForVersion(v) == nil {
		return "", fmt.Errorf("%s has no release series %d.%d in metadata.yaml at %s: add it in a pull request before tagging", tag, v.Major(), v.Minor(), sha)
	}
	missing, err := tagsWithoutSeries(ctx, dir, md)
	if err != nil {
		return "", err
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("metadata.yaml at %s has no release series for the tags %s, and clusterctl upgrade plan fails for them: never drop a series", sha, strings.Join(missing, ", "))
	}

	if err := requireOnMain(ctx, dir, sha); err != nil {
		return "", err
	}
	if err := requireAPITag(ctx, dir, tag, sha); err != nil {
		return "", err
	}
	runs, err := ci.Runs(ctx, sha)
	if err != nil {
		return "", err
	}
	return sha, requireGreen(sha, runs)
}

// A tag is released as an image tag too, which has no place for build metadata.
func parseTag(tag string) (*version.Version, error) {
	v, err := version.ParseSemantic(tag)
	if err != nil {
		return nil, fmt.Errorf("%q is not a semantic version: %w", tag, err)
	}
	if tag != "v"+v.String() {
		return nil, fmt.Errorf("%q is not written vMAJOR.MINOR.PATCH[-PRERELEASE]", tag)
	}
	if v.BuildMetadata() != "" {
		return nil, fmt.Errorf("%q has build metadata, which an image tag cannot carry", tag)
	}
	return v, nil
}

// clusterctl lists the release tags of a private repository and the Go proxy's
// semver tags of a public one, each of which version.ParseSemantic accepts:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_github.go#L364
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/repository_github.go#L118-L133
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/internal/goproxy/goproxy.go#L123-L131
func tagsWithoutSeries(ctx context.Context, dir string, md *clusterctlv1.Metadata) ([]string, error) {
	out, err := git(ctx, dir, "tag", "--list")
	if err != nil {
		return nil, err
	}
	var missing []string
	for name := range strings.FieldsSeq(string(out)) {
		v, err := version.ParseSemantic(name)
		if err != nil {
			continue
		}
		if md.GetReleaseSeriesForVersion(v) == nil {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

func requireOnMain(ctx context.Context, dir, sha string) error {
	if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", originMain); err != nil {
		return errors.New("origin/main does not exist here: fetch it first")
	}
	_, err := git(ctx, dir, "merge-base", "--is-ancestor", sha, originMain)
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return fmt.Errorf("%s is not on origin/main", sha)
	}
	return err
}

// The API module is tagged at the release's commit, under api/ as Go reads the
// tag of a module in a subdirectory: https://go.dev/ref/mod#vcs-version
func requireAPITag(ctx context.Context, dir, tag, sha string) error {
	apiTag := "api/" + tag
	out, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/tags/"+apiTag+"^{commit}")
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return fmt.Errorf("the tag %s does not exist here: tag the API module at %s first", apiTag, sha)
	}
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != sha {
		return fmt.Errorf("the tag %s is at %s, not at %s: both tags of a release name one commit", apiTag, got, sha)
	}
	return nil
}

// ci.yml's run is the one that counts: on the commit of a tag, release.yml's
// own run is in progress.
func requireGreen(sha string, runs []Run) error {
	if len(runs) == 0 {
		return fmt.Errorf("ci.yml has no run for the push of %s to main: it is not started yet or never ran", sha)
	}
	var unmet []string
	for _, run := range runs {
		switch {
		case run.Status != completed:
			unmet = append(unmet, fmt.Sprintf("%s is %s", run.URL, run.Status))
		case run.Conclusion != success:
			unmet = append(unmet, fmt.Sprintf("%s ended as %s", run.URL, run.Conclusion))
		}
	}
	if len(unmet) > 0 {
		return fmt.Errorf("ci.yml is not green on %s: %s", sha, strings.Join(unmet, "; "))
	}
	return nil
}

func checkMetadata(ctx context.Context, dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, metadataFile))
	if err != nil {
		return err
	}
	head, err := decodeMetadata(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", metadataFile, err)
	}
	want := newest(head.ReleaseSeries)

	var problems []error
	crds, err := filepath.Glob(filepath.Join(dir, crdsDir, "*.yaml"))
	if err != nil {
		return err
	}
	if len(crds) == 0 {
		return fmt.Errorf("no CRD in %s", crdsDir)
	}
	for _, path := range crds {
		got, err := crdContract(path)
		if err != nil {
			return err
		}
		if got != want.Contract {
			problems = append(problems, fmt.Errorf("%s has the contract label %s, and the newest series %d.%d of %s has the contract %s", filepath.Base(path), got, want.Major, want.Minor, metadataFile, want.Contract))
		}
	}

	if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", originMain); err != nil {
		return errors.New("origin/main does not exist here: fetch it first")
	}
	// The merge base, not the tip: a branch that dropped nothing must not fail
	// once main adds a series.
	mergeBase, err := git(ctx, dir, "merge-base", "HEAD", originMain)
	if err != nil {
		return fmt.Errorf("HEAD and origin/main have no merge base, as in a shallow clone: %w", err)
	}
	out, err := git(ctx, dir, "show", strings.TrimSpace(string(mergeBase))+":"+metadataFile)
	if err != nil {
		return err
	}
	base, err := decodeMetadata(out)
	if err != nil {
		return fmt.Errorf("%s at the merge base with origin/main: %w", metadataFile, err)
	}
	for _, was := range base.ReleaseSeries {
		i := slices.IndexFunc(head.ReleaseSeries, func(s clusterctlv1.ReleaseSeries) bool {
			return s.Major == was.Major && s.Minor == was.Minor
		})
		switch {
		case i < 0:
			problems = append(problems, fmt.Errorf("%s at the merge base with origin/main has the series %d.%d, and clusterctl upgrade plan fails for the releases of a series the newest release lacks: never drop one", metadataFile, was.Major, was.Minor))
		case head.ReleaseSeries[i].Contract != was.Contract:
			problems = append(problems, fmt.Errorf("the series %d.%d has the contract %s at the merge base with origin/main and %s in %s: a released series keeps its contract", was.Major, was.Minor, was.Contract, head.ReleaseSeries[i].Contract, metadataFile))
		}
	}
	return errors.Join(problems...)
}

func newest(series []clusterctlv1.ReleaseSeries) clusterctlv1.ReleaseSeries {
	return slices.MaxFunc(series, func(a, b clusterctlv1.ReleaseSeries) int {
		return cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor))
	})
}

func crdContract(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	var contracts []string
	for key := range crd.Labels {
		if m := contractLabel.FindStringSubmatch(key); m != nil {
			contracts = append(contracts, m[1])
		}
	}
	if len(contracts) != 1 {
		return "", fmt.Errorf("%s has %d contract labels, want exactly one", filepath.Base(path), len(contracts))
	}
	return contracts[0], nil
}

func metadataAt(ctx context.Context, dir, sha string) (*clusterctlv1.Metadata, error) {
	out, err := git(ctx, dir, "show", sha+":"+metadataFile)
	if err != nil {
		return nil, err
	}
	md, err := decodeMetadata(out)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", metadataFile, sha, err)
	}
	return md, nil
}

// decodeMetadata reads the file as clusterctl does, and refuses what it refuses:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/metadata_client.go#L89-L126
func decodeMetadata(raw []byte) (*clusterctlv1.Metadata, error) {
	scheme := runtime.NewScheme()
	if err := clusterctlv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	md := &clusterctlv1.Metadata{}
	if err := runtime.DecodeInto(serializer.NewCodecFactory(scheme).UniversalDecoder(), raw, md); err != nil {
		return nil, err
	}
	if md.APIVersion != clusterctlv1.GroupVersion.String() {
		return nil, fmt.Errorf("unexpected apiVersion %q, want %q", md.APIVersion, clusterctlv1.GroupVersion.String())
	}
	if len(md.ReleaseSeries) == 0 {
		return nil, errors.New("releaseSeries is empty")
	}
	return md, nil
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	//nolint:gosec // G204: the arguments are git's subcommands, and the tag, commit and paths this program was given
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
