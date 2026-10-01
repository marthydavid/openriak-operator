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
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// operatorProblems returns what is wrong with the operator pods: any restart, and
// in particular an OOMKill. An operator that crash-loops makes the fleet stall
// rather than fail, which showed up only as unexplained slow convergence (issue
// #48), so the harness fails on it explicitly.
func operatorProblems(pods []corev1.Pod) []string {
	var bad []string
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.RestartCount > 0 {
				reason := "unknown"
				if t := cs.LastTerminationState.Terminated; t != nil {
					reason = fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
				}
				bad = append(bad, fmt.Sprintf("operator pod %s container %s restarted %d times, last termination: %s",
					p.Name, cs.Name, cs.RestartCount, reason))
			}
			if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
				bad = append(bad, fmt.Sprintf("operator pod %s container %s is OOMKilled", p.Name, cs.Name))
			}
		}
		if p.Status.Phase != corev1.PodRunning {
			bad = append(bad, fmt.Sprintf("operator pod %s is %s, not Running", p.Name, p.Status.Phase))
		}
	}
	return bad
}

// verifyOperatorHealthy finds the operator pods (label control-plane=controller-manager,
// in -operator-namespace or any namespace) and fails if they restarted or are
// not running.
func verifyOperatorHealthy(ctx context.Context, c client.Client, o opts) error {
	pods := &corev1.PodList{}
	opts := []client.ListOption{client.MatchingLabels{"control-plane": "controller-manager"}}
	if o.operatorNamespace != "" {
		opts = append(opts, client.InNamespace(o.operatorNamespace))
	}
	if err := c.List(ctx, pods, opts...); err != nil {
		return fmt.Errorf("list operator pods: %w", err)
	}
	if len(pods.Items) == 0 {
		fmt.Println("operator health: no pod with control-plane=controller-manager found; skipping (set -operator-namespace)")
		return nil
	}
	if bad := operatorProblems(pods.Items); len(bad) > 0 {
		for _, b := range bad {
			fmt.Println("  OPERATOR:", b)
		}
		return fmt.Errorf("%d operator health problems (see issue #48 for the memory limit)", len(bad))
	}
	fmt.Printf("OPERATOR OK: %d pod(s) running with 0 restarts\n", len(pods.Items))
	return nil
}
