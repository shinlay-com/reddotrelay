package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type uiFileHandler struct {
	directory string
}

func newUIHandler(directory string) http.Handler {
	directory = filepath.Clean(directory)
	if resolved, err := filepath.EvalSymlinks(directory); err == nil {
		directory = resolved
	}
	return uiFileHandler{directory: directory}
}

func (handler uiFileHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	relative := strings.TrimPrefix(request.URL.Path, "/ui/")
	relative = strings.TrimPrefix(filepath.Clean("/"+relative), string(filepath.Separator))
	candidate := filepath.Join(handler.directory, filepath.FromSlash(relative))
	if !withinDirectory(handler.directory, candidate) {
		http.NotFound(writer, request)
		return
	}
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		if relative != "" && filepath.Ext(relative) != "" {
			http.NotFound(writer, request)
			return
		}
		candidate = filepath.Join(handler.directory, "index.html")
		if _, err := os.Stat(candidate); err != nil {
			http.NotFound(writer, request)
			return
		}
	}

	if strings.HasPrefix(filepath.ToSlash(relative), "assets/") {
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		writer.Header().Set("Cache-Control", "no-store")
	}
	if hasSymlinkInPath(handler.directory, candidate) {
		http.NotFound(writer, request)
		return
	}
	file, err := os.Open(candidate)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	serveRequest := request.Clone(request.Context())
	serveRequest.URL.Path = "/" + filepath.Base(candidate)
	http.ServeContent(writer, serveRequest, filepath.Base(candidate), info.ModTime(), file)
}

func withinDirectory(directory, candidate string) bool {
	relative, err := filepath.Rel(directory, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func hasSymlinkInPath(directory, candidate string) bool {
	relative, err := filepath.Rel(directory, candidate)
	if err != nil {
		return true
	}
	current := directory
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "." || segment == "" {
			continue
		}
		current = filepath.Join(current, segment)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}
