ENVTEST_K8S_VERSION ?= 1.35.0
E2E_CLUSTER ?= lodia-e2e-capi
E2E_DIR ?= $(CURDIR)/work/e2e
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

.PHONY: e2e-lodia
e2e-lodia: ## fetch Lodia at LODIA_COMMIT, the commit go.mod pins, into $(E2E_DIR)/lodia; uses only git and is safe to run twice
	mkdir -p $(E2E_DIR)/lodia
	git -C $(E2E_DIR)/lodia init --quiet
	git -C $(E2E_DIR)/lodia fetch --quiet --depth 1 https://github.com/lodia-io/lodia.git $(LODIA_COMMIT)
	git -C $(E2E_DIR)/lodia checkout --quiet --force --detach $(LODIA_COMMIT)

.PHONY: e2e-capi
e2e-capi:
	mkdir -p $(E2E_DIR)/cluster-api
	git -C $(E2E_DIR)/cluster-api init --quiet
	git -C $(E2E_DIR)/cluster-api fetch --quiet --depth 1 https://github.com/kubernetes-sigs/cluster-api.git refs/tags/v1.15.0-beta.0
	git -C $(E2E_DIR)/cluster-api checkout --quiet --force --detach FETCH_HEAD
	if [ -f test/e2e/testdata/cabpk-fix.patch ]; then git -C $(E2E_DIR)/cluster-api apply $(CURDIR)/test/e2e/testdata/cabpk-fix.patch; fi
	git -C $(E2E_DIR)/cluster-api diff --stat

.PHONY: e2e
e2e: e2e-lodia e2e-capi ## the e2e suite, on a kind cluster of its own, E2E_CLUSTER of at most 30 characters, that it deletes afterwards with the containers of the Cluster the suite creates
	mkdir -p $(E2E_DIR)
	go tool kind create cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig --config test/e2e/testdata/kind.yaml
	$(REMOVE_WORKERS) && E2E_KUBECONFIG=$(E2E_DIR)/kubeconfig go test -v -tags e2e -count=1 -timeout 45m ./test/e2e/...; status=$$?; \
	$(REMOVE_WORKERS) || status=1; \
	go tool kind delete cluster --name $(E2E_CLUSTER) --kubeconfig $(E2E_DIR)/kubeconfig; exit $$status
