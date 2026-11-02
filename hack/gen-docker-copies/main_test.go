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

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testModule = "example.com/m"

// writeModule lays out a small module:
//
//	cmd/main.go            imports internal/a   (the root cmd package; cmd/app is a separate main)
//	cmd/app/main.go        imports internal/a, internal/b
//	internal/a             imports internal/c
//	internal/b, internal/c leaves
//	internal/unused        imported by nothing
//	internal/nest          imports nothing, but nest/inner exists and is imported by nobody
//	internal/embedded      embeds assets/hello.txt
func writeModule(t *testing.T) string {
	t.Helper()
	imp := func(pkgs ...string) string {
		var b strings.Builder
		b.WriteString("import (\n")
		for _, p := range pkgs {
			b.WriteString("_ \"" + testModule + "/" + p + "\"\n")
		}
		b.WriteString(")\n")
		return b.String()
	}
	appImports := imp("internal/a", "internal/b", "internal/nest", "internal/embedded") +
		"import _ \"example.com/ext/pkg\"\n"
	embeddedSource := "package embedded\nimport _ \"embed\"\n//go:embed assets/hello.txt\nvar hello string\n"
	files := map[string]string{
		"go.mod": "module " + testModule + "\n\ngo 1.22\n\nrequire example.com/ext v0.0.0\n\n" +
			"replace example.com/ext => ./third_party/ext\n",
		"third_party/ext/go.mod":             "module example.com/ext\n\ngo 1.22\n",
		"third_party/ext/go.sum":             "",
		"third_party/ext/pkg/p.go":           "package pkg\n",
		"third_party/ext/unused/u.go":        "package unused\n",
		"cmd/main.go":                        "package main\n" + imp("internal/a") + "func main() {}\n",
		"cmd/app/main.go":                    "package main\n" + appImports + "func main() {}\n",
		"internal/a/a.go":                    "package a\n" + imp("internal/c"),
		"internal/b/b.go":                    "package b\n",
		"internal/c/c.go":                    "package c\n",
		"internal/unused/u.go":               "package unused\n",
		"internal/nest/n.go":                 "package nest\n",
		"internal/nest/inner/i.go":           "package inner\n",
		"internal/embedded/e.go":             embeddedSource,
		"internal/embedded/assets/hello.txt": "hi\n",
	}
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	return root
}

func lines(t *testing.T, root, pkg string) []string {
	t.Helper()
	replaces, err := localReplaces(root)
	if err != nil {
		t.Fatal(err)
	}
	modules := make([]string, 1, 1+len(replaces))
	modules[0] = testModule
	for _, r := range replaces {
		modules = append(modules, r.Path)
	}
	all, err := repoPackages(root, modules)
	if err != nil {
		t.Fatal(err)
	}
	got, err := copyLines(root, testModule, all, pkg)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCopyLines_OnlyTheImportedPackages(t *testing.T) {
	root := writeModule(t)
	got := strings.Join(lines(t, root, "./cmd/app"), "\n")
	for _, want := range []string{
		"COPY cmd/app/ cmd/app/",
		"COPY internal/a/ internal/a/",
		"COPY internal/b/ internal/b/",
		"COPY internal/c/ internal/c/", // a transitive import
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "unused") {
		t.Errorf("copied a package nothing imports:\n%s", got)
	}
}

func TestCopyLines_RootPackageDoesNotDragInSiblingCommands(t *testing.T) {
	root := writeModule(t)
	got := lines(t, root, "./cmd")
	want := []string{"COPY cmd/*.go cmd/", "COPY internal/a/ internal/a/", "COPY internal/c/ internal/c/"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %v, want %v: copying cmd/ whole would pull in cmd/app", got, want)
	}
}

func TestCopyLines_NestedUnusedPackageMeansFileLevelCopy(t *testing.T) {
	root := writeModule(t)
	got := strings.Join(lines(t, root, "./cmd/app"), "\n")
	if !strings.Contains(got, "COPY internal/nest/*.go internal/nest/") || strings.Contains(got, "COPY internal/nest/ ") {
		t.Errorf("internal/nest has an unused nested package and must be copied file by file:\n%s", got)
	}
}

func TestCopyLines_EmbeddedFilesAreCopied(t *testing.T) {
	root := writeModule(t)
	got := strings.Join(lines(t, root, "./cmd/app"), "\n")
	if !strings.Contains(got, "COPY internal/embedded/ internal/embedded/") {
		t.Errorf("an embedding package without nested packages is copied whole:\n%s", got)
	}
}

func TestCopyLines_IsDeterministicAndSorted(t *testing.T) {
	root := writeModule(t)
	first := strings.Join(lines(t, root, "./cmd/app"), "\n")
	for range 3 {
		if again := strings.Join(lines(t, root, "./cmd/app"), "\n"); again != first {
			t.Fatalf("output changed between runs:\n%s\n---\n%s", first, again)
		}
	}
}

func TestReplacedModulePackagesJoinTheClosure(t *testing.T) {
	root := writeModule(t)
	got := strings.Join(lines(t, root, "./cmd/app"), "\n")
	if !strings.Contains(got, "COPY third_party/ext/pkg/ third_party/ext/pkg/") {
		t.Errorf("an imported package of a replaced module is not copied:\n%s", got)
	}
	if strings.Contains(got, "third_party/ext/unused") {
		t.Errorf("an unimported package of the replaced module is copied:\n%s", got)
	}
	// An image that never imports the module copies none of it.
	if other := strings.Join(lines(t, root, "./cmd"), "\n"); strings.Contains(other, "third_party") {
		t.Errorf("an image that does not import the replaced module copies it:\n%s", other)
	}
}

func TestReplaceStubLines(t *testing.T) {
	root := writeModule(t)
	replaces, err := localReplaces(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaces) != 1 || replaces[0].Path != "example.com/ext" || replaces[0].Dir != "third_party/ext" {
		t.Fatalf("replaces = %+v", replaces)
	}
	got := replaceStubLines(root, replaces)
	want := []string{
		"COPY third_party/ext/go.mod third_party/ext/go.mod",
		"COPY third_party/ext/go.sum third_party/ext/go.sum",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("stubs = %v, want %v", got, want)
	}
	// Without a go.sum only the go.mod is copied: COPY of a missing file fails the build.
	if err := os.Remove(filepath.Join(root, "third_party/ext/go.sum")); err != nil {
		t.Fatal(err)
	}
	if got := replaceStubLines(root, replaces); len(got) != 1 {
		t.Errorf("stubs without a go.sum = %v, want only the go.mod", got)
	}
}

func TestReplaceBlock(t *testing.T) {
	begin, end := beginMarker(blockDeps), endMarker(blockDeps)
	df := "FROM x\n" + begin + " (note)\nCOPY old/ old/\n" + end + "\nRUN build\n"
	got, err := replaceBlock(df, blockDeps, []string{"COPY a/ a/", "COPY b/ b/"})
	if err != nil {
		t.Fatal(err)
	}
	want := "FROM x\n" + begin + " (note)\nCOPY a/ a/\nCOPY b/ b/\n" + end + "\nRUN build\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	again, _ := replaceBlock(got, blockDeps, []string{"COPY a/ a/", "COPY b/ b/"})
	if again != got {
		t.Errorf("replacing with the same lines changed the file")
	}
}

func TestReplaceBlock_OnlyTouchesTheNamedBlock(t *testing.T) {
	df := beginMarker(blockReplaces) + "\nCOPY keep/ keep/\n" + endMarker(blockReplaces) + "\n" +
		beginMarker(blockDeps) + "\nCOPY old/ old/\n" + endMarker(blockDeps) + "\n"
	got, err := replaceBlock(df, blockDeps, []string{"COPY new/ new/"})
	if err != nil {
		t.Fatal(err)
	}
	kept, replaced := strings.Contains(got, "COPY keep/ keep/"), strings.Contains(got, "COPY new/ new/")
	if !kept || !replaced || strings.Contains(got, "old/") {
		t.Errorf("wrong block rewritten:\n%s", got)
	}
}

func TestReplaceBlock_RequiresMarkers(t *testing.T) {
	b, e := beginMarker(blockDeps), endMarker(blockDeps)
	for _, df := range []string{"FROM x\nCOPY a/ a/\n", b + "\n", e + "\n" + b + "\n"} {
		if _, err := replaceBlock(df, blockDeps, nil); err == nil {
			t.Errorf("accepted a Dockerfile with missing or misordered markers: %q", df)
		}
	}
}

func TestRun_RewritesAndChecks(t *testing.T) {
	root := writeModule(t)
	df := filepath.Join(root, "cmd", "app", "Dockerfile")
	staleFile := "FROM x\n" + beginMarker(blockReplaces) + "\nCOPY stale-stub/ x/\n" + endMarker(blockReplaces) + "\n" +
		beginMarker(blockDeps) + "\nCOPY stale/ stale/\n" + endMarker(blockDeps) + "\n"
	if err := os.WriteFile(df, []byte(staleFile), 0o644); err != nil {
		t.Fatal(err)
	}
	tg := []target{{"cmd/app/Dockerfile", "./cmd/app"}}

	stale, err := run(root, tg, true)
	if err != nil || len(stale) != 1 {
		t.Fatalf("check on a stale file = %v, %v; want it reported", stale, err)
	}
	if b, _ := os.ReadFile(df); !strings.Contains(string(b), "stale/") {
		t.Fatalf("check mode modified the file")
	}

	if _, err := run(root, tg, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(df)
	if strings.Contains(string(b), "stale") || !strings.Contains(string(b), "COPY internal/c/ internal/c/") {
		t.Errorf("not rewritten:\n%s", b)
	}
	if !strings.Contains(string(b), "COPY third_party/ext/go.mod third_party/ext/go.mod") {
		t.Errorf("the replaced module's go.mod is not in the stub block:\n%s", b)
	}
	if stale, err := run(root, tg, true); err != nil || len(stale) != 0 {
		t.Errorf("check after rewrite = %v, %v; want clean", stale, err)
	}
}

// The checked-in Dockerfiles must match what the generator produces: this is the
// same check CI runs, so a stale block fails make test too.
func TestRepositoryDockerfilesAreUpToDate(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := run(root, targets, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) > 0 {
		t.Errorf("stale generated COPY block in %v: run `make docker-deps`", stale)
	}
}
