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

// Package switchyard adapts NVIDIA NeMo Switchyard's routing algorithms for the
// model-router.
//
// Switchyard's Go binding is vendored in third_party/switchyard, pinned to one
// upstream commit (third_party/switchyard/UPSTREAM). It runs Switchyard's Rust
// core as a WebAssembly guest inside the process through wazero, so it needs no
// cgo. The guest is stored in Git LFS and embedded as is; see "Bumping
// Switchyard" in docs/development.md for how it is rebuilt and checked.
//
// For now this package only proves the vendored binding builds, loads and routes
// (see embed_test.go). The adapter itself arrives in a later change.
package switchyard
