{{- define "ngas.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 50 | trimSuffix "-" -}}
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
{{- if not .Values.vngcloud.credentials.existingSecret -}}{{- fail "vngcloud.credentials.existingSecret is required" -}}{{- end -}}
{{- if not (and .Values.vngcloud.vserverEndpoint .Values.vngcloud.projectId) -}}{{- fail "vngcloud.vserverEndpoint and vngcloud.projectId are required" -}}{{- end -}}
{{- if and (not .Values.workerTemplate.json) (not .Values.workerTemplate.existingConfigMap) -}}
{{- fail "workerTemplate: pass --set-file workerTemplate.json=<docker inspect of a worker> or workerTemplate.existingConfigMap" -}}
{{- end -}}
{{- end -}}
