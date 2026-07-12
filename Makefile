IMG ?= ghcr.io/abhi1693/k8s-recommendation-engine:latest
CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KUSTOMIZE ?= $(LOCALBIN)/kustomize
ENVTEST ?= $(LOCALBIN)/setup-envtest
CONTROLLER_TOOLS_VERSION ?= v0.20.1
KUSTOMIZE_VERSION ?= v5.8.1
ENVTEST_VERSION ?= release-0.23
ENVTEST_K8S_VERSION ?= 1.35

.PHONY: all
all: build

.PHONY: manifests
manifests: controller-gen ## Generate CRD and RBAC manifests from Kubebuilder markers.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd:allowDangerousTypes=true paths="./..." output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Generate Kubernetes DeepCopy implementations.
	"$(CONTROLLER_GEN)" object paths="./..."

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: manifests generate fmt vet
	go test ./...

.PHONY: test-integration
test-integration: manifests generate setup-envtest
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test -tags=integration ./internal/controller -run TestControllerManagerReconcilesMultipleApplicationProfiles -count=1

.PHONY: build
build: manifests generate fmt vet
	go build -o bin/k8s-recommendation-engine ./cmd/k8s-recommendation-engine

.PHONY: run
run: manifests generate fmt vet
	go run ./cmd/k8s-recommendation-engine controller

.PHONY: docker-build
docker-build:
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push:
	$(CONTAINER_TOOL) push $(IMG)

.PHONY: install
install: manifests kustomize
	"$(KUSTOMIZE)" build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize
	"$(KUSTOMIZE)" build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests kustomize
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=$(IMG)
	"$(KUSTOMIZE)" build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize
	"$(KUSTOMIZE)" build config/default | kubectl delete --ignore-not-found -f -

$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)

$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: kustomize
kustomize: $(KUSTOMIZE)

$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: setup-envtest
setup-envtest: $(ENVTEST)
	"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path >/dev/null

$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3); \
echo "Downloading $${package}"; \
rm -f "$(1)"; \
GOBIN="$(LOCALBIN)" go install $${package}; \
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)"; \
}; \
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef
