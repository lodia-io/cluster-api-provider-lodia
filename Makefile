ENVTEST_K8S_VERSION ?= 1.35.0
E2E_CLUSTER ?= lodia-e2e-capi
E2E_DIR = $(CURDIR)/work/e2e/$(E2E_CLUSTER)
LODIA_DIR = $(CURDIR)/work/lodia
LODIA_COMMIT = 57375832e7fe8b3a06374613797eb4c544fe7612
REMOVE_WORKERS = docker ps --all --quiet --filter label=io.x-k8s.kind.cluster=$(E2E_CLUSTER)-w | xargs -r docker rm --force --volumes
GENERATED = api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/*.yaml config/rbac/role.yaml

.PHONY: generate
generate: ## regenerate deepcopy functions, the CRDs and the controller's ClusterRole
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	go tool controller-gen rbac:roleName=cluster-api-provider-lodia paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: lint
lint:
	golangci-lint run ./...
	golangci-lint run --build-tags e2e ./test/e2e/...

.PHONY: test
test: ## unit and envtest tests; envtest runs a real API server and etcd
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" go test ./...

.PHONY: verify
verify: ## the check that says a change is done
	@before="$$(cat $(GENERATED) | cksum)"; $(MAKE) --no-print-directory generate; \
	[ "$$before" = "$$(cat $(GENERATED) | cksum)" ] || { echo "make generate changed the generated files: review and commit them"; exit 1; }
	$(MAKE) --no-print-directory lint test

.PHONY: e2e-cluster
e2e-cluster: ## refuse an E2E_CLUSTER the suite cannot use, before anything is created or removed
	@name='$(E2E_CLUSTER)'; \
	case "$$name" in lodia-e2e*) ;; *) echo "E2E_CLUSTER $$name must start with lodia-e2e, the prefix the suite's guard accepts"; exit 1;; esac; \
	case "$$name" in *-w) echo "E2E_CLUSTER $$name must not end in -w: the run named $${name%-w} removes the containers labelled $$name, which are the nodes of this kind cluster"; exit 1;; esac; \
	[ $${#name} -le 30 ] || { echo "E2E_CLUSTER $$name has more than 30 characters: Lodia caps the name of the ControlPlane $$name-w at 32"; exit 1; }

.PHONY: e2e-lodia
e2e-lodia: ## fetch Lodia at LODIA_COMMIT, the commit go.mod pins, into $(LODIA_DIR); uses only git and is safe to run twice
	mkdir -p $(LODIA_DIR)
	git -C $(LODIA_DIR) init --quiet
	git -C $(LODIA_DIR) fetch --quiet --depth 1 https://github.com/lodia-io/lodia.git $(LODIA_COMMIT)
	git -C $(LODIA_DIR) checkout --quiet --force --detach $(LODIA_COMMIT)

.PHONY: e2e
e2e: e2e-cluster e2e-lodia ## the e2e suite, on a kind cluster of its own that e2e-cluster accepts, that it deletes afterwards with the containers of the Cluster the suite creates
	mkdir -p $(E2E_DIR)
	go tool kind create cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig --config test/e2e/testdata/kind.yaml
	rm -rf $(E2E_DIR)/artifacts
	$(REMOVE_WORKERS) && E2E_KUBECONFIG=$(E2E_DIR)/kubeconfig go test -v -tags e2e -count=1 -timeout 40m ./test/e2e/...; status=$$?; \
	$(REMOVE_WORKERS) || status=1; \
	go tool kind delete cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig; exit $$status
