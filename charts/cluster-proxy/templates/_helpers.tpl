{{/*
Validate that .Values.metrics.port is an integer in [1, 65535] and return it.
Used by manager args (--metrics-bind-address), manager containerPort, and the
metrics Service port so an invalid value aborts the render instead of
producing a broken Deployment or Service.
*/}}
{{- define "cluster-proxy.metricsPort" -}}
{{- $port := .Values.metrics.port -}}
{{- if kindIs "invalid" $port -}}
{{- fail "metrics.port is required and must be an integer between 1 and 65535" -}}
{{- end -}}
{{- $portStr := toString $port -}}
{{- if not (regexMatch "^[0-9]+$" $portStr) -}}
{{- fail (printf "metrics.port must be an integer between 1 and 65535, got %q" $portStr) -}}
{{- end -}}
{{- $portInt := int $portStr -}}
{{- if or (lt $portInt 1) (gt $portInt 65535) -}}
{{- fail (printf "metrics.port must be between 1 and 65535, got %d" $portInt) -}}
{{- end -}}
{{- $portInt -}}
{{- end -}}
