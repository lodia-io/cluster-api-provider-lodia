# End-to-end test

The suite runs only against a kind cluster whose kubeconfig is named on the command line, in `E2E_KUBECONFIG`, and refuses any other: [`guard.go`](guard.go) wants a single context whose name starts with `kind-capl-e2e` and whose server is a loopback address, as kind writes it, so it never reaches a cluster through the current kube context or one that only carries the name.
The prefix is not Lodia's `lodia-e2e`: this guard refuses Lodia's own e2e cluster, `kind-lodia-e2e-local`, in which the suite would upgrade Lodia, and Lodia's guard refuses this one.
It also refuses to start while a `HELM_KUBE*` variable is set, since helm [lays them over](https://github.com/helm/helm/blob/v4.3.1/pkg/cli/environment.go#L97-L130) `--kubeconfig`.

`make e2e` creates the kind cluster `capl-e2e`, runs the suite and deletes the cluster.
`E2E_CLUSTER` changes the name.
`make e2e` refuses, before it creates anything, a name that does not start with `capl-e2e`, that has a character outside `a-z`, `0-9` and `-`, that ends in `-w` or that is longer than 30 characters.
The prefix is the one the guard wants, and a name ending in `-w` is refused because a run named `x` removes the containers labelled `x-w`, which are also the nodes of a kind cluster named `x-w`.
The Cluster is named `<E2E_CLUSTER>-w`: CAPD [labels](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/internal/docker/util.go#L31) its containers with the Cluster's name in kind's own [label](https://github.com/kubernetes-sigs/kind/blob/v0.33.0/pkg/cluster/internal/providers/docker/constants.go#L21) for the nodes of a cluster, so that name must not be the kind cluster's, and Lodia [requires](https://github.com/lodia-io/lodia/blob/57375832e7fe8b3a06374613797eb4c544fe7612/api/v1alpha1/controlplane_types.go#L17) the name of the `ControlPlane`, the Cluster's, to be a DNS-1035 label of at most 32 characters.
The cluster has one node of kind's default image, with the Docker socket that CAPD needs and the admission plugin `OwnerReferencesPermissionEnforcement`, which [refuses](https://github.com/kubernetes/kubernetes/blob/v1.37.0/plugin/pkg/admission/gc/gc_admission.go#L149) an owner reference with `blockOwnerDeletion` to an object whose finalizers the writer cannot set.
After it creates the cluster and again at the end, `make e2e` removes the containers labelled for the Cluster, with their volumes; on a Linux host, delete the cluster of a killed run first: `go tool kind delete cluster --name capl-e2e --kubeconfig work/e2e/capl-e2e/kubeconfig`.

The suite needs Docker and helm on the `PATH`, and runs where the Docker network `kind` is routable from the test process, as on GitHub's Linux runners: it dials the tenant's API server at a kind node's address on that network.
Docker Desktop does not route it, so the suite does not run on macOS.
Its files are in `work/e2e/<E2E_CLUSTER>`, one directory for each cluster, so that two runs in one checkout share no kubeconfig: the kubeconfig, the release YAMLs and, after a failure, the artifacts.
The clone of Lodia is `work/lodia`.

## What it installs

In this order:

| component | source | installed by |
|---|---|---|
| cert-manager v1.21.3 | its release YAML | the suite, each Deployment waited on until `Available` |
| Lodia and etcd-druid | the chart of the [Lodia commit](https://github.com/lodia-io/lodia/tree/57375832e7fe8b3a06374613797eb4c544fe7612) that `go.mod` pins, fetched by `make e2e-lodia` into `work/lodia`, with images built by ko into the kind cluster | helm, with `--wait` |
| Cluster API and its kubeadm bootstrap provider v1.15.0-beta.0 | their release YAMLs | clusterctl's `Init` |
| Cluster API provider Docker v1.15.0-beta.0 | the release's `infrastructure-components-development.yaml`, whose image is built for the [staging registry](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/Makefile#L1071-L1074) | clusterctl's `Init` |
| this provider | `make release-components`, the target that renders the release's components YAML, with the image repository `kind.local/cluster-api-provider-lodia` and the host's one platform, so that ko loads the image into the kind cluster | clusterctl's `Init` |

Lodia goes in before the providers because the provider's watches need Lodia's CRDs.
The four providers go in through one `Init` of clusterctl's library, with `WaitProviders`, which waits up to 2 minutes for each of their Deployments, on a local repository that the suite lays out in `work/e2e/<E2E_CLUSTER>/repository` as Cluster API's [`CreateRepository`](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/framework/clusterctl/repository.go#L104-L158) does:
- Cluster API's three releases are there under their own version, each with the `metadata.yaml` of that release;
- the provider's components YAML and the tree's `metadata.yaml` are there under `v<major>.<minor>.99` of the tree's newest series, a version no release has;
- cert-manager is marked as externally provisioned, so that clusterctl waits for the one the suite installed and installs none of its own;
- the `LODIA_KUBEADM_BOOTSTRAP_SA_NAMESPACE` and `LODIA_KUBEADM_BOOTSTRAP_SA_NAME` of the provider's grant are in the configuration file the suite writes, with the namespace and name of the real kubeadm bootstrap provider.

clusterctl reads a variable from the environment before its configuration, for every component alike, so the install fails before it starts when the environment sets a variable that one of the four components' YAMLs references, `EXP_*` and `CAPD_*` ones among them.
After `Init`, a SubjectAccessReview asks, for the user and the groups of the ServiceAccount of the installed kubeadm bootstrap provider's Deployment, whether they may get and whether they may update `lodiacontrolplanes`: get has to be allowed and update denied.
The rewrite of a ServiceAccount subject into the provider's namespace, which clusterctl does, and a ClusterRole that grants more than `get` show there.

A release YAML is checked against a sha256 kept in [`install_test.go`](install_test.go), and every `image:` in it gets a digest from the same file; an `image:` without one fails the install, and so does any container or init container image of the decoded documents that has none, whatever the shape of its line.
cert-manager's YAML is rendered with Cluster API's `yamlprocessor` at its defaults and applied by the suite; the three Cluster API YAMLs reach clusterctl with their variables, which it processes.
Every create the suite makes itself, cert-manager's and the testdata's, is retried for a minute on a webhook or x509 error, since a webhook can be `Available` before its certificate is in place, which clusterctl [guards](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/cert_manager.go#L565-L571) against with a throwaway Issuer and Certificate; clusterctl retries its own creates.
kind is a `go tool`; ko and kustomize run through `go run` at pinned versions, which keeps them out of `go.mod` (kustomize's is in the `Makefile`, ko's in the `Makefile` and in `tool_test.go`, and `TestTheSuiteAndTheMakefileRunTheSameKo` fails when they differ); helm v4.3.1 comes from the workflow.

To move a pin, change the version, the sum and the digests together, the sum of `metadata.yaml` with them for Cluster API, and the version in the table above.
Read a digest with `go run github.com/google/go-containerregistry/cmd/crane@v0.22.1 digest <image>` and a sum from the release asset.
Lodia's commit is in two places, the pseudo-version of `go.lodia.io/lodia/api` in `go.mod` and `LODIA_COMMIT` in the `Makefile`; `TestLodiaCommitIsThePinnedAPI` fails when they differ.
From Lodia [81ce021](https://github.com/lodia-io/lodia/blob/81ce021f823c6708ba5eacbc5ffe244c80a3ddaf/.ko.yaml) on, its `.ko.yaml` takes the operator's version from `VERSION` in ko's environment, and ko v0.19.1 stops with `map has no entry for key "VERSION"` without it; the suite sets `VERSION` to the first 12 characters of the commit it cloned for the three builds of Lodia.

## The test

`TestAWorkerJoinsAClusterOfLodiasControlPlane` applies the objects of [`testdata`](testdata) in the namespace `capi-join`, and asserts the steps below.
Each waits within a bound of its own, but the third and the seventh, which read at once, and the last read of the eleventh; the calls between the waits end at their client's request timeout, the kind cluster's or the tenant's, and a `docker` call at a bound of its own, described under "On failure".

1. `Cluster` `ControlPlaneInitialized`.
2. `Cluster.spec.controlPlaneEndpoint` is Lodia's `status.endpoint.apiServer`.
3. `<cluster>-kubeconfig` and `<cluster>-ca` hold the bytes of Lodia's `<cluster>-admin-kubeconfig` and `<cluster>-ca-bundle` under `value` and `tls.crt` and no other key, have the type `cluster.x-k8s.io/secret` and the label `cluster.x-k8s.io/cluster-name`, and the `LodiaControlPlane` controls both.
4. kindnet, from the manifest the node image ships, is applied in the tenant through `<cluster>-kubeconfig`.
5. The `Machine` is `Running` with a `nodeRef`, and `LodiaControlPlane.status.versions` holds the version that the MachineSet's preflight check reads.
6. The tenant's Node has the `DevMachine`'s `providerID`, has lost the `uninitialized` taint and is `Ready`.
7. The worker's container holds the file that its `KubeadmConfigTemplate` renders with `contentFormat: Template` from `{{ .controlPlane.version }}`, and the file holds the `LodiaControlPlane`'s version.
8. The konnectivity agent is ready, and a pod of agnhost, pinned by digest, runs on the Node with its logs readable through the tenant's API server.
9. `Cluster` `Available`.
10. clusterctl's `Move` from the kind cluster to itself fails, and ends within 20 seconds, at its wait for the `block-move` annotation: clusterctl logs that it is blocked on the `LodiaControlPlane`, and the suite ends the move there.
11. After the `Cluster` is unpaused, which the move left it, `Paused` is `False` on the `Cluster` and on the `LodiaControlPlane` within 30 seconds, and the `Cluster` is still `Available`, which the suite reads once the two controllers have run again.
12. Deleting the `Cluster`, once the etcd claim and the two copied Secrets exist and exactly one container is labelled for the `Cluster`, removes the `LodiaControlPlane`, Lodia's `ControlPlane`, the etcd claim, the two Secrets and the worker's container.

The move is the only way the suite tests that the provider refuses `clusterctl move`.
The provider writes `block-move` on every `LodiaControlPlane`, so clusterctl pauses the `Cluster`, finds the annotation and waits for it to go, which with its [backoff](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L366-L377), 5 seconds growing by 1.5 for ten steps, takes about six minutes before it fails.
The error that ends a move is the context's and names no object, so [`MoveUntilBlocked`](../clusterctlproxy/move.go) reads the message that clusterctl logs when it first sees the annotation, requires it to name the `LodiaControlPlane`, and cancels the move there; the move ends at the wait, after the `Cluster` is paused and before anything is created in the target.
A move whose annotation did not hold would [create](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L1060-L1089) each object in the same cluster, which updates it, and then [delete](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L1260-L1314) the source: the suite fails there, on a `Cluster` that it deletes anyway.
The move leaves the `Cluster` paused, as clusterctl leaves it after any failure, since it [unpauses](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/cmd/clusterctl/client/cluster/mover.go#L416-L418) only after it has moved everything, and step 11 unpauses it by dropping `spec.paused`, as clusterctl does, and waits for both controllers to say so: they stop at `Paused`, so `Available` keeps the `True` it had before the move until they run again.
The move's 20 seconds and the 30 of the wait are fields of the bounds, which `TestTheBoundsFitUnderTheTimeouts` adds to the others and holds under `-timeout` and the job's `timeout-minutes`.
`TestUnpausingClearsThePauseOfTheMoveAndWaitsForBothControllers` and `TestUnpausingFailsWhileEitherControllerReportsPaused` hold step 11's wait and checks on a fake client; nothing but a run of the job holds that the step calls them, or step 10.

The `DevCluster` carries the annotation `cluster.x-k8s.io/managed-by`, which Cluster API [reads as an annotation](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/annotations/helpers.go#L38-L41), so that CAPD [leaves it alone](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/reconcilers/devcluster_reconciler.go#L103-L107): otherwise CAPD starts a load balancer, writes its address into the `DevCluster`'s endpoint ([CAPD](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/test/infrastructure/docker/reconcilers/backends/docker/dockercluster_backend.go#L102-L106)), and the Cluster takes it before the control plane's ([Cluster](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/core/reconcilers/cluster/cluster_controller_phases.go#L217-L228)).
The infrastructure contract [gives](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/docs/book/src/developer/providers/contracts/infra-cluster.md?plain=1#L475-L490) the rest to whoever manages such an infrastructure, so the suite patches the one field `DevCluster.status.initialization.provisioned` once.
No assertion reads it.

## On failure

The suite writes to `work/e2e/<E2E_CLUSTER>/artifacts`, and CI uploads it: the objects above with their conditions and Lodia's `Etcd`, the pods, logs and events of cert-manager, of the managers of Cluster API, CAPD, Lodia and the provider, and of the control plane in `capi-join`, through `<cluster>-kubeconfig` once it exists the tenant's Nodes, DaemonSets, and the pods and events of `kube-system` and `default`, and from the worker container `journalctl -u kubelet -u containerd` and `crictl ps -a`.
`make e2e` empties the directory before it starts.
A cancelled job and a `go test` timeout leave no artifacts, since the test binary writes them and neither lets it finish.
The bounds in [`bounds_test.go`](bounds_test.go), the install's and the dump's among them, add up to less than `-timeout`, and `-timeout` to less than the job's `timeout-minutes`, so that a step's own bound fails first; `TestTheBoundsFitUnderTheTimeouts` fails when they do not.
A request to the tenant has a timeout of its own, 30 seconds in the steps and a sixth of the dump's 30 seconds for the tenant in the dump, since controller-runtime's REST mapper [discovers](https://github.com/kubernetes-sigs/controller-runtime/blob/v0.25.2/pkg/client/apiutil/restmapper.go#L285) without the request's context, and a tenant that does not answer would hold a step or the dump past its bound.
A discovery asks `/api`, `/apis` and the version of a group, so a step against such a tenant fails at most three requests after its bound, and the dump stops listing the tenant once its bound has passed, which ends it at most one discovery, half the bound, after it.
The dump's requests to the kind cluster, the pods' logs among them, have a timeout of 10 seconds for the same reason, and the dump stops listing once its own bound has passed, so a kind cluster that does not answer ends it at most one discovery after its bound.
The pods' logs go last, after the tenant: a kubelet that does not answer holds every log request to that timeout, in each container, and would leave the tenant nothing.
The dump reads the worker containers first, within 30 seconds of their own, since `docker` does not depend on the API server: a kind cluster that does not answer uses what remains of the dump's 2 minutes and would leave them nothing, and a `docker` that hangs ends at the 30 seconds and leaves the kind cluster and the tenant the rest.
The install's and the steps' client for the kind cluster has a timeout of 75 seconds on every request, creates included.
That is above the 60 seconds after which kube-apiserver [answers](https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/server/config.go#L453) a request that is not long-running, and it [ignores](https://github.com/kubernetes/kubernetes/blob/v1.37.2/staging/src/k8s.io/apiserver/pkg/endpoints/filters/request_deadline.go#L81-L89) a longer `timeout` query, so the client never cuts a call that the server would answer: a create waits for the webhooks of its object, 30 seconds for cert-manager's, inside those 60.
Its discovery has a timeout of 10 seconds, through a REST mapper of its own, since the mapper discovers without the request's context.
clusterctl's own proxy gives requests and discovery one timeout of 30 seconds, and loads `KUBECONFIG` and `~/.kube/config` with their current context where a call names no kubeconfig, as an empty `FromKubeconfig` of a move does.
The suite gives clusterctl a [Proxy of its own](../clusterctlproxy/proxy.go) instead, over the rest config of the guarded kubeconfig and with the client's two timeouts, for both ends of every call; the factory refuses a kubeconfig that the Proxy would ignore.
`TestInitAndMoveAgainstAClusterThatDoesNotAnswerEndWithinTheirBoundAndOneDiscoveryTimeout` in that package fails a Proxy without the discovery mapper, which overshoots by the request timeout, `TestNoCallReadsAKubeconfigOrTheCurrentContext` a call that falls back to `KUBECONFIG`, and `TestEveryClusterctlClientIsBuiltThroughTheFactory` a clusterctl client that the module builds in any other way.
`TestTheClusterctlClientEndsItsCallsAtTheSuitesTimeouts` holds the same for the suite's own wiring of the two timeouts.
A kind cluster that does not answer ends a wait of a step at most one discovery after its bound, and a call between the waits within a discovery, three requests of 10 seconds, and then the 75 seconds.
A step's context has no deadline, so `docker` ends every call it makes at a bound of its own, 30 seconds, whoever calls it: a `docker` that hangs fails steps 4, 7 and 12 there, and inside the wait of step 12 fails the call, which the next poll repeats.
The dump's workers have a bound over all their calls besides, which ends the call that is running at its end.
`TestADockerCallThatHangsEndsAtItsBound` holds the call's bound for a caller without a deadline and for one with a later deadline, and `TestADockerCallThatHangsEndsWithItsCallersContext` an earlier deadline of its caller.
`TestTheBoundsFitUnderTheTimeouts` holds that three requests, with the tenant's or the dump's timeout, fit in the time left for the calls without a bound, that the client's timeout is above the API server's and fits there after a discovery of three requests, that the four discovery requests that clusterctl asks without a context, the version check and the first call of a client, fit there as well, that a `docker` call's bound fits there too, and that the workers' bound and twice the tenant's are together less than the dump's, so that a `docker` that hangs leaves the kind cluster and the tenant more than twice the tenant's bound.
Go kills a test binary one minute after its `-timeout`, counted from its start, while its own alarm counts from the end of the install.
CAPD does not retry a failed bootstrap; the output of `kubeadm join` is in the `BootstrapCompleted` condition of the `DevMachine`.
