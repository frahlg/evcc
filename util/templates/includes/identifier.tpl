{{- define "identifier" }}
{{- if .identifier }}
identifier:
  source: {{ .identifier.source }}
  register:
    address: {{ .identifier.register.address }}
    type: {{ .identifier.register.type }}
    decode: {{ .identifier.register.decode }}
    {{- if .identifier.register.length }}
    length: {{ .identifier.register.length }}
    {{- end }}
{{- end }}
{{- end }}