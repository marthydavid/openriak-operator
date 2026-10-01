package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func opPod(restarts int32, last *corev1.ContainerStateTerminated, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		Status: corev1.PodStatus{
			Phase: phase,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "manager", RestartCount: restarts,
				LastTerminationState: corev1.ContainerState{Terminated: last},
			}},
		},
	}
}

func TestOperatorProblems(t *testing.T) {
	if bad := operatorProblems([]corev1.Pod{opPod(0, nil, corev1.PodRunning)}); len(bad) != 0 {
		t.Fatalf("a running pod with no restarts is healthy: %v", bad)
	}
	oom := &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}
	bad := operatorProblems([]corev1.Pod{opPod(6, oom, corev1.PodRunning)})
	if len(bad) != 1 || !strings.Contains(bad[0], "restarted 6 times") || !strings.Contains(bad[0], "OOMKilled (exit 137)") {
		t.Fatalf("an OOMKill loop must be reported with its reason: %v", bad)
	}
	if bad := operatorProblems([]corev1.Pod{opPod(0, nil, corev1.PodPending)}); len(bad) != 1 {
		t.Fatalf("a pod that is not Running must be reported: %v", bad)
	}
}
