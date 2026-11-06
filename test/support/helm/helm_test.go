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

package helm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	. "github.com/onsi/gomega"
)

// recordedInvocation captures one subprocess execution.
type recordedInvocation struct {
	argv []string
	env  []string
}

// fakeProcess records invocations and returns a canned error. When a values
// file is present in argv it is read immediately so the test can assert its
// content even though Install removes the file afterwards.
type fakeProcess struct {
	mu          sync.Mutex
	invocations []recordedInvocation
	values      []map[string]any
	err         error
}

func (f *fakeProcess) run(_ context.Context, argv []string, env []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.invocations = append(f.invocations, recordedInvocation{argv: argv, env: env})

	for i, arg := range argv {
		if arg == "--values" && i+1 < len(argv) {
			data, err := os.ReadFile(argv[i+1])
			if err == nil {
				v := map[string]any{}
				if err := yaml.Unmarshal(data, &v); err == nil {
					f.values = append(f.values, v)
				}
			}
		}
	}
	return f.err
}

func (f *fakeProcess) last() recordedInvocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invocations[len(f.invocations)-1]
}

func newTestRunner(g *GomegaWithT, process *fakeProcess) *Runner {
	r, err := NewRunner("/run/private/kubeconfig",
		optionFunc(func(runner *Runner) {
			runner.process = process.run
		}),
	)
	g.Expect(err).NotTo(HaveOccurred())
	return r
}

func TestNewRunnerRequiresKubeconfig(t *testing.T) {
	g := NewWithT(t)

	_, err := NewRunner("")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("run-private kubeconfig"))
}

func TestInstallUsesPinnedCommandAndPrivateKubeconfig(t *testing.T) {
	g := NewWithT(t)
	process := &fakeProcess{}
	r := newTestRunner(g, process)

	err := r.Install(context.Background(), InstallOptions{
		Release:   "fake-gpu-operator",
		Chart:     "oci://example.invalid/chart@sha256:abc",
		Namespace: "gpu-operator",
		Values: map[string]any{
			"devicePlugin": map[string]any{"enabled": true},
		},
	})
	g.Expect(err).NotTo(HaveOccurred())

	invocation := process.last()

	// Pinned go-run Helm CLI, never an unpinned local binary.
	g.Expect(invocation.argv[:3]).To(Equal(DefaultCommand))

	joined := strings.Join(invocation.argv, " ")
	g.Expect(joined).To(ContainSubstring("upgrade --install fake-gpu-operator oci://example.invalid/chart@sha256:abc"))
	g.Expect(joined).To(ContainSubstring("--namespace gpu-operator"))
	g.Expect(joined).To(ContainSubstring("--create-namespace"))
	g.Expect(joined).To(ContainSubstring("--wait"))
	g.Expect(joined).To(ContainSubstring("--timeout"))

	// The run-private kubeconfig reaches the subprocess through both the
	// flag and the environment; the ambient KUBECONFIG never wins.
	g.Expect(joined).To(ContainSubstring("--kubeconfig /run/private/kubeconfig"))
	g.Expect(invocation.env).To(ContainElement("KUBECONFIG=/run/private/kubeconfig"))

	// Values are marshaled to a file with the exact requested content.
	g.Expect(process.values).To(HaveLen(1))
	g.Expect(process.values[0]).To(HaveKeyWithValue("devicePlugin", map[string]any{"enabled": true}))
}

func TestInstallWithoutWaitOmitsWaitFlag(t *testing.T) {
	g := NewWithT(t)
	process := &fakeProcess{}
	r := newTestRunner(g, process)

	g.Expect(r.Install(context.Background(), InstallOptions{
		Release:   "rel",
		Chart:     "oci://example.invalid/chart",
		Namespace: "ns",
		SkipWait:  true,
	})).To(Succeed())

	g.Expect(strings.Join(process.last().argv, " ")).NotTo(ContainSubstring("--wait"))
}

func TestInstallValidation(t *testing.T) {
	g := NewWithT(t)
	r := newTestRunner(g, &fakeProcess{})

	table := []struct {
		name string
		opts InstallOptions
		want string
	}{
		{name: "missing release", opts: InstallOptions{Chart: "c", Namespace: "n"}, want: "release name"},
		{name: "missing chart", opts: InstallOptions{Release: "r", Namespace: "n"}, want: "chart reference"},
		{name: "missing namespace", opts: InstallOptions{Release: "r", Chart: "c"}, want: "namespace"},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			err := r.Install(context.Background(), entry.opts)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(entry.want))
		})
	}
}

func TestSubprocessErrorPropagatesWithOutput(t *testing.T) {
	g := NewWithT(t)
	process := &fakeProcess{err: errors.New("exit status 1: chart not found")}
	r := newTestRunner(g, process)

	err := r.ChartAvailable(context.Background(), "oci://example.invalid/chart@sha256:abc")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("chart not found"))
	g.Expect(err.Error()).To(ContainSubstring("show chart"))
}

func TestUninstallUsesPrivateKubeconfig(t *testing.T) {
	g := NewWithT(t)
	process := &fakeProcess{}
	r := newTestRunner(g, process)

	g.Expect(r.Uninstall(context.Background(), "rel", "ns")).To(Succeed())

	invocation := process.last()
	g.Expect(strings.Join(invocation.argv, " ")).To(ContainSubstring("uninstall rel --namespace ns"))
	g.Expect(strings.Join(invocation.argv, " ")).To(ContainSubstring("--kubeconfig /run/private/kubeconfig"))
	g.Expect(invocation.env).To(ContainElement("KUBECONFIG=/run/private/kubeconfig"))
}

func TestWithCommandOverride(t *testing.T) {
	g := NewWithT(t)
	process := &fakeProcess{}
	r, err := NewRunner("/run/private/kubeconfig",
		WithCommand("helm"),
		optionFunc(func(runner *Runner) {
			runner.process = process.run
		}),
	)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(r.Install(context.Background(), InstallOptions{
		Release: "rel", Chart: "c", Namespace: "n",
	})).To(Succeed())
	g.Expect(process.last().argv[0]).To(Equal("helm"))
}

func TestControllerImageValues(t *testing.T) {
	g := NewWithT(t)

	const testTag = "dev"

	values, err := ControllerImageValues("example.invalid/hardware-discovery-controller", testTag)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(values).To(Equal(map[string]any{
		"controllerImage": map[string]any{
			"repository": "example.invalid/hardware-discovery-controller",
			"tag":        testTag,
		},
	}))

	table := []struct {
		name       string
		repository string
		tag        string
		want       string
	}{
		{name: "missing repository", repository: "", tag: testTag, want: "repository must not be empty"},
		{name: "missing tag", repository: "example.invalid/controller", tag: "", want: "tag must not be empty"},
		{
			name:       "repository with tag separator is rejected, never split",
			repository: "example.invalid/controller:latest",
			tag:        "dev",
			want:       "must not contain a tag separator",
		},
	}
	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := ControllerImageValues(entry.repository, entry.tag)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(entry.want))
		})
	}
}

func TestLocalChartRef(t *testing.T) {
	t.Run("valid chart directory", func(t *testing.T) {
		g := NewWithT(t)
		dir := t.TempDir()
		g.Expect(os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\n"), 0o644)).To(Succeed())

		ref, err := LocalChartRef(dir)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ref).To(Equal(dir))
	})

	t.Run("missing chart directory fails deterministically", func(t *testing.T) {
		g := NewWithT(t)
		ref, err := LocalChartRef(filepath.Join(t.TempDir(), "dist", "chart"))
		g.Expect(err).To(HaveOccurred())
		g.Expect(ref).To(BeEmpty())
		g.Expect(err.Error()).To(ContainSubstring("make helm-chart"),
			"a missing service chart is a prerequisite failure, never a skip")
	})

	t.Run("empty directory is rejected", func(t *testing.T) {
		g := NewWithT(t)
		_, err := LocalChartRef("")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("chart directory must not be empty"))
	})
}
