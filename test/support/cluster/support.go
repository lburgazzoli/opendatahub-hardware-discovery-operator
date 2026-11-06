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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// resourceAPIGroup and resourceAPIVersion identify the stable DRA API the
// DRA scenarios depend on. Alpha or beta DRA groups are never used.
const (
	resourceAPIGroup   = "resource.k8s.io"
	resourceAPIVersion = "v1"
)

// ResourceAPIAvailable reports whether the cluster serves the stable
// resource.k8s.io/v1 API (DRA GA on Kubernetes >= 1.34). DRA-profile
// scenarios must check this explicitly before running; a false result with
// a nil error is an unsupported baseline, not a simulator failure.
func ResourceAPIAvailable(c client.Client) (bool, error) {
	mapping, err := c.RESTMapper().RESTMapping(
		schema.GroupKind{Group: resourceAPIGroup, Kind: "ResourceSlice"},
		resourceAPIVersion,
	)
	if err == nil {
		return mapping != nil, nil
	}
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking %s/%s discovery: %w", resourceAPIGroup, resourceAPIVersion, err)
}

// dumpEvents writes all cluster events to path as YAML. Missing events
// (e.g. API server flake during diagnostics) are recorded as an error but
// never panic the diagnostics path.
func dumpEvents(ctx context.Context, c client.Client, path string) error {
	events := &corev1.EventList{}
	if err := c.List(ctx, events); err != nil {
		return fmt.Errorf("listing events for diagnostics: %w", err)
	}

	data, err := yaml.Marshal(events)
	if err != nil {
		return fmt.Errorf("marshaling events: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating diagnostics directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing events diagnostics: %w", err)
	}
	return nil
}

// CollectPodLogs writes the current logs of every pod in namespace into
// dir, one file per pod, through a client built from the run-private REST
// configuration. Pods whose logs cannot be read (pending, terminated) are
// recorded as an error inside their file; the collection itself never
// aborts on a single pod.
func CollectPodLogs(ctx context.Context, cfg *rest.Config, namespace, dir string) error {
	if cfg == nil {
		return fmt.Errorf("collecting pod logs requires the run-private REST configuration")
	}
	if namespace == "" {
		return fmt.Errorf("collecting pod logs requires a namespace")
	}

	clientset, err := kubernetes.NewForConfig(rest.CopyConfig(cfg))
	if err != nil {
		return fmt.Errorf("building clientset for pod logs: %w", err)
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing pods in %s for diagnostics: %w", namespace, err)
	}

	logsDir := filepath.Join(dir, "pod-logs")
	if err := os.MkdirAll(logsDir, 0o750); err != nil {
		return fmt.Errorf("creating pod logs directory: %w", err)
	}

	var errs []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := writePodLog(ctx, clientset, pod, logsDir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writePodLog streams one pod's logs into logsDir/<pod>.log. A pod with no
// readable logs still produces a file containing the reason, so evidence of
// the pod's existence is retained.
func writePodLog(
	ctx context.Context,
	clientset *kubernetes.Clientset,
	pod *corev1.Pod,
	logsDir string,
) error {
	path := filepath.Join(logsDir, pod.Name+".log")

	var body []byte
	req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		body = []byte(fmt.Sprintf("could not stream logs: %v\n", err))
	} else {
		data, readErr := io.ReadAll(stream)
		_ = stream.Close()
		body = data
		if readErr != nil {
			body = append(body, []byte(fmt.Sprintf("\ncould not read logs: %v\n", readErr))...)
		}
	}

	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("writing pod log %s: %w", path, err)
	}
	return nil
}
