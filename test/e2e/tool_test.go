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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

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
