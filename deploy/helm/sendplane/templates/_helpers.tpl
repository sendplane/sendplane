{{/*
Chart name, truncated and trimmed to fit a Kubernetes name.
*/}}
{{- define "sendplane.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name, e.g. "myrelease-sendplane". Honors
fullnameOverride, and avoids "sendplane-sendplane" when the release is
already named "sendplane".
*/}}
{{- define "sendplane.fullname" -}}
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

{{- define "sendplane.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels, per the Kubernetes recommended set.
*/}}
{{- define "sendplane.labels" -}}
helm.sh/chart: {{ include "sendplane.chart" . }}
{{ include "sendplane.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels shared by every role's Deployment/Service. A caller adds
app.kubernetes.io/component itself so one template serves all three roles.
*/}}
{{- define "sendplane.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sendplane.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
The Secret carrying SENDPLANE_STORE_DSN / SENDPLANE_SECRETS_KEY /
SENDPLANE_WEBHOOK_SECRET: either the caller's existingSecret, or the one
templates/secret.yaml creates from .Values.secrets.*.
*/}}
{{- define "sendplane.secretName" -}}
{{- .Values.secrets.existingSecret | default (printf "%s-secrets" (include "sendplane.fullname" .)) -}}
{{- end -}}

{{- define "sendplane.configMapName" -}}
{{- printf "%s-config" (include "sendplane.fullname" .) -}}
{{- end -}}

{{/*
image.tag defaults to Chart.appVersion, the usual Helm convention (keeps
values.yaml's image.tag optional for a chart whose appVersion tracks the
image it deploys).
*/}}
{{- define "sendplane.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/*
envFrom entry pulling in the DSN/keys Secret. Every role's container needs
it: config.yaml's ${SENDPLANE_*} references are expanded from this env by
the binary itself (cmd/sendplane's os.ExpandEnv over the config file).
*/}}
{{- define "sendplane.envFrom" -}}
- secretRef:
    name: {{ include "sendplane.secretName" . }}
{{- end -}}

{{- define "sendplane.configVolume" -}}
- name: config
  configMap:
    name: {{ include "sendplane.configMapName" . }}
{{- end -}}

{{- define "sendplane.configVolumeMount" -}}
- name: config
  mountPath: /etc/sendplane
  readOnly: true
{{- end -}}

{{/*
/healthz answers regardless of which roles a pod runs (cmd/sendplane always
starts the HTTP server; sp.Handler() is only mounted at "/" when the control
role is present) — see cmd/sendplane/main.go's serve().
*/}}
{{- define "sendplane.probes" -}}
livenessProbe:
  httpGet:
    path: /healthz
    port: http
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 3
readinessProbe:
  httpGet:
    path: /healthz
    port: http
  initialDelaySeconds: 5
  periodSeconds: 10
  failureThreshold: 3
{{- end -}}
