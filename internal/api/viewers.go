package api

import (
	"sync"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// ViewerRegistry tracks connected viewers for a single KVM session.
// Thread-safe; all methods may be called concurrently.
type ViewerRegistry struct {
	mu         sync.RWMutex
	viewers    map[string]*models.Viewer
	controller string   // viewer ID of current controller
	order      []string // connection order for auto-transfer
}

// NewViewerRegistry creates an empty viewer registry.
func NewViewerRegistry() *ViewerRegistry {
	return &ViewerRegistry{
		viewers: make(map[string]*models.Viewer),
	}
}

// Add registers a new viewer. The first viewer automatically receives input control.
func (vr *ViewerRegistry) Add(id, displayName, ip string) *models.Viewer {
	vr.mu.Lock()
	defer vr.mu.Unlock()

	v := &models.Viewer{
		ID:          id,
		DisplayName: displayName,
		IP:          ip,
		ConnectedAt: time.Now(),
	}

	vr.viewers[id] = v
	vr.order = append(vr.order, id)

	// First viewer gets control
	if vr.controller == "" {
		vr.controller = id
	}

	return v
}

// Remove unregisters a viewer. If the removed viewer was the controller,
// control is automatically transferred to the next viewer in connection order.
func (vr *ViewerRegistry) Remove(id string) {
	vr.mu.Lock()
	defer vr.mu.Unlock()

	delete(vr.viewers, id)

	// Remove from order slice
	for i, vid := range vr.order {
		if vid == id {
			vr.order = append(vr.order[:i], vr.order[i+1:]...)
			break
		}
	}

	// Auto-transfer control if the controller left
	if vr.controller == id {
		vr.controller = ""
		if len(vr.order) > 0 {
			vr.controller = vr.order[0]
		}
	}
}

// List returns copies of all viewers with HasControl set correctly.
func (vr *ViewerRegistry) List() []*models.Viewer {
	vr.mu.RLock()
	defer vr.mu.RUnlock()

	result := make([]*models.Viewer, 0, len(vr.viewers))
	for _, id := range vr.order {
		v, ok := vr.viewers[id]
		if !ok {
			continue
		}
		copy := &models.Viewer{
			ID:          v.ID,
			DisplayName: v.DisplayName,
			IP:          v.IP,
			HasControl:  id == vr.controller,
			ConnectedAt: v.ConnectedAt,
		}
		result = append(result, copy)
	}
	return result
}

// RequestControl grants input control to the specified viewer.
// Returns true if the viewer exists and control was granted.
func (vr *ViewerRegistry) RequestControl(id string) bool {
	vr.mu.Lock()
	defer vr.mu.Unlock()

	if _, ok := vr.viewers[id]; !ok {
		return false
	}
	vr.controller = id
	return true
}

// ReleaseControl releases input control from the specified viewer and
// passes it to the next viewer in connection order.
func (vr *ViewerRegistry) ReleaseControl(id string) {
	vr.mu.Lock()
	defer vr.mu.Unlock()

	if vr.controller != id {
		return
	}

	vr.controller = ""
	// Find the next viewer in order that is not the releasing viewer
	for _, vid := range vr.order {
		if vid != id {
			vr.controller = vid
			break
		}
	}
}

// HasControl returns true if the specified viewer currently has input control.
func (vr *ViewerRegistry) HasControl(id string) bool {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	return vr.controller == id
}

// Count returns the number of connected viewers.
func (vr *ViewerRegistry) Count() int {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	return len(vr.viewers)
}
