// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/cluster-api/cmd/clusterctl/client/yamlprocessor"
)

const (
	previousKubernetes = "v1.36.4"
	newestKubernetes   = "v1.37.0"
)

func testdataVariables(cluster, kubernetes string) map[string]string {
	return map[string]string{"CLUSTER_NAME": cluster, "KUBERNETES_VERSION": kubernetes}
}

func render(file string, variables map[string]string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		return nil, err
	}
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
