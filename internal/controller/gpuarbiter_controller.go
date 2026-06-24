package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	gpuv1alpha1 "github.com/shrinedogg/gpu-arbiter-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// Default gate name injected by the MutatingAdmissionPolicy.
	defaultGateName = "gpu.biggs.dog/await-vram"
	// Default VRAM threshold (MiB). Free is ~518 with vLLM up, ~32000 after
	// scale-to-0.
	defaultFreeMiB = 8000
	// Default safety fallback before ungate anyway (kept under direwolf's 60s
	// session reaper).
	defaultTimeoutSeconds = 45
	// Default re-poll interval for the VRAM metric while waiting to ungate.
	defaultIntervalSeconds = 2
	// Idle target replicas for vLLM.
	idleReplicas int32 = 1
	// In-use (a game is running) target replicas for vLLM.
	busyReplicas int32 = 0
)

// GPUArbiterReconciler reconciles a GPUArbiter object. It owns the two pieces
// of behaviour the old bash loop performed:
//
//  1. Scale the vLLM Deployment to 0 while a gaming pod is present, back to 1
//     when idle.
//  2. For each gated gaming pod, remove the await-vram gate once VRAM is free
//     (vLLM reports 0 ready pods, OR DCGM free-VRAM crosses the threshold, OR
//     a bounded timeout elapses).
type GPUArbiterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Metrics queries VictoriaMetrics for the DCGM free-VRAM gauge. Nil-safe:
	// a nil client reports "unknown" and the controller falls back to the
	// vLLM/timeout signals.
	Metrics MetricsQuerier
}

// Reconcile implements the arbiter's per-CR control loop.
func (r *GPUArbiterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var arb gpuv1alpha1.GPUArbiter
	if err := r.Get(ctx, req.NamespacedName, &arb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	spec := withDefaults(arb.Spec)
	now := time.Now()
	status := gpuv1alpha1.GPUArbiterStatus{
		ObservedGeneration: arb.Generation,
	}
	var messages []string
	defer func() {
		// Best-effort status update; failures don't abort the reconcile's
		// primary effect (scaling/ungating). Status is observability only.
		//
		// Guard against chatty writes: the controller re-polls every
		// IntervalSeconds forever, so only persist status when something
		// meaningful changed (or the spec generation advanced). LastReconcile
		// is bumped only when we actually write, so it reflects real activity.
		status.Message = strings.Join(messages, "; ")
		if !statusMeaningfullyChanged(arb.Status, status) {
			return
		}
		status.LastReconcile = metav1.Now()
		updated := arb.DeepCopy()
		updated.Status = status
		// Full status Update (not a JSON merge patch): a merge patch with
		// omitempty fields silently drops zero values (e.g. GamePods going
		// 1->0), leaving stale status. Update writes every field
		// authoritatively. arb.DeepCopy() carries the right resourceVersion;
		// the single-leader/single-worker controller makes conflicts a
		// non-issue, and any conflict simply retries on the next requeue.
		if err := r.Status().Update(ctx, updated); err != nil {
			logger.Error(err, "failed to update GPUArbiter status")
		}
	}()

	// --- Gather inputs ------------------------------------------------------

	gamePods, err := r.listGamePods(ctx, spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	status.GamePods = len(gamePods)

	vllm, err := r.getVLLM(ctx, spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	var currentReplicas *int32
	var vllmDown bool
	if vllm != nil {
		currentReplicas = vllm.Spec.Replicas
		vllmDown = vllmReplicasAreZero(vllm)
		status.CurrentVLLMReplicas = ptr.To(ptr.Deref(vllm.Spec.Replicas, 0))
	}

	// Free VRAM from DCGM via VictoriaMetrics. Treat query failure as -1
	// (unknown) so the vLLM/timeout signals still drive the decision.
	freeMiB, mErr := r.metricsFree(ctx, spec)
	if mErr != nil {
		logger.V(1).Info("vram metric query failed; relying on vllm/timeout", "err", mErr.Error())
		freeMiB = -1
	}
	status.FreeVRAMMiB = &freeMiB

	// --- 1. vLLM scaling ----------------------------------------------------

	desiredReplicas := busyReplicas
	if len(gamePods) == 0 {
		desiredReplicas = idleReplicas
	}
	status.DesiredVLLMReplicas = &desiredReplicas

	if vllm != nil {
		if cur := ptr.Deref(currentReplicas, 0); cur != desiredReplicas {
			if err := r.scaleVLLM(ctx, spec, desiredReplicas); err != nil {
				return ctrl.Result{}, err
			}
			messages = append(messages, fmt.Sprintf("scale %s %d->%d (gaming=%d)",
				spec.VLLM.Name, cur, desiredReplicas, len(gamePods)))
		}
	}

	// --- 2. Scheduling-gate removal ----------------------------------------

	status.GatedPods = r.reconcileGates(ctx, spec, gamePods, vllmDown, freeMiB, now, &messages)

	// --- Requeue ------------------------------------------------------------

	// Match the original bash loop: always re-poll on the configured interval.
	// This keeps the controller event-light (no cluster-wide pod watches, so RBAC
	// stays namespace-scoped like the original Roles) and guarantees we notice a
	// new gaming pod / VRAM change within IntervalSeconds. The cost when idle
	// is a handful of targeted GETs/LISTs every IntervalSeconds — the same work
	// the bash deployment did forever.
	return ctrl.Result{RequeueAfter: time.Duration(spec.IntervalSeconds) * time.Second}, nil
}

// reconcileGates evaluates the gate on every gaming pod and removes it once
// VRAM is free. It returns the (possibly updated) set of still-gated pods for
// status reporting.
func (r *GPUArbiterReconciler) reconcileGates(
	ctx context.Context,
	spec gpuv1alpha1.GPUArbiterSpec,
	gamePods []corev1.Pod,
	vllmDown bool,
	freeMiB int64,
	now time.Time,
	messages *[]string,
) []gpuv1alpha1.GatedPodStatus {
	logger := log.FromContext(ctx)
	var stillGated []gpuv1alpha1.GatedPodStatus

	for i := range gamePods {
		pod := &gamePods[i]
		if !podHasGate(pod, spec.GateName) {
			continue
		}

		age := now.Sub(pod.CreationTimestamp.Time)
		reason := evaluateGate(vllmDown, freeMiB, spec.FreeMiB, age, time.Duration(spec.TimeoutSeconds)*time.Second)

		switch reason.kind {
		case reasonUngate:
			if err := r.removeGate(ctx, pod); err != nil {
				logger.Error(err, "failed to ungate pod", "pod", client.ObjectKeyFromObject(pod))
				// Keep it in the gated set; will retry next reconcile.
				stillGated = append(stillGated, podStatus(pod, reason.String()))
				continue
			}
			*messages = append(*messages, fmt.Sprintf("ungate %s (%s)", pod.Name, reason.String()))
		case reasonWait:
			logger.V(1).Info("wait VRAM",
				"pod", pod.Name, "free", freeMiB, "threshold", spec.FreeMiB,
				"age", age.Truncate(time.Second), "vllmDown", vllmDown)
			stillGated = append(stillGated, podStatus(pod, reason.String()))
		}
	}

	// Deterministic order for stable status.
	slices.SortFunc(stillGated, func(a, b gpuv1alpha1.GatedPodStatus) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		default:
			return 0
		}
	})
	return stillGated
}

// listGamePods lists gaming-session pods matching the configured selector.
func (r *GPUArbiterReconciler) listGamePods(ctx context.Context, spec gpuv1alpha1.GPUArbiterSpec) ([]corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(&spec.Game.LabelSelector)
	if err != nil {
		return nil, fmt.Errorf("parse game labelSelector: %w", err)
	}
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(spec.Game.Namespace),
		client.MatchingLabelsSelector{Selector: sel},
	); err != nil {
		return nil, err
	}
	// Only count pods that are still consuming a slot. Terminal/failed pods
	// don't hold the GPU, so they shouldn't keep vLLM scaled to 0.
	podList.Items = filterActive(podList.Items)
	return podList.Items, nil
}

// getVLLM fetches the vLLM Deployment. A missing Deployment is non-fatal: the
// controller just can't scale it and logs the fact.
func (r *GPUArbiterReconciler) getVLLM(ctx context.Context, spec gpuv1alpha1.GPUArbiterSpec) (*appsv1.Deployment, error) {
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: spec.VLLM.Namespace, Name: spec.VLLM.Name}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &dep, nil
}

// scaleVLLM updates the vLLM Deployment's replica count via the scale
// subresource (deployments/scale), which is exactly what the operator's RBAC
// grants. Writing the Scale object sets spec.replicas to the target value
// rather than emitting a merge patch.
func (r *GPUArbiterReconciler) scaleVLLM(ctx context.Context, spec gpuv1alpha1.GPUArbiterSpec, replicas int32) error {
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Namespace: spec.VLLM.Namespace, Name: spec.VLLM.Name},
		Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: spec.VLLM.Namespace, Name: spec.VLLM.Name},
	}
	if err := r.SubResource("scale").Update(ctx, dep, client.WithSubResourceBody(scale)); err != nil {
		return fmt.Errorf("scale vllm: %w", err)
	}
	return nil
}

// removeGate clears the pod's schedulingGates, which removes the gate. Only the
// kubelet/scheduler may add gates; an arbiter may only remove them.
func (r *GPUArbiterReconciler) removeGate(ctx context.Context, pod *corev1.Pod) error {
	base := pod.DeepCopy()
	pod.Spec.SchedulingGates = nil
	if err := r.Patch(ctx, pod, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("remove scheduling gate from %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// metricsFree returns the DCGM free-VRAM gauge in MiB, or -1 if unknown.
func (r *GPUArbiterReconciler) metricsFree(ctx context.Context, spec gpuv1alpha1.GPUArbiterSpec) (int64, error) {
	if r.Metrics == nil {
		return -1, nil
	}
	to := time.Duration(spec.Metrics.TimeoutSeconds) * time.Second
	if to <= 0 {
		to = 4 * time.Second
	}
	free, ok, err := r.Metrics.FreeVRAM(ctx, spec.Metrics.URL, spec.Metrics.Query, to)
	if err != nil || !ok {
		return -1, err
	}
	return free, nil
}

// SetupWithManager wires the controller to the GPUArbiter CR.
//
// The controller is deliberately poll-driven (RequeueAfter = IntervalSeconds)
// rather than watching pods/deployments: the game and vllm namespaces are
// per-CR, and a cluster-wide watch would force broader RBAC than the original
// namespace-scoped Roles. Polling with a short interval matches the original
// bash loop's behaviour and keeps permissions minimal.
func (r *GPUArbiterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gpuv1alpha1.GPUArbiter{}).
		Complete(r)
}
