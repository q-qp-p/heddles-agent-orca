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

package switchyard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sy "github.com/NVIDIA-NeMo/Switchyard/bindings/go"
)

const (
	vendorDir = "../../../third_party/switchyard"
	guestRel  = "bindings/go/internal/wasm/switchyard.wasm"
	lfsMagic  = "version https://git-lfs.github.com/spec/"
	wasmMagic = "\x00asm"
)

// readGuest returns the vendored WebAssembly guest, or fails with the fix when
// the checkout holds an LFS pointer instead of the file.
func readGuest(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vendorDir, guestRel))
	if err != nil {
		t.Fatalf("reading the Switchyard guest: %v", err)
	}
	if bytes.HasPrefix(b, []byte(lfsMagic)) {
		t.Fatalf("%s is a Git LFS pointer, not the WebAssembly guest.\n"+
			"Install git-lfs and fetch it: git lfs install && git lfs pull", guestRel)
	}
	if !bytes.HasPrefix(b, []byte(wasmMagic)) {
		t.Fatalf("%s is not a WebAssembly module (starts %q)", guestRel, b[:min(len(b), 8)])
	}
	return b
}

func upstreamValue(t *testing.T, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vendorDir, "UPSTREAM"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	t.Fatalf("UPSTREAM has no %s", key)
	return ""
}

// The embedded guest is exactly the one that was built and recorded: a swapped or
// corrupted binary fails here, before it can route a request.
func TestGuestMatchesRecordedChecksum(t *testing.T) {
	sum := sha256.Sum256(readGuest(t))
	got := hex.EncodeToString(sum[:])

	if want := upstreamValue(t, "wasm_sha256"); got != want {
		t.Errorf("guest sha256 = %s, UPSTREAM records %s", got, want)
	}
	recorded, err := os.ReadFile(filepath.Join(vendorDir, "SWITCHYARD_WASM.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(string(recorded)); len(fields) != 2 || fields[0] != got || fields[1] != guestRel {
		t.Errorf("SWITCHYARD_WASM.sha256 = %q, want %q for %s", recorded, got, guestRel)
	}
}

func TestVendoredSourceIsPinned(t *testing.T) {
	if sha := upstreamValue(t, "sha"); len(sha) != 40 {
		t.Errorf("UPSTREAM sha = %q, want a full 40 character commit", sha)
	}
	if got := upstreamValue(t, "repo"); got != "NVIDIA-NeMo/Switchyard" {
		t.Errorf("UPSTREAM repo = %q", got)
	}
	for _, f := range []string{"LICENSE", "NOTICE"} {
		if _, err := os.Stat(filepath.Join(vendorDir, f)); err != nil {
			t.Errorf("vendored %s is missing: %v", f, err)
		}
	}
}

const randomConfig = `{
  "algorithm": {"type": "random", "targets": ["fast", "capable"]},
  "targets": {"fast": "fast-model", "capable": "capable-model"}
}`

func routeInput() sy.Input {
	return sy.Input{
		Request: json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"text","text":"Fix this bug"}]}]}`),
		Models:  map[string][]string{"any": {"fast-model", "capable-model"}},
	}
}

// The binding loads the embedded guest and routes with the pure-Go backend.
func TestBindingRoutesThroughTheWasmGuest(t *testing.T) {
	readGuest(t) // a pointer or a corrupt file must fail with its own message first
	if got := sy.Backend(); got != "wasm" {
		t.Fatalf("Backend() = %q, want wasm: the image must build with CGO off", got)
	}
	router, err := sy.New(json.RawMessage(randomConfig))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer router.Close()

	outcome, err := router.Route(context.Background(), routeInput(), nil)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(outcome.SelectedModelIDs) == 0 {
		t.Fatalf("no model selected: %+v", outcome)
	}
	for _, id := range outcome.SelectedModelIDs {
		if id != "fast-model" && id != "capable-model" {
			t.Errorf("selected %q, which is not a configured model", id)
		}
	}
}

func TestBindingRejectsAnUnknownAlgorithm(t *testing.T) {
	readGuest(t)
	router, err := sy.New(json.RawMessage(`{"algorithm":{"type":"no_such_algorithm"},"targets":{}}`))
	if err == nil {
		router.Close()
		t.Fatal("New accepted an unknown algorithm type")
	}
}

func BenchmarkNew(b *testing.B) {
	for b.Loop() {
		router, err := sy.New(json.RawMessage(randomConfig))
		if err != nil {
			b.Fatal(err)
		}
		router.Close()
	}
}

func BenchmarkRoute(b *testing.B) {
	for _, messages := range []int{10, 100, 500} {
		b.Run("messages="+strconv.Itoa(messages), func(b *testing.B) {
			router, err := sy.New(json.RawMessage(randomConfig))
			if err != nil {
				b.Fatal(err)
			}
			defer router.Close()
			in := sy.Input{Request: conversation(messages), Models: routeInput().Models}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := router.Route(context.Background(), in, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// conversation builds an OpenAI-style request of n alternating user/assistant messages.
func conversation(n int) json.RawMessage {
	var sb strings.Builder
	sb.WriteString(`{"messages":[`)
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sb.WriteString(`{"role":"` + role + `","content":[{"type":"text","text":"message number ` + strconv.Itoa(i) + ` with some filler text to give it weight"}]}`)
	}
	sb.WriteString(`]}`)
	return json.RawMessage(sb.String())
}
