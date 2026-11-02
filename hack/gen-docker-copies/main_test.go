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
	appImports := imp("internal/a", "internal/b", "internal/nest", "internal/embedded")
	embeddedSource := "package embedded\nimport _ \"embed\"\n//go:embed assets/hello.txt\nvar hello string\n"
	files := map[string]string{
		"go.mod":                             "module " + testModule + "\n\ngo 1.22\n",
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
	all, err := repoPackages(root, testModule)
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

func TestReplaceBlock(t *testing.T) {
	df := "FROM x\n" + beginMarker + " (note)\nCOPY old/ old/\n" + endMarker + "\nRUN build\n"
	got, err := replaceBlock(df, []string{"COPY a/ a/", "COPY b/ b/"})
	if err != nil {
		t.Fatal(err)
	}
	want := "FROM x\n" + beginMarker + " (note)\nCOPY a/ a/\nCOPY b/ b/\n" + endMarker + "\nRUN build\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	again, _ := replaceBlock(got, []string{"COPY a/ a/", "COPY b/ b/"})
	if again != got {
		t.Errorf("replacing with the same lines changed the file")
	}
}

func TestReplaceBlock_RequiresMarkers(t *testing.T) {
	for _, df := range []string{"FROM x\nCOPY a/ a/\n", beginMarker + "\n", endMarker + "\n" + beginMarker + "\n"} {
		if _, err := replaceBlock(df, nil); err == nil {
			t.Errorf("accepted a Dockerfile with missing or misordered markers: %q", df)
		}
	}
}

func TestRun_RewritesAndChecks(t *testing.T) {
	root := writeModule(t)
	df := filepath.Join(root, "cmd", "app", "Dockerfile")
	if err := os.WriteFile(df, []byte("FROM x\n"+beginMarker+"\nCOPY stale/ stale/\n"+endMarker+"\n"), 0o644); err != nil {
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
	if strings.Contains(string(b), "stale/") || !strings.Contains(string(b), "COPY internal/c/ internal/c/") {
		t.Errorf("not rewritten:\n%s", b)
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
