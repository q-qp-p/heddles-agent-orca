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
	agentorcav1alpha1 "github.com/heddles/agent-orca/api/v1alpha1"
	"github.com/heddles/agent-orca/internal/router"
)

// filesystemToolCfg resolves Agent.spec.filesystem into the model-router's
// filesystem configuration plus the fs_* built-in tool definitions to inject
// into every chat request. Returns (nil, nil) when filesystem access is not
// configured — the common case, keeping both controller paths unchanged.
//
// The Fallback default here matters: CRDs written before the enum +kubebuilder
// default landed (and objects built in tests without the field) carry an empty
// Mode; the conservative reading of "filesystem enabled" is read-only.
func filesystemToolCfg(fs *agentorcav1alpha1.FilesystemAccessSpec) (*router.FilesystemConfig, []router.ToolDefinition) {
	if fs == nil {
		return nil, nil
	}
	mode := string(fs.Mode)
	if mode == "" {
		mode = string(agentorcav1alpha1.FilesystemReadOnly)
	}
	cfg := &router.FilesystemConfig{Mode: mode}
	return cfg, cfg.ToolDefinitions()
}

// shellToolCfg resolves Agent.spec.shell into the model-router's shell
// configuration plus the sandbox_exec tool definition to inject into every chat
// request. Returns (nil, nil) when the Agent has no shell.
func shellToolCfg(sh *agentorcav1alpha1.ShellAccessSpec) (*router.ShellConfig, []router.ToolDefinition) {
	if sh == nil {
		return nil, nil
	}
	cfg := &router.ShellConfig{MaxTimeoutSeconds: int(sh.MaxTimeoutSeconds)}
	return cfg, cfg.ToolDefinitions()
}
