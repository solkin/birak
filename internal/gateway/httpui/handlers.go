package httpui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/birak/birak/internal/watcher"
)

// fileEntry represents a single file or directory in a listing response.
type fileEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// listResponse is the JSON response for directory listings.
type listResponse struct {
	Path    string      `json:"path"`
	Entries []fileEntry `json:"entries"`
}

// resolvePath validates a relative path and returns the full filesystem path.
// Returns an error for paths that escape syncDir or match ignore patterns.
func (g *Gateway) resolvePath(relPath string) (string, error) {
	cleaned := filepath.ToSlash(filepath.Clean(relPath))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "." {
		cleaned = ""
	}

	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", fmt.Errorf("path traversal")
	}

	if cleaned != "" && watcher.ShouldIgnore(cleaned, g.ignorePatterns) {
		return "", fmt.Errorf("ignored path")
	}

	full := filepath.Join(g.syncDir, filepath.FromSlash(cleaned))

	absSync, _ := filepath.Abs(g.syncDir)
	absFull, _ := filepath.Abs(full)
	if cleaned != "" && !strings.HasPrefix(absFull, absSync+string(filepath.Separator)) {
		return "", fmt.Errorf("path traversal")
	}

	return full, nil
}

// handleList returns a JSON listing of a directory.
func (g *Gateway) handleList(w http.ResponseWriter, r *http.Request) {
	dirPath := r.URL.Query().Get("path")

	fullPath, err := g.resolvePath(dirPath)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		jsonError(w, http.StatusNotFound, "directory not found")
		return
	}
	if !info.IsDir() {
		jsonError(w, http.StatusBadRequest, "not a directory")
		return
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to read directory")
		return
	}

	var result []fileEntry
	for _, e := range entries {
		name := e.Name()
		if watcher.ShouldIgnore(name, g.ignorePatterns) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		result = append(result, fileEntry{
			Name:    name,
			IsDir:   e.IsDir(),
			Size:    fi.Size(),
			ModTime: fi.ModTime().UTC().Format(time.RFC3339),
		})
	}

	cleanPath := filepath.ToSlash(filepath.Clean(dirPath))
	if cleanPath == "." || cleanPath == "/" {
		cleanPath = ""
	}
	cleanPath = strings.TrimPrefix(cleanPath, "/")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(listResponse{
		Path:    cleanPath,
		Entries: result,
	})
}

// handleDownload serves a file for download.
func (g *Gateway) handleDownload(w http.ResponseWriter, r *http.Request) {
	filePath := strings.TrimPrefix(r.URL.Path, "/_api/dl/")

	fullPath, err := g.resolvePath(filePath)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	f, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(fullPath)))
	http.ServeContent(w, r, filepath.Base(fullPath), info.ModTime(), f)
}

// handleUpload accepts multipart file uploads.
func (g *Gateway) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30) // 1 GB limit

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonError(w, http.StatusBadRequest, "failed to parse upload")
		return
	}

	targetDir := r.FormValue("path")
	dirFullPath, err := g.resolvePath(targetDir)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Ensure target directory exists.
	if err := os.MkdirAll(dirFullPath, 0o755); err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to create directory")
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		jsonError(w, http.StatusBadRequest, "no files provided")
		return
	}

	absSync, _ := filepath.Abs(g.syncDir)

	for _, fh := range files {
		if watcher.ShouldIgnore(fh.Filename, g.ignorePatterns) {
			jsonError(w, http.StatusBadRequest, fmt.Sprintf("file name %q is ignored", fh.Filename))
			return
		}

		destPath := filepath.Join(dirFullPath, fh.Filename)
		absDest, _ := filepath.Abs(destPath)
		if !strings.HasPrefix(absDest, absSync+string(filepath.Separator)) {
			jsonError(w, http.StatusBadRequest, "invalid file name")
			return
		}

		src, err := fh.Open()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "failed to read uploaded file")
			return
		}

		// Write atomically via temp file (matches ignore pattern .birak-tmp-*).
		tmpPath := filepath.Join(filepath.Dir(destPath), ".birak-tmp-upload-"+filepath.Base(destPath))
		dst, err := os.Create(tmpPath)
		if err != nil {
			src.Close()
			jsonError(w, http.StatusInternalServerError, "failed to create file")
			return
		}

		_, copyErr := io.Copy(dst, src)
		src.Close()
		dst.Close()

		if copyErr != nil {
			os.Remove(tmpPath)
			jsonError(w, http.StatusInternalServerError, "failed to write file")
			return
		}

		if err := os.Rename(tmpPath, destPath); err != nil {
			os.Remove(tmpPath)
			jsonError(w, http.StatusInternalServerError, "failed to save file")
			return
		}

		g.logger.Info("file uploaded", "name", fh.Filename, "dir", targetDir)
	}

	jsonOK(w)
}

// handleMkdir creates a directory.
func (g *Gateway) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Path == "" {
		jsonError(w, http.StatusBadRequest, "path is required")
		return
	}

	fullPath, err := g.resolvePath(req.Path)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := os.MkdirAll(fullPath, 0o755); err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to create directory")
		return
	}

	g.logger.Info("directory created", "path", req.Path)
	jsonOK(w)
}

// handleRename renames a file or directory.
func (g *Gateway) handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.From == "" || req.To == "" {
		jsonError(w, http.StatusBadRequest, "from and to are required")
		return
	}

	fromFull, err := g.resolvePath(req.From)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid source: "+err.Error())
		return
	}

	toFull, err := g.resolvePath(req.To)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid destination: "+err.Error())
		return
	}

	if _, err := os.Stat(fromFull); err != nil {
		jsonError(w, http.StatusNotFound, "source not found")
		return
	}

	// Ensure parent directory of destination exists.
	if err := os.MkdirAll(filepath.Dir(toFull), 0o755); err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to create parent directory")
		return
	}

	if err := os.Rename(fromFull, toFull); err != nil {
		jsonError(w, http.StatusInternalServerError, "rename failed: "+err.Error())
		return
	}

	g.logger.Info("renamed", "from", req.From, "to", req.To)
	jsonOK(w)
}

// handleDelete deletes a file or directory.
func (g *Gateway) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}

	if req.Path == "" || req.Path == "/" || req.Path == "." {
		jsonError(w, http.StatusBadRequest, "cannot delete root directory")
		return
	}

	fullPath, err := g.resolvePath(req.Path)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	if _, err := os.Stat(fullPath); err != nil {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}

	if err := os.RemoveAll(fullPath); err != nil {
		jsonError(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}

	g.logger.Info("deleted", "path", req.Path)
	jsonOK(w)
}

func jsonOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
