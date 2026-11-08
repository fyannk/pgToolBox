{{- define "gvDetails" -}}
{{- $gv := . -}}

## {{ $gv.GroupVersionString }}

The kinds of the operator, `PgAdmin`, `EvidenceAgent` and `PgAccessRequest`. Field descriptions are
the Go doc comments of `api/v1alpha1`; the Validation column lists the
`kubebuilder` markers, and the cross-field CEL rules are collected at the end of
the page.

{{ if $gv.Kinds  }}
### Resource Types
{{- range $gv.SortedKinds }}
- {{ $gv.TypeForKind . | markdownRenderTypeLink }}
{{- end }}
{{ end }}

{{ range $gv.SortedTypes }}
{{ template "type" . }}
{{ end }}

{{- end -}}
