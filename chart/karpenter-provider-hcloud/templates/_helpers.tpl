{{- define "karpenter-provider-hcloud.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "karpenter-provider-hcloud.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "karpenter-provider-hcloud.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "karpenter-provider-hcloud.labels" -}}
helm.sh/chart: {{ include "karpenter-provider-hcloud.chart" . }}
{{ include "karpenter-provider-hcloud.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: karpenter
{{- end -}}

{{- define "karpenter-provider-hcloud.selectorLabels" -}}
app.kubernetes.io/name: {{ include "karpenter-provider-hcloud.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "karpenter-provider-hcloud.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "karpenter-provider-hcloud.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "karpenter-provider-hcloud.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "karpenter-provider-hcloud.leaderElectionName" -}}
{{- printf "%s-leader-election" (include "karpenter-provider-hcloud.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
