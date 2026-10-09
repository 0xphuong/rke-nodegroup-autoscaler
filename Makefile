IMAGE   ?= docker.io/binhphuong/rke-nodegroup-autoscaler
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART   := charts/rke-nodegroup-autoscaler

CHART_VERSION := $(shell sed -n 's/^version: *//p' charts/rke-nodegroup-autoscaler/Chart.yaml)
APP_VERSION   := $(shell sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/p' charts/rke-nodegroup-autoscaler/Chart.yaml)

.PHONY: build test lint image push chart-lint chart-template check-version

build:
	CGO_ENABLED=0 go build -trimpath -o bin/nodegroup-provider ./cmd/nodegroup-provider

test:
	go test -race -count=1 ./...

lint:
	go vet ./...

image:
	docker buildx build --platform linux/amd64 --load --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# build multi-arch and push to Docker Hub (see scripts/release-image.sh --help); ARGS="--latest --chart ..."
push:
	scripts/release-image.sh $(ARGS)

# tag vX.Y.Z, Chart.yaml version and appVersion must all be X.Y.Z.
# TAG=vX.Y.Z or X.Y.Z (default: the latest v* tag of HEAD's history)
check-version:
	@t="$(TAG)"; t="$${t:-$$(git describe --tags --abbrev=0 --match 'v*')}"; t="$${t#v}"; \
	if [ "$(CHART_VERSION)" != "$$t" ] || [ "$(APP_VERSION)" != "$$t" ]; then \
	  echo "version mismatch: tag $$t, Chart.yaml version $(CHART_VERSION), appVersion $(APP_VERSION)"; exit 1; fi; \
	echo "versions aligned: $$t"

chart-lint:
	helm lint $(CHART) -f $(CHART)/ci/test-values.yaml

chart-template:
	helm template ngas $(CHART) -n kube-system --kube-version 1.32.6 -f $(CHART)/ci/test-values.yaml \
	  --set-file workerTemplate.json=internal/workerplane/testdata/worker-template.json
