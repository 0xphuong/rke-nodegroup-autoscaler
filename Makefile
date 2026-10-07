IMAGE   ?= ghcr.io/0xphuong/rke-nodegroup-autoscaler
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CHART   := charts/rke-nodegroup-autoscaler

.PHONY: build test lint image chart-lint chart-template

build:
	CGO_ENABLED=0 go build -trimpath -o bin/nodegroup-provider ./cmd/nodegroup-provider

test:
	go test -race -count=1 ./...

lint:
	go vet ./...

image:
	docker buildx build --platform linux/amd64 --load --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

chart-lint:
	helm lint $(CHART) -f $(CHART)/ci/test-values.yaml

chart-template:
	helm template ngas $(CHART) -n kube-system --kube-version 1.32.6 -f $(CHART)/ci/test-values.yaml \
	  --set-file workerTemplate.json=internal/workerplane/testdata/worker-template.json
