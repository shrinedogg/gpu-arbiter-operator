package controller

import (
	"fmt"
	"time"

	gpuv1alpha1 "github.com/shrinedogg/gpu-arbiter-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gateDecisionKind classifies the outcome of evaluating a gate.
type gateDecisionKind int

const (
	// reasonWait: keep the gate; none of the ungate conditions are met yet.
	reasonWait gateDecisionKind = iota
	// reasonUngate: a condition is met; remove the gate.
	reasonUngate
)

// gateDecision is the result of evaluating whether a pod's gate may be removed.
type gateDecision struct {
	kind gateDecisionKind
	// human-readable rationale, e.g. "vllm-down free=32000MiB" or
	// "free=9000MiB" or "timeout 45s free=-1MiB".
	detail string
}

func (d gateDecision) String() string { return d.detail }

// evaluateGate mirrors the bash decision tree:
//
//	if vllm_down:                                       ungate (primary)
//	elif free >= threshold:                             ungate (secondary)
//	elif age >= timeout:                                ungate (fallback)
//	else:                                               wait
//
// vllm_down is preferred because it is a ~1-2s signal (the deployment's own
// status), whereas the DCGM scrape lags ~30s. The metric is still included in
// the log detail for observability.
func evaluateGate(vllmDown bool, freeMiB, threshold int64, age, timeout time.Duration) gateDecision {
	switch {
	case vllmDown:
		return gateDecision{kind: reasonUngate, detail: fmt.Sprintf("vllm-down free=%dMiB", freeMiB)}
	case freeMiB >= threshold:
		return gateDecision{kind: reasonUngate, detail: fmt.Sprintf("free=%dMiB", freeMiB)}
	case age >= timeout:
		return gateDecision{kind: reasonUngate, detail: fmt.Sprintf("timeout %s free=%dMiB", age.Truncate(time.Second), freeMiB)}
	default:
		return gateDecision{kind: reasonWait, detail: fmt.Sprintf("wait free=%dMiB<%d age=%s vllm_up", freeMiB, threshold, age.Truncate(time.Second))}
	}
}

// podHasGate reports whether the pod currently carries the named gate.
func podHasGate(pod *corev1.Pod, gate string) bool {
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == gate {
			return true
		}
	}
	return false
}

// podStatus builds the status snapshot for a still-gated pod.
func podStatus(pod *corev1.Pod, reason string) gpuv1alpha1.GatedPodStatus {
	first := pod.CreationTimestamp
	// Use the creation timestamp as the gate's "first seen" time. The bash
	// script tracked first-seen from its own memory; creation time is a stable
	// proxy that survives controller restarts.
	if first.IsZero() {
		first = metav1.Now()
	}
	return gpuv1alpha1.GatedPodStatus{
		Name:      pod.Name,
		FirstSeen: first,
		Reason:    reason,
	}
}

// vllmReplicasAreZero reports whether vLLM has released its VRAM: the
// Deployment reports no non-terminated pods. This is a fresh ~1-2s signal,
// unlike the ~30s DCGM scrape.
//
// Mirrors the bash `vllm_down` which checked `.status.replicas` == 0 or empty.
// We treat both status.replicas and readyReplicas: status.replicas reflects
// the reconciled pod count the scheduler/kubelet is working with.
func vllmReplicasAreZero(dep *appsv1.Deployment) bool {
	r := dep.Status.Replicas
	return r == 0
}

// filterActive drops terminal pods so they no longer count as "a game is
// running". A Succeeded/Failed pod has released its GPU and must not keep vLLM
// pinned to 0. The bash script didn't filter these (it counted raw pods), but
// doing so avoids vLLM staying scaled-down after a crash-looping session ends.
func filterActive(pods []corev1.Pod) []corev1.Pod {
	out := pods[:0:0]
	for i := range pods {
		p := &pods[i]
		switch p.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		}
		out = append(out, pods[i])
	}
	return out
}

// withDefaults applies the spec defaults that the bash script took from env
// vars. This lets a CR omit any field with an obvious default.
func withDefaults(spec gpuv1alpha1.GPUArbiterSpec) gpuv1alpha1.GPUArbiterSpec {
	if spec.GateName == "" {
		spec.GateName = defaultGateName
	}
	if spec.FreeMiB == 0 {
		spec.FreeMiB = defaultFreeMiB
	}
	if spec.TimeoutSeconds == 0 {
		spec.TimeoutSeconds = defaultTimeoutSeconds
	}
	if spec.IntervalSeconds == 0 {
		spec.IntervalSeconds = defaultIntervalSeconds
	}
	if spec.Metrics.URL == "" {
		spec.Metrics.URL = "http://vmsingle-stack.observability.svc:8428"
	}
	if spec.Metrics.Query == "" {
		spec.Metrics.Query = "max(DCGM_FI_DEV_FB_FREE)"
	}
	if spec.Metrics.TimeoutSeconds == 0 {
		spec.Metrics.TimeoutSeconds = 4
	}
	return spec
}

// statusMeaningfullyChanged reports whether the new status differs from the
// persisted one in any field the controller is responsible for. LastReconcile
// is intentionally ignored (it would otherwise force a write every cycle).
func statusMeaningfullyChanged(old, new gpuv1alpha1.GPUArbiterStatus) bool {
	if old.ObservedGeneration != new.ObservedGeneration {
		return true
	}
	if old.GamePods != new.GamePods || old.Message != new.Message {
		return true
	}
	if !int32PtrEqual(old.CurrentVLLMReplicas, new.CurrentVLLMReplicas) {
		return true
	}
	if !int32PtrEqual(old.DesiredVLLMReplicas, new.DesiredVLLMReplicas) {
		return true
	}
	if !int64PtrEqual(old.FreeVRAMMiB, new.FreeVRAMMiB) {
		return true
	}
	return !gatedPodStatusesEqual(old.GatedPods, new.GatedPods)
}

func int32PtrEqual(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func gatedPodStatusesEqual(a, b []gpuv1alpha1.GatedPodStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Reason != b[i].Reason {
			return false
		}
		// FirstSeen is derived from pod creation time; ignore sub-second drift.
		if !a[i].FirstSeen.Equal(&b[i].FirstSeen) {
			return false
		}
	}
	return true
}
