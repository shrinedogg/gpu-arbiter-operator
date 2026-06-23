package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gpuv1alpha1 "github.com/biggs-dog/gpu-arbiter-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEvaluateGate(t *testing.T) {
	const threshold = int64(8000)
	timeout := 45 * time.Second

	tests := []struct {
		name     string
		vllmDown bool
		freeMiB  int64
		age      time.Duration
		want     gateDecisionKind
	}{
		{"vllm down ungates even below threshold", true, 518, 1 * time.Second, reasonUngate},
		{"free above threshold ungates", false, 9000, 1 * time.Second, reasonUngate},
		{"free equal threshold ungates", false, 8000, 1 * time.Second, reasonUngate},
		{"timeout fallback ungates on unknown metric", false, -1, 45 * time.Second, reasonUngate},
		{"timeout fallback ungates below threshold", false, 518, 46 * time.Second, reasonUngate},
		{"below threshold, not timed out, vllm up -> wait", false, 518, 5 * time.Second, reasonWait},
		{"unknown metric and not timed out -> wait", false, -1, 5 * time.Second, reasonWait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateGate(tc.vllmDown, tc.freeMiB, threshold, tc.age, timeout)
			if got.kind != tc.want {
				t.Fatalf("evaluateGate(%v,%d,%v) = %v, want %v (%s)",
					tc.vllmDown, tc.freeMiB, tc.age, got.kind, tc.want, got)
			}
		})
	}
}

func TestVLLMReplicasAreZero(t *testing.T) {
	tests := []struct {
		name string
		rep  int32
		want bool
	}{
		{"zero status replicas is down", 0, true},
		{"one status replica is up", 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dep := &appsv1.Deployment{Status: appsv1.DeploymentStatus{Replicas: tc.rep}}
			if got := vllmReplicasAreZero(dep); got != tc.want {
				t.Fatalf("vllmReplicasAreZero(%d) = %v, want %v", tc.rep, got, tc.want)
			}
		})
	}
}

func TestFilterActive(t *testing.T) {
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodPending}},
		{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
	}
	got := filterActive(pods)
	if len(got) != 2 {
		t.Fatalf("filterActive kept %d pods, want 2 (Pending+Running)", len(got))
	}
}

func TestWithDefaults(t *testing.T) {
	got := withDefaults(gpuv1alpha1.GPUArbiterSpec{})
	if got.GateName != defaultGateName {
		t.Errorf("GateName = %q, want %q", got.GateName, defaultGateName)
	}
	if got.FreeMiB != defaultFreeMiB {
		t.Errorf("FreeMiB = %d, want %d", got.FreeMiB, defaultFreeMiB)
	}
	if got.TimeoutSeconds != defaultTimeoutSeconds {
		t.Errorf("TimeoutSeconds = %d, want %d", got.TimeoutSeconds, defaultTimeoutSeconds)
	}
	if got.IntervalSeconds != defaultIntervalSeconds {
		t.Errorf("IntervalSeconds = %d, want %d", got.IntervalSeconds, defaultIntervalSeconds)
	}
	if got.Metrics.URL == "" || got.Metrics.Query == "" {
		t.Errorf("Metrics defaults not applied: %+v", got.Metrics)
	}
	// Explicit values are preserved.
	custom := withDefaults(gpuv1alpha1.GPUArbiterSpec{GateName: "x", FreeMiB: 1234, TimeoutSeconds: 9, IntervalSeconds: 7})
	if custom.GateName != "x" || custom.FreeMiB != 1234 || custom.TimeoutSeconds != 9 || custom.IntervalSeconds != 7 {
		t.Errorf("withDefaults overrode explicit values: %+v", custom)
	}
}

func TestVictoriaMetricsFreeVRAM(t *testing.T) {
	// A realistic VM instant-query response for max(DCGM_FI_DEV_FB_FREE).
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"instance":"gpu-node"},"value":[1700000000,"32119"]}]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.URL.Query().Get("query") == "" {
			t.Errorf("unexpected request: %s ?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	v := VictoriaMetrics{}
	got, ok, err := v.FreeVRAM(context.Background(), srv.URL, "max(DCGM_FI_DEV_FB_FREE)", 2*time.Second)
	if err != nil {
		t.Fatalf("FreeVRAM error: %v", err)
	}
	if !ok {
		t.Fatalf("FreeVRAM ok=false, want true")
	}
	if got != 32119 {
		t.Fatalf("FreeVRAM = %d, want 32119", got)
	}
}

func TestVictoriaMetricsFreeVRAMFloat(t *testing.T) {
	// VM may return a fractional scalar like "32119.4"; we truncate like bash.
	body := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{},"value":[1700000000,"32119.4"]}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, ok, err := VictoriaMetrics{}.FreeVRAM(context.Background(), srv.URL, "q", 2*time.Second)
	if err != nil || !ok || got != 32119 {
		t.Fatalf("FreeVRAM(float) got=%d ok=%v err=%v, want 32119/true/nil", got, ok, err)
	}
}

func TestVictoriaMetricsFreeVRAMEmpty(t *testing.T) {
	// No series -> unknown (-1 / ok=false), not an error.
	body := `{"status":"success","data":{"resultType":"vector","result":[]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, ok, err := VictoriaMetrics{}.FreeVRAM(context.Background(), srv.URL, "q", 2*time.Second)
	if err != nil || ok || got != 0 {
		t.Fatalf("FreeVRAM(empty) got=%d ok=%v err=%v, want 0/false/nil", got, ok, err)
	}
}

// TestPodHasGate ensures we detect the gate regardless of position.
func TestPodHasGate(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{
		{Name: "other-gate"},
		{Name: "gpu.biggs.dog/await-vram"},
	}}}
	if !podHasGate(pod, "gpu.biggs.dog/await-vram") {
		t.Fatal("podHasGate failed to find the await-vram gate")
	}
	if podHasGate(pod, "missing") {
		t.Fatal("podHasGate found a gate that isn't present")
	}
}

func TestStatusMeaningfullyChanged(t *testing.T) {
	base := gpuv1alpha1.GPUArbiterStatus{GamePods: 1, Message: "x"}
	// LastReconcile alone must NOT count as a change.
	withTime := base
	withTime.LastReconcile = metav1Now()
	if statusMeaningfullyChanged(base, withTime) {
		t.Fatal("LastReconcile-only change should not be considered meaningful")
	}
	// A change to a watched field counts.
	changed := base
	changed.GamePods = 2
	if !statusMeaningfullyChanged(base, changed) {
		t.Fatal("GamePods change should be considered meaningful")
	}
	changed = base
	changed.Message = "y"
	if !statusMeaningfullyChanged(base, changed) {
		t.Fatal("Message change should be considered meaningful")
	}
	changed = base
	changed.FreeVRAMMiB = ptrInt64(5)
	if !statusMeaningfullyChanged(base, changed) {
		t.Fatal("FreeVRAMMiB change should be considered meaningful")
	}
}

func metav1Now() metav1.Time  { return metav1.NewTime(time.Now()) }
func ptrInt64(v int64) *int64 { return &v }
