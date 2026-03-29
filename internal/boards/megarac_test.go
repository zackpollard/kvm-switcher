package boards

import (
	"testing"
)

func TestParseMegaRACMediaURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantIP    string
		wantPath  string
		wantFile  string
		wantType  int
		wantError bool
	}{
		{
			name:     "NFS URL",
			url:      "nfs://192.168.1.100/exports/images/ubuntu.iso",
			wantIP:   "192.168.1.100",
			wantPath: "/exports/images/",
			wantFile: "ubuntu.iso",
			wantType: 0,
		},
		{
			name:     "NFS URL with hostname",
			url:      "nfs://fileserver.local/share/os/debian-12.iso",
			wantIP:   "fileserver.local",
			wantPath: "/share/os/",
			wantFile: "debian-12.iso",
			wantType: 0,
		},
		{
			name:     "CIFS URL",
			url:      "cifs://10.0.0.5/images/windows/win11.iso",
			wantIP:   "10.0.0.5",
			wantPath: "/images/windows/",
			wantFile: "win11.iso",
			wantType: 1,
		},
		{
			name:     "SMB URL (alias for CIFS)",
			url:      "smb://nas/public/boot.iso",
			wantIP:   "nas",
			wantPath: "/public/",
			wantFile: "boot.iso",
			wantType: 1,
		},
		{
			name:     "NFS URL root path",
			url:      "nfs://server/image.iso",
			wantIP:   "server",
			wantPath: "/",
			wantFile: "image.iso",
			wantType: 0,
		},
		{
			name:      "HTTP URL rejected",
			url:       "http://example.com/image.iso",
			wantError: true,
		},
		{
			name:      "HTTPS URL rejected",
			url:       "https://example.com/image.iso",
			wantError: true,
		},
		{
			name:      "NFS URL without filename",
			url:       "nfs://server/path/",
			wantError: true,
		},
		{
			name:      "CIFS URL without filename",
			url:       "cifs://server/share/",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip, srcPath, imgName, shrType, err := parseMegaRACMediaURL(tt.url)
			if tt.wantError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ip != tt.wantIP {
				t.Errorf("ipAddr = %q, want %q", ip, tt.wantIP)
			}
			if srcPath != tt.wantPath {
				t.Errorf("srcPath = %q, want %q", srcPath, tt.wantPath)
			}
			if imgName != tt.wantFile {
				t.Errorf("imgName = %q, want %q", imgName, tt.wantFile)
			}
			if shrType != tt.wantType {
				t.Errorf("shrType = %d, want %d", shrType, tt.wantType)
			}
		})
	}
}

func TestMegaracCheckHAPIStatus(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantError bool
	}{
		{
			name:      "success status 0",
			body:      `WEBVAR_JSONVAR_SETMEDIAIMAGE = { HAPI_STATUS:0 };`,
			wantError: false,
		},
		{
			name:      "error status 1",
			body:      `WEBVAR_JSONVAR_SETMEDIAIMAGE = { HAPI_STATUS:1 };`,
			wantError: true,
		},
		{
			name:      "error status with spaces",
			body:      `WEBVAR_JSONVAR_FOO = { HAPI_STATUS : 5 };`,
			wantError: true,
		},
		{
			name:      "no HAPI_STATUS in response",
			body:      `some random response body`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := megaracCheckHAPIStatus(tt.body)
			if tt.wantError && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
