{{/* at most 40 chars: the longest suffix is "-provider-bootstrap" and Service names stop at 63 */}}
{{- define "ngas.fullname" -}}
{{- if contains "nodegroup-autoscaler" .Release.Name -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "ngas.provider" -}}{{ include "ngas.fullname" . }}-provider{{- end -}}
{{- define "ngas.autoscaler" -}}{{ include "ngas.fullname" . }}-autoscaler{{- end -}}

{{- define "ngas.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "ngas.providerSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: provider
{{- end -}}

{{- define "ngas.autoscalerSelector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: cluster-autoscaler
{{- end -}}

{{- define "ngas.groupLabel" -}}rke-autoscaler.io/nodegroup{{- end -}}

{{/* never schedule onto a node the autoscaler manages: it could remove the node running itself */}}
{{- define "ngas.affinity" -}}
nodeAffinity:
  requiredDuringSchedulingIgnoredDuringExecution:
    nodeSelectorTerms:
      - matchExpressions:
          - key: {{ include "ngas.groupLabel" . }}
            operator: DoesNotExist
{{- end -}}

{{/* the chart creates the credentials Secret when clientId/clientSecret are given in values */}}
{{- define "ngas.createCredentials" -}}
{{- if and .Values.vngcloud.credentials.clientId .Values.vngcloud.credentials.clientSecret -}}true{{- end -}}
{{- end -}}

{{- define "ngas.credentialsSecret" -}}
{{- .Values.vngcloud.credentials.existingSecret | default "vngcloud-credentials" -}}
{{- end -}}

{{/* the RKE node certificate files a new node needs; the same list as bootstrap.CertFiles in the provider */}}
{{- define "ngas.certFileNames" -}}kube-ca.pem kube-node.pem kube-node-key.pem kube-proxy.pem kube-proxy-key.pem kubecfg-kube-node.yaml kubecfg-kube-proxy.yaml{{- end -}}

{{- define "ngas.nodeCertsSecret" -}}
{{- .Values.nodeCerts.existingSecret | default "rke-node-certs" -}}
{{- end -}}

{{- define "ngas.workerTemplateConfigMap" -}}
{{- .Values.workerTemplate.existingConfigMap | default (printf "%s-worker-template" (include "ngas.fullname" .)) -}}
{{- end -}}

{{/* fail early on values that would only break at runtime */}}
{{- define "ngas.validate" -}}
{{- if not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" .Values.clusterName) -}}
{{- fail "clusterName is required (lowercase DNS label)" -}}
{{- end -}}
{{- if not .Values.nodeGroups -}}{{- fail "nodeGroups: define at least one node group" -}}{{- end -}}
{{- if not .Values.bootstrap.nodeIPs -}}{{- fail "bootstrap.nodeIPs: list the IPs of existing nodes new VMs can reach" -}}{{- end -}}
{{- $files := .Values.nodeCerts.files | default dict -}}
{{- if .Values.nodeCerts.hostPath -}}
{{- if or $files .Values.nodeCerts.existingSecret -}}
{{- fail "nodeCerts: hostPath reads the certificates from the node; leave existingSecret and files empty" -}}
{{- end -}}
{{- if not (hasPrefix "/" .Values.nodeCerts.hostPath) -}}{{- fail "nodeCerts.hostPath must be an absolute path, e.g. /etc/kubernetes/ssl" -}}{{- end -}}
{{- else if $files -}}
{{- range splitList " " (include "ngas.certFileNames" .) -}}
{{- if not (index $files .) -}}{{- fail (printf "nodeCerts.files: %s is missing or empty (all 7 files are needed)" .) -}}{{- end -}}
{{- end -}}
{{- else if not .Values.nodeCerts.existingSecret -}}
{{- fail "nodeCerts: set hostPath (read from the node, e.g. /etc/kubernetes/ssl), existingSecret or files (see values.yaml)" -}}
{{- end -}}
{{- $c := .Values.vngcloud.credentials -}}
{{- if and (or $c.clientId $c.clientSecret) (not (and $c.clientId $c.clientSecret)) -}}
{{- fail "vngcloud.credentials: clientId and clientSecret must be set together" -}}
{{- end -}}
{{- if and (not $c.existingSecret) (not (and $c.clientId $c.clientSecret)) -}}
{{- fail "vngcloud.credentials: set existingSecret, or clientId and clientSecret" -}}
{{- end -}}
{{- if not (and .Values.vngcloud.vserverEndpoint .Values.vngcloud.projectId) -}}{{- fail "vngcloud.vserverEndpoint and vngcloud.projectId are required" -}}{{- end -}}
{{- if and (not .Values.workerTemplate.json) (not .Values.workerTemplate.existingConfigMap) -}}
{{- fail "workerTemplate: pass --set-file workerTemplate.json=<docker inspect of a worker> or workerTemplate.existingConfigMap" -}}
{{- end -}}
{{- end -}}

{{/* externalgrpc cloud provider configuration of cluster-autoscaler (--cloud-config) */}}
{{- define "ngas.cloudConfig" -}}
{{- $svc := include "ngas.provider" . -}}
address: {{ $svc }}.{{ .Release.Namespace }}.svc:8086
key: /etc/cluster-autoscaler/grpc/tls.key
cert: /etc/cluster-autoscaler/grpc/tls.crt
cacert: /etc/cluster-autoscaler/grpc/ca.crt
grpc_timeout: {{ .Values.clusterAutoscaler.grpcTimeout }}
{{ end -}}

{{/*
A leaf certificate signed by the release CA, reused from its Secret across upgrades. A new one is issued only
when the Secret is missing, the CA, CN or SANs changed (cert-id), or it expires within 30 days. Helm cannot parse
certificates, so the Secret carries cert-id and cert-not-after annotations for this decision.
Arguments: dict "ns" "secret" "cn" "ips" "dns" "ca" (a genCA/buildCustomCert object). Returns YAML:
cert, key, id, notAfter (unix seconds).
*/}}
{{- define "ngas.leafCert" -}}
{{- $days := 825 -}}
{{- $id := printf "%s|%s|%s|%s" .ca.Cert .cn (toJson .ips) (toJson .dns) | sha256sum | trunc 16 -}}
{{- $now := now | unixEpoch | atoi -}}
{{- $out := dict -}}
{{- $s := lookup "v1" "Secret" .ns .secret -}}
{{- if and $s $s.data -}}
{{- $a := $s.metadata.annotations | default dict -}}
{{- $notAfter := index $a "rke-autoscaler.io/cert-not-after" | default "0" | atoi -}}
{{- if and (eq (index $a "rke-autoscaler.io/cert-id" | default "") $id) (gt (sub $notAfter $now) (mul 30 86400)) -}}
{{- $out = dict "cert" (index $s.data "tls.crt" | b64dec) "key" (index $s.data "tls.key" | b64dec) "notAfter" (toString $notAfter) -}}
{{- end -}}
{{- end -}}
{{- if not $out -}}
{{- $c := genSignedCert .cn .ips .dns $days .ca -}}
{{- $out = dict "cert" $c.Cert "key" $c.Key "notAfter" (toString (add $now (mul $days 86400))) -}}
{{- end -}}
{{- $_ := set $out "id" $id -}}
{{- toYaml $out -}}
{{- end -}}

{{- define "ngas.leafCertAnnotations" -}}
rke-autoscaler.io/cert-id: {{ .id | quote }}
rke-autoscaler.io/cert-not-after: {{ .notAfter | quote }}
rke-autoscaler.io/cert-expires: {{ .notAfter | atoi | date "2006-01-02" | quote }}
{{- end -}}

{{/*
All TLS material of the release (see certs.yaml): the CA, kept across upgrades, and the three leaf certificates.
Deployments include it for their checksum annotations, so pods restart only when a certificate really changed.
*/}}
{{- define "ngas.tls" -}}
{{- $fn := include "ngas.fullname" . -}}
{{- $ns := .Release.Namespace -}}
{{- $existing := lookup "v1" "Secret" $ns (printf "%s-ca" $fn) -}}
{{- $ca := "" -}}
{{- if and $existing $existing.data -}}
{{- $ca = buildCustomCert (index $existing.data "tls.crt") (index $existing.data "tls.key") -}}
{{- else -}}
{{- $ca = genCA (printf "%s-ca" $fn) 3650 -}}
{{- end -}}
{{- $svc := include "ngas.provider" . -}}
{{- $ca2 := include "ngas.autoscaler" . -}}
{{- $grpcDNS := list $svc (printf "%s.%s" $svc $ns) (printf "%s.%s.svc" $svc $ns) (printf "%s.%s.svc.cluster.local" $svc $ns) -}}
{{- $out := dict "ca" (dict "cert" $ca.Cert "key" $ca.Key) -}}
{{- $_ := set $out "grpcServer" (include "ngas.leafCert" (dict "ns" $ns "secret" (printf "%s-grpc-tls" $svc) "cn" $svc "ips" list "dns" $grpcDNS "ca" $ca) | fromYaml) -}}
{{- $_ := set $out "grpcClient" (include "ngas.leafCert" (dict "ns" $ns "secret" (printf "%s-grpc" $ca2) "cn" $ca2 "ips" list "dns" list "ca" $ca) | fromYaml) -}}
{{- $_ := set $out "boot" (include "ngas.leafCert" (dict "ns" $ns "secret" (printf "%s-bootstrap-tls" $svc) "cn" (printf "%s-bootstrap" $fn) "ips" .Values.bootstrap.nodeIPs "dns" list "ca" $ca) | fromYaml) -}}
{{- toYaml $out -}}
{{- end -}}
