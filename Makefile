ENVTEST_K8S_VERSION ?= 1.35.0
GENERATED = api/v1alpha1/zz_generated.deepcopy.go config/crd/bases/*.yaml config/rbac/role.yaml

.PHONY: generate
generate: ## regenerate deepcopy functions, the CRDs and the controller's ClusterRole
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	go tool controller-gen rbac:roleName=cluster-api-provider-lodia paths=./internal/... output:rbac:artifacts:config=config/rbac

.PHONY: lint
lint:
	golangci-lint run ./...

.PHONY: test
test: ## unit and envtest tests; envtest runs a real API server and etcd
	KUBEBUILDER_ASSETS="$$(go tool setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" go test ./...

.PHONY: verify
verify: ## the check that says a change is done
	@before="$$(cat $(GENERATED) | cksum)"; $(MAKE) --no-print-directory generate; \
	[ "$$before" = "$$(cat $(GENERATED) | cksum)" ] || { echo "make generate changed the generated files: review and commit them"; exit 1; }
	$(MAKE) --no-print-directory lint test
