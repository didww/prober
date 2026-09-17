{{- define "prober-backend.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prober-backend.fullname" -}}
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

{{- define "prober-backend.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "prober-backend.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "prober-backend.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prober-backend.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* The config, with every listen host forced to 0.0.0.0 (a loopback listener
in a pod is reachable from nothing). Ports are left as configured. */}}
{{- define "prober-backend.renderedConfig" -}}
{{- $cfg := deepCopy .Values.config -}}
{{- $l := $cfg.listen -}}
{{- $_ := set $l "http" (printf "0.0.0.0:%s" (last (splitList ":" (default "0.0.0.0:8080" $l.http)))) -}}
{{- if $l.metrics -}}
{{- $_ := set $l "metrics" (printf "0.0.0.0:%s" (last (splitList ":" $l.metrics))) -}}
{{- end -}}
{{- if $l.grpc -}}
{{- $_ := set $l.grpc "addr" (printf "0.0.0.0:%s" (last (splitList ":" (default "0.0.0.0:50051" $l.grpc.addr)))) -}}
{{- if include "prober-backend.tlsSecretName" . -}}
{{- $_ := set $l.grpc "cert_file" "/etc/prober/tls/tls.crt" -}}
{{- $_ := set $l.grpc "key_file" "/etc/prober/tls/tls.key" -}}
{{- end -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{- define "prober-backend.httpPort" -}}
{{- last (splitList ":" (default "0.0.0.0:8080" .Values.config.listen.http)) -}}
{{- end -}}
{{- define "prober-backend.grpcPort" -}}
{{- last (splitList ":" (default "0.0.0.0:50051" .Values.config.listen.grpc.addr)) -}}
{{- end -}}

{{/* The TLS secret the gateway mounts: an existing one, or the chart-created
one from tls.cert/tls.key. Empty when TLS is supplied inline in the config. */}}
{{- define "prober-backend.tlsSecretName" -}}
{{- if .Values.tls.secretName -}}
{{- .Values.tls.secretName -}}
{{- else if and .Values.tls.cert .Values.tls.key -}}
{{- printf "%s-tls" (include "prober-backend.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "prober-backend.basePath" -}}
{{- default "" .Values.config.base_path -}}
{{- end -}}

{{- define "prober-backend.metricsPort" -}}
{{- .Values.metricsPort -}}
{{- end -}}

{{- define "prober-backend.metricsPath" -}}
/metrics
{{- end -}}
