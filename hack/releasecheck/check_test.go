// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const (
	apiVersion = "apiVersion: clusterctl.cluster.x-k8s.io/v1alpha3\nkind: Metadata\n"
	green      = "https://example.test/runs/1"
)

func series(entries ...string) string {
	var b strings.Builder
	b.WriteString(apiVersion + "releaseSeries:\n")
	for _, entry := range entries {
		var major, minor int
		var contract string
		if _, err := fmt.Sscanf(strings.ReplaceAll(entry, ".", " "), "%d %d %s", &major, &minor, &contract); err != nil {
			panic(err)
		}
		fmt.Fprintf(&b, "- major: %d\n  minor: %d\n  contract: %s\n", major, minor, contract)
	}
	return b.String()
}

func crd(labels ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: things.example.test\n  labels:\n    cluster.x-k8s.io/provider: control-plane-lodia\n")
	for _, label := range labels {
		fmt.Fprintf(&b, "    %s: v1alpha1\n", label)
	}
	b.WriteString("spec:\n  group: example.test\n")
	return b.String()
}

type repository struct {
	t   *testing.T
	dir string
}

func newRepository(t *testing.T) *repository {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.test",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.test",
	} {
		t.Setenv(k, v)
	}
	r := &repository{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch=main")
	return r
}

func (r *repository) git(args ...string) string {
	r.t.Helper()
	out, err := exec.CommandContext(r.t.Context(), "git", append([]string{"-C", r.dir}, args...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repository) write(path, content string) {
	r.t.Helper()
	full := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repository) commit(metadata string) string {
	r.t.Helper()
	r.write(metadataFile, metadata)
	r.git("add", metadataFile)
	r.git("commit", "--quiet", "--allow-empty", "--message", "metadata")
	return r.git("rev-parse", "HEAD")
}

func (r *repository) originMain(sha string) {
	r.t.Helper()
	r.git("update-ref", originMain, sha)
}

type fakeCI struct {
	runs  []Run
	err   error
	asked []string
}

func (f *fakeCI) Runs(_ context.Context, sha string) ([]Run, error) {
	f.asked = append(f.asked, sha)
	return f.runs, f.err
}

func greenRuns() []Run {
	return []Run{{URL: green, Status: "completed", Conclusion: "success"}}
}

func TestTheTagCheckRefuses(t *testing.T) {
	forbidden := errors.New("listing the runs of ci.yml answered 403: the token needs actions: read")
	tests := []struct {
		name     string
		tag      string
		metadata string
		tags     []string
		offMain  bool
		runs     []Run
		ciErr    error
		want     string
	}{
		{name: "a tag that is a release", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: greenRuns()},
		{name: "a pre-release", tag: "v0.1.0-alpha.1", metadata: series("0.1.v1beta2"), tags: []string{"v0.1.0-alpha.0"}, runs: greenRuns()},
		{name: "the series of a newer minor beside the old", tag: "v0.2.0-alpha.1", metadata: series("0.1.v1beta2", "0.2.v1beta2"), tags: []string{"v0.1.0", "v0.1.1"}, runs: greenRuns()},
		{name: "tags that clusterctl does not list", tag: "v0.1.0", metadata: series("0.1.v1beta2"), tags: []string{"api/v0.3.0", "latest", "v0.1"}, runs: greenRuns()},
		{name: "a tag that is not a version", tag: "latest", metadata: series("0.1.v1beta2"), runs: greenRuns(), want: "not a semantic version"},
		{name: "a tag with two components", tag: "v0.1", metadata: series("0.1.v1beta2"), runs: greenRuns(), want: "not a semantic version"},
		{name: "a tag without the v", tag: "0.1.0", metadata: series("0.1.v1beta2"), runs: greenRuns(), want: "not written vMAJOR.MINOR.PATCH"},
		{name: "a tag with a space", tag: " v0.1.0", metadata: series("0.1.v1beta2"), runs: greenRuns(), want: "not written vMAJOR.MINOR.PATCH"},
		{name: "a tag with build metadata", tag: "v0.1.0+build.1", metadata: series("0.1.v1beta2"), runs: greenRuns(), want: "build metadata"},
		{name: "a tag whose series is missing", tag: "v0.2.0", metadata: series("0.1.v1beta2"), runs: greenRuns(), want: "no release series 0.2"},
		{name: "a tag whose major has no series", tag: "v1.1.0", metadata: series("0.1.v1beta2"), runs: greenRuns(), want: "no release series 1.1"},
		{name: "a tag of an older series that was dropped", tag: "v0.2.0", metadata: series("0.2.v1beta2"), tags: []string{"v0.1.0"}, runs: greenRuns(), want: "no release series for the tags v0.1.0"},
		{name: "every tag of a dropped series is named", tag: "v0.3.0", metadata: series("0.3.v1beta2"), tags: []string{"v0.1.0", "v0.2.0-rc.1"}, runs: greenRuns(), want: "v0.1.0, v0.2.0-rc.1"},
		{name: "a metadata.yaml without series", tag: "v0.1.0", metadata: apiVersion + "releaseSeries: []\n", runs: greenRuns(), want: "releaseSeries is empty"},
		{name: "a metadata.yaml of another kind", tag: "v0.1.0", metadata: "apiVersion: clusterctl.cluster.x-k8s.io/v1alpha3\nkind: Other\nreleaseSeries:\n- {major: 0, minor: 1, contract: v1beta2}\n", runs: greenRuns(), want: `no kind "Other"`},
		{name: "a metadata.yaml without an apiVersion", tag: "v0.1.0", metadata: "kind: Metadata\nreleaseSeries:\n- {major: 0, minor: 1, contract: v1beta2}\n", runs: greenRuns(), want: "unexpected apiVersion"},
		{name: "a commit off main", tag: "v0.1.0", metadata: series("0.1.v1beta2"), offMain: true, runs: greenRuns(), want: "is not on origin/main"},
		{name: "a run that failed", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: []Run{{URL: green, Status: "completed", Conclusion: "failure"}}, want: "ended as failure"},
		{name: "a run that was cancelled", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: []Run{{URL: green, Status: "completed", Conclusion: "cancelled"}}, want: "ended as cancelled"},
		{name: "a run that was skipped", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: []Run{{URL: green, Status: "completed", Conclusion: "skipped"}}, want: "ended as skipped"},
		{name: "a run still going", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: []Run{{URL: green, Status: "in_progress"}}, want: "is in_progress"},
		{name: "a run still queued", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: []Run{{URL: green, Status: "queued"}}, want: "is queued"},
		{name: "a run that waits", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: []Run{{URL: green, Status: "waiting"}}, want: "is waiting"},
		{name: "no run at all", tag: "v0.1.0", metadata: series("0.1.v1beta2"), want: "no run for the push"},
		{name: "one run green and one not", tag: "v0.1.0", metadata: series("0.1.v1beta2"), runs: append(greenRuns(), Run{URL: "https://example.test/runs/2", Status: "completed", Conclusion: "failure"}), want: "runs/2 ended as failure"},
		{name: "a 403 from GitHub", tag: "v0.1.0", metadata: series("0.1.v1beta2"), ciErr: forbidden, want: "answered 403"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepository(t)
			base := r.commit(series("0.1.v1beta2"))
			r.originMain(base)
			if tt.offMain {
				r.git("switch", "--quiet", "--create", "topic")
			}
			sha := r.commit(tt.metadata)
			if !tt.offMain {
				r.originMain(sha)
			}
			for _, tag := range tt.tags {
				r.git("tag", tag, base)
			}
			ci := &fakeCI{runs: tt.runs, err: tt.ciErr}

			got, err := checkTag(t.Context(), r.dir, ci, tt.tag, "HEAD")

			if tt.want == "" {
				if err != nil {
					t.Fatalf("want the tag accepted, got %v", err)
				}
				if got != sha {
					t.Errorf("accepted on %s, want %s", got, sha)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want an error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestTheTagCheckReadsMetadataAtTheCommit(t *testing.T) {
	r := newRepository(t)
	older := r.commit(series("0.1.v1beta2"))
	newer := r.commit(series("0.1.v1beta2", "0.2.v1beta2"))
	r.originMain(newer)

	if _, err := checkTag(t.Context(), r.dir, &fakeCI{runs: greenRuns()}, "v0.2.0", older); err == nil || !strings.Contains(err.Error(), "no release series 0.2") {
		t.Errorf("a tag on the commit before the series was added: want its refusal, got %v", err)
	}
	if _, err := checkTag(t.Context(), r.dir, &fakeCI{runs: greenRuns()}, "v0.2.0", newer); err != nil {
		t.Errorf("a tag on the commit that added the series: %v", err)
	}
}

func TestTheTagCheckAsksForTheCommitsFullSHAEvenWhenGivenATag(t *testing.T) {
	r := newRepository(t)
	sha := r.commit(series("0.1.v1beta2"))
	r.originMain(sha)
	r.git("tag", "--annotate", "--message", "v0.1.0-alpha.0", "v0.1.0-alpha.0", sha)
	tagObject := r.git("rev-parse", "v0.1.0-alpha.0")
	if tagObject == sha {
		t.Fatal("the annotated tag is not its own object")
	}

	for _, commit := range []string{"HEAD", "main", "v0.1.0-alpha.0", sha[:12]} {
		ci := &fakeCI{runs: greenRuns()}
		got, err := checkTag(t.Context(), r.dir, ci, "v0.1.0-alpha.1", commit)
		if err != nil {
			t.Fatalf("%s: %v", commit, err)
		}
		if diff := cmp.Diff([]string{sha}, ci.asked); diff != "" || got != sha {
			t.Errorf("%s: asked CI about (-want +got):\n%s\nreturned %s", commit, diff, got)
		}
	}
}

func TestTheTagCheckDoesNotAskCIAboutACommitOffMain(t *testing.T) {
	r := newRepository(t)
	r.originMain(r.commit(series("0.1.v1beta2")))
	r.git("switch", "--quiet", "--create", "topic")
	r.commit(series("0.1.v1beta2"))
	ci := &fakeCI{runs: greenRuns()}

	if _, err := checkTag(t.Context(), r.dir, ci, "v0.1.0", "HEAD"); err == nil {
		t.Fatal("want a refusal")
	}
	if len(ci.asked) != 0 {
		t.Errorf("asked CI about %v", ci.asked)
	}
}

func TestTheTagCheckAsksOriginMainNotTheLocalMain(t *testing.T) {
	r := newRepository(t)
	r.originMain(r.commit(series("0.1.v1beta2")))
	r.commit(series("0.1.v1beta2"))
	ci := &fakeCI{runs: greenRuns()}

	if _, err := checkTag(t.Context(), r.dir, ci, "v0.1.0", "HEAD"); err == nil || !strings.Contains(err.Error(), "is not on origin/main") {
		t.Errorf("a commit only on the local main: want its refusal, got %v", err)
	}
	if len(ci.asked) != 0 {
		t.Errorf("asked CI about %v", ci.asked)
	}
}

func TestTheTagCheckRefusesWithoutOriginMainOrACommit(t *testing.T) {
	r := newRepository(t)
	r.commit(series("0.1.v1beta2"))

	if _, err := checkTag(t.Context(), r.dir, &fakeCI{runs: greenRuns()}, "v0.1.0", "HEAD"); err == nil || !strings.Contains(err.Error(), "origin/main does not exist") {
		t.Errorf("without origin/main: want its refusal, got %v", err)
	}
	r.originMain(r.git("rev-parse", "HEAD"))
	if _, err := checkTag(t.Context(), r.dir, &fakeCI{runs: greenRuns()}, "v0.1.0", "no-such-commit"); err == nil {
		t.Error("a commit that does not exist: want a refusal")
	}
}

func TestTheMetadataCheck(t *testing.T) {
	v1beta2 := "cluster.x-k8s.io/v1beta2"
	tests := []struct {
		name   string
		head   string
		base   string
		labels [][]string
		want   string
	}{
		{name: "nothing changed", head: series("0.1.v1beta2"), base: series("0.1.v1beta2"), labels: [][]string{{v1beta2}, {v1beta2}}},
		{name: "a series added", head: series("0.1.v1beta2", "0.2.v1beta2"), base: series("0.1.v1beta2"), labels: [][]string{{v1beta2}}},
		{name: "the series reordered", head: series("0.2.v1beta2", "0.1.v1beta2"), base: series("0.1.v1beta2", "0.2.v1beta2"), labels: [][]string{{v1beta2}}},
		{name: "the newest series is not the last in the file", head: series("0.2.v1beta2", "0.1.v1beta1"), base: series("0.1.v1beta1"), labels: [][]string{{v1beta2}}},
		{name: "the newest series is not the first in the file", head: series("0.1.v1beta2", "0.2.v1beta1"), base: series("0.1.v1beta2"), labels: [][]string{{v1beta2}}, want: "has the contract label v1beta2, and the newest series 0.2 of metadata.yaml has the contract v1beta1"},
		{name: "a newer major with an older minor is the newest", head: series("0.9.v1beta1", "1.0.v1beta2"), base: series("0.9.v1beta1"), labels: [][]string{{v1beta2}}},
		{name: "the contract differs from a label", head: series("0.1.v1beta1"), base: series("0.1.v1beta1"), labels: [][]string{{v1beta2}}, want: "has the contract label v1beta2, and the newest series 0.1 of metadata.yaml has the contract v1beta1"},
		{name: "the contract differs from the label of one of two CRDs", head: series("0.1.v1beta2"), base: series("0.1.v1beta2"), labels: [][]string{{v1beta2}, {"cluster.x-k8s.io/v1beta1"}}, want: "has the contract label v1beta1"},
		{name: "a CRD without a contract label", head: series("0.1.v1beta2"), base: series("0.1.v1beta2"), labels: [][]string{{v1beta2}, {}}, want: "has 0 contract labels"},
		{name: "a CRD with two contract labels", head: series("0.1.v1beta2"), base: series("0.1.v1beta2"), labels: [][]string{{v1beta2, "cluster.x-k8s.io/v1beta1"}}, want: "has 2 contract labels"},
		{name: "a series dropped", head: series("0.2.v1beta2"), base: series("0.1.v1beta2", "0.2.v1beta2"), labels: [][]string{{v1beta2}}, want: "has the series 0.1"},
		{name: "a series replaced by the next", head: series("0.2.v1beta2"), base: series("0.1.v1beta2"), labels: [][]string{{v1beta2}}, want: "has the series 0.1"},
		{name: "a series with another contract", head: series("0.1.v1beta2", "0.2.v1beta2"), base: series("0.1.v1beta1"), labels: [][]string{{v1beta2}}, want: "the series 0.1 has the contract v1beta1 at the merge base with origin/main and v1beta2"},
		{name: "a metadata.yaml that is not metadata", head: "kind: Other\n", base: series("0.1.v1beta2"), labels: [][]string{{v1beta2}}, want: "metadata.yaml:"},
		{name: "a metadata.yaml on origin/main that is not metadata", head: series("0.1.v1beta2"), base: "releaseSeries: []\n", labels: [][]string{{v1beta2}}, want: "at the merge base with origin/main: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRepository(t)
			r.originMain(r.commit(tt.base))
			r.write(metadataFile, tt.head)
			for i, labels := range tt.labels {
				r.write(fmt.Sprintf("%s/%d.yaml", crdsDir, i), crd(labels...))
			}

			err := checkMetadata(t.Context(), r.dir)

			if tt.want == "" {
				if err != nil {
					t.Fatalf("want the metadata accepted, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want an error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestTheMetadataCheckComparesWithTheMergeBase(t *testing.T) {
	v1beta2 := "cluster.x-k8s.io/v1beta2"
	t.Run("a series dropped by a commit of the branch", func(t *testing.T) {
		r := newRepository(t)
		r.originMain(r.commit(series("0.1.v1beta2", "0.2.v1beta2")))
		r.commit(series("0.2.v1beta2"))
		r.write(crdsDir+"/0.yaml", crd(v1beta2))

		if err := checkMetadata(t.Context(), r.dir); err == nil || !strings.Contains(err.Error(), "has the series 0.1") {
			t.Errorf("want its refusal, got %v", err)
		}
	})
	t.Run("a series that main added after the branch point", func(t *testing.T) {
		r := newRepository(t)
		base := r.commit(series("0.1.v1beta2"))
		r.originMain(base)
		r.git("switch", "--quiet", "--create", "upstream")
		r.originMain(r.commit(series("0.1.v1beta2", "0.2.v1beta2")))
		r.git("switch", "--quiet", "--create", "topic", base)
		r.commit(series("0.1.v1beta2"))
		r.write(crdsDir+"/0.yaml", crd(v1beta2))

		if err := checkMetadata(t.Context(), r.dir); err != nil {
			t.Errorf("a branch that dropped nothing: %v", err)
		}
	})
	t.Run("a series dropped by a branch that main has left behind", func(t *testing.T) {
		r := newRepository(t)
		base := r.commit(series("0.1.v1beta2"))
		r.originMain(base)
		r.git("switch", "--quiet", "--create", "upstream")
		r.originMain(r.commit(series("0.1.v1beta2", "0.2.v1beta2")))
		r.git("switch", "--quiet", "--create", "topic", base)
		r.commit(series("0.3.v1beta2"))
		r.write(crdsDir+"/0.yaml", crd(v1beta2))

		if err := checkMetadata(t.Context(), r.dir); err == nil || !strings.Contains(err.Error(), "has the series 0.1") {
			t.Errorf("want its refusal, got %v", err)
		}
	})
	t.Run("a history that shares nothing with origin/main", func(t *testing.T) {
		r := newRepository(t)
		r.originMain(r.commit(series("0.1.v1beta2")))
		r.git("switch", "--quiet", "--orphan", "other")
		r.commit(series("0.2.v1beta2"))
		r.write(crdsDir+"/0.yaml", crd(v1beta2))

		if err := checkMetadata(t.Context(), r.dir); err == nil || !strings.Contains(err.Error(), "no merge base") {
			t.Errorf("want its refusal, got %v", err)
		}
	})
}

func TestTheMetadataCheckNamesEveryProblem(t *testing.T) {
	r := newRepository(t)
	r.originMain(r.commit(series("0.1.v1beta2", "0.2.v1beta2")))
	r.write(metadataFile, series("0.3.v1beta1"))
	r.write(crdsDir+"/0.yaml", crd("cluster.x-k8s.io/v1beta2"))

	err := checkMetadata(t.Context(), r.dir)

	for _, want := range []string{"has the contract label v1beta2", "has the series 0.1", "has the series 0.2"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q among the problems, got %v", want, err)
		}
	}
}

func TestTheMetadataCheckRefusesWithoutOriginMainOrCRDs(t *testing.T) {
	r := newRepository(t)
	r.commit(series("0.1.v1beta2"))

	if err := checkMetadata(t.Context(), r.dir); err == nil || !strings.Contains(err.Error(), "no CRD") {
		t.Errorf("without CRDs: want its refusal, got %v", err)
	}
	r.write(crdsDir+"/0.yaml", crd("cluster.x-k8s.io/v1beta2"))
	if err := checkMetadata(t.Context(), r.dir); err == nil || !strings.Contains(err.Error(), "origin/main does not exist") {
		t.Errorf("without origin/main: want its refusal, got %v", err)
	}
	r.originMain(r.git("rev-parse", "HEAD"))
	if err := checkMetadata(t.Context(), r.dir); err != nil {
		t.Errorf("with both: %v", err)
	}
}

func TestTheMetadataCheckAcceptsTheRepositoryItself(t *testing.T) {
	r := newRepository(t)
	for _, file := range []string{metadataFile, crdsDir + "/controlplane.lodia.io_lodiacontrolplanes.yaml", crdsDir + "/controlplane.lodia.io_lodiacontrolplanetemplates.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", file))
		if err != nil {
			t.Fatal(err)
		}
		r.write(file, string(raw))
	}
	r.git("add", ".")
	r.git("commit", "--quiet", "--message", "the repository")
	r.originMain(r.git("rev-parse", "HEAD"))

	if err := checkMetadata(t.Context(), r.dir); err != nil {
		t.Fatal(err)
	}
}
