FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/nodegroup-provider ./cmd/nodegroup-provider

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nodegroup-provider /nodegroup-provider
USER 65532:65532
ENTRYPOINT ["/nodegroup-provider"]
