# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Managed node groups + cluster-autoscaler for **RKE1** clusters on **VNG Cloud**, without changing RKE
(`cluster.yml`, `rke up`) or the existing cluster. One Go binary (`nodegroup-provider`, the externalgrpc cloud
provider) and one Helm chart (`charts/rke-nodegroup-autoscaler`) that also runs upstream cluster-autoscaler.
README, docs, CHANGELOG and the comments in `values.yaml` are in Vietnamese; Go code and comments are in English.

Workspace git rules (Conventional Commits, no `Co-Authored-By`, `github-0xphuong` remote alias, one tag per push)
come from `../CLAUDE.md`.

## Commands

```bash
make test                    # go test -race -count=1 ./...
make lint                    # go vet
go test ./internal/provider -run 'TestRepair' -v      # one package / one test
make chart-lint              # helm lint with ci/test-values.yaml
make chart-template          # helm template, already passes --kube-version 1.32.6
make check-version TAG=vX.Y.Z  # tag == Chart.yaml version == appVersion
make push ARGS="..."         # scripts/release-image.sh: tests, multi-arch buildx, push to Docker Hub
```

- Any ad-hoc `helm template` needs `--kube-version 1.32.6`: the chart requires `>=1.30`, Helm defaults to 1.29.
- Rendering against a real cluster (so `lookup` sees existing Secrets) is read-only with
  `helm template ... --kube-context <ctx> --dry-run=server --kube-version 1.32.6`.
- Go deps are pinned to versions that build with Go 1.23 (`go.mod`); do not bump them casually.
- `scripts/release-image.sh` refuses to overwrite an existing image tag (`--force`), asks before building a dirty
  tree (`--allow-dirty`), never tags `latest` unless `--latest`. The CI release job (`.github/workflows/ci.yaml`)
  currently fails at Docker Hub login (repo secrets not set), so images are released with this script.
- Versioning: one version everywhere (git tag `vX.Y.Z` = chart `version` = `appVersion` = image tag). Pods use
  `IfNotPresent`, so changed code needs a new version, not an overwritten tag.

## Architecture

**Provider (`cmd/nodegroup-provider`, `internal/`)**
- `provider/` implements the externalgrpc `CloudProvider` service. cluster-autoscaler only decides sizes; the
  provider creates/deletes VMs. `NodeGroupForNode` returns an empty id for anything it did not create, so control
  plane, RKE workers and hand-joined nodes are never touched.
- `state/` is the **source of truth**: one ConfigMap (`<release>-provider-state`, key `instances.json`) recording
  every VM, because the VNG SDK has no list-servers call. Single writer, hence 1 replica with `Recreate`.
  Phases: `Creating → Running → Deleting`, `Failed` (reported to CA as a creating instance with error info, so CA
  deletes it and backs off), `Replacing` (repair, reported as deleting). Target size = Creating + Running.
- `provider/reconcile.go` runs every 30s and moves records toward reality (VM status via `cloud.Driver`, Node
  objects by `providerID` = `vngcloud://<id>`); deletes the VM first and the Node object only after the VM is gone.
- `provider/repair.go` replaces broken nodes itself. Rule: **at minSize** replace (create-first while the VM still
  runs, delete+create together when the VM is gone/stopped/error or the Node object is lost); **above minSize**
  only delete and let CA scale up. Circuit breaker (`repair.maxUnhealthyPercent`, per group and cluster wide), one
  repair per group, `notReadySince` kept in state. Full case list with timings: `docs/node-repair.md`.
- `cloud/` is the driver interface (`Create/Get/Delete`, `ErrNotFound`, `ErrRetryLater`); `cloud/vngcloud` maps
  vServer statuses to phases.
- `bootstrap/`: a new VM gets a one-time token in `user_data` (only its sha256 is stored) and fetches its join
  script over HTTPS from the provider via a **NodePort** on existing nodes (it is not in the cluster yet).
  `user_data` is MIME multipart: site cloud-init parts first, join script last. The join script carries the shared
  RKE node certificates (`kube-node`/`kube-proxy`, the same on every RKE1 node) and the docker commands.
- `workerplane/` turns `docker inspect` of an existing RKE worker (service-sidekick, nginx-proxy, kubelet,
  kube-proxy) into docker run commands for the new node (hostname, node IP, labels, taints, provider id).
- `preflight/` refuses to start on mismatches: node cert chain/key/kubeconfig server, cert CA vs cluster CA,
  kubelet image vs cluster version, `CP_HOSTS` vs control plane IPs.
- The same binary has an init-container mode: `nodegroup-provider copy-node-certs --from --to`.

**Chart (`charts/rke-nodegroup-autoscaler`)**
- `certs.yaml` + `_helpers.tpl` (`ngas.tls`, `ngas.leafCert`): a release CA and three leaf certs (provider gRPC
  server, CA client, bootstrap server) are **kept across upgrades with `lookup`**; leaves are re-issued only when
  `rke-autoscaler.io/cert-id` (CA+CN+SANs) changes or within 30 days of `cert-not-after`. Deployments carry
  `checksum/tls` / `checksum/grpc` so pods restart only when what they load at start changes. Consequence:
  installs must go through helm/helmfile/Flux; `helm template | kubectl apply` and Argo CD would rotate the CA.
- `nodeCerts.hostPath` (recommended): an init container mounts each of the 7 cert files from the node
  individually (never `kube-ca-key.pem`) and copies them into a memory volume; the provider stays non-root.
  Alternatives: `existingSecret` or inline `files`.
- `policies.yaml`: a ValidatingAdmissionPolicy lets the two service accounts delete only Nodes carrying
  `rke-autoscaler.io/nodegroup`; NetworkPolicy limits gRPC to the CA pod.
- `ngas.affinity` keeps both pods off node-group nodes. `ngas.validate` (included from `certs.yaml`) fails the
  render on bad values.
- CA flags come from `clusterAutoscaler.extraArgs`; `enforce-node-group-min-size: true` is required for minSize to
  create nodes. CA image minor must match the cluster minor (NOTES.txt warns).

## Cluster and secrets

- The default kube context on this machine is a **production** cluster. Always pass `--context`/`--kube-context`
  explicitly; the test cluster's context is `CONTEXT` in `deploy/<cluster>/deploy.sh`. Do not change existing nodes
  or RKE; only act on node-group nodes.
- `helm_vars/`, `helmfile.yaml` and `deploy/` are git-ignored and hold real values (VNG client secret, IPs).
  Never print their secrets. This repo is public: grep new work for internal IPs, hostnames and IDs before
  committing.
- Real worker template and test manifests live in `deploy/<cluster>/`; refresh the template after `rke up`
  with `scripts/update-worker-template.sh`.
- `helm-diff` 3.6 renders without `lookup`, so `helmfile diff` always shows the TLS Secrets as changed; that is
  display only.
