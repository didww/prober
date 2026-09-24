{{- define "prober-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prober-agent.fullname" -}}
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

{{- define "prober-agent.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "prober-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "prober-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prober-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The rendered agent config, built from values. */}}
{{- define "prober-agent.renderedConfig" -}}
backend:
  address: {{ required "prober-agent: backend.address is required" .Values.backend.address | quote }}
  site: {{ required "prober-agent: site is required" .Values.site | quote }}
  token: {{ required "prober-agent: backend.token is required (or set existingSecret)" .Values.backend.token | quote }}
{{- with .Values.backend.serverName }}
  server_name: {{ . | quote }}
{{- end }}
{{- if .Values.backend.insecureSkipVerify }}
  insecure_skip_verify: true
{{- else }}
{{- if .Values.backend.skipHostnameVerify }}
  skip_hostname_verify: true
{{- end }}
{{- with .Values.backend.ca }}
  ca: |
    {{- . | nindent 4 }}
{{- end }}
{{- end }}
limits:
  max_concurrent_jobs: {{ .Values.limits.maxConcurrentJobs }}
  max_cycles: {{ .Values.limits.maxCycles }}
  min_interval_ms: {{ .Values.limits.minIntervalMs }}
  max_ttl: {{ .Values.limits.maxTtl }}
{{- with .Values.dns.nameservers }}
dns:
  nameservers:
{{- range . }}
    - {{ . | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "prober-agent.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-config" (include "prober-agent.fullname" .) -}}
{{- end -}}
{{- end -}}
