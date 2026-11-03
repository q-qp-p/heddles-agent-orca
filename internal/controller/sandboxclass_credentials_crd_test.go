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

package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	agentwebhook "github.com/heddles/agent-orca/internal/webhook"
)

// The CRD schema, with its CEL rules, rejects malformed class credentials before
// the admission webhook (which checks egress reachability) ever sees them.
var _ = Describe("SandboxClass credentials schema", func() {
	secret := agentorcav1alpha1.SandboxCredentialSource{
		SecretRef: &agentorcav1alpha1.SandboxCredentialSecretRef{Name: "s", Key: "k"},
	}
	bind := agentorcav1alpha1.SandboxCredentialBind{Host: "db.example.com"}

	create := func(name string, creds ...agentorcav1alpha1.SandboxCredential) error {
		class := &agentorcav1alpha1.SandboxClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       agentorcav1alpha1.SandboxClassSpec{Credentials: creds},
		}
		err := k8sClient.Create(ctx, class)
		if err == nil {
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, class) })
		}
		return err
	}

	It("accepts a secretRef credential and defaults the bind port to 443", func() {
		Expect(create("cred-ok", agentorcav1alpha1.SandboxCredential{Name: "db", Source: secret, Bind: bind})).To(Succeed())
		var got agentorcav1alpha1.SandboxClass
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "cred-ok"}, &got)).To(Succeed())
		Expect(got.Spec.Credentials[0].Bind.Port).To(Equal(int32(443)))
	})

	It("rejects a source with neither or both of githubApp and secretRef", func() {
		both := agentorcav1alpha1.SandboxCredentialSource{
			SecretRef: secret.SecretRef,
			GitHubApp: &agentorcav1alpha1.SandboxCredentialGitHubApp{Repositories: []string{"heddles/agent-orca"}},
		}
		Expect(create("cred-none", agentorcav1alpha1.SandboxCredential{Name: "a", Bind: bind})).To(MatchError(ContainSubstring("exactly one of githubApp or secretRef")))
		Expect(create("cred-both", agentorcav1alpha1.SandboxCredential{Name: "a", Source: both, Bind: bind})).To(MatchError(ContainSubstring("exactly one of githubApp or secretRef")))
	})

	It("rejects duplicate credential names", func() {
		c := agentorcav1alpha1.SandboxCredential{Name: "dup", Source: secret, Bind: bind}
		Expect(create("cred-dup", c, c)).To(HaveOccurred())
	})

	It("rejects an invalid name, host and port", func() {
		Expect(create("cred-name", agentorcav1alpha1.SandboxCredential{Name: "Bad_Name", Source: secret, Bind: bind})).To(HaveOccurred())
		Expect(create("cred-host", agentorcav1alpha1.SandboxCredential{Name: "a", Source: secret,
			Bind: agentorcav1alpha1.SandboxCredentialBind{Host: "*.example.com"}})).To(HaveOccurred())
		Expect(create("cred-port", agentorcav1alpha1.SandboxCredential{Name: "a", Source: secret,
			Bind: agentorcav1alpha1.SandboxCredentialBind{Host: "db.example.com", Port: 70000}})).To(HaveOccurred())
	})

	It("checks the inject style's fields", func() {
		withInject := func(i agentorcav1alpha1.SandboxCredentialInject) agentorcav1alpha1.SandboxCredential {
			return agentorcav1alpha1.SandboxCredential{Name: "a", Source: secret, Bind: bind, Inject: &i}
		}
		Expect(create("inj-header-ok", withInject(agentorcav1alpha1.SandboxCredentialInject{
			Style: agentorcav1alpha1.SandboxCredentialInjectHeader, Name: "X-Api-Key"}))).To(Succeed())
		Expect(create("inj-header-noname", withInject(agentorcav1alpha1.SandboxCredentialInject{
			Style: agentorcav1alpha1.SandboxCredentialInjectHeader}))).To(MatchError(ContainSubstring("needs a header name")))
		Expect(create("inj-bearer-name", withInject(agentorcav1alpha1.SandboxCredentialInject{
			Style: agentorcav1alpha1.SandboxCredentialInjectBearer, Name: "X"}))).To(MatchError(ContainSubstring("style header only")))
		Expect(create("inj-basic-ok", withInject(agentorcav1alpha1.SandboxCredentialInject{
			Style: agentorcav1alpha1.SandboxCredentialInjectBasic}))).To(Succeed())
	})

	It("rejects githubApp permissions other than read or write", func() {
		gh := agentorcav1alpha1.SandboxCredentialSource{GitHubApp: &agentorcav1alpha1.SandboxCredentialGitHubApp{
			Repositories: []string{"heddles/agent-orca"}, Permissions: map[string]agentorcav1alpha1.SandboxCredentialPermission{"contents": "admin"}}}
		Expect(create("cred-perm", agentorcav1alpha1.SandboxCredential{Name: "gh", Source: gh,
			Bind: agentorcav1alpha1.SandboxCredentialBind{Host: "github.com"}})).To(MatchError(ContainSubstring("Unsupported value")))
	})

	// The shipped samples must stay valid: the schema accepts them and admission would too.
	It("accepts the SandboxClasses in config/samples", func() {
		files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "sandboxclass-*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		more, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "software-factory", "00-infrastructure.yaml"))
		Expect(err).NotTo(HaveOccurred())
		files = append(files, more...)
		Expect(files).NotTo(BeEmpty())

		classes := 0
		for _, f := range files {
			raw, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
			for {
				var obj map[string]any
				if err := dec.Decode(&obj); err == io.EOF {
					break
				} else {
					Expect(err).NotTo(HaveOccurred(), f)
				}
				if obj["kind"] != "SandboxClass" {
					continue
				}
				class := &agentorcav1alpha1.SandboxClass{}
				doc, err := json.Marshal(obj)
				Expect(err).NotTo(HaveOccurred())
				Expect(json.Unmarshal(doc, class)).To(Succeed(), f)

				warnings, err := agentwebhook.ValidateClassCredentials(&class.Spec)
				Expect(err).NotTo(HaveOccurred(), "%s: %s", f, class.Name)
				Expect(warnings).To(BeEmpty(), "%s: %s", f, class.Name)

				// Create it under a test-only name so it cannot clash with other specs.
				class.Name = "sample-" + class.Name
				class.ResourceVersion = ""
				Expect(k8sClient.Create(ctx, class)).To(Succeed(), "%s: %s", f, class.Name)
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, class) })
				classes++
			}
		}
		Expect(classes).To(BeNumerically(">=", 4)) // coder, general, factory-writer, factory-reviewer
	})
})
