// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	lodiav1alpha1 "go.lodia.io/lodia/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/cluster-api/util/secret"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"go.lodia.io/cluster-api-provider-lodia/api/v1alpha1"
	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

const sourceUID = "5e1f7c3a-0000-4000-8000-0000000000aa"

var withoutTheUID = map[string]map[string]string{
	"a fresh object": nil,
	"a copy that kept block-move and lost the UID annotation": {clusterctlv1.BlockMoveAnnotation: "true"},
}

var ofAnotherUID = map[string]map[string]string{
	"a copy that kept block-move": {v1alpha1.UIDAnnotation: sourceUID, clusterctlv1.BlockMoveAnnotation: "true"},
	"a copy that lost block-move, as a move after it was removed by hand leaves one": {v1alpha1.UIDAnnotation: sourceUID},
	"an object whose UID annotation was emptied rather than removed":                 {v1alpha1.UIDAnnotation: "", clusterctlv1.BlockMoveAnnotation: "true"},
}

func annotated(annotations map[string]string) func(*clusterv1.Cluster, *v1alpha1.LodiaControlPlane) {
	return func(_ *clusterv1.Cluster, l *v1alpha1.LodiaControlPlane) { l.Annotations = annotations }
}

func untilControlPlane(t *testing.T, r *controller.Reconciler, lcp *v1alpha1.LodiaControlPlane, cp *lodiav1alpha1.ControlPlane) {
	t.Helper()
	for range 6 {
		passes(t, r, lcp)
		err := k8s.Get(t.Context(), client.ObjectKeyFromObject(cp), cp)
		if err == nil {
			return
		}
		if !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
	t.Fatal("no ControlPlane after six passes")
}

func isControlPlaneApply(obj runtime.ApplyConfiguration) bool {
	_, secret := obj.(*corev1ac.SecretApplyConfiguration)
	return !secret
}

func TestTheUIDIsRecordedBeforeTheControlPlaneIsApplied(t *testing.T) {
	for name, annotations := range withoutTheUID {
		t.Run(name, func(t *testing.T) {
			cluster, lcp := create(t, namespace(t), "recorded", annotated(annotations))
			var applied bool
			var recorded string
			r := direct(t, interceptor.Funcs{
				Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					if isControlPlaneApply(obj) {
						stored := &v1alpha1.LodiaControlPlane{}
						if err := k8s.Get(ctx, client.ObjectKeyFromObject(lcp), stored); err != nil {
							return err
						}
						applied, recorded = true, stored.Annotations[v1alpha1.UIDAnnotation]
					}
					return c.Apply(ctx, obj, opts...)
				},
			})

			cp := controlPlaneOf(cluster)
			untilControlPlane(t, r, lcp, cp)
			if !applied || recorded != string(lcp.UID) {
				t.Errorf("the ControlPlane was applied: %t, with %s in the LodiaControlPlane as stored, want its own UID %s", applied, recorded, lcp.UID)
			}
			get(t, lcp)
			if got := lcp.Annotations[v1alpha1.UIDAnnotation]; got != string(lcp.UID) {
				t.Errorf("%s is %q, want the object's UID %s", v1alpha1.UIDAnnotation, got, lcp.UID)
			}
		})
	}
}

func TestAPassWhoseAnnotationPatchFailsAppliesNothing(t *testing.T) {
	for name, annotations := range withoutTheUID {
		t.Run(name, func(t *testing.T) {
			cluster, lcp := create(t, namespace(t), "lost-patch", annotated(annotations))
			healthy := direct(t, interceptor.Funcs{})
			passes(t, healthy, lcp)
			passes(t, healthy, lcp)

			lost := errors.New("etcdserver: leader changed")
			var applied bool
			failing := direct(t, interceptor.Funcs{
				Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					applied = true
					return c.Apply(ctx, obj, opts...)
				},
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return lost
				},
			})
			if err := pass(t.Context(), failing, lcp); !errors.Is(err, lost) {
				t.Fatalf("the pass returned %v", err)
			}
			if applied {
				t.Error("the pass applied something while its annotation patch did not land")
			}
			absent(t, controlPlaneOf(cluster))
			get(t, lcp)
			if got, ok := lcp.Annotations[v1alpha1.UIDAnnotation]; ok {
				t.Errorf("%s is %q after a patch that failed", v1alpha1.UIDAnnotation, got)
			}

			cp := controlPlaneOf(cluster)
			untilControlPlane(t, healthy, lcp, cp)
			get(t, lcp)
			if got := lcp.Annotations[v1alpha1.UIDAnnotation]; got != string(lcp.UID) {
				t.Errorf("%s is %q, want the object's UID %s", v1alpha1.UIDAnnotation, got, lcp.UID)
			}
		})
	}
}

func TestAPassThatReadAStaleCopyDoesNotWriteItsUIDOverAnotherObjects(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "stale", annotated(map[string]string{clusterctlv1.BlockMoveAnnotation: "true"}))
	healthy := direct(t, interceptor.Funcs{})
	passes(t, healthy, lcp)
	passes(t, healthy, lcp)
	get(t, lcp)
	read := lcp.DeepCopy()
	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { l.Annotations[v1alpha1.UIDAnnotation] = sourceUID })

	lagging := direct(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if l, ok := obj.(*v1alpha1.LodiaControlPlane); ok {
				read.DeepCopyInto(l)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if err := pass(t.Context(), lagging, lcp); !apierrors.IsConflict(err) {
		t.Errorf("the pass returned %v, want a conflict", err)
	}
	get(t, lcp)
	if got := lcp.Annotations[v1alpha1.UIDAnnotation]; got != sourceUID {
		t.Errorf("%s is %q after a pass that read a stale copy, want the source's %s kept", v1alpha1.UIDAnnotation, got, sourceUID)
	}
	for range 3 {
		passes(t, healthy, lcp)
	}
	get(t, lcp)
	if got := lcp.Annotations[v1alpha1.UIDAnnotation]; got != sourceUID {
		t.Errorf("%s is %q after the next passes, want the source's %s kept", v1alpha1.UIDAnnotation, got, sourceUID)
	}
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.CopyOfAnotherObjectReason, ""); err != nil {
		t.Error(err)
	}
	absent(t, controlPlaneOf(cluster))
}

func TestBlockMoveIsPutBackWhenItIsRemovedByHand(t *testing.T) {
	_, lcp := create(t, namespace(t), "unblocked")
	r := direct(t, interceptor.Funcs{})
	untilBlockMove(t, r, lcp)
	recorded := lcp.Annotations[v1alpha1.UIDAnnotation]
	if recorded != string(lcp.UID) {
		t.Fatalf("%s is %q, want the object's UID %s", v1alpha1.UIDAnnotation, recorded, lcp.UID)
	}

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { delete(l.Annotations, clusterctlv1.BlockMoveAnnotation) })
	passes(t, r, lcp)
	get(t, lcp)
	if got := lcp.Annotations[clusterctlv1.BlockMoveAnnotation]; got != "true" {
		t.Errorf("%s is %q after a pass, want it put back as \"true\"", clusterctlv1.BlockMoveAnnotation, got)
	}
	if got := lcp.Annotations[v1alpha1.UIDAnnotation]; got != recorded {
		t.Errorf("%s is %q, want it kept as %s", v1alpha1.UIDAnnotation, got, recorded)
	}
}

func TestACopyOfAnotherObjectGetsNeitherAControlPlaneNorSecretsNorARewrittenAnnotation(t *testing.T) {
	for name, kept := range ofAnotherUID {
		for _, leftover := range []bool{false, true} {
			t.Run(name+map[bool]string{false: ", with no ControlPlane of its name", true: ", with a ControlPlane of another owner of its name"}[leftover], func(t *testing.T) {
				ns := namespace(t)
				var previous *lodiav1alpha1.ControlPlane
				if leftover {
					_, previous = leftoverOfAnotherOwner(t, ns, "refused")
				}
				cluster, lcp := create(t, ns, "refused", annotated(kept))
				if leftover {
					lodiaSecrets(t, cluster, ownedBy(previous))
				}
				r := direct(t, interceptor.Funcs{})
				for range 5 {
					passes(t, r, lcp)
				}

				get(t, lcp)
				if diff := cmp.Diff(kept, lcp.Annotations); diff != "" {
					t.Errorf("annotations (-created +now):\n%s", diff)
				}
				if err := ready(lcp, metav1.ConditionFalse, v1alpha1.CopyOfAnotherObjectReason, "copy of another LodiaControlPlane"); err != nil {
					t.Error(err)
				}
				available(t, lcp, metav1.ConditionFalse, v1alpha1.CopyOfAnotherObjectReason)
				if len(lcp.Finalizers) != 0 || condition(lcp, clusterv1.PausedCondition) != nil {
					t.Errorf("the pass wrote more than the condition: finalizers %v, Paused %+v", lcp.Finalizers, condition(lcp, clusterv1.PausedCondition))
				}
				absent(t, secretOf(cluster, secret.Kubeconfig), secretOf(cluster, secret.ClusterCA))
				got := controlPlaneOf(cluster)
				if !leftover {
					absent(t, got)
					return
				}
				get(t, got)
				if got.UID != previous.UID || got.Generation != previous.Generation || !got.DeletionTimestamp.IsZero() || metav1.GetControllerOf(got).UID != metav1.GetControllerOf(previous).UID {
					t.Errorf("the ControlPlane changed: uid %s, generation %d, deletion %v, owners %v", got.UID, got.Generation, got.DeletionTimestamp, got.OwnerReferences)
				}
			})
		}
	}
}

func TestRemovingTheUIDAnnotationOfACopyBuildsAControlPlaneForIt(t *testing.T) {
	cluster, lcp := create(t, namespace(t), "renewed", annotated(ofAnotherUID["a copy that kept block-move"]))
	r := direct(t, interceptor.Funcs{})
	for range 4 {
		passes(t, r, lcp)
	}
	get(t, lcp)
	if err := ready(lcp, metav1.ConditionFalse, v1alpha1.CopyOfAnotherObjectReason, ""); err != nil {
		t.Fatal(err)
	}
	cp := controlPlaneOf(cluster)
	absent(t, cp)

	updateSpec(t, lcp, func(l *v1alpha1.LodiaControlPlane) { delete(l.Annotations, v1alpha1.UIDAnnotation) })
	untilControlPlane(t, r, lcp, cp)
	get(t, lcp)
	if got := lcp.Annotations[v1alpha1.UIDAnnotation]; got != string(lcp.UID) {
		t.Errorf("%s is %q, want the object's UID %s", v1alpha1.UIDAnnotation, got, lcp.UID)
	}
	if !metav1.IsControlledBy(cp, lcp) {
		t.Errorf("the ControlPlane's owners are %v", cp.OwnerReferences)
	}
}

func TestARefusedCopyCanBeDeletedAndLeavesTheControlPlaneOfItsNameAlone(t *testing.T) {
	for name, kept := range ofAnotherUID {
		t.Run(name, func(t *testing.T) {
			ns := namespace(t)
			_, previous := leftoverOfAnotherOwner(t, ns, "deleted")
			cluster, lcp := create(t, ns, "deleted", func(c *clusterv1.Cluster, l *v1alpha1.LodiaControlPlane) {
				annotated(kept)(c, l)
				l.Finalizers = []string{v1alpha1.Finalizer}
			})
			r := direct(t, interceptor.Funcs{})
			for range 3 {
				passes(t, r, lcp)
			}
			get(t, lcp)
			if err := ready(lcp, metav1.ConditionFalse, v1alpha1.CopyOfAnotherObjectReason, ""); err != nil {
				t.Fatal(err)
			}

			if err := k8s.Delete(t.Context(), lcp); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				passes(t, r, lcp)
			}
			absent(t, lcp)
			get(t, cluster)
			got := controlPlaneOf(cluster)
			get(t, got)
			if got.UID != previous.UID || !got.DeletionTimestamp.IsZero() || metav1.GetControllerOf(got).UID != metav1.GetControllerOf(previous).UID {
				t.Errorf("the ControlPlane changed: uid %s, deletion %v, owners %v", got.UID, got.DeletionTimestamp, got.OwnerReferences)
			}
		})
	}
}
