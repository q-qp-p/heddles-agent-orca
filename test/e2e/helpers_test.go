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
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/heddles/agent-orca/test/utils"
)

// streamingE2E returns true when streaming tests should run.
// These also require a valid LLM provider credential to get token responses.
func streamingE2E() bool { return os.Getenv("STREAMING_E2E") == "true" }

// routingE2E returns true when the routing telemetry specs should run. They
// deploy their own PostgreSQL and a deterministic fake LLM (test/fakellm), so
// they need no credentials.
func routingE2E() bool { return os.Getenv("ROUTING_E2E") == "true" }

// --- CRD readiness ---

// waitForCRDReady blocks until the named CRD exists, has no deletionTimestamp,
// and has Established=True. Checking deletionTimestamp is necessary because
// Established=True persists while a CRD is terminating.
func waitForCRDReady(crdName string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		// Must exist (no error) and have no deletionTimestamp.
		ts, err := utils.Run(exec.Command(
			"kubectl", "get", "crd", crdName,
			"-o", "jsonpath={.metadata.deletionTimestamp}",
		))
		g.Expect(err).NotTo(HaveOccurred(), "CRD %s not found", crdName)
		g.Expect(ts).To(BeEmpty(), "CRD %s still has deletionTimestamp", crdName)

		// Must be Established.
		status, err := utils.Run(exec.Command(
			"kubectl", "get", "crd", crdName,
			"-o", `jsonpath={.status.conditions[?(@.type=="Established")].status}`,
		))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(status).To(Equal("True"), "CRD %s not yet Established", crdName)
	}, 90*time.Second, 2*time.Second).Should(Succeed(),
		"CRD %s did not become ready within 90s", crdName)
}

// --- kubectl helpers ---

// applyYAML pipes the given YAML string to kubectl apply -f -.
func applyYAML(yaml string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	_, err := utils.Run(cmd)
	return err
}

// deleteResource deletes a resource by kind/name/namespace, ignoring not-found.
func deleteResource(kind, name, ns string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "delete", kind, name,
		"-n", ns, "--ignore-not-found", "--wait=true", "--timeout=60s")
	_, _ = utils.Run(cmd)
}

// waitForPodRunning waits until the named pod in ns reaches phase Running.
func waitForPodRunning(name, ns string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command(
			"kubectl", "get", "pod", name,
			"-n", ns,
			"-o", "jsonpath={.status.phase}",
		))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(Equal("Running"), "pod %s not yet Running", name)
	}, 4*time.Minute, 2*time.Second).Should(Succeed(),
		"pod %s did not reach Running within 4 min", name)
}

// getRunToken waits for the per-run token Secret to appear and returns the
// base64-decoded OPENAI_API_KEY value.
func getRunToken(runName, ns string) string {
	GinkgoHelper()
	secret := "agentorca-run-" + runName + "-token"
	var token string
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command(
			"kubectl", "get", "secret", secret,
			"-n", ns,
			"-o", "jsonpath={.data.OPENAI_API_KEY}",
		))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).NotTo(BeEmpty(), "token not yet in secret %s", secret)
		decoded, decErr := utils.Run(exec.Command(
			"sh", "-c", fmt.Sprintf("echo '%s' | base64 -d", strings.TrimSpace(out)),
		))
		g.Expect(decErr).NotTo(HaveOccurred())
		token = strings.TrimSpace(decoded)
		g.Expect(token).NotTo(BeEmpty())
	}, 90*time.Second, 2*time.Second).Should(Succeed(),
		"token secret %s not ready within 90s", secret)
	return token
}

// --- Port-forward ---

// portForward starts a background kubectl port-forward.
// Returns a cleanup function that kills the process.
func portForward(ns, target string, localPort, remotePort int) func() {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "port-forward",
		"-n", ns,
		target,
		fmt.Sprintf("%d:%d", localPort, remotePort),
	)
	logf, _ := os.CreateTemp("", "e2e-pf-*.log")
	if logf != nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	Expect(cmd.Start()).To(Succeed(), "port-forward failed to start")
	// Wait until the port accepts connections.
	Eventually(func() error {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", localPort))
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}, 60*time.Second, time.Second).Should(Succeed(),
		"model-router on :%d did not respond within 60s", localPort)
	return func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if logf != nil {
			_ = logf.Close()
			_ = os.Remove(logf.Name())
		}
	}
}

// --- HTTP helpers ---

type httpResult struct {
	Status int
	Body   string
}

// postChatCompletion sends a minimal chat completion request to the model-router.
func postChatCompletion(localPort int, token string) httpResult {
	GinkgoHelper()
	payload := `{"model":"default","messages":[{"role":"user","content":"e2e"}]}`
	req, err := http.NewRequest("POST",
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", localPort),
		bytes.NewBufferString(payload),
	)
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return httpResult{Status: resp.StatusCode, Body: string(body)}
}

// --- SSE helpers ---

type sseEvent struct {
	EventType string
	Data      string
}

// collectSSE subscribes to a server-sent events endpoint and collects events
// until the stream ends or timeout is reached. Returns all events received.
func collectSSE(url, token string, timeout time.Duration) []sseEvent {
	GinkgoHelper()
	req, err := http.NewRequest("GET", url, nil)
	Expect(err).NotTo(HaveOccurred())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: timeout + 5*time.Second}
	resp, err := client.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer resp.Body.Close()

	var events []sseEvent
	scanner := bufio.NewScanner(resp.Body)
	current := sseEvent{}
	deadline := time.Now().Add(timeout)
	for scanner.Scan() && time.Now().Before(deadline) {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			current.EventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			current.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		case line == "":
			if current.EventType != "" || current.Data != "" {
				events = append(events, current)
			}
			current = sseEvent{}
		}
	}
	return events
}

// countSSEEvents counts events whose JSON "type" field matches eventType.
func countSSEEvents(events []sseEvent, eventType string) int {
	n := 0
	for _, e := range events {
		var m map[string]interface{}
		if json.Unmarshal([]byte(e.Data), &m) == nil {
			if t, _ := m["type"].(string); t == eventType {
				n++
			}
		}
		if e.EventType == eventType {
			n++
		}
	}
	return n
}

// assertStreamingOrder verifies at least one token event arrives before the terminal event.
func assertStreamingOrder(events []sseEvent) {
	GinkgoHelper()
	terminalTypes := map[string]bool{"final_output": true, "done": true, "error": true, "fail": true}
	firstToken, firstTerminal := -1, -1
	for i, e := range events {
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(e.Data), &m)
		t, _ := m["type"].(string)
		if t == "" {
			t = e.EventType
		}
		if t == "token" && firstToken < 0 {
			firstToken = i
		}
		if terminalTypes[t] && firstTerminal < 0 {
			firstTerminal = i
		}
	}
	Expect(firstToken).To(BeNumerically(">=", 0), "no token events received in SSE stream")
	Expect(firstTerminal).To(BeNumerically(">=", 0), "no terminal event received in SSE stream")
	Expect(firstToken).To(BeNumerically("<", firstTerminal),
		"terminal event arrived before first token")
}

// --- configmap helpers ---

// getRouterConfigJSON returns the router-config.json content from the per-run ConfigMap.
func getRouterConfigJSON(runName, ns string) map[string]interface{} {
	GinkgoHelper()
	out, err := utils.Run(exec.Command(
		"kubectl", "get", "configmap",
		"agentorca-run-"+runName,
		"-n", ns,
		"-o", "go-template={{index .data \"router-config.json\"}}",
	))
	Expect(err).NotTo(HaveOccurred())
	var cfg map[string]interface{}
	Expect(json.Unmarshal([]byte(out), &cfg)).To(Succeed())
	return cfg
}

// waitForPodPhase blocks until the named pod in ns reaches the given phase
// (e.g. "Running"), or fails the current Ginkgo spec on timeout.
func waitForPodPhase(podName, ns, phase string, timeout time.Duration) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command(
			"kubectl", "get", "pod", podName,
			"-n", ns,
			"-o", "jsonpath={.status.phase}",
		))
		g.Expect(err).NotTo(HaveOccurred(),
			"pod %s/%s not found while waiting for phase %s", ns, podName, phase)
		g.Expect(out).To(Equal(phase),
			"pod %s/%s in phase %q, expected %s", ns, podName, out, phase)
	}, timeout, 2*time.Second).Should(Succeed(),
		"pod %s/%s did not reach phase %s within %s", ns, podName, phase, timeout)
}

// execInPod runs a command in a container of the named pod and returns
// combined stdout/stderr. Mirrors `kubectl exec -c <container> -- <args...>`.
func execInPod(ns, podName, container string, command ...string) (string, error) {
	GinkgoHelper()
	args := []string{"exec", podName, "-n", ns, "-c", container, "--"}
	args = append(args, command...)
	return utils.Run(exec.Command("kubectl", args...))
}
