ENVTEST_K8S_VERSION ?= 1.35.0
E2E_CLUSTER ?= capl-e2e
E2E_DIR = $(CURDIR)/work/e2e/$(E2E_CLUSTER)
LODIA_DIR = $(CURDIR)/work/lodia
LODIA_COMMIT = 57375832e7fe8b3a06374613797eb4c544fe7612
REMOVE_WORKERS = docker ps --all --quiet --filter label=io.x-k8s.kind.cluster=$(E2E_CLUSTER)-w | xargs -r docker rm --force --volumes
KUSTOMIZE = sigs.k8s.io/kustomize/kustomize/v5@v5.7.0
KO = github.com/google/ko@v0.19.1
CRANE = go run github.com/google/go-containerregistry/cmd/crane@v0.22.1
IMAGE_REPOSITORY ?= ghcr.io/lodia-io/cluster-api-provider-lodia
PLATFORMS ?= linux/amd64,linux/arm64
KO_PUSH ?= true
IMAGE_TAG ?= latest
COMMIT ?= HEAD
RELEASE_DIR ?= out
GITHUB_REPO = lodia-io/cluster-api-provider-lodia
RELEASE_IMAGE = go run ./hack/releasecheck image --repository=$(IMAGE_REPOSITORY) --tag=$(IMAGE_TAG) $(RELEASE_DIR)/control-plane-components.yaml
RELEASE_WORKFLOW = $(GITHUB_REPO)/.github/workflows/release.yml
VERIFY_IDENTITY = --certificate-identity https://github.com/$(RELEASE_WORKFLOW)@refs/tags/$(IMAGE_TAG) --certificate-oidc-issuer https://token.actions.githubusercontent.com
VERIFY_ATTESTATION = -R $(GITHUB_REPO) --signer-workflow $(RELEASE_WORKFLOW) --source-ref refs/tags/$(IMAGE_TAG)
GENERATED = api/v1alpha1/zz_generated.deepcopy.go api/crds/*.yaml config/crd/bases/*.yaml config/rbac/role.yaml

.PHONY: generate
generate: ## regenerate deepcopy functions, the CRDs in api/crds with the copy kustomize reads in config/crd/bases, and the controller's ClusterRole
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=api/crds
	rm -f config/crd/bases/*.yaml && cp api/crds/*.yaml config/crd/bases/
	go tool controller-gen rbac:roleName=manager-role paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: lint
lint:
	golangci-lint run ./...
	golangci-lint run --build-tags e2e ./test/e2e/...
	cd api && golangci-lint run ./...

.PHONY: test
test: ## unit and envtest tests; envtest runs a real API server and etcd
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" go test ./...
	cd api && go test ./...

.PHONY: components
components: ## render config/default and check that every variable has a non-empty default, its Namespace, labels and manager container as rendered, and, on what clusterctl installs with the defaults and with explicit values, the roles that reach the manager's ServiceAccount, the kubeadm bootstrap provider's grant and the subjects of every binding
	@rendered="$$(go run $(KUSTOMIZE) build config/default)" || exit 1; \
	printf '%s\n' "$$rendered" | go run ./test/components

.PHONY: release-components
release-components: ## write $(RELEASE_DIR)/control-plane-components.yaml: config/default with the manager's image built by ko for $(PLATFORMS) and named by digest in $(IMAGE_REPOSITORY), tagged $(IMAGE_TAG), pushed unless KO_PUSH=false
	@rendered="$$(go run $(KUSTOMIZE) build config/default)" || exit 1; \
	mkdir -p $(RELEASE_DIR) && \
	printf '%s\n' "$$rendered" | KO_DOCKER_REPO=$(IMAGE_REPOSITORY) go run $(KO) resolve --bare --platform=$(PLATFORMS) --push=$(KO_PUSH) --tags=$(IMAGE_TAG) -f - > $(RELEASE_DIR)/control-plane-components.yaml.tmp && \
	mv $(RELEASE_DIR)/control-plane-components.yaml.tmp $(RELEASE_DIR)/control-plane-components.yaml

.PHONY: release-image
release-image: ## print the image that $(RELEASE_DIR)/control-plane-components.yaml names, after requiring it to be the only one, $(IMAGE_REPOSITORY):$(IMAGE_TAG) with a digest
	@$(RELEASE_IMAGE)

.PHONY: release-login
release-login: ## log in to the registry of $(IMAGE_REPOSITORY) as $$GITHUB_ACTOR with $$GITHUB_TOKEN, which ko pushes with
	@printf '%s' "$$GITHUB_TOKEN" | go run $(KO) login $(firstword $(subst /, ,$(IMAGE_REPOSITORY))) --username "$$GITHUB_ACTOR" --password-stdin

.PHONY: release-sign
release-sign: ## sign keyless, with the workflow's OIDC token, the image that release-image prints by its digest, the index and each platform's image, then $(RELEASE_DIR)/control-plane-components.yaml and metadata.yaml, each with a bundle in $(RELEASE_DIR)
	image="$$($(RELEASE_IMAGE))" && cosign sign --yes --recursive $(IMAGE_REPOSITORY)@$${image##*@}
	cosign sign-blob --yes --bundle $(RELEASE_DIR)/control-plane-components.yaml.sigstore.json $(RELEASE_DIR)/control-plane-components.yaml
	cosign sign-blob --yes --bundle $(RELEASE_DIR)/metadata.yaml.sigstore.json metadata.yaml

.PHONY: release-verify-signatures
release-verify-signatures: ## run what a user runs for the tag IMAGE_TAG, with the signer pinned to release.yml at refs/tags/$(IMAGE_TAG): cosign verify of the image's index by its digest and of each image that the index lists, then cosign verify-blob of $(RELEASE_DIR)/control-plane-components.yaml and metadata.yaml
	image="$$($(RELEASE_IMAGE))" && \
	ref=$(IMAGE_REPOSITORY)@$${image##*@} && \
	cosign verify $(VERIFY_IDENTITY) $$ref && \
	manifest="$$($(CRANE) manifest $$ref)" && \
	digests="$$(printf '%s\n' "$$manifest" | jq -r '.manifests[].digest')" && \
	[ -n "$$digests" ] && \
	for digest in $$digests; do cosign verify $(VERIFY_IDENTITY) $(IMAGE_REPOSITORY)@$$digest || exit 1; done
	cosign verify-blob --bundle $(RELEASE_DIR)/control-plane-components.yaml.sigstore.json $(VERIFY_IDENTITY) $(RELEASE_DIR)/control-plane-components.yaml
	cosign verify-blob --bundle $(RELEASE_DIR)/metadata.yaml.sigstore.json $(VERIFY_IDENTITY) metadata.yaml

.PHONY: release-verify-attestations
release-verify-attestations: ## run what a user runs for the tag IMAGE_TAG, with the repository, release.yml and refs/tags/$(IMAGE_TAG) pinned: gh attestation verify of the image by its digest and of $(RELEASE_DIR)/control-plane-components.yaml and metadata.yaml; gh reads $$GH_TOKEN
	image="$$($(RELEASE_IMAGE))" && gh attestation verify oci://$(IMAGE_REPOSITORY)@$${image##*@} $(VERIFY_ATTESTATION)
	gh attestation verify $(RELEASE_DIR)/control-plane-components.yaml $(VERIFY_ATTESTATION)
	gh attestation verify metadata.yaml $(VERIFY_ATTESTATION)

.PHONY: release-check
release-check: ## refuse TAG before it is pushed: a semver tag with a series in metadata.yaml at COMMIT, a series for every tag there is, COMMIT on origin/main with every run of ci.yml green, the local tags TAG and api/TAG at COMMIT; reads GitHub with $$GITHUB_TOKEN, or with gh's
	@[ -n "$(TAG)" ] || { echo "usage: make release-check TAG=vX.Y.Z [COMMIT=<sha>]"; exit 1; }
	git fetch --quiet --tags origin main
	GITHUB_TOKEN="$${GITHUB_TOKEN:-$$(gh auth token)}" go run ./hack/releasecheck tag --commit="$(COMMIT)" "$(TAG)"

.PHONY: metadata
metadata: ## check metadata.yaml: the newest series' contract is the CRDs' contract label, and every series of the merge base with origin/main is still there with its contract
	go run ./hack/releasecheck metadata

.PHONY: verify
verify: ## the check that says a change is done
	@before="$$(cat $(GENERATED) | cksum)"; $(MAKE) --no-print-directory generate || exit 1; \
	[ "$$before" = "$$(cat $(GENERATED) | cksum)" ] || { echo "make generate changed the generated files: review and commit them"; exit 1; }
	go mod tidy -diff
	cd api && go mod tidy -diff
	$(MAKE) --no-print-directory components metadata lint test
	$(MAKE) --no-print-directory release-components release-image KO_PUSH=false IMAGE_TAG=v0.0.0-verify

# LC_ALL=C: a range in a case pattern follows the locale, and on macOS en_US.UTF-8 puts A in a-z.
.PHONY: e2e-cluster
e2e-cluster: ## refuse an E2E_CLUSTER the suite cannot use, before anything is created or removed
	@name='$(E2E_CLUSTER)'; LC_ALL=C; \
	case "$$name" in capl-e2e*) ;; *) echo "E2E_CLUSTER $$name must start with capl-e2e, the prefix the suite's guard accepts"; exit 1;; esac; \
	case "$$name" in *[!a-z0-9-]*) echo "E2E_CLUSTER $$name has a character outside a-z, 0-9 and -: Lodia wants the name of the ControlPlane $$name-w to be a DNS-1035 label"; exit 1;; esac; \
	case "$$name" in *-w) echo "E2E_CLUSTER $$name must not end in -w: the run named $${name%-w} removes the containers labelled $$name, which are the nodes of this kind cluster"; exit 1;; esac; \
	[ $${#name} -le 30 ] || { echo "E2E_CLUSTER $$name has more than 30 characters: Lodia caps the name of the ControlPlane $$name-w at 32"; exit 1; }

.PHONY: e2e-lodia
e2e-lodia: ## fetch Lodia at LODIA_COMMIT, the commit go.mod pins, into $(LODIA_DIR); uses only git and is safe to run twice
	mkdir -p $(LODIA_DIR)
	git -C $(LODIA_DIR) init --quiet
	git -C $(LODIA_DIR) fetch --quiet --depth 1 https://github.com/lodia-io/lodia.git $(LODIA_COMMIT)
	git -C $(LODIA_DIR) checkout --quiet --force --detach $(LODIA_COMMIT)

.PHONY: e2e
e2e: e2e-cluster ## the e2e suite, on a kind cluster of its own that e2e-cluster accepts, that it deletes afterwards with the containers of the Cluster the suite creates
	$(MAKE) --no-print-directory e2e-lodia
	mkdir -p $(E2E_DIR)
	go tool kind create cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig --config test/e2e/testdata/kind.yaml
	rm -rf $(E2E_DIR)/artifacts
	$(REMOVE_WORKERS) && E2E_KUBECONFIG=$(E2E_DIR)/kubeconfig go test -v -tags e2e -count=1 -timeout 64m ./test/e2e/...; status=$$?; \
	$(REMOVE_WORKERS) || status=1; \
	go tool kind delete cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig; exit $$status
