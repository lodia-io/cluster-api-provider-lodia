// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Command components reads the rendered config/default on its standard input
// and fails unless it meets what clusterctl asks of a components YAML
// (https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/clusterctl.md#components-yaml):
// one Namespace, a Deployment whose container is named manager, and the label
// cluster.x-k8s.io/provider on every object.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const providerLabel = "cluster.x-k8s.io/provider"

func main() {
	if err := check(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(r io.Reader) error {
	var errs []error
	namespaces, managers := 0, 0
	decoder := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if len(obj.Object) == 0 {
			continue
		}
		if _, ok := obj.GetLabels()[providerLabel]; !ok {
			errs = append(errs, fmt.Errorf("%s %s has no label %s", obj.GetKind(), obj.GetName(), providerLabel))
		}
		switch obj.GetKind() {
		case "Namespace":
			namespaces++
		case "Deployment":
			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			for _, c := range containers {
				if container, ok := c.(map[string]any); ok && container["name"] == "manager" {
					managers++
				}
			}
		}
	}
	if namespaces != 1 {
		errs = append(errs, fmt.Errorf("config/default renders %d Namespaces, clusterctl wants one", namespaces))
	}
	if managers == 0 {
		errs = append(errs, errors.New("config/default renders no Deployment with a container named manager, which clusterctl looks for"))
	}
	return errors.Join(errs...)
}
