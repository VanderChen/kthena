package plugins

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// HookRequest carries the context for plugin hook invocations.
type HookRequest struct {
	ModelServing *workloadv1alpha1.ModelServing
	ServingGroup string
	RoleName     string
	RoleID       string
	IsEntry      bool
	Pod          *corev1.Pod
}

// Plugin defines the lifecycle hooks a plugin may implement.
type Plugin interface {
	Name() string
	// OnPodCreate is invoked before the controller creates the Pod. Mutations are applied in-place to req.Pod.
	OnPodCreate(ctx context.Context, req *HookRequest) error
	// OnPodReady is invoked when the controller observes the Pod running and ready.
	OnPodReady(ctx context.Context, req *HookRequest) error
}
