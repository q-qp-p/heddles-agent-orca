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

// Command gen-docker-copies keeps each Go Dockerfile's source COPY lines equal to
// the packages its binary actually imports, and makes the modules go.mod replaces
// with local directories resolvable before "go mod download".
//
// A Dockerfile that copies all of internal/ rebuilds (and, under skaffold, is
// rebuilt) whenever any package in it changes, however unrelated. This command
// asks the Go toolchain which repository packages each binary depends on and
// rewrites the block between the "BEGIN generated go-deps" and "END generated
// go-deps" markers of each Dockerfile, so an image changes only when its own
// code does.
//
// A local "replace" (the vendored Switchyard binding) must have its go.mod in the
// image before "go mod download" runs, in every image, even those that never
// import it. A second block, "go-mod-replaces", carries just those go.mod and
// go.sum files; the package sources of a replaced module are copied by go-deps
// only into images that import them.
//
//	go run ./hack/gen-docker-copies          rewrite the blocks
//	go run ./hack/gen-docker-copies -check   fail if any block is stale
//
// Run it from the repository root (make docker-deps / make verify-docker-deps).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Names of the generated blocks a Dockerfile can carry.
const (
	blockDeps     = "go-deps"
	blockReplaces = "go-mod-replaces"
)

func beginMarker(block string) string { return "# BEGIN generated " + block }
func endMarker(block string) string   { return "# END generated " + block }

// target ties a Dockerfile to the main package it builds.
type target struct {
	// Dockerfile is the path from the repository root.
	Dockerfile string
	// Package is the main package, as a go list pattern.
	Package string
}

// targets lists every Dockerfile that builds a Go binary from this module.
var targets = []target{
	{"Dockerfile", "./cmd"},
	{"cmd/model-router/Dockerfile", "./cmd/model-router"},
	{"cmd/oauth-proxy/Dockerfile", "./cmd/oauth-proxy"},
	{"Dockerfile.mcp-ingester", "./cmd/mcp-ingester"},
	{"Dockerfile.ui-proxy", "./cmd/ui-proxy"},
	{"Dockerfile.dev-sandbox", "./cmd/coder-agent"},
	{"Dockerfile.go.openai-reference", "./pkg/go/openai-reference"},
}

func main() {
	check := flag.Bool("check", false, "fail instead of writing when a Dockerfile is stale")
	flag.Parse()
	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	stale, err := run(root, targets, *check)
	if err != nil {
		fatal(err)
	}
	if *check && len(stale) > 0 {
		fmt.Fprintf(os.Stderr, "stale generated COPY block in: %s\nrun `make docker-deps` and commit the result\n",
			strings.Join(stale, ", "))
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gen-docker-copies:", err)
	os.Exit(1)
}

// run regenerates the block of every target under root. With check it writes
// nothing. It returns the Dockerfiles whose block differed from the generated one.
func run(root string, targets []target, check bool) ([]string, error) {
	module, err := goList(root, "-m", "-f", "{{.Path}}")
	if err != nil {
		return nil, fmt.Errorf("reading the module path: %w", err)
	}
	module = strings.TrimSpace(module)
	replaces, err := localReplaces(root)
	if err != nil {
		return nil, err
	}
	modules := []string{module}
	for _, r := range replaces {
		modules = append(modules, r.Path)
	}
	all, err := repoPackages(root, modules)
	if err != nil {
		return nil, err
	}
	stubs := replaceStubLines(root, replaces)
	var stale []string
	for _, t := range targets {
		lines, err := copyLines(root, module, all, t.Package)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Dockerfile, err)
		}
		file := filepath.Join(root, t.Dockerfile)
		old, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		updated, err := replaceBlock(string(old), blockDeps, lines)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Dockerfile, err)
		}
		updated, err = replaceBlock(updated, blockReplaces, stubs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.Dockerfile, err)
		}
		if updated == string(old) {
			continue
		}
		stale = append(stale, t.Dockerfile)
		if !check {
			if err := os.WriteFile(file, []byte(updated), 0o644); err != nil { //nolint:gosec // a source file
				return nil, err
			}
		}
	}
	return stale, nil
}

// pkgInfo is what go list reports about one repository package.
type pkgInfo struct {
	// Dir is the package directory relative to the repository root.
	Dir string
	// Embed lists files the package embeds, relative to Dir.
	Embed []string
}

// repoPackages lists every package of the given modules: this one and any that
// go.mod replaces with a local directory (broken ones included: the ui-proxy
// embeds a directory that only exists after the UI build).
func repoPackages(root string, modules []string) (map[string]pkgInfo, error) {
	// go list reports resolved paths (on macOS /var is a symlink to /private/var),
	// so relative paths are only right against a resolved root.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	patterns := []string{"./..."}
	for _, m := range modules[1:] {
		patterns = append(patterns, m+"/...")
	}
	args := append([]string{"-e", "-f", `{{.ImportPath}}|{{.Dir}}|{{join .EmbedFiles ","}}`}, patterns...)
	out, err := goList(root, args...)
	if err != nil {
		return nil, fmt.Errorf("listing packages: %w", err)
	}
	pkgs := map[string]pkgInfo{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || !inModules(parts[0], modules) {
			continue
		}
		rel, err := filepath.Rel(root, parts[1])
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, fmt.Errorf("package %s is at %s, outside the repository root %s", parts[0], parts[1], root)
		}
		info := pkgInfo{Dir: filepath.ToSlash(rel)}
		if parts[2] != "" {
			info.Embed = strings.Split(parts[2], ",")
		}
		pkgs[parts[0]] = info
	}
	return pkgs, nil
}

func inModules(importPath string, modules []string) bool {
	for _, m := range modules {
		if importPath == m || strings.HasPrefix(importPath, m+"/") {
			return true
		}
	}
	return false
}

// localReplace is a go.mod replace directive pointing at a directory of this
// repository.
type localReplace struct {
	// Path is the replaced module's path.
	Path string
	// Dir is where it lives, relative to the repository root.
	Dir string
}

// localReplaces reads the replace directives of go.mod that name a local directory.
func localReplaces(root string) ([]localReplace, error) {
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go mod edit -json: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var mod struct {
		Replace []struct {
			Old struct{ Path string }
			New struct{ Path string }
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &mod); err != nil {
		return nil, err
	}
	var out []localReplace
	for _, r := range mod.Replace {
		if strings.HasPrefix(r.New.Path, "./") {
			out = append(out, localReplace{Path: r.Old.Path, Dir: path.Clean(r.New.Path)})
		}
	}
	slices.SortFunc(out, func(a, b localReplace) int { return strings.Compare(a.Dir, b.Dir) })
	return out, nil
}

// replaceStubLines returns the COPY lines that put each replaced module's go.mod
// (and go.sum, when it has one) in an image ahead of "go mod download".
func replaceStubLines(root string, replaces []localReplace) []string {
	var lines []string
	for _, r := range replaces {
		lines = append(lines, fmt.Sprintf("COPY %s/go.mod %s/go.mod", r.Dir, r.Dir))
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(r.Dir), "go.sum")); err == nil {
			lines = append(lines, fmt.Sprintf("COPY %s/go.sum %s/go.sum", r.Dir, r.Dir))
		}
	}
	return lines
}

// copyLines returns the COPY lines that give pkg's build exactly the repository
// packages it imports. Linux is assumed: that is what the images are built for.
func copyLines(root, module string, all map[string]pkgInfo, pkg string) ([]string, error) {
	out, err := goList(root, "-e", "-deps", "-f", "{{.ImportPath}}", pkg)
	if err != nil {
		return nil, fmt.Errorf("listing the dependencies of %s: %w", pkg, err)
	}
	inClosure := map[string]bool{}
	for ip := range strings.FieldsSeq(out) {
		if _, ok := all[ip]; ok {
			inClosure[all[ip].Dir] = true
		}
	}
	if len(inClosure) == 0 {
		return nil, fmt.Errorf("%s depends on no package of %s", pkg, module)
	}

	dirs := make([]string, 0, len(inClosure))
	for d := range inClosure {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)

	// A directory can be copied whole only if no package nested in it falls
	// outside the closure; copying cmd/ would otherwise drag in every other cmd.
	whole := func(dir string) bool {
		for _, p := range all {
			if p.Dir != dir && strings.HasPrefix(p.Dir, dir+"/") && !inClosure[p.Dir] {
				return false
			}
		}
		return true
	}
	var wholeDirs []string
	for _, d := range dirs {
		if whole(d) {
			wholeDirs = append(wholeDirs, d)
		}
	}
	covered := func(dir string) bool {
		for _, w := range wholeDirs {
			if w != dir && strings.HasPrefix(dir, w+"/") {
				return true
			}
		}
		return false
	}

	var lines []string
	for _, d := range dirs {
		if covered(d) {
			continue
		}
		if whole(d) {
			lines = append(lines, fmt.Sprintf("COPY %s/ %s/", d, d))
			continue
		}
		lines = append(lines, fmt.Sprintf("COPY %s/*.go %s/", d, d))
		for _, p := range all {
			if p.Dir == d {
				for _, e := range p.Embed {
					lines = append(lines, fmt.Sprintf("COPY %s %s", path.Join(d, e), path.Join(d, path.Dir(e))+"/"))
				}
			}
		}
	}
	return lines, nil
}

// replaceBlock swaps what lies between the markers of the named block of
// dockerfile for lines.
func replaceBlock(dockerfile, block string, lines []string) (string, error) {
	begin := strings.Index(dockerfile, beginMarker(block))
	end := strings.Index(dockerfile, endMarker(block))
	if begin < 0 || end < 0 || end < begin {
		return "", errors.New("no \"" + beginMarker(block) + "\" / \"" + endMarker(block) +
			"\" markers; add them where the COPY lines go")
	}
	beginEnd := begin + strings.Index(dockerfile[begin:], "\n") + 1
	var b strings.Builder
	b.WriteString(dockerfile[:beginEnd])
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString(dockerfile[end:])
	return b.String(), nil
}

// goList runs `go list` in dir for linux, without cgo, as the images build.
func goList(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go list %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
