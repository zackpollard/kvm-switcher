package boards

import "time"

// NanoKVMVersionCacheState holds the state of the NanoKVM version cache for test save/restore.
type NanoKVMVersionCacheState struct {
	App       string
	Image     string
	CheckTime time.Time
	URL       string
}

// SaveNanoKVMVersionCache saves the current NanoKVM version cache state and returns it.
// This is intended for use in tests.
func SaveNanoKVMVersionCache() NanoKVMVersionCacheState {
	nanoKVMLatestMu.Lock()
	defer nanoKVMLatestMu.Unlock()
	return NanoKVMVersionCacheState{
		App:       nanoKVMLatestApp,
		Image:     nanoKVMLatestImage,
		CheckTime: nanoKVMLatestCheckTime,
		URL:       nanoKVMReleasesURL,
	}
}

// RestoreNanoKVMVersionCache restores the NanoKVM version cache to a previous state.
func RestoreNanoKVMVersionCache(state NanoKVMVersionCacheState) {
	nanoKVMLatestMu.Lock()
	defer nanoKVMLatestMu.Unlock()
	nanoKVMLatestApp = state.App
	nanoKVMLatestImage = state.Image
	nanoKVMLatestCheckTime = state.CheckTime
	nanoKVMReleasesURL = state.URL
}

// ResetNanoKVMVersionCache clears the cache and sets a custom releases URL.
func ResetNanoKVMVersionCache(url string) {
	nanoKVMLatestMu.Lock()
	defer nanoKVMLatestMu.Unlock()
	nanoKVMLatestApp = ""
	nanoKVMLatestImage = ""
	nanoKVMLatestCheckTime = time.Time{}
	nanoKVMReleasesURL = url
}

// ExpireNanoKVMVersionCache sets the cache check time to the past to force a re-fetch.
func ExpireNanoKVMVersionCache() {
	nanoKVMLatestMu.Lock()
	defer nanoKVMLatestMu.Unlock()
	nanoKVMLatestCheckTime = time.Now().Add(-2 * time.Hour)
}

// NanoKVMVersions is the exported version of nanoKVMVersions for testing.
type NanoKVMVersions = nanoKVMVersions

// GetNanoKVMLatestVersions is the exported test helper for getNanoKVMLatestVersions.
func GetNanoKVMLatestVersions() NanoKVMVersions {
	return getNanoKVMLatestVersions()
}
