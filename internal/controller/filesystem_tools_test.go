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
	"testing"

	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
)

func TestFilesystemToolCfg(t *testing.T) {
	// Unset spec → nothing: the common path, no injected tools.
	cfg, defs := filesystemToolCfg(nil)
	if cfg != nil || defs != nil {
		t.Fatalf("nil filesystem should produce nothing, got (%+v, %d defs)", cfg, len(defs))
	}

	// readWrite exposes all three tools and configures the router policy.
	cfg, defs = filesystemToolCfg(&agentorcav1alpha1.FilesystemAccessSpec{
		Mode:          agentorcav1alpha1.FilesystemReadWrite,
		WriteApproval: true,
	})
	if cfg == nil || cfg.Mode != "readWrite" {
		t.Fatalf("expected readWrite config, got %+v", cfg)
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if !names["fs_read_text_file"] || !names["fs_write_text_file"] || !names["fs_list_dir"] {
		t.Fatalf("readWrite should expose read/write/list fs tools, got %v", names)
	}

	// readOnly exposes the read + list tools but never the write tool.
	cfg, defs = filesystemToolCfg(&agentorcav1alpha1.FilesystemAccessSpec{
		Mode: agentorcav1alpha1.FilesystemReadOnly,
	})
	if cfg == nil || cfg.Mode != "readOnly" {
		t.Fatalf("expected readOnly config, got %+v", cfg)
	}
	for _, d := range defs {
		if d.Name == "fs_write_text_file" {
			t.Fatal("readOnly must not expose fs_write_text_file")
		}
	}

	// Empty mode (pre-enum CRs) resolves conservatively to readOnly.
	cfg, defs = filesystemToolCfg(&agentorcav1alpha1.FilesystemAccessSpec{})
	if cfg == nil || cfg.Mode != "readOnly" || len(defs) != 2 {
		t.Fatalf("empty mode should resolve to readOnly with read+list tools, got (%+v, %d)", cfg, len(defs))
	}
}

func TestShellToolCfg(t *testing.T) {
	if cfg, defs := shellToolCfg(nil); cfg != nil || defs != nil {
		t.Fatalf("an Agent without spec.shell gets no tool, got (%+v, %d defs)", cfg, len(defs))
	}
	cfg, defs := shellToolCfg(&agentorcav1alpha1.ShellAccessSpec{ClassRef: "coder", Image: "x", MaxTimeoutSeconds: 300})
	if cfg == nil || cfg.MaxTimeoutSeconds != 300 {
		t.Fatalf("the cap must reach the router, got %+v", cfg)
	}
	if len(defs) != 1 || defs[0].Name != "sandbox_exec" {
		t.Fatalf("expected the sandbox_exec definition, got %+v", defs)
	}
}
