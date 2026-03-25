-include make.txt


REGISTRY ?= your-docker-repo
IMAGE ?= name-it-yours
PLATFORM ?= linux/amd64

# Prefer VERSION passed in by caller; fallback to git short SHA.
VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: docker_build docker_build_local version

docker_build:
	@set -e; \
	echo "🚀 Building worker image version: $(VERSION)"; \
	docker buildx create --use --name multi 2>/dev/null || true; \
	docker buildx inspect --bootstrap >/dev/null; \
	docker buildx build \
		--platform $(PLATFORM) \
		-t $(REGISTRY)/$(IMAGE):latest \
		-t $(REGISTRY)/$(IMAGE):$(VERSION) \
		--push .

docker_build_local:
	@set -e; \
	echo "🚀 Building local worker image version: $(VERSION)"; \
	docker buildx create --use --name multi 2>/dev/null || true; \
	docker buildx inspect --bootstrap >/dev/null; \
	docker buildx build \
		--platform $(PLATFORM) \
		-t $(REGISTRY)/$(IMAGE):latest \
		-t $(REGISTRY)/$(IMAGE):$(VERSION) \
		--load .

version:
	@echo "📦 Worker image version: $(VERSION)"
