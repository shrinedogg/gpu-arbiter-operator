// Package controller contains the GPUArbiter reconciler and its helpers.
//
// RBAC markers are package-level in controller-tools (DescribesPackage), so
// they live here rather than on the reconciler struct.
//
// +kubebuilder:rbac:groups=gpu.biggs.dog,resources=gpuarbiters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gpu.biggs.dog,resources=gpuarbiters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gpu.biggs.dog,resources=gpuarbiters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments/scale,verbs=get;update;patch
package controller
