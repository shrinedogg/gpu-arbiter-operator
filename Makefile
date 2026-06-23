# Image URL to use all building/pushing image targets (Docker Hub).
IMG ?= shrinedogg/gpu-arbiter-operator:latest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION ?= 1.36.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set).
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows the execution of commands with && to run even on systems where /bin/sh is symlinked to dash.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their headings.
.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRD, RBAC, and webhook manifests.
	$(CONTROLLER_GEN) crd:crdVersions=v1 rbac:roleName=manager-role paths="./api/..." paths="./internal/controller/..." output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Generate deepcopy, clientset, and other code.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager ./cmd/manager

.PHONY: run
run: manifests generate ## Run a controller against the current cluster using your kubeconfig.
	go run ./cmd/manager

.PHONY: test
test: manifests generate ## Run tests.
	go test ./... -coverprofile cover.out

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the cluster.
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the cluster.
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone config/crd | kubectl delete -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the cluster (build image first if needed).
	cd config/manager && $(KUSTOMIZE) edit set image gpu-arbiter-operator=${IMG}
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the cluster.
	$(KUSTOMIZE) build --load-restrictor LoadRestrictionsNone config/default | kubectl delete -f -

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.19.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	@if [ ! -f "$(KUSTOMIZE)-$(KUSTOMIZE_VERSION)" ]; then \
		set -e ; \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION) ; \
		touch "$(KUSTOMIZE)-$(KUSTOMIZE_VERSION)" ; \
	fi

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (i.e. $(LOCALBIN)/controller-gen (the path is not included in the package))
# $2 - package (i.e. sigs.k8s.io/controller-runtime/cmd/controller-gen)
# $3 - version (i.e. $(CONTROLLER_TOOLS_VERSION))
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e ;\
tmp_dir=$$(mktemp -d) ;\
GOBIN=$(LOCALBIN) go install "$(2)@$(3)" ;\
bin=$$(echo "$(2)" | sed 's/^.*\///') ;\
mv "$(LOCALBIN)/$$bin" "$(1)" ;\
touch "$(1)-$(3)" ;\
rm -rf $$tmp_dir ;\
}
endef

##@ Docker

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}
