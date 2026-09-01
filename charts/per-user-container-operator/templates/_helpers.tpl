{{/*
puc.name is the operator's own name -- deliberately NOT release-prefixed.
This is the fixed value api/v1alpha1.OperatorServiceAccountName pins as a
Go constant: the workspace-rejection check in ValidateApp compares a
PerUserApp's spec.workspace.serviceAccountName against that exact string,
so if this chart named the SA `{{ include "chart.fullname" . }}` instead
(the reflexive scaffolding default), the two strings would differ and the
check that is supposed to reject "a workspace pod running as the
operator's own ServiceAccount" would silently pass a release-prefixed name
through instead.
*/}}
{{- define "puc.name" -}}
per-user-container-operator
{{- end -}}

{{/*
puc.routerRoleName mirrors api/v1alpha1.RouterRoleName byte-for-byte: the
controller (Task 11) hardcodes this exact string as the roleRef.name on
every <app>-router RoleBinding it creates at runtime, so a Role rendered
under any other name is a dangling reference invisible to both tasks' own
test suites.
*/}}
{{- define "puc.routerRoleName" -}}
per-user-container-operator-router
{{- end -}}

{{/*
puc.controllerPodLabels is the single source of the controller Deployment's
pod labels, its Service selector, its metrics Service's own labels, and the
controller ServiceMonitor's selector -- all four must agree byte-for-byte or
the Service/selector mismatch selects nothing with no error and no series.
LabelPartOf/PartOfValue and LabelComponent/ComponentController mirror
api/v1alpha1/constants.go exactly.
*/}}
{{- define "puc.controllerPodLabels" -}}
app.kubernetes.io/part-of: per-user-container-operator
puc.kettleofketchup/component: controller
{{- end -}}

{{/*
puc.monitoringLabels merges .Values.metrics.serviceMonitor.labels (an
operator-supplied selection label, e.g. release: prometheus, for
Prometheus Operator's own ServiceMonitorSelector/RuleSelector) onto every
monitoring object this chart renders.
*/}}
{{- define "puc.monitoringLabels" -}}
{{- if .Values.metrics.serviceMonitor.labels -}}
{{ toYaml .Values.metrics.serviceMonitor.labels }}
{{- else -}}
{}
{{- end -}}
{{- end -}}

{{/*
puc.image renders the single image reference both the controller container
and RELATED_IMAGE_ROUTER resolve to -- one image, two subcommands
(cmd/main.go's argv[1] dispatch), so these two call sites must never be
allowed to name different images.
*/}}
{{- define "puc.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
