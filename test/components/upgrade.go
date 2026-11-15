// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.lodia.io/cluster-api-provider-lodia/test/pin"
)

const (
	namespaceKind = "Namespace"
	crdKind       = "CustomResourceDefinition"
	bindingKind   = "ClusterRoleBinding"
)

func upgrade(ctx context.Context, r io.Reader, release string, components func(context.Context) ([]byte, error)) error {
	rendered, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	tree, err := decodeObjects(rendered)
	if err != nil {
		return err
	}
	raw, err := components(ctx)
	if err != nil {
		return err
	}
	previous, err := decodeObjects(raw)
	if err != nil {
		return fmt.Errorf("the components of %s: %w", release, err)
	}
	return requireUpgradable(release, previous, tree)
}

func fromThePin(dir string) (release string, components func(context.Context) ([]byte, error), err error) {
	pinned, err := pin.Previous()
	if err != nil {
		return "", nil, err
	}
	return pinned.Version, func(ctx context.Context) ([]byte, error) {
		return pin.Fetch(ctx, filepath.Join(dir, pinned.Version), pinned.ComponentsURL(), pinned.Components)
	}, nil
}

type objectID struct{ kind, name string }

func (o objectID) String() string { return o.kind + " " + o.name }

func idOf(obj *unstructured.Unstructured) objectID { return objectID{obj.GetKind(), obj.GetName()} }

// clusterctlKeeps reports whether the upgrade leaves an object of a release in
// the cluster. It deletes the namespaced objects of the provider and, of the
// cluster-scoped ones, the webhook configurations and those whose name starts
// with the provider's namespace and a dash, and keeps the Namespace, the CRDs
// and every other, which it then merge-patches with the new release's.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader.go#L468-L473
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/components.go#L153-L199
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/components.go#L122-L128
func clusterctlKeeps(obj *unstructured.Unstructured, namespace string) bool {
	switch kind := obj.GetKind(); {
	case !slices.Contains(clusterScoped, kind):
		return false
	case kind == namespaceKind || kind == crdKind:
		return true
	case strings.HasSuffix(kind, "WebhookConfiguration"):
		return false
	}
	return !strings.HasPrefix(obj.GetName(), namespace+"-")
}

// requireUpgradable names what the upgrade from a release leaves behind or
// fails on. An object that the release has and the render lacks stays in every
// management cluster that was upgraded. A ClusterRoleBinding that the render
// gives another roleRef fails the patch, since a roleRef is immutable:
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/apis/rbac/validation/validation.go#L206
// A CRD that lacks a version that the release serves is no longer served at it,
// and the API server refuses the patch of a CRD that lacks a version of its
// status.storedVersions:
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation/validation.go#L263-L287
func requireUpgradable(release string, previous, tree []*unstructured.Unstructured) error {
	var namespaces []string
	for _, obj := range previous {
		if obj.GetKind() == namespaceKind {
			namespaces = append(namespaces, obj.GetName())
		}
	}
	if len(namespaces) != 1 {
		return fmt.Errorf("the components of %s hold %d Namespaces, clusterctl wants one", release, len(namespaces))
	}
	rendered := make(map[objectID]*unstructured.Unstructured, len(tree))
	for _, obj := range tree {
		rendered[idOf(obj)] = obj
	}
	var errs []error
	for _, was := range previous {
		if !clusterctlKeeps(was, namespaces[0]) {
			continue
		}
		now, ok := rendered[idOf(was)]
		if !ok {
			errs = append(errs, fmt.Errorf("%s of %s is not in the render of config/default: clusterctl's upgrade leaves it in every management cluster it upgrades, renamed or dropped", idOf(was), release))
			continue
		}
		switch was.GetKind() {
		case bindingKind:
			errs = append(errs, requireSameRoleRef(release, was, now))
		case crdKind:
			errs = append(errs, requireServedVersions(release, was, now))
		}
	}
	return errors.Join(errs...)
}

func requireSameRoleRef(release string, was, now *unstructured.Unstructured) error {
	before, _, _ := unstructured.NestedStringMap(was.Object, "roleRef")
	after, _, _ := unstructured.NestedStringMap(now.Object, "roleRef")
	if maps.Equal(before, after) {
		return nil
	}
	return fmt.Errorf("%s has the roleRef %s in %s and %s in the render of config/default: a roleRef is immutable, and clusterctl's upgrade patches the binding it keeps, so it fails", idOf(now), roleRef(before), release, roleRef(after))
}

func roleRef(fields map[string]string) string {
	return fmt.Sprintf("%s %s (%s)", fields["kind"], fields["name"], fields["apiGroup"])
}

func requireServedVersions(release string, was, now *unstructured.Unstructured) error {
	var dropped []string
	for _, version := range crdVersions(was) {
		if version.served && !slices.ContainsFunc(crdVersions(now), func(v crdVersion) bool { return v.name == version.name }) {
			dropped = append(dropped, version.name)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	return fmt.Errorf("%s serves %s in %s and not in the render of config/default: clusterctl's upgrade keeps the CRD, and the objects stored at a version it no longer lists", idOf(now), strings.Join(dropped, ", "), release)
}

type crdVersion struct {
	name   string
	served bool
}

func crdVersions(crd *unstructured.Unstructured) []crdVersion {
	items, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	var versions []crdVersion
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := fields["name"].(string)
		served, _ := fields["served"].(bool)
		versions = append(versions, crdVersion{name, served})
	}
	return versions
}
