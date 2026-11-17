# Image build operator. Own Go module; nothing here touches the Lifecycle Manager's build.
IMG ?= image-operator:latest
CONTAINER_TOOL ?= podman
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
# Source of the embedded recipe (lab/generic-image of the parity project), for sync-recipe.
RECIPE_SRC ?=
RECIPE_FILES = build.sh inspect.sh defaults.env deviations.json lib targets

.PHONY: all
all: build

.PHONY: generate
generate: ## DeepCopy methods and the CRD.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases

.PHONY: build
build: ## Static binary in bin/.
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -o bin/image-operator ./cmd

.PHONY: test
test: ## Unit tests (no cluster needed).
	go vet ./...
	go test ./...

.PHONY: container
container: ## Operator container image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: sync-recipe
sync-recipe: ## Refresh the embedded recipe from RECIPE_SRC. Changes the recipe digest and so every cache key.
	@test -d "$(RECIPE_SRC)" || { echo "set RECIPE_SRC to the recipe directory"; exit 2; }
	for f in $(RECIPE_FILES); do rm -rf internal/recipe/files/$$f; cp -a "$(RECIPE_SRC)/$$f" internal/recipe/files/; done
	find internal/recipe/files -name __pycache__ -prune -exec rm -rf {} \;
