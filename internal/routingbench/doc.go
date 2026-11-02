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

// Package routingbench turns benchmark evidence into routing findings. It is
// pure: it reads samples and statistics the caller has gathered and returns
// findings, so every number is reproducible from its input. A finding says
// what the evidence supports; a person (or an approved recommendation) decides
// whether to act on it.
//
// Evidence is kept per Agent. A sample is one served call that was replayed on
// another model and judged; routing is judged by whether the dearer ("capable")
// model's answer was better often enough to be worth what it costs.
package routingbench
