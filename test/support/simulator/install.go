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

package simulator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/cluster"
	"github.com/lburgazzoli/opendatahub-hardware-discovery-operator/test/support/helm"
)

// Setup installs the simulator for the profile on a run-owned cluster:
//
//  1. every node is labeled with the profile's node labels (the fake
//     plugins' DaemonSet selectors and the topology input assignment);
//  2. for the DRA profile, the stable resource.k8s.io/v1 API is verified
//     to be served (explicitly, per the approved test profile);
//  3. the digest-pinned FGO chart is installed with the frozen values.
//
// A failed install uninstalls whatever Helm created, so a Setup error
// leaves the cluster clean; the error is still returned visibly.
func Setup(ctx context.Context, cli client.Client, runner *helm.Runner, profile Profile) error {
	if profile.Values() == nil {
		return fmt.Errorf("unknown simulator profile %q", profile)
	}

	if err := LabelNodes(ctx, cli, profile.NodeLabels()); err != nil {
		return fmt.Errorf("labeling nodes for profile %s: %w", profile, err)
	}

	if profile == ProfileDRA {
		available, err := cluster.ResourceAPIAvailable(cli)
		if err != nil {
			return fmt.Errorf("checking resource.k8s.io/v1 availability: %w", err)
		}
		if !available {
			return fmt.Errorf("stable resource.k8s.io/v1 is not served by this cluster; " +
				"the DRA profile requires the pinned Kubernetes >= 1.34 baseline")
		}
	}

	if err := runner.Install(ctx, helm.InstallOptions{
		Release:   ReleaseName,
		Chart:     ChartRef(),
		Namespace: Namespace,
		Values:    profile.Values(),
	}); err != nil {
		return errors.Join(fmt.Errorf("installing simulator profile %s: %w", profile, err), Teardown(ctx, cli, runner))
	}

	return nil
}

// Teardown uninstalls the simulator release if it is present. It is
// cluster-state driven (Helm release secrets), so calling it when nothing
// was installed is a no-op rather than an error.
func Teardown(ctx context.Context, cli client.Client, runner *helm.Runner) error {
	installed, err := ReleaseInstalled(ctx, cli)
	if err != nil {
		return fmt.Errorf("checking simulator release: %w", err)
	}
	if !installed {
		return nil
	}

	if err := runner.Uninstall(ctx, ReleaseName, Namespace); err != nil {
		return fmt.Errorf("uninstalling simulator release: %w", err)
	}
	return nil
}

// ReleaseInstalled reports whether the simulator's Helm release exists in
// the fixed namespace (Helm stores releases as sh.helm.release.v1.<name>.vN
// secrets).
func ReleaseInstalled(ctx context.Context, cli client.Client) (bool, error) {
	secrets := &corev1.SecretList{}
	if err := cli.List(ctx, secrets, client.InNamespace(Namespace)); err != nil {
		return false, fmt.Errorf("listing helm release secrets: %w", err)
	}

	prefix := fmt.Sprintf("sh.helm.release.v1.%s.", ReleaseName)
	for i := range secrets.Items {
		if strings.HasPrefix(secrets.Items[i].Name, prefix) {
			return true, nil
		}
	}
	return false, nil
}

// LabelNodes applies labels to every node of the cluster. Node labels are
// test-only simulator input; production discovery code never writes them.
func LabelNodes(ctx context.Context, cli client.Client, labels map[string]string) error {
	if len(labels) == 0 {
		return errors.New("no labels to apply")
	}

	nodes := &corev1.NodeList{}
	if err := cli.List(ctx, nodes); err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return errors.New("no nodes found to label")
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		patch := client.MergeFrom(node.DeepCopy())
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		maps.Copy(node.Labels, labels)
		if err := cli.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("labeling node %s: %w", node.Name, err)
		}
	}

	return nil
}
