// github-core-clone-01 entry: a standalone GitHub-compatible git forge.
// One process serves git smart HTTP, REST v3, GraphQL and MCP from its own store.
// Lifecycle: bind an ephemeral port, register it with the env, poll /done, exit 0.
// Local practice mode (no MAKEFUN_ENV_URL): serve on FORGE_PORT for sdk/run.ts.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	app := NewApp()
	envURL := trimSlash(os.Getenv("MAKEFUN_ENV_URL"))
	srv := &http.Server{Handler: app}

	if envURL == "" {
		// Local practice: serve on a fixed port, no env lifecycle.
		port := os.Getenv("FORGE_PORT")
		if port == "" {
			port = "8090"
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[forge] listen:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "[forge] local serve on", ln.Addr().String())
		if err := srv.Serve(ln); err != nil {
			fmt.Fprintln(os.Stderr, "[forge] serve:", err)
			os.Exit(1)
		}
		return
	}

	// Trial: bind 0.0.0.0 so the env (a separate container) can dial the address it saw register from.
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		if err := srv.Serve(ln); err != nil {
			fmt.Fprintln(os.Stderr, "[forge] serve:", err)
		}
	}()
	fmt.Fprintf(os.Stderr, "[forge] listening on port %d\n", port)

	register(envURL, port)
	fmt.Fprintln(os.Stderr, "[forge] registered; waiting for done")
	waitDone(envURL)
	srv.Close()
	fmt.Fprintln(os.Stderr, "[forge] done")
	os.Exit(0)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func register(envURL string, port int) {
	body, _ := json.Marshal(map[string]int{"port": port})
	delay := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		cl := &http.Client{Timeout: 30 * time.Second}
		res, err := cl.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				fmt.Fprintf(os.Stderr, "[forge] register HTTP %d\n", res.StatusCode)
				os.Exit(1)
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func waitDone(envURL string) {
	delay := 250 * time.Millisecond
	for {
		cl := &http.Client{Timeout: 30 * time.Second}
		res, err := cl.Get(envURL + "/done")
		if err == nil {
			var b struct {
				Done bool `json:"done"`
			}
			data, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode == 200 && json.Unmarshal(data, &b) == nil && b.Done {
				return
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}