package main

// admin.go — runtime API-key (account) management for commandcode-bridge.
// The current key lives in memory + a local key file (file wins over the
// CMDC_API_KEY env bootstrap, so a runtime swap survives pm2 restarts).
//   GET  /admin/account          → live whoami for the active key
//   POST /admin/account {"api_key":"..."} → validate + switch account

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// keyStore holds the current Command Code API key.
type keyStore struct {
	mu  sync.RWMutex
	key string
}

var keys keyStore

func (k *keyStore) current() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.key
}

func (k *keyStore) set(newKey string) {
	k.mu.Lock()
	k.key = newKey
	k.mu.Unlock()
}

func keyFilePath() string {
	return envOr("CMDC_KEY_FILE", "/home/agentuser/apps/commandcode-bridge/account.json")
}

func persistKey(k string) error {
	b, _ := json.Marshal(map[string]string{"api_key": k})
	return os.WriteFile(keyFilePath(), b, 0600)
}

// loadKey resolves the startup key: key file first, env var as bootstrap
// (which is then written to the file so later swaps persist cleanly).
func loadKey() {
	if b, err := os.ReadFile(keyFilePath()); err == nil {
		var m struct {
			APIKey string `json:"api_key"`
		}
		if json.Unmarshal(b, &m) == nil && m.APIKey != "" {
			keys.set(m.APIKey)
			log.Printf("api key loaded from %s", keyFilePath())
			return
		}
	}
	keys.set(apiKey)
	if apiKey != "" {
		if err := persistKey(apiKey); err != nil {
			log.Printf("warn: could not persist key file: %v", err)
		}
	}
}

// upstreamWhoami validates a key against /alpha/whoami.
func upstreamWhoami(key string) (status int, body string) {
	req, err := http.NewRequest("GET", baseURL+"/alpha/whoami", nil)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", cliVersion)
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, string(b)
}

func adminAuthOK(r *http.Request) bool {
	tok := os.Getenv("CMDC_ADMIN_TOKEN")
	if tok == "" {
		return true // open admin, same posture as the other bridges
	}
	return r.Header.Get("Authorization") == "Bearer "+tok || r.URL.Query().Get("token") == tok
}

// GET /admin/account — current key preview + live whoami.
func handleAccountGet(w http.ResponseWriter, r *http.Request) {
	if !adminAuthOK(r) {
		writeOAError(w, 401, "authentication_error", "bad admin token")
		return
	}
	key := keys.current()
	if key == "" {
		writeOAError(w, 500, "server_error", "no API key configured")
		return
	}
	status, body := upstreamWhoami(key)
	writeJSON(w, status, map[string]interface{}{
		"key_preview": keyPreview(key),
		"whoami":      json.RawMessage(body),
	})
}

// POST /admin/account {"api_key":"..."} — validate then switch account.
func handleAccountSet(w http.ResponseWriter, r *http.Request) {
	if !adminAuthOK(r) {
		writeOAError(w, 401, "authentication_error", "bad admin token")
		return
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.APIKey == "" {
		writeOAError(w, 400, "invalid_request_error", `expected {"api_key":"..."}`)
		return
	}
	status, respBody := upstreamWhoami(body.APIKey)
	if status != 200 {
		writeJSON(w, status, map[string]interface{}{
			"error":  "key validation failed (not switched)",
			"whoami": json.RawMessage(respBody),
		})
		return
	}
	keys.set(body.APIKey)
	if err := persistKey(body.APIKey); err != nil {
		writeOAError(w, 500, "server_error", "key swapped in memory but persist failed: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"ok":          true,
		"key_preview": keyPreview(body.APIKey),
		"whoami":      json.RawMessage(respBody),
	})
}

func keyPreview(k string) string {
	if len(k) <= 14 {
		return k[:4] + "…"
	}
	return k[:8] + "…" + k[len(k)-4:]
}
