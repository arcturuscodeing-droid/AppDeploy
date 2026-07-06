package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/webview/webview_go"
)

const PORT = "9843"

var (
	mu      sync.Mutex
	tempDir string
	currentWindow webview.WebView
)

type FilePayload struct {
	HTML     string `json:"html"`
	Filename string `json:"filename"`
}

func main() {
	// Create a temp working dir
	var err error
	tempDir, err = os.MkdirTemp("", "appdeploy-*")
	if err != nil {
		fmt.Println("Failed to create temp dir:", err)
		return
	}
	defer os.RemoveAll(tempDir)

	// Start HTTP server
	mux := http.NewServeMux()

	// CORS + ping
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "app": "AppDeploy Launcher"})
	})

	// Launch a file as an app
	mux.HandleFunc("/launch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Content-Type", "application/json")

		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}

		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}

		var payload FilePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid json"})
			return
		}

		// Write HTML to temp file
		mu.Lock()
		defer mu.Unlock()

		// Close existing window if open
		if currentWindow != nil {
			currentWindow.Destroy()
			currentWindow = nil
		}

		// Clean temp dir
		os.RemoveAll(tempDir)
		tempDir, _ = os.MkdirTemp("", "appdeploy-*")

		tmpFile := filepath.Join(tempDir, "app.html")
		if err := os.WriteFile(tmpFile, []byte(payload.HTML), 0644); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "failed to write file"})
			return
		}

		// Launch webview in a goroutine
		go func() {
			w := webview.New(false)
			defer w.Destroy()
			mu.Lock()
			currentWindow = w
			mu.Unlock()

			w.SetTitle(payload.Filename)
			w.SetSize(800, 600, webview.HintNone)
			w.Navigate("file:///" + filepath.ToSlash(tmpFile))
			w.Run()

			// Cleanup when window closes
			mu.Lock()
			currentWindow = nil
			mu.Unlock()
			os.RemoveAll(tempDir)
			tempDir, _ = os.MkdirTemp("", "appdeploy-*")
		}()

		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Close current app
	mux.HandleFunc("/close", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if currentWindow != nil {
			currentWindow.Destroy()
			currentWindow = nil
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	fmt.Println("AppDeploy Launcher running on localhost:" + PORT)
	http.ListenAndServe("127.0.0.1:"+PORT, mux)
}
