{{/*
Cross-value checks of evidence.agents the schema cannot express. Included by
every evidence template, so each fails the same way.
*/}}
{{- define "pgtools.evidence.validate" -}}
{{- $e := .Values.evidence -}}
{{- $seen := dict -}}
{{- range $agent := $e.agents -}}
{{- if hasKey $seen $agent.cluster -}}
{{- fail (printf "evidence.agents: the Cluster %s is listed twice" $agent.cluster) -}}
{{- end -}}
{{- $_ := set $seen $agent.cluster true -}}
{{- if and $agent.stanza $agent.archive -}}
{{- fail (printf "evidence.agents: the Cluster %s names both a stanza and an archive; a Cluster uses one pgBackRest plugin" $agent.cluster) -}}
{{- end -}}
{{- if and $agent.objectStore (or $agent.stanza $agent.archive) -}}
{{- fail (printf "evidence.agents: the Cluster %s names an objectStore and a pgBackRest stanza or archive; a Cluster uses one backup plugin" $agent.cluster) -}}
{{- end -}}
{{- with $agent.pgbackrest -}}
{{- if not (or $agent.stanza $agent.archive) -}}
{{- fail (printf "evidence.agents: the Cluster %s sets pgbackrest without its stanza (Dalibo's plugin) or archive (Opera's)" $agent.cluster) -}}
{{- end -}}
{{- with .endpointCA -}}
{{- if not (has .name ($agent.secrets | default list)) -}}
{{- fail (printf "evidence.agents: the Cluster %s names the endpoint CA Secret %s, which its secrets must list" $agent.cluster .name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* The final object names, after the length cut, must still differ. */ -}}
{{- $names := dict -}}
{{- range $agent := $e.agents -}}
{{- $n := include "pgtools.evidence.name" (dict "root" $ "agent" $agent) -}}
{{- if hasKey $names $n -}}
{{- fail (printf "evidence.agents: the Clusters %s and %s both resolve to the object name %s; shorten the release name or the Cluster names" (get $names $n) $agent.cluster $n) -}}
{{- end -}}
{{- $_ := set $names $n $agent.cluster -}}
{{- end -}}
{{- if and $e.agents (eq $e.tls.mode "certManager") (not $e.tls.certManager.issuerRef.name) -}}
{{- fail "evidence.tls.certManager.issuerRef.name is required with evidence.tls.mode=certManager" -}}
{{- end -}}
{{- if and $e.agents .Values.console.enabled .Values.console.evidence.enabled -}}
{{- $watched := include "pgtools.console.watchNamespaces" . | fromJsonArray -}}
{{- if not (has .Release.Namespace $watched) -}}
{{- fail "evidence.agents run in the release namespace, which console.watchNamespaces must include for the console to read them" -}}
{{- end -}}
{{- end -}}
{{- end -}}
