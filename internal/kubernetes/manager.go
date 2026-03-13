package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	containermgr "github.com/zackpollard/kvm-switcher/internal/container"
	"github.com/zackpollard/kvm-switcher/internal/models"
)

var _ containermgr.Manager = (*Manager)(nil)

// portForwardSession tracks an active port-forward to a pod.
type portForwardSession struct {
	stopChan  chan struct{}
	readyChan chan struct{}
}

// Manager handles Kubernetes pod lifecycle for KVM sessions.
type Manager struct {
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
	image      string
	namespace  string
	mu         sync.Mutex
	portAlloc  int
	forwards   map[string]*portForwardSession // pod name -> active forward
}

// NewManager creates a new Kubernetes manager.
func NewManager(image, namespace, kubeconfigPath string) (*Manager, error) {
	var restConfig *rest.Config
	var err error

	if kubeconfigPath != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		restConfig, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	// Verify connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("connecting to kubernetes: %w", err)
	}
	_ = ctx

	return &Manager{
		clientset:  clientset,
		restConfig: restConfig,
		image:      image,
		namespace:  namespace,
		portAlloc:  16900,
		forwards:   make(map[string]*portForwardSession),
	}, nil
}

// allocatePort returns the next available host port for port-forwarding.
func (m *Manager) allocatePort() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	port := m.portAlloc
	m.portAlloc++
	if m.portAlloc > 17999 {
		m.portAlloc = 16900
	}
	return port
}

// StartContainer creates a pod for the KVM session, waits for it to run,
// and sets up a port-forward so websockify is reachable on localhost.
func (m *Manager) StartContainer(ctx context.Context, session *models.KVMSession, args *models.JViewerArgs) (int, error) {
	hostPort := m.allocatePort()
	podName := fmt.Sprintf("kvm-switcher-%s", session.ID)

	env := []corev1.EnvVar{
		{Name: "BMC_HOST", Value: args.Hostname},
		{Name: "KVM_TOKEN", Value: args.KVMToken},
		{Name: "WEB_COOKIE", Value: args.WebCookie},
		{Name: "KVM_PORT", Value: args.KVMPort},
		{Name: "KVM_SECURE", Value: args.KVMSecure},
		{Name: "VM_SECURE", Value: args.VMSecure},
		{Name: "SINGLE_PORT", Value: args.SinglePortEnabled},
		{Name: "EXTENDED_PRIV", Value: args.ExtendedPriv},
		{Name: "OEM_FEATURES", Value: args.OEMFeatures},
		{Name: "CD_STATE", Value: args.CDState},
		{Name: "FD_STATE", Value: args.FDState},
		{Name: "HD_STATE", Value: args.HDState},
		{Name: "CD_NUM", Value: args.CDNum},
		{Name: "FD_NUM", Value: args.FDNum},
		{Name: "HD_NUM", Value: args.HDNum},
		{Name: "VNC_PORT", Value: "6080"},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.namespace,
			Labels: map[string]string{
				"kvm-switcher.session-id": session.ID,
				"kvm-switcher.server":     session.ServerName,
				"kvm-switcher.managed":    "true",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Force amd64 -- JViewer's native JNI libraries are x86_64 only
			NodeSelector: map[string]string{
				"kubernetes.io/arch": "amd64",
			},
			Containers: []corev1.Container{
				{
					Name:  "jviewer",
					Image: m.image,
					Env:   env,
					Ports: []corev1.ContainerPort{
						{
							Name:          "websockify",
							ContainerPort: 6080,
							Protocol:      corev1.ProtocolTCP,
						},
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("512Mi"),
							corev1.ResourceCPU:    resource.MustParse("1"),
						},
						Requests: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("256Mi"),
							corev1.ResourceCPU:    resource.MustParse("250m"),
						},
					},
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
			},
		},
	}

	created, err := m.clientset.CoreV1().Pods(m.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return 0, fmt.Errorf("creating pod: %w", err)
	}

	session.ContainerID = created.Name

	// Wait for pod to be running
	if err := m.waitForPodRunning(ctx, podName); err != nil {
		// Clean up on failure
		_ = m.clientset.CoreV1().Pods(m.namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		return 0, fmt.Errorf("waiting for pod to start: %w", err)
	}

	// Start port-forward
	if err := m.startPortForward(podName, hostPort, 6080); err != nil {
		_ = m.clientset.CoreV1().Pods(m.namespace).Delete(ctx, podName, metav1.DeleteOptions{})
		return 0, fmt.Errorf("starting port-forward: %w", err)
	}

	return hostPort, nil
}

// waitForPodRunning blocks until the pod reaches the Running phase or the context is cancelled.
func (m *Manager) waitForPodRunning(ctx context.Context, podName string) error {
	watcher, err := m.clientset.CoreV1().Pods(m.namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("metadata.name=%s", podName),
	})
	if err != nil {
		return fmt.Errorf("watching pod: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			if event.Type == watch.Deleted {
				return fmt.Errorf("pod was deleted")
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return fmt.Errorf("pod entered %s phase", pod.Status.Phase)
			}
		}
	}
}

// startPortForward establishes an in-process port-forward from localhost:localPort to pod:remotePort.
func (m *Manager) startPortForward(podName string, localPort, remotePort int) error {
	reqURL := m.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(m.namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(m.restConfig)
	if err != nil {
		return fmt.Errorf("creating round tripper: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	stopChan := make(chan struct{})
	readyChan := make(chan struct{})

	ports := []string{fmt.Sprintf("%d:%d", localPort, remotePort)}

	fw, err := portforward.New(dialer, ports, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("creating port forwarder: %w", err)
	}

	errChan := make(chan error, 1)
	go func() {
		errChan <- fw.ForwardPorts()
	}()

	select {
	case <-readyChan:
		// Port-forward is ready
	case err := <-errChan:
		return fmt.Errorf("port-forward failed: %w", err)
	case <-time.After(30 * time.Second):
		close(stopChan)
		return fmt.Errorf("port-forward timed out")
	}

	m.mu.Lock()
	m.forwards[podName] = &portForwardSession{
		stopChan:  stopChan,
		readyChan: readyChan,
	}
	m.mu.Unlock()

	// Log if port-forward exits unexpectedly
	go func() {
		if err := <-errChan; err != nil {
			log.Printf("Port-forward to pod %s exited: %v", podName, err)
		}
	}()

	return nil
}

// stopPortForward closes the port-forward for a pod.
func (m *Manager) stopPortForward(podName string) {
	m.mu.Lock()
	pf, ok := m.forwards[podName]
	if ok {
		delete(m.forwards, podName)
	}
	m.mu.Unlock()

	if ok {
		close(pf.stopChan)
	}
}

// StopContainer stops the port-forward and deletes the pod.
func (m *Manager) StopContainer(ctx context.Context, containerID string) error {
	m.stopPortForward(containerID)

	err := m.clientset.CoreV1().Pods(m.namespace).Delete(ctx, containerID, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("deleting pod %s: %w", containerID, err)
	}

	return nil
}

// IsContainerRunning checks if the pod is still running.
func (m *Manager) IsContainerRunning(ctx context.Context, containerID string) bool {
	pod, err := m.clientset.CoreV1().Pods(m.namespace).Get(ctx, containerID, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return pod.Status.Phase == corev1.PodRunning
}

// GetContainerLogs returns recent logs from the pod.
func (m *Manager) GetContainerLogs(ctx context.Context, containerID string) (string, error) {
	tailLines := int64(50)
	req := m.clientset.CoreV1().Pods(m.namespace).GetLogs(containerID, &corev1.PodLogOptions{
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("streaming logs: %w", err)
	}
	defer stream.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, stream); err != nil {
		return "", fmt.Errorf("reading logs: %w", err)
	}

	return buf.String(), nil
}

// CleanupOrphans removes any pods with kvm-switcher labels from previous runs.
func (m *Manager) CleanupOrphans(ctx context.Context) error {
	pods, err := m.clientset.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kvm-switcher.managed=true",
	})
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	for _, pod := range pods.Items {
		log.Printf("Cleaning up orphaned pod %s (%s)", pod.Name, pod.Labels["kvm-switcher.server"])
		_ = m.clientset.CoreV1().Pods(m.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
	}

	return nil
}

// Close stops all active port-forwards.
func (m *Manager) Close() error {
	m.mu.Lock()
	forwards := make(map[string]*portForwardSession, len(m.forwards))
	for k, v := range m.forwards {
		forwards[k] = v
	}
	m.forwards = make(map[string]*portForwardSession)
	m.mu.Unlock()

	for name, pf := range forwards {
		log.Printf("Closing port-forward to pod %s", name)
		close(pf.stopChan)
	}

	return nil
}
