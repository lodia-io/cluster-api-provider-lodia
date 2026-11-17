// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
	"sigs.k8s.io/yaml"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
)

const (
	previousKubernetes = "v1.36.4"
	newestKubernetes   = "v1.37.0"
)

func testdataVariables(cluster, kubernetes string) map[string]string {
	return map[string]string{"CLUSTER_NAME": cluster, "KUBERNETES_VERSION": kubernetes}
}

type testdataSource func(file string) ([]byte, error)

func treeTestdata(file string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", file))
}

func releaseTestdata(ctx context.Context, dir, version string) testdataSource {
	return func(file string) ([]byte, error) {
		object := "refs/tags/" + version + ":test/e2e/testdata/" + file
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "cat-file", "blob", object).Output()
		if err != nil {
			if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
				err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(exit.Stderr))
			}
			return nil, fmt.Errorf("git cat-file blob %s, which needs the tags fetched: %w", object, err)
		}
		return out, nil
	}
}

func render(file string, variables map[string]string) ([]byte, error) {
	raw, err := treeTestdata(file)
	if err != nil {
		return nil, err
	}
	return fill(raw, variables)
}

func fill(raw []byte, variables map[string]string) ([]byte, error) {
	return yamlprocessor.NewSimpleProcessor().Process(raw, func(name string) (string, error) {
		value, ok := variables[name]
		if !ok {
			return "", fmt.Errorf("%s is not set", name)
		}
		return value, nil
	})
}

func renderedFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("testdata/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var rendered []string
	for _, file := range files {
		if name := filepath.Base(file); name != "kind.yaml" {
			rendered = append(rendered, name)
		}
	}
	if len(rendered) == 0 {
		t.Fatal("testdata holds no file that render reads")
	}
	return rendered
}

func TestRenderGivesTheTestdataTheVariablesItIsGiven(t *testing.T) {
	for _, file := range renderedFiles(t) {
		t.Run(file, func(t *testing.T) {
			rendered, err := render(file, testdataVariables("capl-w", "v1.36.4"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(rendered), "${") {
				t.Errorf("a variable is left in %s:\n%s", file, rendered)
			}
		})
	}
	rendered, err := render("cluster.yaml", testdataVariables("capl-w", "v1.36.4"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n  name: capl-w\n", "\n  version: v1.36.4\n"} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("cluster.yaml lacks %q:\n%s", want, rendered)
		}
	}
}

func TestTheLodiaControlPlaneOfTheTestdataNamesOnlyFieldsTheAPIHas(t *testing.T) {
	rendered, err := render("cluster.yaml", testdataVariables("capl-w", "v1.36.4"))
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for doc := range strings.SplitSeq(string(rendered), "\n---\n") {
		if !strings.Contains(doc, "\nkind: LodiaControlPlane\n") {
			continue
		}
		found++
		if err := yaml.UnmarshalStrict([]byte(doc), &v1alpha1.LodiaControlPlane{}); err != nil {
			t.Errorf("the LodiaControlPlane of cluster.yaml: %v", err)
		}
	}
	if found != 1 {
		t.Fatalf("cluster.yaml holds %d LodiaControlPlanes, want 1", found)
	}
}

func TestRenderRefusesAVariableItIsNotGiven(t *testing.T) {
	variables := testdataVariables("capl-w", "v1.36.4")
	delete(variables, "KUBERNETES_VERSION")
	if _, err := render("cluster.yaml", variables); err == nil || !strings.Contains(err.Error(), "KUBERNETES_VERSION") {
		t.Fatalf("want an error naming KUBERNETES_VERSION, got %v", err)
	}
}

func TestTheTestdataNamesAKubernetesVersionOnlyThroughItsVariable(t *testing.T) {
	version := regexp.MustCompile(`v1\.\d+\.\d+`)
	for _, file := range renderedFiles(t) {
		raw, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		if found := version.Find(raw); found != nil {
			t.Errorf("%s names the version %s, which the job that applies it sets in KUBERNETES_VERSION", file, found)
		}
	}
}

func TestTheTestdataOfAReleaseIsWhatItsTagHolds(t *testing.T) {
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
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "test", "e2e", "testdata"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "test", "e2e", "testdata", "cluster.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--quiet", "--initial-branch=main")
	write("released: true\n")
	git("add", ".")
	git("commit", "--quiet", "--message", "release")
	git("tag", "--annotate", "--message", "v9.9.9", "v9.9.9")
	write("released: false\n")
	git("commit", "--quiet", "--all", "--message", "after")
	git("branch", "v9.9.8")
	write("committed: false\n")

	cases := []struct {
		name, version, file, want, failure string
	}{
		{"the file at the tag, not at a later commit or in the work tree", "v9.9.9", "cluster.yaml", "released: true\n", ""},
		{"a branch of the version's name", "v9.9.8", "cluster.yaml", "", "refs/tags/v9.9.8:test/e2e/testdata/cluster.yaml, which needs the tags fetched"},
		{"no tag of the version", "v9.9.7", "cluster.yaml", "", "which needs the tags fetched"},
		{"a file the tag does not hold", "v9.9.9", "workers.yaml", "", "refs/tags/v9.9.9:test/e2e/testdata/workers.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := releaseTestdata(t.Context(), dir, c.version)(c.file)
			requireFailure(t, err, c.failure)
			if string(got) != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
