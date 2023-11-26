
# go parameters
GO_CMD=go
GO_BUILD=$(GO_CMD) build
GO_TEST=$(GO_CMD) test -v
GO_PACKAGE=github.com/apecloud/open-local

# build info
NAME=open-local
OUTPUT_DIR?=./bin
IMAGE_NAME_FOR_ACR=ack-agility-registry.cn-shanghai.cr.aliyuncs.com/ecp_builder/${NAME}
IMAGE_NAME_FOR_DOCKERHUB?=apecloud/${NAME}
BUILDX_BUILDER=$(NAME)-builder
GOPROXY?=https://proxy.golang.org,direct
MAIN_FILE=./cmd/main.go
LD_FLAGS=-ldflags "-X '${GO_PACKAGE}/pkg/version.GitCommit=$(GIT_COMMIT)' -X '${GO_PACKAGE}/pkg/version.Version=$(VERSION)' -X 'main.VERSION=$(VERSION)' -X 'main.COMMITID=$(GIT_COMMIT)'"
GC_FLAGS=-gcflags "${SKAFFOLD_GO_GCFLAGS}"
GIT_COMMIT=$(shell git rev-parse HEAD)
VERSION ?= v0.7.3

CRD_OPTIONS ?= "crd:trivialVersions=true"
CRD_VERSION=v1alpha1

# build binary
all: test fmt vet build

.PHONY: test
test:
	$(GO_TEST) -coverprofile=covprofile ./...
	$(GO_CMD) tool cover -html=covprofile -o coverage.html

.PHONY: build
build:
	mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 $(GO_BUILD) $(LD_FLAGS) $(GC_FLAGS) -v -o $(OUTPUT_DIR)/$(NAME) $(MAIN_FILE)

.PHONY: develop
develop:
	docker build . -t ${IMAGE_NAME_FOR_DOCKERHUB}:${VERSION} \
		--build-arg TARGETOS=$${TARGETOS:-linux} \
		--build-arg TARGETARCH=$${TARGETARCH:-arm64} \
		--build-arg GOPROXY=$(GOPROXY) \
		${DOCKER_BUILD_EXTRA_ARGS}

.PHONY: install-docker-buildx
install-docker-buildx: ## Create `docker buildx` builder.
	@if ! docker buildx inspect $(BUILDX_BUILDER) > /dev/null; then \
		echo "Buildx builder $(BUILDX_BUILDER) does not exist, creating..."; \
		docker buildx create --name=$(BUILDX_BUILDER) --use --driver=docker-container --platform linux/amd64,linux/arm64; \
	else \
		echo "Buildx builder $(BUILDX_BUILDER) already exists"; \
	fi

# build image
.PHONY: image
image: install-docker-buildx
	docker buildx build ./ --builder $(BUILDX_BUILDER) $(BUILDX_OUTPUT) \
	    --platform linux/amd64,linux/arm64 --build-arg GOPROXY=$(GOPROXY) \
	    --file ./Dockerfile --tag ${IMAGE_NAME_FOR_DOCKERHUB}:${VERSION}

.PHONY: push-image
push-image: BUILDX_OUTPUT=--output=type=registry
push-image: image

.PHONY: image-tools
image-tools:
	docker build . -t ${IMAGE_NAME_FOR_DOCKERHUB}:tools -f ./Dockerfile.tools
	docker tag ${IMAGE_NAME_FOR_DOCKERHUB}:tools ${IMAGE_NAME_FOR_ACR}:tools

# generate manifests e.g. CRD, RBAC etc.
manifests: controller-gen
	./hack/update-codegen.sh
	$(CONTROLLER_GEN) $(CRD_OPTIONS) rbac:roleName=manager-role crd paths="./pkg/apis/storage/$(CRD_VERSION)/..." output:crd:artifacts:config=deploy/helm/crds/

.PHONY: fmt
fmt:
	go fmt ./...
.PHONY: vet
vet:
	go vet `go list ./... | grep -v /vendor/`

# find or download controller-gen
# download controller-gen if necessary
controller-gen:
ifeq (, $(wildcard bin/controller-gen))
	GOBIN=$(shell pwd)/bin go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.5.0
endif
CONTROLLER_GEN=$(shell pwd)/bin/controller-gen

.PHONY: protobuf
protobuf: protoc protoc-gen-go
	cd pkg/csi/lib && PATH="$(shell pwd)/bin:$$PATH" $(PROTOC) \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		lvm.proto

protoc:
ifeq (, $(shell which protoc))
	$(error "$@ is not found, please install it first.")
else
PROTOC=$(shell which protoc)
endif

protoc-gen-go:
ifeq (, $(wildcard bin/protoc-gen-go))
	GOBIN=$(shell pwd)/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.31.0
endif
ifeq (, $(wildcard bin/protoc-gen-go-grpc))
	GOBIN=$(shell pwd)/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.3.0
endif

##@ Helm Chart Tasks

GOOS ?= $(shell $(GO_CMD) env GOOS)

bump-single-chart-appver.%: chart=$*
bump-single-chart-appver.%:
ifeq ($(GOOS), darwin)
	sed -i '' "s/^appVersion:.*/appVersion: $(VERSION)/" deploy/$(chart)/Chart.yaml
else
	sed -i "s/^appVersion:.*/appVersion: $(VERSION)/" deploy/$(chart)/Chart.yaml
endif

bump-single-chart-ver.%: chart=$*
bump-single-chart-ver.%:
ifeq ($(GOOS), darwin)
	sed -i '' "s/^version:.*/version: $(VERSION)/" deploy/$(chart)/Chart.yaml
else
	sed -i "s/^version:.*/version: $(VERSION)/" deploy/$(chart)/Chart.yaml
endif

.PHONY: bump-chart-ver
bump-chart-ver: \
	bump-single-chart-ver.helm \
	bump-single-chart-appver.helm
