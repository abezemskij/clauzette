# Recipes use '>' instead of a tab.
.RECIPEPREFIX = >
IMAGE   ?= registry.local/clauzette
VERSION ?= 0.1.0

.PHONY: build test vet image push run run-local
build:
> CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/clauzette ./cmd/clauzette

test:
> go test ./...

vet:
> go vet ./...

image:
> docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

push: image
> docker push $(IMAGE):$(VERSION)

# Local development against a port-forwarded Ollama:
#   kubectl -n ai port-forward svc/ollama 11434:11434
run: build
> CLAUZETTE_OLLAMA_URL=http://localhost:11434 CLAUZETTE_WORKSPACE=$(PWD)/scratch ./bin/clauzette -config config.example.json

# Fully local: Ollama on this machine, workspace ./scratch, state in ./.local.
#   make run-local ARGS=-resume
run-local: build
> mkdir -p scratch .local/sessions
> ./bin/clauzette -config clauzette.local.json $(ARGS)
