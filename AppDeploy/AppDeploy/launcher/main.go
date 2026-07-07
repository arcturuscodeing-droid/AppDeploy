package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/webview/webview_go"
)

const API_PORT = "9843"
const SERVE_PORT = "9844"

var (
	mu            sync.Mutex
	appTempDir    string
	serverTempDir string
	currentWin    webview.WebView
	fileServer    *http.Server
	liveServer    *http.Server
)

type LaunchPayload struct {
	HTML     string `json:"html"`
	Filename string `json:"filename"`
	Mode     string `json:"mode"` // "overlay", "window", "fullscreen", "float"
}

type ZipPayload struct {
	Data     string `json:"data"` // base64 encoded zip
	Filename string `json:"filename"`
	Mode     string `json:"mode"`
}

type ServePayload struct {
	Files map[string]string `json:"files"` // filename -> base64 content
	Entry string            `json:"entry"` // entry file e.g. "index.html"
}

func corsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func startAPIServer() {
	mux := http.NewServeMux()

	// Ping
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "2.0.0", "app": "AppDeploy"})
	})

	// Launch single HTML file as app
	mux.HandleFunc("/launch", func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		if r.Method != "POST" { w.WriteHeader(405); return }

		var payload LaunchPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid json"})
			return
		}

		go launchApp(payload.HTML, payload.Filename, payload.Mode)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Launch a full zip as app (multi-file)
	mux.HandleFunc("/launch-zip", func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		if r.Method != "POST" { w.WriteHeader(405); return }

		// Parse multipart form - zip uploaded as binary
		r.ParseMultipartForm(512 << 20) // 512mb max
		file, header, err := r.FormFile("zip")
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "no zip file"})
			return
		}
		defer file.Close()

		mode := r.FormValue("mode")
		if mode == "" { mode = "window" }

		// Save zip to temp
		tmpZip, _ := os.CreateTemp("", "appdeploy-*.zip")
		io.Copy(tmpZip, file)
		tmpZip.Close()

		// Extract zip
		mu.Lock()
		if appTempDir != "" { os.RemoveAll(appTempDir) }
		appTempDir, _ = os.MkdirTemp("", "appdeploy-app-*")
		extractDir := appTempDir
		mu.Unlock()

		if err := unzip(tmpZip.Name(), extractDir); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "failed to extract zip"})
			return
		}
		os.Remove(tmpZip.Name())

		// Find entry point - look for index.html, unwrap single nested folder
		entryDir := findEntryDir(extractDir)
		entryFile := filepath.Join(entryDir, "index.html")

		// Check meta tag for mode override
		if htmlBytes, err := os.ReadFile(entryFile); err == nil {
			if m := extractMetaMode(string(htmlBytes)); m != "" {
				mode = m
			}
		}

		// Start file server for this app
		stopFileServer()
		startFileServer(entryDir)

		go openWindow("http://localhost:"+SERVE_PORT+"/", header.Filename, mode)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Serve a folder as live server
	mux.HandleFunc("/serve", func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		if r.Method != "POST" { w.WriteHeader(405); return }

		// Accept zip of files to serve
		r.ParseMultipartForm(512 << 20)
		file, _, err := r.FormFile("zip")
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "no zip"})
			return
		}
		defer file.Close()

		// Save and extract to serve dir
		tmpZip, _ := os.CreateTemp("", "appdeploy-serve-*.zip")
		io.Copy(tmpZip, file)
		tmpZip.Close()

		mu.Lock()
		if serverTempDir != "" { os.RemoveAll(serverTempDir) }
		serverTempDir, _ = os.MkdirTemp("", "appdeploy-serve-*")
		serveDir := serverTempDir
		mu.Unlock()

		unzip(tmpZip.Name(), serveDir)
		os.Remove(tmpZip.Name())

		entryDir := findEntryDir(serveDir)
		stopLiveServer()
		startLiveServer(entryDir)

		json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"url":    "http://localhost:9845/",
		})
	})

	// Stop live server
	mux.HandleFunc("/serve/stop", func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		stopLiveServer()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Close current app window
	mux.HandleFunc("/close", func(w http.ResponseWriter, r *http.Request) {
		corsHeaders(w)
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		if currentWin != nil {
			currentWin.Destroy()
			currentWin = nil
		}
		mu.Unlock()
		stopFileServer()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	fmt.Println("AppDeploy API running on http://localhost:" + API_PORT)
	http.ListenAndServe("127.0.0.1:"+API_PORT, mux)
}

func launchApp(html, filename, mode string) {
	mu.Lock()
	if currentWin != nil {
		currentWin.Destroy()
		currentWin = nil
	}
	if appTempDir != "" {
		os.RemoveAll(appTempDir)
	}
	appTempDir, _ = os.MkdirTemp("", "appdeploy-app-*")
	dir := appTempDir
	mu.Unlock()

	// Check meta tag for mode
	if m := extractMetaMode(html); m != "" {
		mode = m
	}

	tmpFile := filepath.Join(dir, "app.html")
	os.WriteFile(tmpFile, []byte(html), 0644)

	openWindow("file:///"+filepath.ToSlash(tmpFile), filename, mode)

	mu.Lock()
	currentWin = nil
	os.RemoveAll(dir)
	appTempDir, _ = os.MkdirTemp("", "appdeploy-app-*")
	mu.Unlock()
}

func openWindow(url, title, mode string) {
	debug := false
	w := webview.New(debug)
	defer w.Destroy()

	mu.Lock()
	currentWin = w
	mu.Unlock()

	w.SetTitle(title)

	switch mode {
	case "fullscreen":
		w.SetSize(1920, 1080, webview.HintFixed)
	case "overlay", "float":
		w.SetSize(1920, 1080, webview.HintFixed)
	case "small":
		w.SetSize(400, 300, webview.HintNone)
	default:
		w.SetSize(1280, 800, webview.HintNone)
	}

	w.Navigate(url)
	w.Run()
}

func startFileServer(dir string) {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(dir)))
	fileServer = &http.Server{Addr: "127.0.0.1:" + SERVE_PORT, Handler: mux}
	go fileServer.ListenAndServe()
}

func stopFileServer() {
	if fileServer != nil {
		fileServer.Close()
		fileServer = nil
	}
}

func startLiveServer(dir string) {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(dir)))
	liveServer = &http.Server{Addr: "127.0.0.1:9845", Handler: mux}
	go liveServer.ListenAndServe()
}

func stopLiveServer() {
	if liveServer != nil {
		liveServer.Close()
		liveServer = nil
	}
	if serverTempDir != "" {
		os.RemoveAll(serverTempDir)
		serverTempDir = ""
	}
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil { return err }
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}
		os.MkdirAll(filepath.Dir(fpath), os.ModePerm)
		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil { continue }
		rc, err := f.Open()
		if err != nil { outFile.Close(); continue }
		io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
	}
	return nil
}

func findEntryDir(dir string) string {
	// If index.html exists at root, use root
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return dir
	}
	// Otherwise look one level deep for a folder with index.html
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			sub := filepath.Join(dir, e.Name())
			if _, err := os.Stat(filepath.Join(sub, "index.html")); err == nil {
				return sub
			}
		}
	}
	return dir
}

func extractMetaMode(html string) string {
	lower := strings.ToLower(html)
	idx := strings.Index(lower, `name="appdeploy"`)
	if idx == -1 { return "" }
	// Find content attribute nearby
	chunk := lower[idx : min(idx+200, len(lower))]
	ci := strings.Index(chunk, `content="`)
	if ci == -1 { return "" }
	rest := chunk[ci+9:]
	end := strings.Index(rest, `"`)
	if end == -1 { return "" }
	return strings.TrimSpace(rest[:end])
}

func min(a, b int) int {
	if a < b { return a }
	return b
}

func main() {
	appTempDir, _ = os.MkdirTemp("", "appdeploy-app-*")
	defer os.RemoveAll(appTempDir)

	startAPIServer()
}
