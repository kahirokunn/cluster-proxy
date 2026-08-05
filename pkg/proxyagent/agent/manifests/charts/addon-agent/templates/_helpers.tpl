{{- define "cluster-proxy-addon.oidcAuthenticationEnabled" -}}
{{- if .Values.oidcIssuerURL -}}true{{- else -}}false{{- end -}}
{{- end -}}
