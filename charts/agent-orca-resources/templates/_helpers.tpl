{{/*
Expand the name of the chart.
*/}}
{{- define "agent-orca-resources.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "agent-orca-resources.labels" -}}
helm.sh/chart: {{ include "agent-orca-resources.name" . }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "agent-orca-resources.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Render a credentialsRef secret block.
Accepts a dict with keys: name (secret name), value (apiKey), existingSecret (map with name/key).
Outputs nothing if existingSecret is set (user manages the secret externally).
*/}}
{{- define "agent-orca-resources.providerSecrets" -}}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.providerSecrets }}
---
apiVersion: v1
kind: Secret
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
  annotations:
    helm.sh/resource-policy: keep
type: Opaque
stringData:
  api-key: {{ .apiKey | quote }}
{{- end }}
{{- end }}

{{- define "agent-orca-resources.agents" -}}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.agents }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  modelSelectorRef: {{ .modelSelectorRef | quote }}
  {{- if .defaultRuntimePolicyRef }}
  defaultRuntimePolicyRef:
    {{- toYaml .defaultRuntimePolicyRef | nindent 4 }}
  {{- end }}
  {{- if .guardrailPolicyRef }}
  guardrailPolicyRef: {{ .guardrailPolicyRef | quote }}
  {{- end }}
  disableClarify: {{ .disableClarify | default false }}
  runtime:
    ociRef: {{ .runtime.ociRef | quote }}
    {{- if .runtime.framework }}
    framework: {{ .runtime.framework }}
    {{- end }}
    {{- if .runtime.inputMode }}
    inputMode: {{ .runtime.inputMode }}
    {{- end }}
    {{- if .runtime.inputPort }}
    inputPort: {{ .runtime.inputPort }}
    {{- end }}
    {{- if .runtime.inputPath }}
    inputPath: {{ .runtime.inputPath | quote }}
    {{- end }}
    {{- if .runtime.shimTarget }}
    shimTarget: {{ .runtime.shimTarget | quote }}
    {{- end }}
    {{- if .runtime.command }}
    command:
      {{- toYaml .runtime.command | nindent 6 }}
    {{- end }}
    {{- if .runtime.args }}
    args:
      {{- toYaml .runtime.args | nindent 6 }}
    {{- end }}
    {{- if .runtime.securityContextOverride }}
    securityContextOverride:
      {{- toYaml .runtime.securityContextOverride | nindent 6 }}
    {{- end }}
    {{- if .runtime.secretRefs }}
    secretRefs:
      {{- toYaml .runtime.secretRefs | nindent 6 }}
    {{- end }}
  {{- if .systemPrompt }}
  systemPrompt: |
    {{- .systemPrompt | nindent 4 }}
  {{- end }}
  {{- if .tools }}
  tools:
    {{- toYaml .tools | nindent 4 }}
  {{- end }}
  {{- if .knowledgeBases }}
  knowledgeBases:
    {{- toYaml .knowledgeBases | nindent 4 }}
  {{- end }}
  {{- if .filesystem }}
  {{/* ACP fs/* relay policy (issue #34): nil omits the field entirely, meaning
      no filesystem tools — fail closed like the CRD default. */}}
  filesystem:
    {{- toYaml .filesystem | nindent 4 }}
  {{- end }}
  {{- if .shell }}
  {{/* sandbox_exec tool (spec.shell): needs an OpenShell gateway, a namespace
      labelled agentorca.io/enable-sandboxes=true and a SandboxClass. */}}
  shell:
    {{- toYaml .shell | nindent 4 }}
  {{- end }}
  {{- if .memory }}
  memory:
    {{- toYaml .memory | nindent 4 }}
  {{- end }}
  {{- if .resources }}
  resources:
    {{- toYaml .resources | nindent 4 }}
  {{- end }}
  {{- if .serviceAccountRef }}
  serviceAccountRef:
    {{- toYaml .serviceAccountRef | nindent 4 }}
  {{- end }}
  {{- if .cloudAuth }}
  cloudAuth:
    {{- toYaml .cloudAuth | nindent 4 }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.agentdeployments" -}}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.agentDeployments }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: AgentDeployment
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  agentRef: {{ .agentRef | quote }}
  {{- if .inputSource }}
  inputSource:
    {{- toYaml .inputSource | nindent 4 }}
  {{- end }}
  {{- if .restartPolicy }}
  restartPolicy:
    {{- toYaml .restartPolicy | nindent 4 }}
  {{- end }}
  {{- if .replicas }}
  replicas: {{ .replicas }}
  {{- end }}
  {{- if .checkpointTTL }}
  checkpointTTL: {{ .checkpointTTL }}
  {{- end }}
  {{- if hasKey . "warmPoolSize" }}
  warmPoolSize: {{ .warmPoolSize }}
  {{- end }}
  {{- if hasKey . "maxRequestsPerPod" }}
  maxRequestsPerPod: {{ .maxRequestsPerPod }}
  {{- end }}
  {{- if hasKey . "recycleOnConfigDrift" }}
  recycleOnConfigDrift: {{ .recycleOnConfigDrift }}
  {{- end }}
  {{- if hasKey . "toolExecutionTimeoutSec" }}
  toolExecutionTimeoutSec: {{ .toolExecutionTimeoutSec }}
  {{- end }}
  {{- if hasKey . "warmLocalCache" }}
  warmLocalCache: {{ .warmLocalCache }}
  {{- end }}
  {{- if hasKey . "warmLocalCacheSizeMi" }}
  warmLocalCacheSizeMi: {{ .warmLocalCacheSizeMi }}
  {{- end }}
  {{- if hasKey . "webhookNotify" }}
  webhookNotify:
    {{- toYaml .webhookNotify | nindent 4 }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.tools" -}}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.tools }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Tool
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  {{- if .type }}
  type: {{ .type }}
  {{- end }}
  {{- if .ociRef }}
  ociRef: {{ .ociRef | quote }}
  {{- end }}
  {{- if .command }}
  command:
    {{- toYaml .command | nindent 4 }}
  {{- end }}
  {{- if .args }}
  args:
    {{- toYaml .args | nindent 4 }}
  {{- end }}
  {{- if .agentRef }}
  agentRef: {{ .agentRef | quote }}
  {{- end }}
  {{- if .executionMode }}
  executionMode: {{ .executionMode }}
  {{- end }}
  {{- if .schema }}
  schema:
    {{- toYaml .schema | nindent 4 }}
  {{- end }}
  {{- if .mcpConfig }}
  mcpConfig:
    {{- toYaml .mcpConfig | nindent 4 }}
  {{- end }}
  {{- if .networkEgress }}
  networkEgress:
    {{- toYaml .networkEgress | nindent 4 }}
  {{- end }}
  {{- if .resources }}
  resources:
    {{- toYaml .resources | nindent 4 }}
  {{- end }}
  {{- if .secretRefs }}
  secretRefs:
    {{- toYaml .secretRefs | nindent 4 }}
  {{- end }}
  {{- if .cloudAuth }}
  cloudAuth:
    {{- toYaml .cloudAuth | nindent 4 }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.mcpservers" -}}
{{- $root := . }}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.mcpServers }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: MCPServer
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  {{- if .transport }}
  transport: {{ .transport }}
  {{- end }}
  {{- if .url }}
  url: {{ tpl .url $root | quote }}
  {{- end }}
  {{- if .ociRef }}
  ociRef: {{ .ociRef | quote }}
  {{- end }}
  {{- if .args }}
  args:
    {{- toYaml .args | nindent 4 }}
  {{- end }}
  {{- if .env }}
  env:
    {{- toYaml .env | nindent 4 }}
  {{- end }}
  {{- if .envFrom }}
  envFrom:
    {{- toYaml .envFrom | nindent 4 }}
  {{- end }}
  {{- if .auth }}
  auth:
    {{- toYaml .auth | nindent 4 }}
  {{- end }}
  tools:
    {{- toYaml .tools | nindent 4 }}
  {{- if .allowApps }}
  allowApps: {{ .allowApps }}
  {{- end }}
  {{- if .allowedAgents }}
  allowedAgents:
    {{- toYaml .allowedAgents | nindent 4 }}
  {{- end }}
  {{- if .networkEgress }}
  networkEgress:
    {{- range .networkEgress }}
    - host: {{ tpl .host $root | quote }}
      port: {{ .port }}
      protocol: {{ .protocol }}
    {{- end }}
  {{- end }}
  {{- if .resources }}
  resources:
    {{- toYaml .resources | nindent 4 }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.knowledgebases" -}}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.knowledgeBases }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: KnowledgeBase
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  {{- if .description }}
  description: {{ .description | quote }}
  {{- end }}
  {{- if .allowedAgents }}
  allowedAgents:
    {{- toYaml .allowedAgents | nindent 4 }}
  {{- end }}
  embedding:
    modelSelectorRef: {{ .embedding.modelSelectorRef | quote }}
    {{- if .embedding.chunkSize }}
    chunkSize: {{ .embedding.chunkSize }}
    {{- end }}
    {{- if .embedding.chunkOverlap }}
    chunkOverlap: {{ .embedding.chunkOverlap }}
    {{- end }}
  {{- if .vectorStore }}
  vectorStore:
    {{- toYaml .vectorStore | nindent 4 }}
  {{- end }}
  {{- if .ingestion }}
  ingestion:
    {{- toYaml .ingestion | nindent 4 }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.modelselectors" -}}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.modelSelectors }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelSelector
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  {{- if .strategy }}
  strategy: {{ .strategy }}
  {{- end }}
  providers:
    {{- toYaml .providers | nindent 4 }}
  {{- if .fallbackChain }}
  fallbackChain:
    {{- toYaml .fallbackChain | nindent 4 }}
  {{- end }}
  {{- if .budgetCap }}
  budgetCap:
    {{- toYaml .budgetCap | nindent 4 }}
  {{- end }}
  {{- if .capabilityRouting }}
  capabilityRouting:
    {{- toYaml .capabilityRouting | nindent 4 }}
  {{- end }}
  {{- if .metaRouter }}
  metaRouter:
    {{- toYaml .metaRouter | nindent 4 }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.modelproviders" -}}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.modelProviders }}
{{- $secretName := .credentialsRef.existingSecret | default (printf "%s-api-key" .name) }}
{{- if not .credentialsRef.existingSecret }}
---
apiVersion: v1
kind: Secret
metadata:
  name: {{ $secretName }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
  annotations:
    helm.sh/resource-policy: keep
type: Opaque
stringData:
  {{ .credentialsRef.key | default "api-key" }}: {{ .credentialsRef.apiKey | default "unused" | quote }}
{{- end }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelProvider
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  litellmModel: {{ .litellmModel | quote }}
  {{- if .baseURL }}
  baseURL: {{ .baseURL | quote }}
  {{- end }}
  credentialsRef:
    name: {{ $secretName }}
    key: {{ .credentialsRef.key | default "api-key" }}
  {{- if .capabilities }}
  capabilities:
    {{- toYaml .capabilities | nindent 4 }}
  {{- end }}
  {{- if .latencyProfile }}
  latencyProfile: {{ .latencyProfile }}
  {{- end }}
  {{- if .constraints }}
  constraints:
    {{- toYaml .constraints | nindent 4 }}
  {{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.guardrailpolicies" -}}
{{- if .Values.guardrailPolicies }}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.guardrailPolicies }}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: GuardrailPolicy
metadata:
  name: {{ .name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  {{- if .spec.inputFilters }}
  inputFilters:
    {{- toYaml .spec.inputFilters | nindent 4 }}
  {{- end }}
  {{- if .spec.outputFilters }}
  outputFilters:
    {{- toYaml .spec.outputFilters | nindent 4 }}
  {{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "agent-orca-resources.mcpserverdeployments" -}}
{{- if .Values.mcpServerDeployments }}
{{- $ns := .Release.Namespace }}
{{- $labels := include "agent-orca-resources.labels" . }}
{{- range .Values.mcpServerDeployments }}
{{- $name := .name }}
{{- $fileName := .fileName }}
{{- $image := .image | default "python:3.12-slim" }}
{{- $port := .port | default 8080 }}
{{- $cpuRequest := (.resources).requests.cpu | default "50m" }}
{{- $memRequest := (.resources).requests.memory | default "64Mi" }}
{{- $cpuLimit := (.resources).limits.cpu | default "200m" }}
{{- $memLimit := (.resources).limits.memory | default "128Mi" }}
{{- $probeInitial := (.readinessProbe).initialDelaySeconds | default 3 }}
{{- $probePeriod := (.readinessProbe).periodSeconds | default 5 }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ $name }}-code
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
data:
  {{ $fileName }}: |-
{{ $.Files.Get (printf "files/%s" $fileName) | indent 4 }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
    app: {{ $name }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ $name }}
  template:
    metadata:
      labels:
        app: {{ $name }}
        {{- $labels | nindent 8 }}
    spec:
      containers:
        - name: server
          image: {{ $image }}
          command: ["python3", "/app/{{ $fileName }}"]
          env:
            - name: PORT
              value: {{ $port | quote }}
            {{- if .env }}
            {{- toYaml .env | nindent 12 }}
            {{- end }}
          ports:
            - containerPort: {{ $port }}
              name: http
          volumeMounts:
            - name: code
              mountPath: /app
          readinessProbe:
            httpGet:
              path: /healthz
              port: {{ $port }}
            initialDelaySeconds: {{ $probeInitial }}
            periodSeconds: {{ $probePeriod }}
          resources:
            requests:
              cpu: {{ $cpuRequest }}
              memory: {{ $memRequest }}
            limits:
              cpu: {{ $cpuLimit }}
              memory: {{ $memLimit }}
      volumes:
        - name: code
          configMap:
            name: {{ $name }}-code
---
apiVersion: v1
kind: Service
metadata:
  name: {{ $name }}
  namespace: {{ $ns }}
  labels:
    {{- $labels | nindent 4 }}
spec:
  selector:
    app: {{ $name }}
  ports:
    - name: http
      port: {{ $port }}
      targetPort: {{ $port }}
{{- end }}
{{- end }}
{{- end -}}
