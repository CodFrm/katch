{{/* chart 名，可被 nameOverride 覆盖 */}}
{{- define "katch.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* 资源名前缀 */}}
{{- define "katch.fullname" -}}
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

{{- define "katch.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "katch.labels" -}}
helm.sh/chart: {{ include "katch.chart" . }}
{{ include "katch.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "katch.selectorLabels" -}}
app.kubernetes.io/name: {{ include "katch.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
装配置的 Secret 叫什么。自带 Secret 时就是它，否则是 chart 生成的那个。
挂载处和 checksum 注解处都要用到同一个名字，所以抽出来只写一次。
*/}}
{{- define "katch.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "katch.fullname" . -}}
{{- end -}}
{{- end -}}

{{/* 数据卷用哪个 PVC */}}
{{- define "katch.claimName" -}}
{{- if .Values.persistence.existingClaim -}}
{{- .Values.persistence.existingClaim -}}
{{- else -}}
{{- printf "%s-data" (include "katch.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
渲染出来的 config.yaml 正文。
secret.yaml 要它当内容，deployment.yaml 要它算 checksum——两处必须是同一份字节，
否则「改了配置但 Pod 没重启」和「没改配置却每次 upgrade 都重启」会轮流出现。
*/}}
{{- define "katch.configYaml" -}}
{{- toYaml .Values.config -}}
{{- end -}}
