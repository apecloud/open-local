
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
GIT_COMMIT=$(shell git rev-parse HEAD)
VERSION=v0.7.3

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
	CGO_ENABLED=0 $(GO_BUILD) $(LD_FLAGS) -v -o $(OUTPUT_DIR)/$(NAME) $(MAIN_FILE)

.PHONY: develop
develop:
	GOARCH=amd64 GOOS=linux CGO_ENABLED=0 $(GO_BUILD) $(LD_FLAGS) -v -o $(OUTPUT_DIR)/$(NAME) $(MAIN_FILE)
	chmod +x $(OUTPUT_DIR)/$(NAME)
	docker build . -t ${IMAGE_NAME_FOR_DOCKERHUB}:${VERSION} -f ./Dockerfile.dev
	docker tag ${IMAGE_NAME_FOR_DOCKERHUB}:${VERSION} ${IMAGE_NAME_FOR_ACR}:${VERSION}

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
	#docker tag ${IMAGE_NAME_FOR_DOCKERHUB}:${VERSION} ${IMAGE_NAME_FOR_ACR}:${VERSION}

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
	$(CONTROLLER_GEN) $(CRD_OPTIONS) rbac:roleName=manager-role crd paths="./pkg/apis/storage/$(CRD_VERSION)/..." output:crd:artifacts:config=helm/crds/

.PHONY: fmt
fmt:
	go fmt ./...
.PHONY: vet
vet:
	go vet `go list ./... | grep -v /vendor/`

# find or download controller-gen
# download controller-gen if necessary
controller-gen:
ifeq (, $(shell which controller-gen))
	go get sigs.k8s.io/controller-tools/cmd/controller-gen@v0.5.0
CONTROLLER_GEN=$(shell go env GOPATH)/bin/controller-gen
else
CONTROLLER_GEN=$(shell which controller-gen)
endif
