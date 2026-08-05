{{/*
Normalize enableHubTokenAuthentication to "true" or "false", inheriting the
deprecated enableImpersonation value when unset (nil or empty string).
*/}}
{{- define "cluster-proxy-common.hubTokenAuthenticationEnabled" -}}
{{- $value := .Values.enableHubTokenAuthentication -}}
{{- if or (kindIs "invalid" $value) (eq (toString $value) "") -}}
{{- $value = .Values.enableImpersonation -}}
{{- end -}}
{{- if eq (toString $value) "true" -}}true{{- else -}}false{{- end -}}
{{- end -}}
