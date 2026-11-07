/*
Copyright 2026 lburgazzoli.

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

package discovery_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

// TestPackageBoundary proves the core discovery contract package stays
// pure: non-test sources import only k8s.io/apimachinery
// and the standard library — no controller-runtime, no client-go, no
// k8s.io/api, no Cobra, no project API/controller/CLI packages, so neither
// controllers nor simulator internals can leak into the generic contracts
// — and the package never reads the wall clock: collection time is
// injected by callers at the behavior boundary.
func TestPackageBoundary(t *testing.T) {
	g := NewWithT(t)

	entries, err := os.ReadDir(".")
	g.Expect(err).NotTo(HaveOccurred())

	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(name), nil, 0)
		g.Expect(err).NotTo(HaveOccurred(), "parsing %s", name)
		parsed++

		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			g.Expect(isAllowedImport(path)).To(BeTrue(),
				"%s imports %q: core discovery may import only k8s.io/apimachinery and stdlib", name, path)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "time" && sel.Sel.Name == "Now" {
					t.Fatalf("%s reads the wall clock via time.Now: inject the clock at the behavior boundary", name)
				}
			}
			return true
		})
	}

	// The check must have seen the package sources, not an empty set.
	g.Expect(parsed).To(BeNumerically(">", 0))
}

// isAllowedImport reports whether a core discovery import path is
// k8s.io/apimachinery or the standard library (standard library paths have
// no dot in their first segment).
func isAllowedImport(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".") || strings.HasPrefix(path, "k8s.io/apimachinery/")
}
