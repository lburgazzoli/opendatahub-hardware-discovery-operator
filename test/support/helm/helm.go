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

// Package helm runs the pinned Helm CLI as bounded subprocesses against a
// run-private kubeconfig. The simulator (Run:ai fake-gpu-operator) is
// installed and uninstalled exclusively through this package, so every
// Helm invocation is visible, bounded and isolated from the ambient
// kubeconfig/current-context.
package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// DefaultCommand is the approved Helm CLI pin:
// the CLI is executed through `go run` at the exact published version,
// never through an unpinned local binary.
var DefaultCommand = []string{"go", "run", "helm.sh/helm/v4/cmd/helm@v4.3.1"}

// kubeconfigEnvKey is set for every subprocess to the run-private path so
// the ambient KUBECONFIG can never leak in.
const kubeconfigEnvKey = "KUBECONFIG"

// Runner executes bounded Helm CLI subprocesses against one run-private
// kubeconfig.
type Runner struct {
	argv       []string
	kubeconfig string
	process    processFunc
}

// processFunc executes one bounded subprocess. It is a field so unit tests
// can record invocations without running Helm.
type processFunc func(ctx context.Context, argv []string, env []string) error

// Option configures a Runner.
type Option interface {
	apply(*Runner)
}

type optionFunc func(*Runner)

func (fn optionFunc) apply(r *Runner) {
	if fn != nil && r != nil {
		fn(r)
	}
}

// WithCommand overrides the Helm command prefix (e.g. a locally verified
// helm binary). The default is the pinned go-run invocation.
func WithCommand(argv ...string) Option {
	return optionFunc(func(r *Runner) {
		if len(argv) > 0 {
			r.argv = argv
		}
	})
}

// NewRunner returns a Runner bound to the given run-private kubeconfig
// path. The path is passed to every subprocess both as the KUBECONFIG env
// variable and as an explicit --kubeconfig flag.
func NewRunner(kubeconfigPath string, opts ...Option) (*Runner, error) {
	if kubeconfigPath == "" {
		return nil, errors.New("helm runner requires a run-private kubeconfig path")
	}

	r := &Runner{
		argv:       DefaultCommand,
		kubeconfig: kubeconfigPath,
		process:    runProcess,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.apply(r)
		}
	}
	return r, nil
}

// InstallOptions describe one release installation.
type InstallOptions struct {
	// Release is the Helm release name.
	Release string

	// Chart is the chart reference. Simulator profiles use a
	// digest-pinned OCI reference so the exact reviewed artifact is
	// installed.
	Chart string

	// Namespace is the install namespace (created when missing).
	Namespace string

	// Values are marshaled to a temporary values file.
	Values map[string]any

	// SkipWait disables Helm's readiness wait for the release resources.
	// Waiting is the default: installs must be bounded and ready when they
	// return, so tests never race a half-started simulator.
	SkipWait bool

	// WaitTimeout bounds Helm's own wait. Defaults to DefaultWaitTimeout.
	WaitTimeout time.Duration
}

// DefaultWaitTimeout bounds Helm's --wait.
const DefaultWaitTimeout = 5 * time.Minute

// Install installs or upgrades a release (helm upgrade --install).
func (r *Runner) Install(ctx context.Context, opts InstallOptions) error {
	if err := validateInstall(opts); err != nil {
		return err
	}

	waitTimeout := opts.WaitTimeout
	if waitTimeout == 0 {
		waitTimeout = DefaultWaitTimeout
	}

	args := []string{
		"upgrade", "--install", opts.Release, opts.Chart,
		"--namespace", opts.Namespace,
		"--create-namespace",
		"--kubeconfig", r.kubeconfig,
		"--timeout", waitTimeout.String(),
	}
	if !opts.SkipWait {
		args = append(args, "--wait")
	}

	valuesFile := ""
	if len(opts.Values) > 0 {
		data, err := yaml.Marshal(opts.Values)
		if err != nil {
			return fmt.Errorf("marshaling helm values: %w", err)
		}
		tmp, err := os.CreateTemp("", "hw-disc-helm-values-*.yaml")
		if err != nil {
			return fmt.Errorf("creating helm values file: %w", err)
		}
		defer func() { _ = os.Remove(tmp.Name()) }()

		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("writing helm values file: %w", err)
		}
		if err := tmp.Close(); err != nil {
			return fmt.Errorf("closing helm values file: %w", err)
		}
		valuesFile = tmp.Name()
		args = append(args, "--values", valuesFile)
	}

	return r.run(ctx, args...)
}

// Uninstall removes a release from the given namespace.
func (r *Runner) Uninstall(ctx context.Context, release, namespace string) error {
	if release == "" || namespace == "" {
		return errors.New("uninstall requires release and namespace")
	}
	return r.run(ctx, "uninstall", release, "--namespace", namespace, "--kubeconfig", r.kubeconfig)
}

// ChartAvailable verifies that the chart reference can be resolved and
// pulled from its registry (helm show chart). Runtime artifact
// availability problems fail visibly here, before any cluster is created.
func (r *Runner) ChartAvailable(ctx context.Context, chart string) error {
	if chart == "" {
		return errors.New("chart reference must not be empty")
	}
	return r.run(ctx, "show", "chart", chart, "--kubeconfig", r.kubeconfig)
}

// controllerImageValueKey is the chart value key carrying the service
// controller image (approved packaging contract:
// controllerImage.{repository,tag} kept as distinct whole values).
const controllerImageValueKey = "controllerImage"

// ControllerImageValues returns Helm values setting the service chart's
// controller image from whole repository and tag values. Splitting a
// combined image reference is rejected: repository and tag stay distinct
// values, and a repository containing a tag separator is a configuration
// error, not something to split silently.
func ControllerImageValues(repository, tag string) (map[string]any, error) {
	if repository == "" {
		return nil, errors.New("controller image repository must not be empty")
	}
	if tag == "" {
		return nil, errors.New("controller image tag must not be empty")
	}
	if strings.Contains(repository, ":") {
		return nil, fmt.Errorf("controller image repository %q must not contain a tag separator: "+
			"pass repository and tag as separate values", repository)
	}

	return map[string]any{
		controllerImageValueKey: map[string]any{
			"repository": repository,
			"tag":        tag,
		},
	}, nil
}

// LocalChartRef validates that dir is a local Helm chart directory (it must
// contain Chart.yaml) and returns it as a chart reference for Install.
//
// The reviewed service chart does not exist yet at the time of writing (it
// is generated by `make helm-chart`, a later task); a missing chart is a
// deterministic prerequisite failure, never a silent skip.
func LocalChartRef(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("chart directory must not be empty")
	}

	chartFile := filepath.Join(dir, "Chart.yaml")
	if _, err := os.Stat(chartFile); err != nil {
		return "", fmt.Errorf("chart directory %s is not usable (missing %s; generate it with "+
			"`make helm-chart` before running the e2e suite): %w", dir, chartFile, err)
	}
	return dir, nil
}

// run executes the Helm command with the run-private kubeconfig in the
// subprocess environment. The context bounds the subprocess (SIGKILL on
// expiry) and the error honestly reports the timeout.
func (r *Runner) run(ctx context.Context, args ...string) error {
	argv := make([]string, 0, len(r.argv)+len(args))
	argv = append(argv, r.argv...)
	argv = append(argv, args...)

	env := make([]string, 0, len(os.Environ())+1)
	env = append(env, os.Environ()...)
	env = append(env, kubeconfigEnvKey+"="+r.kubeconfig)

	if err := r.process(ctx, argv, env); err != nil {
		return fmt.Errorf("helm %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func validateInstall(opts InstallOptions) error {
	switch {
	case opts.Release == "":
		return errors.New("install requires a release name")
	case opts.Chart == "":
		return errors.New("install requires a chart reference")
	case opts.Namespace == "":
		return errors.New("install requires a namespace")
	}
	return nil
}

// runProcess is the real subprocess implementation: bounded by ctx,
// environment overridden with the run-private kubeconfig, combined output
// captured for error reporting.
func runProcess(ctx context.Context, argv []string, env []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %s (output: %s)", ctx.Err(), argv, strings.TrimSpace(out.String()))
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}
