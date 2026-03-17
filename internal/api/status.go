package api

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/boards"
	"github.com/zackpollard/kvm-switcher/internal/models"
)

// DeviceStatus is an alias for models.DeviceStatus for backward compatibility within this package.
type DeviceStatus = models.DeviceStatus

// StatusCache stores device status per server with thread-safe access.
type StatusCache struct {
	mu      sync.RWMutex
	entries map[string]*DeviceStatus // server name -> status
}

// NewStatusCache creates a new StatusCache.
func NewStatusCache() *StatusCache {
	return &StatusCache{
		entries: make(map[string]*DeviceStatus),
	}
}

// Get returns the cached status for a server by name.
func (sc *StatusCache) Get(name string) (*DeviceStatus, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	s, ok := sc.entries[name]
	return s, ok
}

// Set stores the status for a server by name.
func (sc *StatusCache) Set(name string, status *DeviceStatus) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.entries[name] = status
}

// GetAll returns a copy of all cached statuses.
func (sc *StatusCache) GetAll() map[string]*DeviceStatus {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	result := make(map[string]*DeviceStatus, len(sc.entries))
	for k, v := range sc.entries {
		cp := *v
		result[k] = &cp
	}
	return result
}

// GetAllProxyEntries iterates the bmcProxies sync.Map and returns all entries.
func GetAllProxyEntries() map[string]*bmcProxyEntry {
	result := make(map[string]*bmcProxyEntry)
	bmcProxies.Range(func(key, value any) bool {
		result[key.(string)] = value.(*bmcProxyEntry)
		return true
	})
	return result
}

// bmcBaseURL returns the base URL for a BMC, handling default ports.
func bmcBaseURL(boardType, bmcIP string, bmcPort int) string {
	return boards.BMCBaseURL(boardType, bmcIP, bmcPort)
}

// checkDeviceOnline performs a simple HTTP HEAD request to see if a BMC is reachable.
func checkDeviceOnline(bmcIP string, bmcPort int, boardType string) bool {
	url := bmcBaseURL(boardType, bmcIP, bmcPort) + "/"

	client := boards.NewStatusHTTPClient(3*time.Second, true)
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// fetchDeviceStatus fetches the status for a single server, dispatching to the
// appropriate board handler.
func fetchDeviceStatus(cfg *models.ServerConfig) *DeviceStatus {
	// Check if we have cached BMC credentials from a proxy entry
	entries := GetAllProxyEntries()
	entry, hasEntry := entries[cfg.Name]

	var creds *models.BMCCredentials
	if hasEntry {
		creds = entry.getBMCCredentials()
	}

	handler, hasHandler := boards.Get(cfg.BoardType)

	// Dispatch to board handler (it decides how to handle nil creds)
	if hasHandler {
		if status := handler.FetchStatus(cfg, creds); status != nil {
			return status
		}
	}

	// No handler or handler returned nil (needs creds) — just check reachability
	online := checkDeviceOnline(cfg.BMCIP, cfg.BMCPort, cfg.BoardType)
	return &DeviceStatus{Online: online}
}

// PollStatuses fetches status for all configured servers in parallel and updates the cache.
func PollStatuses(servers []models.ServerConfig, cache *StatusCache) {
	var wg sync.WaitGroup
	for i := range servers {
		wg.Add(1)
		go func(cfg *models.ServerConfig) {
			defer wg.Done()
			// Hard deadline per server to prevent a hung connection
			// from blocking the entire poll cycle.
			done := make(chan *DeviceStatus, 1)
			go func() {
				done <- fetchDeviceStatus(cfg)
			}()
			select {
			case status := <-done:
				cache.Set(cfg.Name, status)
			case <-time.After(30 * time.Second):
				cache.Set(cfg.Name, &DeviceStatus{Online: false})
			}
		}(&servers[i])
	}
	wg.Wait()
}

// StartStatusPoller starts a background goroutine that polls all server statuses
// every 30 seconds. It runs an initial poll immediately.
func StartStatusPoller(servers []models.ServerConfig, cache *StatusCache) {
	go func() {
		log.Printf("Status poller: starting initial poll for %d servers", len(servers))
		PollStatuses(servers, cache)
		log.Printf("Status poller: initial poll complete")

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			log.Printf("Status poller: tick, polling %d servers", len(servers))
			PollStatuses(servers, cache)
			log.Printf("Status poller: tick complete")
		}
	}()
}
