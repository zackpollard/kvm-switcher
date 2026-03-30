package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/models"
	kvmoidc "github.com/zackpollard/kvm-switcher/internal/oidc"
)

var validISOFilename = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// sanitizeISOFilename validates an ISO filename for safety.
func sanitizeISOFilename(name string) error {
	if name == "" {
		return fmt.Errorf("filename is required")
	}
	if len(name) > 255 {
		return fmt.Errorf("filename too long (max 255 characters)")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("filename must not contain '..'")
	}
	if !validISOFilename.MatchString(name) {
		return fmt.Errorf("filename contains invalid characters (only a-z, A-Z, 0-9, '.', '_', '-' allowed)")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".iso") {
		return fmt.Errorf("filename must end with .iso")
	}
	return nil
}

// ListISOs godoc
// @Summary List ISO files
// @Description Returns all ISO files in the local library with total and max size information.
// @Tags iso
// @Produce json
// @Success 200 {object} object "ISO file listing"
// @Router /api/isos [get]
func (s *Server) ListISOs(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		writeError(w, http.StatusInternalServerError, "database not available")
		return
	}

	isos, err := s.DB.ListISOs()
	if err != nil {
		log.Printf("ListISOs: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list ISOs")
		return
	}

	var totalSize int64
	for _, iso := range isos {
		totalSize += iso.SizeBytes
	}

	maxSizeBytes := int64(s.Config.Settings.ISOMaxSizeGB) * 1024 * 1024 * 1024

	writeJSON(w, http.StatusOK, map[string]any{
		"isos":            isos,
		"total_size_bytes": totalSize,
		"max_size_bytes":   maxSizeBytes,
	})
}

// UploadISO godoc
// @Summary Upload an ISO file
// @Description Uploads an ISO file to the local library. Streams to disk with SHA256 computation.
// @Tags iso
// @Accept multipart/form-data
// @Produce json
// @Param file formance file true "ISO file to upload"
// @Success 201 {object} models.ISOFile "Uploaded ISO metadata"
// @Failure 400 {object} models.ErrorResponse "Invalid filename or request"
// @Failure 409 {object} models.ErrorResponse "File already exists"
// @Failure 413 {object} models.ErrorResponse "File exceeds size limit"
// @Failure 500 {object} models.ErrorResponse "Upload failed"
// @Router /api/isos [post]
func (s *Server) UploadISO(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		writeError(w, http.StatusInternalServerError, "database not available")
		return
	}

	// Disable ReadTimeout for this long-running upload
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Time{})

	// Parse multipart form — limit to configured max + 1MB overhead for headers
	maxBytes := int64(s.Config.Settings.ISOMaxSizeGB)*1024*1024*1024 + 1<<20
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	// Use a small memory buffer; the rest goes to disk via multipart's temp files
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' field in multipart form")
		return
	}
	defer file.Close()

	filename := header.Filename
	if err := sanitizeISOFilename(filename); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Check if file already exists in DB
	existing, err := s.DB.GetISO(filename)
	if err != nil {
		log.Printf("UploadISO: DB check failed: %v", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("ISO %q already exists", filename))
		return
	}

	// Check total size limit
	isos, err := s.DB.ListISOs()
	if err != nil {
		log.Printf("UploadISO: list ISOs failed: %v", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	var totalSize int64
	for _, iso := range isos {
		totalSize += iso.SizeBytes
	}
	maxTotalBytes := int64(s.Config.Settings.ISOMaxSizeGB) * 1024 * 1024 * 1024
	if header.Size > 0 && totalSize+header.Size > maxTotalBytes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload would exceed total ISO storage limit of %d GB", s.Config.Settings.ISOMaxSizeGB))
		return
	}

	// Stream to disk with SHA256 computation
	isoDir := s.Config.Settings.ISODir
	destPath := filepath.Join(isoDir, filename)

	// Atomic file creation — fails if file already exists on disk
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			writeError(w, http.StatusConflict, fmt.Sprintf("ISO file %q already exists on disk", filename))
			return
		}
		log.Printf("UploadISO: create file failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create file")
		return
	}

	hasher := sha256.New()
	reader := io.TeeReader(file, hasher)

	written, err := io.Copy(out, reader)
	if closeErr := out.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(destPath) // clean up partial file
		log.Printf("UploadISO: write failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to write file")
		return
	}

	sha256sum := hex.EncodeToString(hasher.Sum(nil))

	// Check size limit again with actual written bytes
	if totalSize+written > maxTotalBytes {
		os.Remove(destPath)
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload exceeds total ISO storage limit of %d GB", s.Config.Settings.ISOMaxSizeGB))
		return
	}

	// Determine uploader email from OIDC context if available
	var uploadedBy string
	if user := userFromRequest(r); user != nil {
		uploadedBy = user.Email
	}

	// Insert metadata into DB
	if err := s.DB.InsertISO(filename, written, sha256sum, uploadedBy); err != nil {
		os.Remove(destPath) // clean up on DB failure
		log.Printf("UploadISO: DB insert failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to record ISO metadata")
		return
	}

	// Fetch the complete record back
	isoFile, err := s.DB.GetISO(filename)
	if err != nil || isoFile == nil {
		log.Printf("UploadISO: failed to read back ISO record: %v", err)
		// File and DB entry exist, just return a basic response
		writeJSON(w, http.StatusCreated, map[string]any{
			"filename":   filename,
			"size_bytes": written,
			"sha256":     sha256sum,
		})
		return
	}

	// Audit log
	s.logAudit("iso.upload", uploadedBy, "", "", r.RemoteAddr, map[string]any{
		"filename":   filename,
		"size_bytes": written,
		"sha256":     sha256sum,
	})

	log.Printf("ISO uploaded: %s (%d bytes, sha256=%s)", filename, written, sha256sum)
	writeJSON(w, http.StatusCreated, isoFile)
}

// DeleteISO godoc
// @Summary Delete an ISO file
// @Description Removes an ISO file from the local library (disk and database).
// @Tags iso
// @Produce json
// @Param name path string true "ISO filename"
// @Success 200 {object} models.StatusOkResponse "ISO deleted"
// @Failure 404 {object} models.ErrorResponse "ISO not found"
// @Failure 500 {object} models.ErrorResponse "Delete failed"
// @Router /api/isos/{name} [delete]
func (s *Server) DeleteISO(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		writeError(w, http.StatusInternalServerError, "database not available")
		return
	}

	filename := r.PathValue("name")
	if err := sanitizeISOFilename(filename); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Check file exists in DB
	existing, err := s.DB.GetISO(filename)
	if err != nil {
		log.Printf("DeleteISO: DB lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("ISO %q not found", filename))
		return
	}

	// Remove file from disk
	destPath := filepath.Join(s.Config.Settings.ISODir, filename)
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		log.Printf("DeleteISO: remove file failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to remove file from disk")
		return
	}

	// Remove from DB
	if err := s.DB.DeleteISO(filename); err != nil {
		log.Printf("DeleteISO: DB delete failed: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to remove ISO from database")
		return
	}

	// Audit log
	var userEmail string
	if user := userFromRequest(r); user != nil {
		userEmail = user.Email
	}
	s.logAudit("iso.delete", userEmail, "", "", r.RemoteAddr, map[string]any{
		"filename": filename,
	})

	log.Printf("ISO deleted: %s", filename)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// DownloadISO godoc
// @Summary Download an ISO file
// @Description Downloads an ISO file from the local library. Supports range requests.
// @Tags iso
// @Produce application/octet-stream
// @Param name path string true "ISO filename"
// @Success 200 {file} binary "ISO file content"
// @Failure 404 {object} models.ErrorResponse "ISO not found"
// @Router /api/isos/{name}/download [get]
func (s *Server) DownloadISO(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		writeError(w, http.StatusInternalServerError, "database not available")
		return
	}

	filename := r.PathValue("name")
	if err := sanitizeISOFilename(filename); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate file exists in DB
	existing, err := s.DB.GetISO(filename)
	if err != nil {
		log.Printf("DownloadISO: DB lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("ISO %q not found", filename))
		return
	}

	destPath := filepath.Join(s.Config.Settings.ISODir, filename)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	http.ServeFile(w, r, destPath)
}

// MountLocalISO godoc
// @Summary Mount a local ISO from the library
// @Description Mounts an ISO file from the local library to a BMC via virtual media.
// @Tags iso,virtual-media
// @Accept json
// @Produce json
// @Param id path string true "Session ID"
// @Param request body models.MountLocalISORequest true "ISO filename"
// @Success 200 {object} models.StatusOkResponse "ISO mounted"
// @Failure 400 {object} models.ErrorResponse "Invalid request"
// @Failure 404 {object} models.ErrorResponse "Session or ISO not found"
// @Failure 500 {object} models.ErrorResponse "Mount failed"
// @Router /api/sessions/{id}/virtual-media/mount-local [post]
func (s *Server) MountLocalISO(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		writeError(w, http.StatusInternalServerError, "database not available")
		return
	}

	id := r.PathValue("id")

	var req models.MountLocalISORequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := sanitizeISOFilename(req.Filename); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate ISO exists in DB
	isoFile, err := s.DB.GetISO(req.Filename)
	if err != nil {
		log.Printf("MountLocalISO: DB lookup failed: %v", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if isoFile == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("ISO %q not found in library", req.Filename))
		return
	}

	// Resolve session -> server config -> board handler (reuse resolveVirtualMedia pattern)
	serverCfg, creds, vmHandler := s.resolveVirtualMedia(w, r)
	if vmHandler == nil {
		return // error already written
	}

	// Generate URL based on board type
	serveAddr := s.Config.Settings.ISOServeAddress
	if serveAddr == "" {
		writeError(w, http.StatusInternalServerError, "iso_serve_address is not configured")
		return
	}

	var mediaURL string
	switch serverCfg.BoardType {
	case "ami_megarac":
		// MegaRAC uses NFS: nfs://<serve_address><abs_iso_dir>/<filename>
		absISODir, err := filepath.Abs(s.Config.Settings.ISODir)
		if err != nil {
			log.Printf("MountLocalISO: failed to resolve ISO dir: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to resolve ISO directory")
			return
		}
		// Ensure path ends with /
		if !strings.HasSuffix(absISODir, "/") {
			absISODir += "/"
		}
		mediaURL = fmt.Sprintf("nfs://%s%s%s", serveAddr, absISODir, req.Filename)

	case "dell_idrac8", "dell_idrac9":
		// iDRAC uses HTTP: http://<serve_address>:<listen_port>/iso/<filename>
		_, port, _ := strings.Cut(s.Config.Settings.ListenAddress, ":")
		if port == "" {
			port = "8080"
		}
		mediaURL = fmt.Sprintf("http://%s:%s/iso/%s", serveAddr, port, req.Filename)

	default:
		// For other board types, try HTTP as default
		_, port, _ := strings.Cut(s.Config.Settings.ListenAddress, ":")
		if port == "" {
			port = "8080"
		}
		mediaURL = fmt.Sprintf("http://%s:%s/iso/%s", serveAddr, port, req.Filename)
	}

	// Mount via virtual media handler
	if err := vmHandler.MountMedia(serverCfg, creds, mediaURL); err != nil {
		log.Printf("MountLocalISO: mount failed: %v", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("mount failed: %v", err))
		return
	}

	// Update last_used timestamp
	if err := s.DB.TouchISOLastUsed(req.Filename); err != nil {
		log.Printf("MountLocalISO: failed to update last_used: %v", err)
	}

	// Audit log
	var userEmail string
	if user := userFromRequest(r); user != nil {
		userEmail = user.Email
	}
	session, _ := s.Sessions.Get(id)
	serverName := ""
	if session != nil {
		serverName = session.ServerName
	}
	s.logAudit("iso.mount", userEmail, serverName, id, r.RemoteAddr, map[string]any{
		"filename":  req.Filename,
		"media_url": mediaURL,
	})

	log.Printf("Local ISO mounted: %s via %s", req.Filename, mediaURL)
	writeJSON(w, http.StatusOK, map[string]string{
		"status":    "ok",
		"media_url": mediaURL,
	})
}

// userFromRequest extracts the authenticated user from the request context, if present.
func userFromRequest(r *http.Request) *models.UserInfo {
	if user, ok := r.Context().Value(kvmoidc.UserContextKey).(*models.UserInfo); ok {
		return user
	}
	return nil
}
