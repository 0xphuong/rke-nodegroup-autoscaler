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
{{- if not .Values.nodeCerts.existingSecret -}}{{- fail "nodeCerts.existingSecret: create the node certificate Secret first (see values.yaml)" -}}{{- end -}}
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
