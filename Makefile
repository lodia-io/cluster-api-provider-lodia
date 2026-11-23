ENVTEST_K8S_VERSION ?= 1.35.0
E2E_CLUSTER ?= capl-e2e
E2E_UPGRADE_CLUSTER ?= capl-e2e-upgrade
E2E_MINOR_CLUSTER ?= capl-e2e-minor
E2E_DIR = $(CURDIR)/work/e2e/$(E2E_CLUSTER)
LODIA_DIR = $(CURDIR)/work/lodia
PIN_DIR = $(CURDIR)/work/pin
RENDER = $(CURDIR)/work/default.yaml
LODIA_COMMIT = 4bb6047b2c8222d7566f8fd64e96214a4ce2c838
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
GENERATED = api/v1alpha1/zz_generated.deepcopy.go api/crds/*.yaml config/rbac/role.yaml

.PHONY: generate
generate: ## regenerate deepcopy functions, the CRDs in api/crds, and the controller's ClusterRole
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=api/crds
	go tool controller-gen rbac:roleName=manager-role paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: lint
lint:
	golangci-lint run ./...
	golangci-lint run --build-tags e2e ./test/e2e/...
	golangci-lint run --build-tags e2e,upgrade ./test/e2e/...
	golangci-lint run --build-tags e2e,minor ./test/e2e/...
	cd api && golangci-lint run ./...

.PHONY: test
test: ## unit and envtest tests; envtest runs a real API server and etcd
	assets="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" && KUBEBUILDER_ASSETS="$$assets" go test ./...
	cd api && go test ./...

.PHONY: render
render: ## render config/default into $(RENDER), which the targets that read the render take, so that a failed kustomize stops them before they read anything
	mkdir -p $(dir $(RENDER))
	go run $(KUSTOMIZE) build config/default > $(RENDER).tmp
	mv $(RENDER).tmp $(RENDER)

.PHONY: components
components: render ## check the render of config/default: every variable has a non-empty default, its Namespace, labels and manager container as rendered, and, on what clusterctl installs with the defaults and with explicit values, the roles that reach the manager's ServiceAccount, the kubeadm bootstrap provider's grant and the subjects of every binding
	go run ./test/components < $(RENDER)

.PHONY: upgrade-components
upgrade-components: render ## compare the render of config/default with the components of the release that test/pin names, which the first run downloads into $(PIN_DIR) and checks against the pinned sha256: fail on a cluster-scoped object of the release, or a custom resource of a CRD that carries the provider label, that clusterctl's upgrade keeps and the render drops or renames, a ClusterRoleBinding whose roleRef the render changes, a CRD version that the release serves and the render drops or does not serve, and a storage version that the render drops
	go run ./test/components upgrade $(PIN_DIR) < $(RENDER)

.PHONY: release-components
release-components: render ## write $(RELEASE_DIR)/control-plane-components.yaml: config/default with the manager's image built by ko for $(PLATFORMS) and named by digest in $(IMAGE_REPOSITORY), tagged $(IMAGE_TAG), pushed unless KO_PUSH=false
	mkdir -p $(RELEASE_DIR)
	KO_DOCKER_REPO=$(IMAGE_REPOSITORY) go run $(KO) resolve --bare --platform=$(PLATFORMS) --push=$(KO_PUSH) --tags=$(IMAGE_TAG) -f $(RENDER) > $(RELEASE_DIR)/control-plane-components.yaml.tmp
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
release-check: ## refuse TAG before it is pushed: a semver tag with a series in metadata.yaml at COMMIT, a series for every tag there is, TAG above every published release of its line, COMMIT on origin/main with every run of ci.yml green, the local tags TAG and api/TAG at COMMIT, the pin of the upgrade e2e at COMMIT naming the newest published release below TAG; reads GitHub with $$GITHUB_TOKEN, or with gh's
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
	$(MAKE) --no-print-directory components upgrade-components metadata lint test verify-release

# A variable on the command line of verify's make would reach, through MAKEFLAGS and the environment, every make that the tests of make test run.
.PHONY: verify-release
verify-release: KO_PUSH = false
verify-release: IMAGE_TAG = v0.0.0-verify
verify-release: release-components release-image ## the release's render and the check of its image, with no push

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

e2e: E2E_TAGS = e2e
e2e: E2E_TIMEOUT = 64m
e2e: E2E_RUN =
e2e-upgrade: E2E_CLUSTER = $(E2E_UPGRADE_CLUSTER)
e2e-upgrade: E2E_TAGS = e2e,upgrade
e2e-upgrade: E2E_TIMEOUT = 46m
e2e-upgrade: E2E_RUN = -run '^TestTheProviderUpgradesFromThePreviousRelease$$'
e2e-minor: E2E_CLUSTER = $(E2E_MINOR_CLUSTER)
e2e-minor: E2E_TAGS = e2e,minor
e2e-minor: E2E_TIMEOUT = 47m
e2e-minor: E2E_RUN = -run '^TestAMinorStepHoldsNewWorkersUntilLodiaRunsIt$$'

.PHONY: e2e e2e-upgrade e2e-minor
e2e e2e-upgrade e2e-minor: e2e-cluster ## the suite of the target, with the tags, -timeout and -run of its variables, on a kind cluster of its own that e2e-cluster accepts, which it deletes afterwards with the containers of the Cluster the suite creates
	$(MAKE) --no-print-directory e2e-lodia
	mkdir -p $(E2E_DIR)
	go tool kind create cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig --config test/e2e/testdata/kind.yaml
	rm -rf $(E2E_DIR)/artifacts
	$(REMOVE_WORKERS) && E2E_KUBECONFIG=$(E2E_DIR)/kubeconfig go test -v -tags $(E2E_TAGS) -count=1 -timeout $(E2E_TIMEOUT) $(E2E_RUN) ./test/e2e/...; status=$$?; \
	$(REMOVE_WORKERS) || status=1; \
	go tool kind delete cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig; exit $$status
