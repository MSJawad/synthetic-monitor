{{/* The release name, plus the chart name unless the release name has it already. */}}
{{- define "synthetic-monitor.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "synthetic-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The image's tag: image.tag, or else the appVersion CI gives a published chart. */}}
{{- define "synthetic-monitor.imageTag" -}}
{{- required "image.tag is required when the chart has no appVersion: the commit SHA of the image to run" (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/*
helm.sh/chart has "+" replaced because label values can't contain one, and
a version can: Flux adds a chart's digest to the version of a chart it pulls
from a registry, as in 0.1.17+b3ead67ba00c.
*/}}
{{- define "synthetic-monitor.labels" -}}
{{ include "synthetic-monitor.selectorLabels" . }}
app.kubernetes.io/version: {{ include "synthetic-monitor.imageTag" . | trunc 63 | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}
