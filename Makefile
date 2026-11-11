ENVTEST_K8S_VERSION ?= 1.35.0
E2E_CLUSTER ?= capl-e2e
E2E_DIR = $(CURDIR)/work/e2e/$(E2E_CLUSTER)
LODIA_DIR = $(CURDIR)/work/lodia
LODIA_COMMIT = 57375832e7fe8b3a06374613797eb4c544fe7612
REMOVE_WORKERS = docker ps --all --quiet --filter label=io.x-k8s.kind.cluster=$(E2E_CLUSTER)-w | xargs -r docker rm --force --volumes
KUSTOMIZE = sigs.k8s.io/kustomize/kustomize/v5@v5.7.0
GENERATED = api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/*.yaml config/rbac/role.yaml

.PHONY: generate
generate: ## regenerate deepcopy functions, the CRDs and the controller's ClusterRole
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	go tool controller-gen rbac:roleName=manager-role paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: lint
lint:
	golangci-lint run ./...
	golangci-lint run --build-tags e2e ./test/e2e/...

.PHONY: test
test: ## unit and envtest tests; envtest runs a real API server and etcd
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" go test ./...

.PHONY: components
components: ## render config/default and check its Namespace, labels, manager container and leader election grant
	@rendered="$$(go run $(KUSTOMIZE) build config/default)" || exit 1; \
	printf '%s\n' "$$rendered" | go run ./test/components

.PHONY: verify
verify: ## the check that says a change is done
	@before="$$(cat $(GENERATED) | cksum)"; $(MAKE) --no-print-directory generate; \
	[ "$$before" = "$$(cat $(GENERATED) | cksum)" ] || { echo "make generate changed the generated files: review and commit them"; exit 1; }
	go mod tidy -diff
	$(MAKE) --no-print-directory components lint test

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
	$(REMOVE_WORKERS) && E2E_KUBECONFIG=$(E2E_DIR)/kubeconfig go test -v -tags e2e -count=1 -timeout 40m ./test/e2e/...; status=$$?; \
	$(REMOVE_WORKERS) || status=1; \
	go tool kind delete cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig; exit $$status
