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
	namespaceKind   = "Namespace"
	crdKind         = "CustomResourceDefinition"
	bindingKind     = "ClusterRoleBinding"
	clusterAPIGroup = "cluster.x-k8s.io"
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

type previousRelease func(dir string) (release string, components func(context.Context) ([]byte, error), err error)

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
// the cluster. It reads the new components with the inventory's namespace, so
// that the Namespace is reused and nothing is left in another. It deletes the
// namespaced objects of the provider and, of the cluster-scoped ones, the
// webhook configurations and those whose name starts with the provider's
// namespace and a dash. It keeps the CRDs, the other cluster-scoped objects
// and the custom resources of the CRDs that carry the provider label, which it
// does not list, and merge-patches them with the new release's.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader.go#L364-L367
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader.go#L468-L473
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/components.go#L153-L199
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/components.go#L122-L128
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/proxy.go#L245-L263
func clusterctlKeeps(obj *unstructured.Unstructured, namespace string, labelledGroups []string) bool {
	switch kind := obj.GetKind(); {
	case kind == namespaceKind:
		return false
	case isLabelledCustomResource(obj, labelledGroups):
		return true
	case !slices.Contains(clusterScoped, kind):
		return false
	case kind == crdKind:
		return true
	case strings.HasSuffix(kind, "WebhookConfiguration"):
		return false
	}
	return !strings.HasPrefix(obj.GetName(), namespace+"-")
}

// isLabelledCustomResource reports whether the object is of a CRD that carries
// the provider label, which clusterctl adds to everything it installs:
// the release's own CRDs and Cluster API's, whose group is cluster.x-k8s.io or
// a subdomain of it. The CRDs of other providers are not known here.
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/repository/components.go#L544-L564
func isLabelledCustomResource(obj *unstructured.Unstructured, labelledGroups []string) bool {
	group := obj.GroupVersionKind().Group
	return slices.Contains(labelledGroups, group) || group == clusterAPIGroup || strings.HasSuffix(group, "."+clusterAPIGroup)
}

func groupsOfTheCRDs(objs []*unstructured.Unstructured) []string {
	var groups []string
	for _, obj := range objs {
		if obj.GetKind() != crdKind {
			continue
		}
		if group, _, _ := unstructured.NestedString(obj.Object, "spec", "group"); group != "" {
			groups = append(groups, group)
		}
	}
	return groups
}

// requireUpgradable names what the upgrade from a release leaves behind or
// fails on. An object that the release has and the render lacks stays in every
// management cluster that was upgraded. A ClusterRoleBinding that the render
// gives another roleRef fails the patch, since a roleRef is immutable:
// https://github.com/kubernetes/kubernetes/blob/v1.37.2/pkg/apis/rbac/validation/validation.go#L206
// A CRD version that the release serves and the render does not, whether it
// lacks it or serves it no more, is one that clusterctl counts and refuses the
// upgrade for wherever a ClusterClass names it, and the API server refuses the
// patch of a CRD that lacks a version of its status.storedVersions, of which
// the release's storage version is one:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/upgrader.go#L660-L676
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
	labelledGroups := groupsOfTheCRDs(previous)
	var errs []error
	for _, was := range previous {
		if !clusterctlKeeps(was, namespaces[0], labelledGroups) {
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
	rendered := crdVersions(now)
	var stopped []string
	var errs []error
	for _, version := range crdVersions(was) {
		if version.served && !slices.ContainsFunc(rendered, func(v crdVersion) bool { return v.name == version.name && v.served }) {
			stopped = append(stopped, version.name)
		}
		if version.storage && !slices.ContainsFunc(rendered, func(v crdVersion) bool { return v.name == version.name }) {
			errs = append(errs, fmt.Errorf("%s has the storage version %s in %s and the render of config/default does not list it: the API server refuses the patch of a CRD whose status.storedVersions names a version that spec.versions lacks", idOf(now), version.name, release))
		}
	}
	if len(stopped) > 0 {
		errs = append(errs, fmt.Errorf("%s serves %s in %s and not in the render of config/default: clusterctl's upgrade keeps the CRD, so the version stops being served in every management cluster, and it refuses the upgrade where a ClusterClass names it", idOf(now), strings.Join(stopped, ", "), release))
	}
	return errors.Join(errs...)
}

type crdVersion struct {
	name            string
	served, storage bool
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
		storage, _ := fields["storage"].(bool)
		versions = append(versions, crdVersion{name, served, storage})
	}
	return versions
}
