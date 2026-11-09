# End-to-end test

The suite runs only against a kind cluster whose kubeconfig is named on the command line, in `E2E_KUBECONFIG`, and refuses any other: [`guard.go`](guard.go) wants a single context whose name starts with `kind-lodia-e2e`, so it never reaches a cluster through the current kube context.
It also refuses to start while a `HELM_KUBE*` variable is set, since helm [lays them over](https://github.com/helm/helm/blob/v4.3.1/pkg/cli/environment.go#L97-L130) `--kubeconfig`.

`make e2e` creates the kind cluster `lodia-e2e-capi`, runs the suite and deletes the cluster.
`E2E_CLUSTER` changes the name.
`make e2e` refuses, before it creates anything, a name that does not start with `lodia-e2e`, that ends in `-w` or that is longer than 30 characters.
The prefix is the one the guard wants, and a name ending in `-w` is refused because a run named `x` removes the containers labelled `x-w`, which are also the nodes of a kind cluster named `x-w`.
The Cluster is named `<E2E_CLUSTER>-w`: CAPD [labels](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/internal/docker/util.go#L31) its containers with the Cluster's name in kind's own [label](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/cluster/internal/providers/docker/constants.go#L21) for the nodes of a cluster, so that name must not be the kind cluster's, and Lodia [caps](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/v1alpha1/controlplane_types.go#L17) the name of the `ControlPlane`, the Cluster's, at 32 characters.
The cluster has one node of kind's default image, with the Docker socket that CAPD needs and the admission plugin `OwnerReferencesPermissionEnforcement`, which [refuses](https://github.com/kubernetes/kubernetes/blob/v1.37.0/plugin/pkg/admission/gc/gc_admission.go#L149) an owner reference with `blockOwnerDeletion` to an object whose finalizers the writer cannot set.
After it creates the cluster and again at the end, `make e2e` removes the containers labelled for the Cluster, with their volumes; on a Linux host, delete the cluster of a killed run first: `go tool kind delete cluster --name lodia-e2e-capi --kubeconfig work/e2e/lodia-e2e-capi/kubeconfig`.

The suite needs Docker and helm on the `PATH`, and runs where the Docker network `kind` is routable from the test process, as on GitHub's Linux runners: it dials the tenant's API server at a kind node's address on that network.
Docker Desktop does not route it, so the suite does not run on macOS.
Its files are in `work/e2e/<E2E_CLUSTER>`, one directory for each cluster, so that two runs in one checkout share no kubeconfig: the kubeconfig, the release YAMLs and, after a failure, the artifacts.
The clone of Lodia is `work/lodia`.

## What it installs

In this order, each Deployment waited on until `Available`:

| component | source |
|---|---|
| cert-manager v1.21.3 | its release YAML |
| Cluster API and its kubeadm bootstrap provider v1.15.0-beta.0 | their release YAMLs |
| Cluster API provider Docker v1.15.0-beta.0 | the release's `infrastructure-components-development.yaml`, whose image is built for the [staging registry](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/Makefile#L1071-L1074) |
| Lodia and etcd-druid | the chart of the [Lodia commit](https://github.com/lodia-io/lodia/tree/57375832e7fe8b3a06374613797eb4c544fe7612) that `go.mod` pins, fetched by `make e2e-lodia` into `work/lodia`, with images built by ko into the kind cluster |
| this provider | `config/default`, built by kustomize and ko into the kind cluster |

A release YAML is checked against a sha256 kept in [`install_test.go`](install_test.go), rendered with Cluster API's `yamlprocessor`, and every `image:` in it gets a digest from the same file; an `image:` without one fails the install.
Every create is retried for a minute on a webhook or x509 error, since a webhook can be `Available` before its certificate is in place, which clusterctl [guards](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/cert_manager.go#L565-L571) against with a throwaway Issuer and Certificate.
kind is a `go tool`; ko and kustomize run through `go run` at pinned versions, which keeps them out of `go.mod`; helm v4.3.1 comes from the workflow.

To move a pin, change the version, the sum and the digests together.
Read a digest with `go run github.com/google/go-containerregistry/cmd/crane@v0.22.1 digest <image>` and a sum from the release asset.
Lodia's commit is in two places, the pseudo-version of `go.lodia.io/lodia/api` in `go.mod` and `LODIA_COMMIT` in the `Makefile`; `TestLodiaCommitIsThePinnedAPI` fails when they differ.

## The test

`TestAWorkerJoinsAClusterOfLodiasControlPlane` applies the objects of [`testdata`](testdata) in the namespace `capi-join`, and asserts, each within a bound of its own:

1. `Cluster` `ControlPlaneInitialized`.
2. `Cluster.spec.controlPlaneEndpoint` is Lodia's `status.endpoint.apiServer`.
3. `<cluster>-kubeconfig` and `<cluster>-ca` exist, the CA's Secret holds `tls.crt` and no `tls.key`, and the `LodiaControlPlane` controls both.
4. kindnet, from the manifest the node image ships, is applied in the tenant through `<cluster>-kubeconfig`.
5. The `Machine` is `Running` with a `nodeRef`, and `LodiaControlPlane.status.versions` holds the version that the MachineSet's preflight check reads.
6. The tenant's Node has the `DevMachine`'s `providerID`, has lost the `uninitialized` taint and is `Ready`.
7. The konnectivity agent is ready, and a pod of agnhost, pinned by digest, runs on the Node with its logs readable through the tenant's API server.
8. `Cluster` `Available`.
9. Deleting the `Cluster`, once the etcd claim exists and exactly one container is labelled for the `Cluster`, removes the `LodiaControlPlane`, Lodia's `ControlPlane`, the etcd claim and the worker's container.

The `DevCluster` carries the annotation `cluster.x-k8s.io/managed-by`, which Cluster API [reads as an annotation](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/annotations/helpers.go#L38-L41), so that CAPD [leaves it alone](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/reconcilers/devcluster_reconciler.go#L103-L107): otherwise CAPD starts a load balancer, writes its address into the `DevCluster`'s endpoint ([CAPD](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/reconcilers/backends/docker/dockercluster_backend.go#L102-L106)), and the Cluster takes it before the control plane's ([Cluster](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_phases.go#L217-L228)).
The infrastructure contract [gives](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/infra-cluster.md?plain=1#L475-L490) the rest to whoever manages such an infrastructure, so the suite patches the one field `DevCluster.status.initialization.provisioned` once.
No assertion reads it.

## On failure

The suite writes to `work/e2e/<E2E_CLUSTER>/artifacts`, and CI uploads it: the objects above with their conditions and Lodia's `Etcd`, the pods and logs of cert-manager, of the managers of Cluster API, CAPD, Lodia and the provider, and of the control plane in `capi-join`, and from the worker container `journalctl -u kubelet -u containerd` and `crictl ps -a`.
`make e2e` empties the directory before it starts.
A cancelled job leaves no artifacts, since the test binary writes them and the cancel kills it.
CAPD does not retry a failed bootstrap; the output of `kubeadm join` is in the `BootstrapCompleted` condition of the `DevMachine`.
