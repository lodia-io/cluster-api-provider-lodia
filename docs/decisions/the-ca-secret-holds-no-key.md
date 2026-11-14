# The Secret <cluster>-ca holds the cluster CA's certificate and never its key

Decided 2026-10-26.

## Decision

- `<cluster>-ca` carries `tls.crt`, the `ca.crt` of Lodia's `<cp>-ca-bundle`, and no `tls.key`, and never a placeholder key.
- The key stays in Lodia's `<cp>-cakey-cluster`, which belongs to PKI alone and is never mounted.

## Why

- A worker needs no key: Cluster API's kubeadm bootstrap provider (CABPK) hashes the certificates in `tls.crt` ([what Cluster API needs](../README.md#the-ca-secret)).
- Lodia's `<cp>-ca-bundle` has the cluster CA under `ca.crt`, two during a trust anchor rotation, and the key lives in `<cp>-cakey-cluster`, PKI's alone ([README][lo-readme-secrets]).
- CABPK's cache keeps the data of every labelled `-ca` Secret ([CABPK cache][cabpk-cache]), so the trust anchor's key would sit in another controller's memory and in a Secret every reader of the namespace's Secrets can read.

## Rejected

- A placeholder `tls.key` — a workaround, which the provider replaces by requiring the Cluster API release that carries the fix.

## Consequences

- A worker joins with such a Secret only since [kubernetes-sigs/cluster-api#14389](https://github.com/kubernetes-sigs/cluster-api/pull/14389), merged to `main` as [bccfd69](https://github.com/kubernetes-sigs/cluster-api/commit/bccfd69caf12e9703c2711d6d961ff99d6c0ebc3): `EnsureAllExist` asks for the key only of a certificate that has a key file, and a worker's CA certificate has none ([v1.15.0-beta.0](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/secret/certificates.go#L273-L288), [worker certificates](https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/util/secret/certificates.go#L189-L200)).
- v1.14.4, the newest stable release on 2026-11-13, asks for the key of every certificate that is not external, so the kubeadm bootstrap provider fails the join of every worker with `missing key data`, and no worker gets bootstrap data ([`EnsureAllExist`](https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.4/util/secret/certificates.go#L272-L287), [the join](https://github.com/kubernetes-sigs/cluster-api/blob/v1.14.4/bootstrap/kubeadm/reconcilers/kubeadmconfig/kubeadmconfig_controller.go#L653-L679)).
  The change is on `main` and in v1.15.0-beta.0 only: no backport to the v1.14 line existed on 2026-11-13.
- A release of the provider needs Cluster API v1.15.0-beta.0, for the core provider and for the kubeadm bootstrap provider, until the provider's pin moves to v1.15.0 ([`todo.md`](../../todo.md)).

## Code

- `internal/controller/lodiacontrolplane.go` — `copySecrets` writes the two copies, the CA's with `tls.crt` alone.
- `internal/controller/contract_test.go` — `TestCABPKAcceptsTheCASecretWithoutAKey` calls `NewCertificatesForWorker`, `Lookup` and `EnsureAllExist` on the Secret the provider writes; `TestAClusterGetsItsControlPlaneAndTheContract` holds that no Secret it writes holds a private key.

<!-- Cluster API v1.15.0-beta.0 -->
[cabpk-cache]: https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/bootstrap/kubeadm/setup/setup.go#L48-L75

<!-- Lodia at 5f212ed -->
[lo-readme-secrets]: https://github.com/lodia-io/lodia/blob/5f212ed9acd71fe2a944e5fe63cad73e3d17812e/docs/README.md?plain=1#L151-L163
