{{/* 名字与标签惯例(Helm 官方 _helpers 形态;component 维度 = braid 特有)。 */}}

{{- define "braid.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "braid.fullname" -}}
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

{{- define "braid.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* 通用标签;component 必传(dict "ctx" . "component" "api")。
     app.kubernetes.io/component 由 selectorLabels 提供(勿在此重复)。 */}}
{{- define "braid.labels" -}}
helm.sh/chart: {{ include "braid.chart" .ctx }}
{{ include "braid.selectorLabels" . }}
app.kubernetes.io/version: {{ .ctx.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
{{- end -}}

{{- define "braid.selectorLabels" -}}
app.kubernetes.io/name: {{ include "braid.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* Service 主机名(组件 DSN/连接目标;release 限定)。 */}}
{{- define "braid.mysqlHost" -}}
{{ include "braid.fullname" . }}-mysql
{{- end -}}

{{- define "braid.redisHost" -}}
{{ include "braid.fullname" . }}-redis-queue
{{- end -}}

{{/* Secret 名:existingSecret 优先,否则渲染 <fullname>-secret。 */}}
{{- define "braid.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{ include "braid.fullname" . }}-secret
{{- end -}}
{{- end -}}
