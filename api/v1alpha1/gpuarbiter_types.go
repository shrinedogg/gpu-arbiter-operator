package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GPUArbiterSpec defines the desired state of GPUArbiter.
type GPUArbiterSpec struct {
	// VLLM is the inference Deployment to scale down while a game is running
	// (and back up when idle), so the GPU VRAM it holds is released.
	VLLM VLLMTarget `json:"vllm"`

	// Game describes the gaming-session pods whose presence triggers scale-down.
	Game GameTarget `json:"game"`

	// GateName is the scheduling gate that the MutatingAdmissionPolicy injects
	// at pod CREATE. The arbiter removes it once VRAM is confirmed free.
	//
	//+kubebuilder:default="gpu.biggs.dog/await-vram"
	GateName string `json:"gateName,omitempty"`

	// Metrics is the VictoriaMetrics (Prometheus-compatible) endpoint polled
	// for the DCGM free-VRAM gauge as a secondary ungate signal.
	Metrics MetricsSource `json:"metrics,omitempty"`

	// FreeMiB is the DCGM_FI_DEV_FB_FREE threshold (in MiB) at which the GPU
	// is considered to have enough free VRAM to ungate. With vLLM up, free is
	// ~518 MiB; after scale-to-0 it jumps to ~32000 MiB.
	//
	//+kubebuilder:default=8000
	FreeMiB int64 `json:"freeMiB,omitempty"`

	// TimeoutSeconds is a safety fallback: a gated pod is ungate anyway after
	// this many seconds, so a stuck metric degrades to the old software-encode
	// path instead of 401-ing the game session. Kept under direwolf's 60s
	// session reaper.
	//
	//+kubebuilder:default=45
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`

	// IntervalSeconds is how often the controller re-checks non-terminal state
	// (e.g. the VRAM metric while waiting to ungate). Terminal k8s events
	// (pod/vllm changes) trigger an immediate reconcile, so this only governs
	// the periodic metric re-poll.
	//
	//+kubebuilder:default=2
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
}

// VLLMTarget identifies the Deployment to scale.
type VLLMTarget struct {
	// Namespace where the vLLM Deployment lives.
	Namespace string `json:"namespace"`
	// Name of the vLLM Deployment.
	Name string `json:"name"`
}

// GameTarget identifies the gaming-session pods.
type GameTarget struct {
	// Namespace where gaming-session pods live.
	Namespace string `json:"namespace"`
	// LabelSelector selects gaming-session pods. Equivalent to -l on kubectl.
	LabelSelector metav1.LabelSelector `json:"labelSelector"`
}

// MetricsSource configures the VictoriaMetrics query used as the secondary
// ungate signal.
type MetricsSource struct {
	// URL is the base VictoriaMetrics / Prometheus URL, e.g.
	// http://vmsingle-stack.observability.svc:8428
	//
	//+kubebuilder:default="http://vmsingle-stack.observability.svc:8428"
	URL string `json:"url,omitempty"`
	// Query is the instant PromQL query evaluated for the free-VRAM gauge.
	//
	//+kubebuilder:default="max(DCGM_FI_DEV_FB_FREE)"
	Query string `json:"query,omitempty"`
	// TimeoutSeconds for the HTTP query. Defaults to 4.
	//
	//+kubebuilder:default=4
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// GatedPodStatus is the arbiter's view of one gated pod.
type GatedPodStatus struct {
	// Name of the pod.
	Name string `json:"name"`
	// FirstSeen is the time the gate was first observed on this pod.
	FirstSeen metav1.Time `json:"firstSeen"`
	// Reason is the most recent ungate evaluation ("wait", "vllm-down", etc).
	Reason string `json:"reason"`
}

// GPUArbiterStatus defines the observed state of GPUArbiter.
type GPUArbiterStatus struct {
	// Observed generation of the spec the controller has acted on.
	//+optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// GamePods is the number of currently-running gaming-session pods.
	//+optional
	GamePods int `json:"gamePods,omitempty"`

	// CurrentVLLMReplicas is the vLLM Deployment's last observed spec.replicas.
	//+optional
	CurrentVLLMReplicas *int32 `json:"currentVLLMReplicas,omitempty"`
	// DesiredVLLMReplicas is what the arbiter most recently wanted vLLM at.
	//+optional
	DesiredVLLMReplicas *int32 `json:"desiredVLLMReplicas,omitempty"`

	// FreeVRAMMiB is the last DCGM free-VRAM sample (MiB), or -1 if unknown.
	//+optional
	FreeVRAMMiB *int64 `json:"freeVRAMMiB,omitempty"`

	// GatedPods is the set of pods currently held by the await-vram gate.
	//+optional
	//+listType=map
	//+listMapKey=name
	GatedPods []GatedPodStatus `json:"gatedPods,omitempty"`

	// LastReconcile is the time of the last reconcile pass.
	//+optional
	LastReconcile metav1.Time `json:"lastReconcile,omitempty"`
	// Message is a human-readable summary of the last decision (mirrors the
	// old bash log lines, e.g. "scale vllm 1->0 (gaming=1)").
	//+optional
	Message string `json:"message,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:path=gpuarbiters,shortName=gpa;gpuarbiter,scope=Cluster

// GPUArbiter arbitrates access to a shared GPU between a vLLM inference
// Deployment and transient gaming-session pods.
//
// It (1) scales vLLM to 0 while a game is running and back to 1 when idle, and
// (2) removes an injected scheduling gate once VRAM is confirmed free, with a
// bounded timeout fallback.
type GPUArbiter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUArbiterSpec   `json:"spec,omitempty"`
	Status GPUArbiterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GPUArbiterList contains a list of GPUArbiter.
type GPUArbiterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUArbiter `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUArbiter{}, &GPUArbiterList{})
}
