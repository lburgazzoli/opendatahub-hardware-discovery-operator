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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	. "github.com/onsi/gomega"
)

// fakeProvider records every call and never touches a container runtime:
// ordinary unit tests must not create clusters (task 010 acceptance).
type fakeProvider struct {
	mu sync.Mutex

	createSpecs []Spec
	deleteCalls []struct {
		name       string
		kubeconfig string
	}
	loadCalls []struct {
		name  string
		image string
	}
	logsCalls   []string
	createErr   error
	createBlock <-chan struct{} // when non-nil, Create blocks until closed
	deleteErr   error
	loadErr     error
	loadBlock   <-chan struct{} // when non-nil, LoadImage blocks until closed
}

func (f *fakeProvider) Create(spec Spec) error {
	f.mu.Lock()
	f.createSpecs = append(f.createSpecs, spec)
	f.mu.Unlock()

	if f.createBlock != nil {
		<-f.createBlock
	}
	if f.createErr == nil {
		// Real kind writes the run-private kubeconfig at create time.
		if err := os.WriteFile(spec.KubeconfigPath, []byte(fakeKubeconfig), 0o600); err != nil {
			return err
		}
	}
	return f.createErr
}

func (f *fakeProvider) Delete(name, kubeconfigPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, struct {
		name       string
		kubeconfig string
	}{name: name, kubeconfig: kubeconfigPath})
	return f.deleteErr
}

func (f *fakeProvider) CollectLogs(name, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logsCalls = append(f.logsCalls, name)
	return nil
}

func (f *fakeProvider) LoadImage(name, image string) error {
	f.mu.Lock()
	f.loadCalls = append(f.loadCalls, struct {
		name  string
		image string
	}{name: name, image: image})
	f.mu.Unlock()

	if f.loadBlock != nil {
		<-f.loadBlock
	}
	return f.loadErr
}

func (f *fakeProvider) firstCreateSpec() Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.createSpecs) == 0 {
		return Spec{}
	}
	return f.createSpecs[0]
}

func (f *fakeProvider) deletedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.deleteCalls))
	for _, d := range f.deleteCalls {
		names = append(names, d.name)
	}
	return names
}

// fakeKubeconfig is a minimal standalone kubeconfig document the fake
// provider "writes" for the run-private path, mirroring what kind does.
const fakeKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user:
    token: fake-token
`

// newTestCluster wires a fake provider plus a fake controller-runtime
// client factory so nothing in the unit tests dials a network endpoint.
func newTestCluster(t *testing.T, g *GomegaWithT, p *fakeProvider) Instance {
	t.Helper()

	inst, err := NewWithProvider(context.Background(), p, optionFunc(func(o *options) {
		o.clientFactory = func(_ *rest.Config) (client.Client, error) {
			return fake.NewClientBuilder().WithScheme(newFakeScheme()).Build(), nil
		}
	}))
	g.Expect(err).NotTo(HaveOccurred())
	return inst
}

func newFakeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func TestNewCreatesUniqueRunOwnedCluster(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}

	inst := newTestCluster(t, g, p)
	other := newTestCluster(t, g, p)

	g.Expect(inst.Name()).To(HavePrefix(namePrefix))
	g.Expect(other.Name()).To(HavePrefix(namePrefix))
	g.Expect(inst.Name()).NotTo(Equal(other.Name()), "each run must own a unique cluster name")

	spec := p.firstCreateSpec()
	g.Expect(spec.Name).To(Equal(inst.Name()))
	g.Expect(spec.NodeImage).To(Equal(DefaultNodeImage), "pinned node image must be the default")
	g.Expect(spec.KubeconfigPath).To(Equal(inst.KubeconfigPath()))
	g.Expect(spec.KubeconfigPath).To(HaveSuffix("kubeconfig"))
	g.Expect(spec.WaitForReady).To(BeNumerically(">", 0))
	g.Expect(spec.FeatureGates).To(BeEmpty(), "no feature gate is enabled unless a profile requires one")

	// The private kubeconfig was written by the provider (kind writes it at
	// create) and parses standalone, with no ambient fallback.
	cfg, err := loadKubeconfig(inst.KubeconfigPath())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg.Host).To(Equal("https://127.0.0.1:6443"))
}

func TestStopDeletesExactlyTheOwnedCluster(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}

	inst := newTestCluster(t, g, p)
	other := newTestCluster(t, g, p)

	g.Expect(inst.Stop(context.Background())).To(Succeed())

	g.Expect(p.deletedNames()).To(ConsistOf(inst.Name()), "Stop deletes exactly the run-owned cluster")
	g.Expect(p.deletedNames()).NotTo(ContainElement(other.Name()), "no prefix-wide purge, no foreign clusters")

	g.Expect(p.deleteCalls[0].kubeconfig).To(Equal(inst.KubeconfigPath()),
		"delete must receive the run-private kubeconfig")

	_, err := os.Stat(inst.KubeconfigPath())
	g.Expect(os.IsNotExist(err)).To(BeTrue(), "run-private kubeconfig must be removed by Stop")
}

func TestStopRunsIndependentlyOfCancelledContext(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}
	inst := newTestCluster(t, g, p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g.Expect(inst.Stop(ctx)).To(Succeed(), "cleanup must not inherit caller cancellation")
	g.Expect(p.deletedNames()).To(ConsistOf(inst.Name()))
}

func TestCreateFailureCleansUpAndReports(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{createErr: errors.New("podman is not running")}

	inst, err := NewWithProvider(context.Background(), p,
		optionFunc(func(o *options) {
			o.clientFactory = func(_ *rest.Config) (client.Client, error) {
				return nil, errors.New("must not be reached")
			}
		}),
	)

	g.Expect(inst).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("podman is not running"))
	g.Expect(p.deletedNames()).To(ConsistOf(p.firstCreateSpec().Name), "partial setup must be cleaned up")
	g.Expect(p.logsCalls).To(BeEmpty(), "no kind node logs exist for a failed create")
}

func TestCreateTimeoutPropagatesHonestly(t *testing.T) {
	g := NewWithT(t)

	block := make(chan struct{})
	defer close(block) // release the leaked create goroutine after the test

	p := &fakeProvider{createBlock: block}
	inst, err := NewWithProvider(context.Background(), p,
		WithCreateTimeout(50*time.Millisecond),
		WithWaitForReady(20*time.Millisecond),
		optionFunc(func(o *options) {
			o.clientFactory = func(_ *rest.Config) (client.Client, error) {
				return nil, errors.New("must not be reached")
			}
		}),
	)

	g.Expect(inst).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("timed out after 50ms"), "timeout must be reported honestly")
	g.Expect(err.Error()).To(ContainSubstring("could not be cancelled"), "no false cancellation claim")
	g.Expect(p.deletedNames()).To(ConsistOf(p.firstCreateSpec().Name), "cleanup must still be attempted after a timeout")
}

func TestCallerContextCancellationPropagatesHonestly(t *testing.T) {
	g := NewWithT(t)

	block := make(chan struct{})
	defer close(block)

	p := &fakeProvider{createBlock: block}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	inst, err := NewWithProvider(ctx, p,
		optionFunc(func(o *options) {
			o.clientFactory = func(_ *rest.Config) (client.Client, error) {
				return nil, errors.New("must not be reached")
			}
		}),
	)

	g.Expect(inst).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("caller context done"))
	g.Expect(p.deletedNames()).To(ConsistOf(p.firstCreateSpec().Name))
}

// wantUnsupportedRuntime is the shared rejection substring of every
// unsupported-runtime case.
const wantUnsupportedRuntime = "unsupported runtime"

func TestOptionsValidation(t *testing.T) {
	table := []struct {
		name string
		opts []Option
		want string
	}{
		{
			name: "unsupported runtime",
			opts: []Option{WithRuntime(Runtime("k3s"))},
			want: wantUnsupportedRuntime,
		},
		{
			name: "docker runtime is rejected",
			opts: []Option{WithRuntime(Runtime("docker"))},
			want: "unsupported runtime \"docker\": only podman is supported",
		},
		{
			name: "uppercase runtime is rejected",
			opts: []Option{WithRuntime(Runtime("Podman"))},
			want: wantUnsupportedRuntime,
		},
		{
			name: "wait must fit inside create bound",
			opts: []Option{WithWaitForReady(5 * time.Minute)},
			want: "must be smaller than createTimeout",
		},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := NewWithProvider(context.Background(), &fakeProvider{}, entry.opts...)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(entry.want))
		})
	}
}

// TestRuntimeOptionAcceptsOnlyPodman proves the option surface still
// accepts the empty value (podman default) and an explicit podman, while
// every other value fails validation before any provider or engine is
// involved.
func TestRuntimeOptionAcceptsOnlyPodman(t *testing.T) {
	g := NewWithT(t)

	for _, entry := range []struct {
		name    string
		runtime Runtime
	}{
		{name: "empty keeps the podman default", runtime: ""},
		{name: "explicit podman", runtime: RuntimePodman},
	} {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			p := &fakeProvider{}
			inst, err := NewWithProvider(context.Background(), p,
				WithRuntime(entry.runtime),
				optionFunc(func(o *options) {
					o.clientFactory = func(_ *rest.Config) (client.Client, error) {
						return fake.NewClientBuilder().WithScheme(newFakeScheme()).Build(), nil
					}
				}),
			)
			g.Expect(err).NotTo(HaveOccurred(), "the runtime must stay accepted")
			g.Expect(p.firstCreateSpec().Name).To(Equal(inst.Name()))
			g.Expect(inst.Stop(context.Background())).To(Succeed())
		})
	}

	// Docker and unknown values never reach a provider: option validation
	// fails first, with the remedy in the message.
	for _, value := range []string{"docker", "containerd", "podman-rootless"} {
		_, err := NewWithProvider(context.Background(), &fakeProvider{}, WithRuntime(Runtime(value)))
		g.Expect(err).To(HaveOccurred(), "runtime %q must be rejected", value)
		g.Expect(err.Error()).To(ContainSubstring("only podman is supported"))
		g.Expect(err.Error()).To(ContainSubstring("`make test-e2e`"))
	}
}

// TestNewKindProviderRejectsNonPodman proves provider construction also
// rejects every non-podman runtime — including docker — before any engine
// side effect (the rejection happens before the kind provider object is
// even built).
func TestNewKindProviderRejectsNonPodman(t *testing.T) {
	for _, value := range []string{"docker", "Docker", "k3s", ""} {
		t.Run("runtime "+value, func(t *testing.T) {
			g := NewWithT(t)
			provider, err := newKindProvider(Runtime(value))
			g.Expect(provider).To(BeNil(), "runtime %q must not yield a provider", value)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(wantUnsupportedRuntime))
		})
	}
}

func TestOptionsValidateRejectsInvalidBounds(t *testing.T) {
	table := []struct {
		name string
		opts func() options
		want string
	}{
		{
			name: "empty node image",
			opts: func() options {
				o := defaults()
				o.nodeImage = ""
				return o
			},
			want: "node image must not be empty",
		},
		{
			name: "non-positive create timeout",
			opts: func() options {
				o := defaults()
				o.createTimeout = 0
				return o
			},
			want: "createTimeout must be positive",
		},
		{
			name: "non-positive setup timeout",
			opts: func() options {
				o := defaults()
				o.setupTimeout = -time.Second
				return o
			},
			want: "setupTimeout must be positive",
		},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			err := entry.opts().validate()
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(entry.want))
		})
	}
}

func TestSetupWaitsForNodeReadiness(t *testing.T) {
	scheme := newFakeScheme()

	readyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-node"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionTrue,
		}}},
	}

	notReadyNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ready-node"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: corev1.ConditionFalse,
		}}},
	}

	table := []struct {
		name    string
		nodes   []client.Object
		wantErr string
	}{
		{name: "all nodes ready", nodes: []client.Object{readyNode}},
		{
			name:    "node not ready fails the bounded wait",
			nodes:   []client.Object{notReadyNode},
			wantErr: "not ready",
		},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			p := &fakeProvider{}

			fakeCli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(entry.nodes...).Build()
			inst, err := NewWithProvider(context.Background(), p,
				WithSetupTimeout(200*time.Millisecond),
				optionFunc(func(o *options) {
					o.clientFactory = func(_ *rest.Config) (client.Client, error) {
						return fakeCli, nil
					}
				}),
			)
			g.Expect(err).NotTo(HaveOccurred())

			setupErr := inst.Setup(context.Background())
			if entry.wantErr == "" {
				g.Expect(setupErr).NotTo(HaveOccurred())
			} else {
				g.Expect(setupErr).To(HaveOccurred())
				g.Expect(setupErr.Error()).To(ContainSubstring(entry.wantErr))
			}

			g.Expect(inst.Stop(context.Background())).To(Succeed())
		})
	}
}

func TestCollectDiagnosticsWritesLogs(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}

	fakeCli := fake.NewClientBuilder().WithScheme(newFakeScheme()).Build()
	inst, err := NewWithProvider(context.Background(), p,
		optionFunc(func(o *options) {
			o.clientFactory = func(_ *rest.Config) (client.Client, error) {
				return fakeCli, nil
			}
		}),
	)
	g.Expect(err).NotTo(HaveOccurred())

	dir := t.TempDir()
	g.Expect(inst.CollectDiagnostics(context.Background(), dir)).To(Succeed())
	g.Expect(p.logsCalls).To(ConsistOf(inst.Name()))

	events, err := os.ReadFile(filepath.Join(dir, "events.yaml"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(events)).To(ContainSubstring("items:"))

	g.Expect(inst.Stop(context.Background())).To(Succeed())
}

// TestWithFeatureGatesReachesTheProvider proves the feature-gates option
// is plumbed into the provider Spec as a defensive copy.
func TestWithFeatureGatesReachesTheProvider(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}

	gates := map[string]bool{"DRAExtendedResource": true}
	inst, err := NewWithProvider(context.Background(), p,
		WithFeatureGates(gates),
		optionFunc(func(o *options) {
			o.clientFactory = func(_ *rest.Config) (client.Client, error) {
				return fake.NewClientBuilder().WithScheme(newFakeScheme()).Build(), nil
			}
		}),
	)
	g.Expect(err).NotTo(HaveOccurred())

	spec := p.firstCreateSpec()
	g.Expect(spec.Name).To(Equal(inst.Name()))
	g.Expect(spec.NodeImage).To(Equal(DefaultNodeImage), "the pinned node image must survive the feature-gate path")
	g.Expect(spec.FeatureGates).To(Equal(map[string]bool{"DRAExtendedResource": true}))

	// The option copies the map: later caller mutations must not leak
	// into the spec handed to the provider.
	gates["Mutated"] = false
	g.Expect(p.firstCreateSpec().FeatureGates).NotTo(HaveKey("Mutated"))

	g.Expect(inst.Stop(context.Background())).To(Succeed())
}

func TestKubeconfigIsNeverAmbient(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}
	inst := newTestCluster(t, g, p)

	// The private kubeconfig lives in a dedicated run directory, not in the
	// ambient location, and kind receives it as an explicit path.
	g.Expect(inst.KubeconfigPath()).To(HavePrefix(os.TempDir()))
	g.Expect(strings.Contains(inst.KubeconfigPath(), ".kube")).To(BeFalse())
	g.Expect(p.firstCreateSpec().KubeconfigPath).To(Equal(inst.KubeconfigPath()))

	g.Expect(inst.Stop(context.Background())).To(Succeed())
}

func TestRunBoundedSemantics(t *testing.T) {
	table := []struct {
		name    string
		ctx     func() (context.Context, context.CancelFunc)
		bound   time.Duration
		fn      func() error
		want    string
		wantNo  string
		wantErr bool
	}{
		{
			name:    "operation success",
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			bound:   time.Minute,
			fn:      func() error { return nil },
			wantErr: false,
		},
		{
			name:    "operation error is wrapped",
			ctx:     func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			bound:   time.Minute,
			fn:      func() error { return errors.New("boom") },
			want:    "boom",
			wantErr: true,
		},
		{
			name: "timeout is honest",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			bound:   10 * time.Millisecond,
			fn:      func() error { time.Sleep(time.Second); return nil },
			want:    "could not be cancelled",
			wantErr: true,
		},
	}

	for _, entry := range table {
		t.Run(entry.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx, cancel := entry.ctx()
			defer cancel()

			err := runBounded(ctx, entry.bound, "op", entry.fn)
			if !entry.wantErr {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(entry.want))
			if entry.wantNo != "" {
				g.Expect(err.Error()).NotTo(ContainSubstring(entry.wantNo))
			}
		})
	}
}

func TestRunBoundedNotStarted(t *testing.T) {
	g := NewWithT(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runBounded(ctx, time.Minute, "op", func() error { return nil })
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("not started"))
}

func TestLoadImageTargetsTheOwnedCluster(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}
	inst := newTestCluster(t, g, p)

	g.Expect(inst.LoadImage(context.Background(), "example.invalid/manager:latest")).To(Succeed())

	p.mu.Lock()
	calls := append([]struct {
		name  string
		image string
	}(nil), p.loadCalls...)
	p.mu.Unlock()

	g.Expect(calls).To(HaveLen(1))
	g.Expect(calls[0].name).To(Equal(inst.Name()), "the image must go to the exact run-owned cluster")
	g.Expect(calls[0].image).To(Equal("example.invalid/manager:latest"))

	g.Expect(inst.Stop(context.Background())).To(Succeed())
}

func TestLoadImageRequiresImageReference(t *testing.T) {
	g := NewWithT(t)
	p := &fakeProvider{}
	inst := newTestCluster(t, g, p)

	err := inst.LoadImage(context.Background(), "")
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("image reference must not be empty"))

	p.mu.Lock()
	calls := len(p.loadCalls)
	p.mu.Unlock()
	g.Expect(calls).To(Equal(0), "an empty reference must not reach the provider")

	g.Expect(inst.Stop(context.Background())).To(Succeed())
}

func TestLoadImageTimeoutPropagatesHonestly(t *testing.T) {
	g := NewWithT(t)

	block := make(chan struct{})
	defer close(block)

	p := &fakeProvider{loadBlock: block}
	inst, err := NewWithProvider(context.Background(), p,
		WithLoadTimeout(50*time.Millisecond),
		optionFunc(func(o *options) {
			o.clientFactory = func(_ *rest.Config) (client.Client, error) {
				return fake.NewClientBuilder().WithScheme(newFakeScheme()).Build(), nil
			}
		}),
	)
	g.Expect(err).NotTo(HaveOccurred())

	loadErr := inst.LoadImage(context.Background(), "example.invalid/manager:latest")
	g.Expect(loadErr).To(HaveOccurred())
	g.Expect(loadErr.Error()).To(ContainSubstring("timed out after 50ms"))
	g.Expect(loadErr.Error()).To(ContainSubstring("could not be cancelled"))

	g.Expect(inst.Stop(context.Background())).To(Succeed())
}

func TestCollectPodLogsWritesPodLogs(t *testing.T) {
	g := NewWithT(t)

	const namespace = "gpu-operator"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/namespaces/"+namespace+"/pods", func(w http.ResponseWriter, _ *http.Request) {
		podList := corev1.PodList{
			Items: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-ok", Namespace: namespace}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-broken", Namespace: namespace}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		g.Expect(json.NewEncoder(w).Encode(&podList)).To(Succeed())
	})
	mux.HandleFunc("/api/v1/namespaces/"+namespace+"/pods/pod-ok/log", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pod-ok log line\n")
	})
	mux.HandleFunc("/api/v1/namespaces/"+namespace+"/pods/pod-broken/log", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "container not started", http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := &rest.Config{Host: server.URL}
	dir := t.TempDir()

	g.Expect(CollectPodLogs(context.Background(), cfg, namespace, dir)).To(Succeed())

	okLog, err := os.ReadFile(filepath.Join(dir, "pod-logs", "pod-ok.log"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(okLog)).To(ContainSubstring("pod-ok log line"))

	// A pod without readable logs still produces evidence of the reason.
	brokenLog, err := os.ReadFile(filepath.Join(dir, "pod-logs", "pod-broken.log"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(brokenLog)).To(ContainSubstring("could not stream logs"))
}

func TestCollectPodLogsRejectsMissingInputs(t *testing.T) {
	g := NewWithT(t)

	g.Expect(CollectPodLogs(context.Background(), nil, "ns", t.TempDir())).To(HaveOccurred())
	g.Expect(CollectPodLogs(context.Background(), &rest.Config{Host: "http://127.0.0.1:1"}, "", t.TempDir())).
		To(HaveOccurred())
}
