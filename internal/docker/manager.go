package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	containermgr "github.com/zackpollard/kvm-switcher/internal/container"
	"github.com/zackpollard/kvm-switcher/internal/models"
)

var _ containermgr.Manager = (*Manager)(nil)

// Manager handles Docker container lifecycle for KVM sessions.
type Manager struct {
	client    *client.Client
	image     string
	mu        sync.Mutex
	portAlloc int // next port to allocate
}

// NewManager creates a new Docker manager.
func NewManager(image string) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to docker daemon: %w", err)
	}

	return &Manager{
		client:    cli,
		image:     image,
		portAlloc: 16900,
	}, nil
}

// allocatePort returns the next available host port for WebSocket.
// It probes with net.Listen to skip ports already in use.
func (m *Manager) allocatePort() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	start := m.portAlloc
	for {
		port := m.portAlloc
		m.portAlloc++
		if m.portAlloc > 17999 {
			m.portAlloc = 16900
		}

		// Probe to ensure port is free
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return port, nil
		}

		// Wrapped all the way around — no free ports
		if m.portAlloc == start {
			return 0, fmt.Errorf("no free ports in range 16900-17999")
		}
	}
}

// StartContainer launches a JViewer Docker container for a KVM session.
func (m *Manager) StartContainer(ctx context.Context, session *models.KVMSession, args *models.JViewerArgs) (int, error) {
	hostPort, err := m.allocatePort()
	if err != nil {
		return 0, fmt.Errorf("allocating port: %w", err)
	}

	env := []string{
		"BMC_HOST=" + args.Hostname,
		"KVM_TOKEN=" + args.KVMToken,
		"WEB_COOKIE=" + args.WebCookie,
		"KVM_PORT=" + args.KVMPort,
		"KVM_SECURE=" + args.KVMSecure,
		"VM_SECURE=" + args.VMSecure,
		"SINGLE_PORT=" + args.SinglePortEnabled,
		"EXTENDED_PRIV=" + args.ExtendedPriv,
		"OEM_FEATURES=" + args.OEMFeatures,
		"CD_STATE=" + args.CDState,
		"FD_STATE=" + args.FDState,
		"HD_STATE=" + args.HDState,
		"CD_NUM=" + args.CDNum,
		"FD_NUM=" + args.FDNum,
		"HD_NUM=" + args.HDNum,
		fmt.Sprintf("VNC_PORT=%d", 6080),
	}

	containerConfig := &container.Config{
		Image: m.image,
		Env:   env,
		ExposedPorts: nat.PortSet{
			"6080/tcp": struct{}{},
		},
		Labels: map[string]string{
			"kvm-switcher.session-id": session.ID,
			"kvm-switcher.server":     session.ServerName,
			"kvm-switcher.managed":    "true",
		},
	}

	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			"6080/tcp": []nat.PortBinding{
				{
					HostIP:   "127.0.0.1",
					HostPort: fmt.Sprintf("%d", hostPort),
				},
			},
		},
		Resources: container.Resources{
			Memory:   512 * 1024 * 1024, // 512MB
			NanoCPUs: 1000000000,        // 1 CPU
		},
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyDisabled,
		},
	}

	networkConfig := &network.NetworkingConfig{}

	// Force linux/amd64 platform -- JViewer's native JNI libraries (.so files)
	// from the BMC are compiled for x86_64 only
	platform := &ocispec.Platform{
		OS:           "linux",
		Architecture: "amd64",
	}

	containerName := fmt.Sprintf("kvm-switcher-%s", session.ID)

	resp, err := m.client.ContainerCreate(ctx, containerConfig, hostConfig, networkConfig, platform, containerName)
	if err != nil {
		return 0, fmt.Errorf("creating container: %w", err)
	}

	session.ContainerID = resp.ID

	if err := m.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up created container on start failure
		_ = m.client.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return 0, fmt.Errorf("starting container: %w", err)
	}

	return hostPort, nil
}

// StopContainer stops and removes a KVM session container.
func (m *Manager) StopContainer(ctx context.Context, containerID string) error {
	timeout := 10
	stopOptions := container.StopOptions{Timeout: &timeout}

	if err := m.client.ContainerStop(ctx, containerID, stopOptions); err != nil {
		log.Printf("Warning: failed to stop container %s: %v", containerID[:12], err)
	}

	if err := m.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("removing container %s: %w", containerID[:12], err)
	}

	return nil
}

// IsContainerRunning checks if a container is still running.
func (m *Manager) IsContainerRunning(ctx context.Context, containerID string) bool {
	inspect, err := m.client.ContainerInspect(ctx, containerID)
	if err != nil {
		return false
	}
	return inspect.State.Running
}

// GetContainerLogs returns the logs from a container.
func (m *Manager) GetContainerLogs(ctx context.Context, containerID string) (string, error) {
	reader, err := m.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "50",
	})
	if err != nil {
		return "", err
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// CleanupOrphans removes any containers with kvm-switcher labels that shouldn't be running.
func (m *Manager) CleanupOrphans(ctx context.Context) error {
	// List all containers with our label
	containers, err := m.client.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}

	for _, c := range containers {
		if c.Labels["kvm-switcher.managed"] == "true" {
			log.Printf("Cleaning up orphaned container %s (%s)", c.ID[:12], c.Labels["kvm-switcher.server"])
			_ = m.client.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true})
		}
	}

	return nil
}

// Close closes the Docker client connection.
func (m *Manager) Close() error {
	return m.client.Close()
}
