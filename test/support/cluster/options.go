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

package cluster

import (
	"fmt"
	"maps"
	"os"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultNodeImage is the approved Kind baseline (testing.md "Approved
	// simulator/test profile"): Kubernetes 1.34 with native resource.k8s.io/v1.
	DefaultNodeImage = "kindest/node:v1.34.11"

	// DefaultRuntime is the approved container runtime baseline.
	DefaultRuntime = "podman"

	// namePrefix prefixes every run-owned cluster name.
	namePrefix = "hw-disc-"
)

// Runtime selects the container runtime backing the Kind node containers.
type Runtime string

const (
	// RuntimePodman is the only supported runtime (rootless podman).
	RuntimePodman Runtime = "podman"
)

// Option configures a Kind cluster Instance.
type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (fn optionFunc) apply(opts *options) {
	if fn != nil && opts != nil {
		fn(opts)
	}
}

type options struct {
	runtime       Runtime
	nodeImage     string
	featureGates  map[string]bool
	waitForReady  time.Duration
	createTimeout time.Duration
	deleteTimeout time.Duration
	setupTimeout  time.Duration
	loadTimeout   time.Duration
	logFn         func(format string, args ...any)
	clientFactory func(cfg *rest.Config) (client.Client, error)
}

// defaults returns the validated default option set. Bounds: kind's own
// control-plane wait (waitForReady) is nested inside the overall create
// bound (createTimeout), which must therefore be strictly larger.
func defaults() options {
	return options{
		runtime:       RuntimePodman,
		nodeImage:     DefaultNodeImage,
		waitForReady:  3 * time.Minute,
		createTimeout: 5 * time.Minute,
		deleteTimeout: 3 * time.Minute,
		setupTimeout:  3 * time.Minute,
		loadTimeout:   5 * time.Minute,
		logFn: func(format string, args ...any) {
			_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
}

func (o options) validate() error {
	if o.runtime != RuntimePodman {
		return fmt.Errorf("unsupported runtime %q: only %s is supported (docker is not); "+
			"run the suite through `make test-e2e` or `make demo`, or leave the runtime unset for podman",
			o.runtime, RuntimePodman)
	}
	if o.nodeImage == "" {
		return fmt.Errorf("node image must not be empty")
	}
	for _, check := range []struct {
		name  string
		value time.Duration
	}{
		{"waitForReady", o.waitForReady},
		{"createTimeout", o.createTimeout},
		{"deleteTimeout", o.deleteTimeout},
		{"setupTimeout", o.setupTimeout},
		{"loadTimeout", o.loadTimeout},
	} {
		if check.value <= 0 {
			return fmt.Errorf("%s must be positive, got %s", check.name, check.value)
		}
	}
	if o.waitForReady >= o.createTimeout {
		return fmt.Errorf("waitForReady (%s) must be smaller than createTimeout (%s)", o.waitForReady, o.createTimeout)
	}
	return nil
}

// WithRuntime selects the container runtime. Podman is the only supported
// value and the default; an empty value keeps the default. Every other
// value — including docker — fails option validation and provider
// construction before any engine side effect, and is never silently
// ignored or mapped to another engine.
func WithRuntime(runtime Runtime) Option {
	return optionFunc(func(opts *options) {
		if runtime != "" {
			opts.runtime = runtime
		}
	})
}

// WithNodeImage overrides the pinned node image. The default is the
// approved baseline DefaultNodeImage; overrides are for investigation only.
func WithNodeImage(image string) Option {
	return optionFunc(func(opts *options) {
		if image != "" {
			opts.nodeImage = image
		}
	})
}

// WithFeatureGates enables Kubernetes feature gates on the run-owned
// cluster's control-plane components. The map is copied, so later changes
// by the caller cannot leak into a created cluster. Gates stay off by
// default: only frozen profiles whose contract facts require an alpha
// field opt in (e.g. DRAExtendedResource for the DeviceClass
// extendedResourceName fact on resource.k8s.io/v1).
func WithFeatureGates(gates map[string]bool) Option {
	return optionFunc(func(opts *options) {
		if len(gates) == 0 {
			return
		}
		copied := make(map[string]bool, len(gates))
		maps.Copy(copied, gates)
		opts.featureGates = copied
	})
}

// WithCreateTimeout bounds the context-less provider Create call.
func WithCreateTimeout(d time.Duration) Option {
	return optionFunc(func(opts *options) {
		if d > 0 {
			opts.createTimeout = d
		}
	})
}

// WithDeleteTimeout bounds the context-less provider Delete call during
// Stop. It is also the bound of the independent cleanup context.
func WithDeleteTimeout(d time.Duration) Option {
	return optionFunc(func(opts *options) {
		if d > 0 {
			opts.deleteTimeout = d
		}
	})
}

// WithSetupTimeout bounds Setup's node-readiness wait.
func WithSetupTimeout(d time.Duration) Option {
	return optionFunc(func(opts *options) {
		if d > 0 {
			opts.setupTimeout = d
		}
	})
}

// WithLoadTimeout bounds LoadImage's save-and-import operation.
func WithLoadTimeout(d time.Duration) Option {
	return optionFunc(func(opts *options) {
		if d > 0 {
			opts.loadTimeout = d
		}
	})
}

// WithWaitForReady sets kind's own control-plane readiness wait, which is
// nested inside the overall create bound.
func WithWaitForReady(d time.Duration) Option {
	return optionFunc(func(opts *options) {
		if d > 0 {
			opts.waitForReady = d
		}
	})
}

// WithLogFn installs a log sink for harness progress messages.
func WithLogFn(logFn func(format string, args ...any)) Option {
	return optionFunc(func(opts *options) {
		if logFn != nil {
			opts.logFn = logFn
		}
	})
}
