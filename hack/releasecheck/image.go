// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/distribution/reference"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// checkImage requires the components YAML to name one image, repository:tag@digest,
// and returns it. clusterctl's images: override of the repository keeps the digest
// and the tag when it rewrites such a name:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/config/imagemeta_client.go#L150-L164
func checkImage(components []byte, repository, tag string) (string, error) {
	images, err := imagesOf(components)
	if err != nil {
		return "", err
	}
	switch len(images) {
	case 0:
		return "", errors.New("the components YAML names no image")
	case 1:
	default:
		return "", fmt.Errorf("the components YAML names %d images, want one: %s", len(images), strings.Join(images, ", "))
	}
	image := images[0]

	named, err := reference.ParseNamed(image)
	if err != nil {
		return "", fmt.Errorf("the image %q is not a reference that clusterctl parses: %w", image, err)
	}
	if named.Name() != repository {
		return "", fmt.Errorf("the image %q is not in %s", image, repository)
	}
	if _, ok := named.(reference.Digested); !ok {
		return "", fmt.Errorf("the image %q names no digest, so an images: override of its repository could not keep the pin", image)
	}
	if tagged, ok := named.(reference.Tagged); !ok || tagged.Tag() != tag {
		return "", fmt.Errorf("the image %q is not tagged %s, which the release pushes beside the digest", image, tag)
	}
	return image, nil
}

func imagesOf(components []byte) ([]string, error) {
	images := map[string]struct{}{}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(components), 4096)
	for {
		var doc any
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return slices.Sorted(maps.Keys(images)), nil
			}
			return nil, fmt.Errorf("the components YAML: %w", err)
		}
		collectImages(doc, images)
	}
}

func collectImages(node any, into map[string]struct{}) {
	switch n := node.(type) {
	case map[string]any:
		for key, value := range n {
			if image, ok := value.(string); ok && key == "image" {
				into[image] = struct{}{}
				continue
			}
			collectImages(value, into)
		}
	case []any:
		for _, value := range n {
			collectImages(value, into)
		}
	}
}
