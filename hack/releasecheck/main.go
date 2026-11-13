// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Command releasecheck refuses what must not reach a release, in the
// repository's root:
//
//	releasecheck tag [--commit=HEAD] vX.Y.Z
//	releasecheck metadata
//	releasecheck image --repository=REPOSITORY --tag=TAG components.yaml
//
// tag reads GitHub with $GITHUB_TOKEN, as the repository of origin.
// image prints the one image the components YAML names.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/go-github/v92/github"
)

const usage = "usage: releasecheck tag [--commit=HEAD] vX.Y.Z | releasecheck metadata | releasecheck image --repository=REPOSITORY --tag=TAG components.yaml"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "metadata":
		if len(args) != 1 {
			return errors.New(usage)
		}
		if err := checkMetadata(ctx, "."); err != nil {
			return err
		}
		fmt.Println("metadata.yaml: the newest series matches the CRDs, and every series of the merge base with origin/main is still there")
		return nil
	case "image":
		flags := flag.NewFlagSet("image", flag.ContinueOnError)
		repository := flags.String("repository", "", "the repository the image must be in")
		tag := flags.String("tag", "", "the tag the image must carry beside its digest")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New(usage)
		}
		components, err := os.ReadFile(flags.Arg(0))
		if err != nil {
			return err
		}
		image, err := checkImage(components, *repository, *tag)
		if err != nil {
			return fmt.Errorf("%s: %w", flags.Arg(0), err)
		}
		fmt.Println(image)
		return nil
	case "tag":
		flags := flag.NewFlagSet("tag", flag.ContinueOnError)
		commit := flags.String("commit", "HEAD", "the commit the tag will name")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New(usage)
		}
		owner, repo, err := originRepository(ctx, ".")
		if err != nil {
			return err
		}
		ci, err := newGitHub(owner, repo, os.Getenv("GITHUB_TOKEN"), github.WithTimeout(time.Minute))
		if err != nil {
			return err
		}
		sha, err := checkTag(ctx, ".", ci, flags.Arg(0), *commit)
		if err != nil {
			return err
		}
		fmt.Printf("%s may be tagged on %s\n", flags.Arg(0), sha)
		return nil
	default:
		return errors.New(usage)
	}
}
