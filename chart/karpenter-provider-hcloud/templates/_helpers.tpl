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

{{/*
Feature gates reach core only through controller.featureGates, because RBAC is
rendered from that map. A gate set through extraArgs or extraEnv would run
without its grant, and the flag would override the map. Values must be bools:
core parses the rendered string with ParseBool, while a template `if` treats
the string "false" as true.
*/}}
{{- define "karpenter-provider-hcloud.validateFeatureGates" -}}
{{- range .Values.controller.extraArgs -}}
{{- if regexMatch "^--?feature-gates" (toString .) -}}
{{- fail "set feature gates through controller.featureGates, not controller.extraArgs" -}}
{{- end -}}
{{- end -}}
{{- range .Values.controller.extraEnv -}}
{{- if eq (toString .name) "FEATURE_GATES" -}}
{{- fail "set feature gates through controller.featureGates, not controller.extraEnv" -}}
{{- end -}}
{{- end -}}
{{- range $name, $enabled := .Values.controller.featureGates -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "controller.featureGates.%s must be true or false, got %q" $name (toString $enabled)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "karpenter-provider-hcloud.featureGates" -}}
{{- $pairs := list -}}
{{- range $name, $enabled := .Values.controller.featureGates -}}
{{- $pairs = append $pairs (printf "%s=%t" $name $enabled) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end -}}

{{- define "karpenter-provider-hcloud.leaderElectionName" -}}
{{- printf "%s-leader-election" (include "karpenter-provider-hcloud.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
