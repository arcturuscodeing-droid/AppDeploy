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
	"time"

	"github.com/webview/webview_go"
)

const API_PORT = "9843"
const FILE_SERVER_PORT = "9844"
const LIVE_SERVER_PORT = "9845"

var mu sync.Mutex
var appTempDir string
var serveTempDir string
var liveServeTempDir string
var currentWin webview.WebView
var fileServeMux *http.ServeMux
var fileServer *http.Server
var liveServer *http.Server
var winClosed = make(chan bool, 1)

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "application/json")
}

func main() {
	var err error
	appTempDir, err = os.MkdirTemp("", "appdeploy-app-*")
	if err != nil {
		fmt.Println("Failed to create temp dir")
		return
	}
	defer os.RemoveAll(appTempDir)

	startFileServer()
	startAPIServer()
}

func startAPIServer() {
	mux := http.NewServeMux()

	// Ping
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "3.0.0"})
	})

	// Launch - handles both HTML and ZIP
	mux.HandleFunc("/launch", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		if r.Method != "POST" { w.WriteHeader(405); return }

		r.ParseMultipartForm(512 << 20)
		file, header, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "no file"})
			return
		}
		defer file.Close()

		mode := r.FormValue("mode")
		if mode == "" { mode = "window" }

		// Kill existing window cleanly
		closeCurrentWindow()

		// Fresh temp dir
		mu.Lock()
		os.RemoveAll(appTempDir)
		appTempDir, _ = os.MkdirTemp("", "appdeploy-app-*")
		dir := appTempDir
		mu.Unlock()

		isZip := strings.HasSuffix(strings.ToLower(header.Filename), ".zip")

		if isZip {
			// Save zip
			tmpZip := filepath.Join(dir, "upload.zip")
			f, _ := os.Create(tmpZip)
			io.Copy(f, file)
			f.Close()

			// Extract
			extractDir := filepath.Join(dir, "app")
			os.MkdirAll(extractDir, 0755)
			if err := unzip(tmpZip, extractDir); err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": "unzip failed"})
				return
			}
			os.Remove(tmpZip)

			// Find index.html
			entryDir := findEntryDir(extractDir)

			// Check mode override in meta tag
			idxFile := filepath.Join(entryDir, "index.html")
			if b, err := os.ReadFile(idxFile); err == nil {
				if m := extractMode(string(b)); m != "" { mode = m }
			}

			// Update file server root
			updateFileServer(entryDir)

			go launchWindow("http://localhost:"+FILE_SERVER_PORT+"/", header.Filename, mode)
		} else {
			// Single HTML file
			htmlBytes, _ := io.ReadAll(file)
			html := string(htmlBytes)

			// Check mode override
			if m := extractMode(html); m != "" { mode = m }

			// Write to temp
			htmlPath := filepath.Join(dir, "index.html")
			os.WriteFile(htmlPath, htmlBytes, 0644)

			// Serve via file server so relative paths work
			updateFileServer(dir)

			go launchWindow("http://localhost:"+FILE_SERVER_PORT+"/index.html", header.Filename, mode)
		}

		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Close window
	mux.HandleFunc("/close", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		closeCurrentWindow()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Live server - serve a zip as a local website
	mux.HandleFunc("/serve", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		if r.Method != "POST" { w.WriteHeader(405); return }

		r.ParseMultipartForm(512 << 20)
		file, _, err := r.FormFile("file")
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "no file"})
			return
		}
		defer file.Close()

		// Stop existing live server
		if liveServer != nil {
			liveServer.Close()
			liveServer = nil
		}
		if liveServeTempDir != "" {
			os.RemoveAll(liveServeTempDir)
		}
		liveServeTempDir, _ = os.MkdirTemp("", "appdeploy-live-*")

		tmpZip := filepath.Join(liveServeTempDir, "upload.zip")
		f, _ := os.Create(tmpZip)
		io.Copy(f, file)
		f.Close()

		extractDir := filepath.Join(liveServeTempDir, "site")
		os.MkdirAll(extractDir, 0755)
		unzip(tmpZip, extractDir)
		os.Remove(tmpZip)

		entryDir := findEntryDir(extractDir)

		// Start live server
		lsMux := http.NewServeMux()
		lsMux.Handle("/", http.FileServer(http.Dir(entryDir)))
		liveServer = &http.Server{Addr: "127.0.0.1:" + LIVE_SERVER_PORT, Handler: lsMux}
		go liveServer.ListenAndServe()

		json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"url":    "http://localhost:" + LIVE_SERVER_PORT + "/",
		})
	})

	// Stop live server
	mux.HandleFunc("/serve/stop", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		if r.Method == "OPTIONS" { w.WriteHeader(200); return }
		if liveServer != nil {
			liveServer.Close()
			liveServer = nil
		}
		if liveServeTempDir != "" {
			os.RemoveAll(liveServeTempDir)
			liveServeTempDir = ""
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	fmt.Println("AppDeploy running on http://localhost:" + API_PORT)
	http.ListenAndServe("127.0.0.1:"+API_PORT, mux)
}

func startFileServer() {
	fileServeMux = http.NewServeMux()
	fileServeMux.Handle("/", http.FileServer(http.Dir(appTempDir)))
	fileServer = &http.Server{Addr: "127.0.0.1:" + FILE_SERVER_PORT, Handler: fileServeMux}
	go fileServer.ListenAndServe()
}

func updateFileServer(dir string) {
	// Restart file server pointing at new dir
	if fileServer != nil {
		fileServer.Close()
		time.Sleep(100 * time.Millisecond)
	}
	newMux := http.NewServeMux()
	newMux.Handle("/", http.FileServer(http.Dir(dir)))
	fileServer = &http.Server{Addr: "127.0.0.1:" + FILE_SERVER_PORT, Handler: newMux}
	go fileServer.ListenAndServe()
	time.Sleep(150 * time.Millisecond)
}

func closeCurrentWindow() {
	mu.Lock()
	w := currentWin
	currentWin = nil
	mu.Unlock()

	if w != nil {
		w.Dispatch(func() {
			w.Terminate()
		})
		// Give it time to close
		time.Sleep(300 * time.Millisecond)
	}
}

func launchWindow(url, title, mode string) {
	w := webview.New(false)

	mu.Lock()
	currentWin = w
	mu.Unlock()

	w.SetTitle(title)

	switch mode {
	case "fullscreen":
		w.SetSize(1920, 1080, webview.HintFixed)
	case "overlay":
		w.SetSize(1920, 1080, webview.HintFixed)
	case "float":
		w.SetSize(480, 320, webview.HintNone)
	case "small":
		w.SetSize(400, 300, webview.HintNone)
	default:
		w.SetSize(1280, 800, webview.HintNone)
	}

	w.Navigate(url)
	w.Run()
	w.Destroy()

	mu.Lock()
	if currentWin == w {
		currentWin = nil
	}
	mu.Unlock()
}

func unzip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil { return err }
	defer r.Close()

	for _, f := range r.File {
		fpath := filepath.Join(dest, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}
		os.MkdirAll(filepath.Dir(fpath), os.ModePerm)
		out, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil { continue }
		rc, err := f.Open()
		if err != nil { out.Close(); continue }
		io.Copy(out, rc)
		out.Close()
		rc.Close()
	}
	return nil
}

func findEntryDir(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return dir
	}
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

func extractMode(html string) string {
	lower := strings.ToLower(html)
	idx := strings.Index(lower, `name="appdeploy"`)
	if idx == -1 { return "" }
	chunk := lower[idx:min(idx+200, len(lower))]
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
