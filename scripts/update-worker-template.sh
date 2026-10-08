#!/usr/bin/env bash
# update-worker-template.sh — refresh workerTemplate.json in a values file from a live RKE worker.
#
# Run it after every `rke up` that changes the cluster (Kubernetes/RKE upgrade, kubelet or kube-proxy
# extra_args/extra_binds, cluster DNS/CIDR, control plane hosts, private registries).
#
# Usage:
#   scripts/update-worker-template.sh --worker ubuntu@10.0.0.20 [--values helm_vars/values.yaml]
#                                     [--context my-cluster] [--check] [--yes] [--ssh-opt "-p 2222"] [--no-sudo]
#
#   --worker   SSH target of a worker RKE manages (never a node of a node group); only read from
#   --values   values file whose `workerTemplate.json` block is replaced (default: helm_vars/values.yaml).
#              Only that block changes; comments and every other key stay as they are.
#   --context  also check the new template against the cluster (read only): kubelet version and control plane
#              IPs, the same checks the provider runs at start
#   --check    only report whether the values file is out of date; exit 1 if it is, 0 if not. Changes nothing.
#   --yes      write without asking
#
# Afterwards: helmfile apply (or helm upgrade). Nodes of the node group that are already running keep the old
# template — replace them (delete the nodes and let cluster-autoscaler recreate them, or scale to 0 and back).
set -euo pipefail

WORKER="" VALUES="helm_vars/values.yaml" CONTEXT="" CHECK=0 YES=0 SUDO="sudo -n"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)

die() { echo "ERROR: $*" >&2; exit 2; }
usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit "${1:-0}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --worker)  WORKER="$2"; shift 2 ;;
    --values)  VALUES="$2"; shift 2 ;;
    --context) CONTEXT="$2"; shift 2 ;;
    --check)   CHECK=1; shift ;;
    --yes|-y)  YES=1; shift ;;
    --ssh-opt) read -r -a extra <<<"$2"; SSH_OPTS+=("${extra[@]}"); shift 2 ;;
    --no-sudo) SUDO=""; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown option: $1" >&2; usage 2 ;;
  esac
done
[ -n "$WORKER" ] || usage 2
[ -f "$VALUES" ] || die "values file not found: $VALUES"
command -v python3 >/dev/null || die "python3 is required"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/worker-template.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

echo "==> reading docker inspect from $WORKER"
ssh "${SSH_OPTS[@]}" "$WORKER" "$SUDO docker inspect service-sidekick nginx-proxy kubelet kube-proxy" >"$WORK/new.json" \
  || die "docker inspect failed on $WORKER — is it an RKE worker, and does '$SUDO docker' work without a password?"

if [ -n "$CONTEXT" ]; then
  command -v kubectl >/dev/null || die "--context needs kubectl"
  echo "==> reading nodes of $CONTEXT (read only)"
  kubectl --context "$CONTEXT" get nodes -o json >"$WORK/nodes.json"
fi

# exit codes of the helper: 0 = up to date, 1 = out of date (and written unless --check), 2 = error, 3 = declined
set +e
python3 -I - "$VALUES" "$WORK/new.json" "$WORK/nodes.json" "$CHECK" "$YES" <<'PY'
import json, os, re, sys

values_path, new_path, nodes_path, check, yes = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == "1", sys.argv[5] == "1"
CONTAINERS = ["service-sidekick", "nginx-proxy", "kubelet", "kube-proxy"]
GROUP_LABEL = "rke-autoscaler.io/nodegroup"

def fail(msg):
    print("ERROR: " + msg, file=sys.stderr)
    sys.exit(2)

def by_name(raw):
    data = json.loads(raw)
    return {c["Name"].lstrip("/"): c for c in data}

# only what the provider reads (internal/workerplane); runtime fields (State, Pid, ...) are dropped so a
# restarted container does not look like a change, and the values file stays small
CFG_KEYS = ["Image", "Entrypoint", "Cmd", "Env", "Labels"]
HC_KEYS = ["NetworkMode", "PidMode", "Privileged", "Binds", "VolumesFrom", "SecurityOpt", "LogConfig"]

def relevant(raw):
    out = []
    for c in json.loads(raw):
        hc = c.get("HostConfig") or {}
        r = {"Name": c["Name"],
             "Config": {k: (c.get("Config") or {}).get(k) for k in CFG_KEYS},
             "HostConfig": {k: hc.get(k) for k in HC_KEYS}}
        r["HostConfig"]["RestartPolicy"] = {"Name": (hc.get("RestartPolicy") or {}).get("Name", "")}
        out.append(r)
    return sorted(out, key=lambda c: c["Name"])

def tokens(c):
    cfg = c["Config"]
    return (cfg.get("Entrypoint") or []) + (cfg.get("Cmd") or [])

def flag(c, name):
    for t in tokens(c):
        if t.startswith(name + "="):
            return t.split("=", 1)[1]
    return ""

def cp_hosts(cs):
    for e in cs["nginx-proxy"]["Config"].get("Env") or []:
        if e.startswith("CP_HOSTS="):
            return sorted(x for x in e[len("CP_HOSTS="):].split(",") if x)
    return []

# ---- the new template: complete, from an RKE-managed worker, usable by the provider
new_raw = open(new_path).read()
try:
    new = by_name(new_raw)
except Exception as e:
    fail("docker inspect output is not JSON: %s" % e)
missing = [n for n in CONTAINERS if n not in new]
if missing:
    fail("worker has no %s container(s): not an RKE worker?" % ", ".join(missing))
k = new["kubelet"]
if flag(k, "--provider-id") or GROUP_LABEL in flag(k, "--node-labels"):
    fail("this worker belongs to a node group (it has --provider-id / %s); take the template from a worker RKE manages" % GROUP_LABEL)
if not flag(k, "--hostname-override"):
    fail("kubelet has no --hostname-override (cloud provider mode is not supported)")
if "kube-kubelet" in flag(k, "--tls-cert-file"):
    fail("generate_serving_certificate is enabled; the provider cannot build nodes for this cluster")
if not cp_hosts(new):
    fail("nginx-proxy has no CP_HOSTS")

# ---- optional: compare with the live cluster (same checks as the provider's preflight)
if os.path.getsize(nodes_path) if os.path.exists(nodes_path) else 0:
    nodes = json.load(open(nodes_path))["items"]
    versions, cps = set(), []
    for n in nodes:
        labels = n["metadata"].get("labels", {})
        if GROUP_LABEL in labels:
            continue
        versions.add(n["status"]["nodeInfo"]["kubeletVersion"])
        if labels.get("node-role.kubernetes.io/controlplane") == "true":
            cps += [a["address"] for a in n["status"]["addresses"] if a["type"] == "InternalIP"]
    tag = k["Config"]["Image"].rsplit(":", 1)[-1]
    if not any(tag == v or tag.startswith(v + "-") for v in versions):
        fail("kubelet image %s does not match the cluster's kubelet versions %s — is `rke up` still running?" % (k["Config"]["Image"], sorted(versions)))
    if cps and sorted(cps) != cp_hosts(new):
        fail("CP_HOSTS %s differ from the control plane nodes %s" % (cp_hosts(new), sorted(cps)))
    print("==> template matches the cluster: kubelet %s, control planes %s" % (tag, ",".join(sorted(cps))))

# ---- locate workerTemplate.json in the values file (text level, to keep comments)
lines = open(values_path).read().split("\n")
try:
    top = next(i for i, l in enumerate(lines) if l.rstrip() == "workerTemplate:")
except StopIteration:
    fail("%s has no top-level 'workerTemplate:' key" % values_path)
end_top = next((i for i in range(top + 1, len(lines)) if lines[i] and not lines[i].startswith(" ") and not lines[i].startswith("#")), len(lines))
jline = next((i for i in range(top + 1, end_top) if re.match(r"^  json:", lines[i])), None)
if jline is None:
    fail("no '  json:' under workerTemplate in %s" % values_path)
m = re.match(r"^  json:\s*(.*)$", lines[jline])
head = m.group(1).split(" #")[0].strip()
if head.startswith("|") or head.startswith(">"):
    jend = jline + 1
    while jend < end_top and (lines[jend] == "" or lines[jend].startswith("    ")):
        jend += 1
    # trailing blank lines belong to what follows, not to the block
    while jend > jline + 1 and lines[jend - 1] == "":
        jend -= 1
    old_text = "\n".join(l[4:] for l in lines[jline + 1:jend])
else:
    jend = jline + 1
    old_text = json.loads(head) if head.startswith('"') else (head.strip("'") if head not in ("", "null", "~") else "")

# ---- what changes
def summary(old_raw, new_cs):
    if not old_raw.strip():
        return ["(values had no worker template yet)"]
    try:
        old_cs = by_name(old_raw)
    except Exception:
        return ["(the old template in the values file is not valid JSON)"]
    out = []
    for n in CONTAINERS:
        o, c = old_cs.get(n), new_cs[n]
        if o is None:
            out.append("%s: added" % n)
            continue
        if o["Config"]["Image"] != c["Config"]["Image"]:
            out.append("%s image: %s -> %s" % (n, o["Config"]["Image"], c["Config"]["Image"]))
        ot, nt = set(tokens(o)), set(tokens(c))
        for t in sorted(nt - ot):
            out.append("%s arg  + %s" % (n, t))
        for t in sorted(ot - nt):
            out.append("%s arg  - %s" % (n, t))
        ob, nb = set(o["HostConfig"].get("Binds") or []), set(c["HostConfig"].get("Binds") or [])
        for b in sorted(nb - ob):
            out.append("%s bind + %s" % (n, b))
        for b in sorted(ob - nb):
            out.append("%s bind - %s" % (n, b))
        oe, ne = set(o["Config"].get("Env") or []), set(c["Config"].get("Env") or [])
        for e in sorted(ne - oe):
            out.append("%s env  + %s" % (n, e))
        for e in sorted(oe - ne):
            out.append("%s env  - %s" % (n, e))
    return out

def canonical(raw):
    try:
        return json.dumps(relevant(raw), sort_keys=True)
    except Exception:
        return None

if canonical(old_text) == canonical(new_raw):
    print("==> %s is up to date with %s" % (values_path, flag(k, "--hostname-override")))
    sys.exit(0)

print("==> changes (template node: %s):" % flag(k, "--hostname-override"))
for line in summary(old_text, new):
    print("   " + line)

if check:
    print("==> out of date (--check: nothing written)")
    sys.exit(1)
if not yes:
    sys.stdout.write("Write the new template into %s? [y/N] " % values_path)
    sys.stdout.flush()
    try:
        with open("/dev/tty") as tty:
            ans = tty.readline()
    except OSError:
        fail("no terminal to confirm on; pass --yes")
    if ans.strip().lower() not in ("y", "yes"):
        print("aborted")
        sys.exit(3)

body = json.dumps(relevant(new_raw), indent=1).split("\n")
block = ["  json: |"] + [("    " + l) if l else "" for l in body]
lines[jline:jend] = block
tmp = values_path + ".tmp"
with open(tmp, "w") as f:
    f.write("\n".join(lines))
os.chmod(tmp, os.stat(values_path).st_mode & 0o777)
os.replace(tmp, values_path)

# read it back the way helm will
try:
    import yaml
    got = yaml.safe_load(open(values_path))["workerTemplate"]["json"]
    if canonical(got) != canonical(new_raw):
        fail("written file does not read back to the new template")
    print("==> written and verified: %s" % values_path)
except ImportError:
    print("==> written: %s (PyYAML not installed, read-back check skipped)" % values_path)
sys.exit(1)
PY
rc=$?
set -e
case $rc in
  0) exit 0 ;;
  1) if [ "$CHECK" = 0 ]; then echo "Next: helmfile apply (provider restarts with the new template), then replace running node group nodes."; exit 0; fi; exit 1 ;;
  3) exit 1 ;;
  *) exit 2 ;;
esac
