// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	koModule            = "github.com/google/ko@v0.19.1"
	kindImageRepository = "kind.local/cluster-api-provider-lodia"
)

var (
	root   = filepath.Join("..", "..")
	makeKo = regexp.MustCompile(`(?m)^KO\s*[:?]?=\s*(\S+)\s*$`)
)

func releaseComponentsCommand(ctx context.Context, cluster, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", "release-components",
		"IMAGE_REPOSITORY="+kindImageRepository, "PLATFORMS=linux/"+goruntime.GOARCH, "RELEASE_DIR="+dir)
	cmd.Dir = root
	cmd.Env = koEnv(os.Environ(), cluster)
	return cmd
}

// go run starts the tool as a child of go. Once the context kills go, the tool
// still holds the pipes, and Output would wait for it to end.
func toolOutput(cmd *exec.Cmd) ([]byte, error) {
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w\n%s", err, stderr.String())
	}
	return out, nil
}

func koEnv(environ []string, cluster string, extra ...string) []string {
	// ko refuses --platform beside GOARCH.
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool { return strings.HasPrefix(kv, "GOARCH=") })
	return append(env, append([]string{"KO_DOCKER_REPO=kind.local", "KIND_CLUSTER_NAME=" + cluster}, extra...)...)
}

func pidIn(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(string(raw))
	return pid, err == nil
}

func TestKoRunsOnTheClusterWithTheVersionAndWithoutGOARCH(t *testing.T) {
	environ := []string{"PATH=/bin", "GOARCH=arm64", "VERSION=inherited"}
	got := koEnv(environ, "capl-e2e", "VERSION=57375832e7fe")
	want := []string{"PATH=/bin", "VERSION=inherited", "KO_DOCKER_REPO=kind.local", "KIND_CLUSTER_NAME=capl-e2e", "VERSION=57375832e7fe"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !slices.Equal(environ, []string{"PATH=/bin", "GOARCH=arm64", "VERSION=inherited"}) {
		t.Fatalf("koEnv changed its argument: %q", environ)
	}
	if got := koEnv(nil, "c"); !slices.Equal(got, []string{"KO_DOCKER_REPO=kind.local", "KIND_CLUSTER_NAME=c"}) {
		t.Fatalf("with nothing extra, got %q", got)
	}
}

func TestToolOutputReturnsWhenTheContextKillsGoRun(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	const tool = `package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	os.WriteFile(os.Args[1], []byte(strconv.Itoa(os.Getpid())), 0o644)
	time.Sleep(30 * time.Second)
}
`
	if err := os.WriteFile(filepath.Join(dir, "tool.go"), []byte(tool), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := make(chan time.Time, 1)
	go func() {
		_ = wait.PollUntilContextCancel(ctx, 50*time.Millisecond, true, func(context.Context) (bool, error) {
			if _, ok := pidIn(pidFile); !ok {
				return false, nil
			}
			canceled <- time.Now()
			cancel()
			return true, nil
		})
	}()
	cmd := exec.CommandContext(ctx, "go", "run", "tool.go", pidFile)
	cmd.Dir = dir
	_, err := toolOutput(cmd)

	var killedAt time.Time
	select {
	case killedAt = <-canceled:
	default:
		t.Fatalf("the tool did not start: %v", err)
	}
	pid, _ := pidIn(pidFile)
	t.Cleanup(func() {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	})
	if err == nil {
		t.Fatal("want the error of the killed go")
	}
	if elapsed := time.Since(killedAt); elapsed > 20*time.Second {
		t.Fatalf("toolOutput returned %s after the context ended, with the tool still running", elapsed)
	}
}

func recipeOf(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	cmd.Args = slices.Insert(cmd.Args, 1, "-n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", cmd.Args, err)
	}
	return string(out)
}

func TestTheSuiteRendersTheProviderWithTheTargetTheReleaseUses(t *testing.T) {
	const dir = "/work/e2e/capl-e2e/release"
	suite := recipeOf(t, releaseComponentsCommand(t.Context(), "capl-e2e", dir))
	release := exec.CommandContext(t.Context(), "make", "--no-print-directory", "release-components")
	release.Dir = root
	released := recipeOf(t, release)
	if !strings.Contains(suite, "KO_DOCKER_REPO="+kindImageRepository+" go run "+koModule+" resolve") {
		t.Errorf("the suite's recipe does not resolve into %s with ko %s:\n%s", kindImageRepository, koModule, suite)
	}
	got := strings.NewReplacer(
		"KO_DOCKER_REPO="+kindImageRepository, "KO_DOCKER_REPO=ghcr.io/lodia-io/cluster-api-provider-lodia",
		"--platform=linux/"+goruntime.GOARCH, "--platform=linux/amd64,linux/arm64",
		dir, "out",
	).Replace(suite)
	if got != released {
		t.Errorf("the suite's render differs from the release's beyond the image repository, the platforms and the directory:\nsuite:\n%s\nrelease:\n%s", suite, released)
	}
}

func TestTheSuiteAndTheMakefileRunTheSameKo(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	found := makeKo.FindSubmatch(makefile)
	if found == nil {
		t.Fatal("the Makefile sets no KO")
	}
	if got := string(found[1]); got != koModule {
		t.Fatalf("the Makefile's KO is %s, the suite's is %s", got, koModule)
	}
}
