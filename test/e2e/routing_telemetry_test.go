//go:build e2e
// +build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/heddles/agent-orca/test/utils"
)

// Routing telemetry: a run's LLM calls become routing_events rows, and the
// archived run gets a run_outcomes row. The model is a deterministic fake
// (test/fakellm), so the assertions are exact.
var _ = Describe("Routing telemetry", Label("routing-e2e"), Ordered, func() {
	const (
		ns        = "default"
		fakeImage = "example.com/fakellm:v0.0.1"
		// internalAPIService fronts the operator's internal API (port 8082) for routers.
		internalAPIService = "agent-orca-internal-api"
		pgDSN              = "postgres://postgres:e2e@e2e-postgres.default.svc:5432/agentorca?sslmode=disable"
		runName            = "e2e-routing-telemetry"
		localPort          = 19090
		routerPort         = 8080
	)

	psql := func(query string) string {
		GinkgoHelper()
		out, err := utils.Run(exec.Command("kubectl", "exec", "deploy/e2e-postgres", "-n", ns, "--",
			"psql", "-U", "postgres", "-d", "agentorca", "-tAc", query))
		Expect(err).NotTo(HaveOccurred())
		return strings.TrimSpace(out)
	}

	// When a spec fails, say what the pipeline looked like: the failure is in
	// the router, the operator or the database, and a bare timeout names none.
	AfterEach(func() {
		if !CurrentSpecReport().Failed() || !routingE2E() {
			return
		}
		dump := func(title string, cmd *exec.Cmd) {
			out, err := utils.Run(cmd)
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- %s (err=%v) ---\n%s\n", title, err, out)
		}
		dump("routing_events", exec.Command("kubectl", "exec", "deploy/e2e-postgres", "-n", ns, "--", "psql", "-U", "postgres",
			"-d", "agentorca", "-c", "select decision_id, run_name, provider, path, prompt_tokens, cost_usd, ttft_ms from routing_events"))
		dump("migrations", exec.Command("kubectl", "exec", "deploy/e2e-postgres", "-n", ns, "--", "psql", "-U", "postgres",
			"-d", "agentorca", "-c", "select version, name from schema_migrations"))
		dump("operator log (routing, postgres)", exec.Command("sh", "-c", fmt.Sprintf(
			"kubectl logs deploy/agent-orca-controller-manager -n %s --tail=400 | grep -iE 'routing|postgres|migrat|events' | tail -40", namespace)))
		dump("router log", exec.Command("sh", "-c", fmt.Sprintf(
			"kubectl logs agentorca-run-%s -n %s -c model-router --tail=200 | grep -iE 'routing|telemetry|events|operator|WARN|ERROR' | tail -40", runName, ns)))
	})

	BeforeAll(func() {
		if !routingE2E() {
			Skip("requires ROUTING_E2E=true (fake LLM and PostgreSQL are deployed by this spec)")
		}
		waitForCRDReady("agentruns.agentorca.agentorca.io")

		By("building and loading the fake LLM image")
		_, err := utils.Run(exec.Command("docker", "build", "-f", "test/fakellm/Dockerfile", "-t", fakeImage, "."))
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.LoadImageToKindClusterWithName(fakeImage)).To(Succeed())

		By("deploying PostgreSQL and the fake LLM")
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: apps/v1
kind: Deployment
metadata: {name: e2e-postgres, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: e2e-postgres}}
  template:
    metadata: {labels: {app: e2e-postgres}}
    spec:
      containers:
        - name: postgres
          image: postgres:16-alpine
          env:
            - {name: POSTGRES_PASSWORD, value: e2e}
            - {name: POSTGRES_DB, value: agentorca}
          ports: [{containerPort: 5432}]
          readinessProbe:
            exec: {command: ["pg_isready", "-U", "postgres", "-d", "agentorca"]}
            periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata: {name: e2e-postgres, namespace: %[1]s}
spec:
  selector: {app: e2e-postgres}
  ports: [{port: 5432}]
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: fakellm, namespace: %[1]s}
spec:
  replicas: 1
  selector: {matchLabels: {app: fakellm}}
  template:
    metadata: {labels: {app: fakellm}}
    spec:
      containers:
        - name: fakellm
          image: %[2]s
          imagePullPolicy: Never
          ports: [{containerPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: fakellm, namespace: %[1]s}
spec:
  selector: {app: fakellm}
  ports: [{port: 8080}]
`, ns, fakeImage))).To(Succeed())
		for _, d := range []string{"e2e-postgres", "fakellm"} {
			_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deploy/"+d, "-n", ns, "--timeout=180s"))
			Expect(err).NotTo(HaveOccurred(), "deployment %s did not become ready", d)
		}

		// The Helm chart gives routers the operator's internal API through a Service and
		// OPERATOR_API_URL; the kustomize deployment this suite runs on has neither. A
		// router without the URL posts to http://localhost:8082, which in its own pod is
		// its Gemini listener, so every post would 404.
		By("giving routers the operator's internal API and pointing the operator at PostgreSQL")
		Expect(applyYAML(fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata: {name: %[2]s, namespace: %[1]s}
spec:
  selector: {control-plane: controller-manager}
  ports: [{name: internal-api, port: 8082, targetPort: 8082}]
`, namespace, internalAPIService))).To(Succeed())
		_, err = utils.Run(exec.Command("kubectl", "set", "env", "deployment/agent-orca-controller-manager",
			"POSTGRES_DSN="+pgDSN,
			fmt.Sprintf("OPERATOR_API_URL=http://%s.%s.svc.cluster.local:8082", internalAPIService, namespace),
			"-n", namespace))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/agent-orca-controller-manager",
			"-n", namespace, "--timeout=180s"))
		Expect(err).NotTo(HaveOccurred())

		By("creating the model wiring and a run")
		deleteResource("agentrun", runName, ns)
		// The operator was just rolled to pick up POSTGRES_DSN. The rollout reports
		// ready slightly before its admission webhook answers, so the first apply
		// can time out in the webhook; applying is idempotent, so retry until it
		// goes through.
		resources := fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata: {name: e2e-routing-key, namespace: %[1]s}
stringData: {api-key: e2e-placeholder}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelProvider
metadata: {name: e2e-fake-strong, namespace: %[1]s}
spec:
  litellmModel: "openai/strong"
  baseURL: "http://fakellm.%[1]s.svc:8080"
  credentialsRef: {name: e2e-routing-key, key: api-key}
  constraints:
    contextWindow: 128000
    costPerMillionInputTokens: "3.00"
    costPerMillionOutputTokens: "15.00"
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: ModelSelector
metadata: {name: e2e-routing, namespace: %[1]s}
spec:
  strategy: rule-based
  providers:
    - {name: e2e-fake-strong, weight: 100}
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: Agent
metadata: {name: e2e-routing-agent, namespace: %[1]s}
spec:
  modelSelectorRef: e2e-routing
  runtime:
    ociRef: "busybox:1.36"
    framework: openai-compatible
    command: ["sleep"]
    args: ["infinity"]
---
apiVersion: agentorca.agentorca.io/v1alpha1
kind: AgentRun
metadata:
  name: %[2]s
  namespace: %[1]s
  annotations: {agentorca.io/enforcement-mode: "off"}
spec:
  agentRef: e2e-routing-agent
  input: "routing telemetry e2e"
`, ns, runName)
		Eventually(func() error { return applyYAML(resources) }, 3*time.Minute, 5*time.Second).
			Should(Succeed(), "the run's resources were never admitted")
	})

	AfterAll(func() {
		if !routingE2E() {
			return
		}
		deleteResource("agentrun", runName, ns)
		deleteResource("agent", "e2e-routing-agent", ns)
		deleteResource("modelselector", "e2e-routing", ns)
		deleteResource("modelprovider", "e2e-fake-strong", ns)
		deleteResource("deployment", "fakellm", ns)
		deleteResource("deployment", "e2e-postgres", ns)
		deleteResource("service", internalAPIService, namespace)
	})

	It("records one routing event per LLM call with its cost and path", func() {
		pod := "agentorca-run-" + runName
		waitForPodRunning(pod, ns)
		token := getRunToken(runName, ns)
		cleanup := portForward(ns, "pod/"+pod, localPort, routerPort)
		defer cleanup()

		res := postChatCompletion(localPort, token)
		Expect(res.Status).To(Equal(200))
		// The fake streams its reply in small chunks ("ok from stro", "ng"), so the
		// raw body never holds it whole: check the stream ran to a clean end.
		Expect(res.Body).To(ContainSubstring(`"finish_reason":"stop"`))
		Expect(res.Body).To(ContainSubstring("[DONE]"))

		// The router batches events and flushes every couple of seconds.
		query := fmt.Sprintf("select count(*) from routing_events where run_name='%s' and provider='e2e-fake-strong' and path='rule:weighted' and prompt_tokens>0 and cost_usd>0 and ttft_ms is not null", runName)
		Eventually(func() int {
			n, _ := strconv.Atoi(psql(query))
			return n
		}, 60*time.Second, 2*time.Second).Should(Equal(1), "expected exactly one fully-populated routing event")
	})

	It("stores no prompt or response content", func() {
		cols := psql("select string_agg(column_name, ',') from information_schema.columns where table_name='routing_events'")
		for _, banned := range []string{"prompt,", "response", "content", "message", "output"} {
			Expect(cols).NotTo(ContainSubstring(banned), "routing_events must hold metadata only")
		}
	})
})
