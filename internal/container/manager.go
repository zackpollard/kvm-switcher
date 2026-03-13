package container

import (
	"context"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// Manager defines the interface for container runtimes that host KVM sessions.
// Both Docker and Kubernetes implementations satisfy this interface.
type Manager interface {
	// StartContainer launches a KVM session container and returns the local
	// port where websockify is reachable. Sets session.ContainerID as a side effect.
	StartContainer(ctx context.Context, session *models.KVMSession, args *models.JViewerArgs) (int, error)

	// StopContainer stops and removes a container by ID (Docker container ID or K8s pod name).
	StopContainer(ctx context.Context, containerID string) error

	// IsContainerRunning checks if the container is still alive.
	IsContainerRunning(ctx context.Context, containerID string) bool

	// GetContainerLogs returns recent logs from the container.
	GetContainerLogs(ctx context.Context, containerID string) (string, error)

	// CleanupOrphans removes any containers with kvm-switcher labels from previous runs.
	CleanupOrphans(ctx context.Context) error

	// Close releases any resources held by the manager.
	Close() error
}
