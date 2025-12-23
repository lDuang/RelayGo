package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

type keyFile struct {
	Proxy string   `json:"proxy"`
	Keys  []string `json:"keys"`
}

var (
	port     = flag.Int("port", 8080, "listen port")
	keysPath = flag.String("keys", "", "path to keys.json")
	timeout  = flag.Duration("timeout", 120*time.Second, "upstream timeout")
)

const upstreamHost = "generativelanguage.googleapis.com"
const upstreamBase = "https://" + upstreamHost

var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func main() {
	flag.Parse()

	cfg, err := loadConfig(*keysPath)
	if err != nil {
		log.Fatalf("load keys failed: %v", err)
	}
	if len(cfg.Keys) == 0 {
		log.Fatal("no keys found")
	}

	client, err := newHTTPClient(cfg.Proxy, *timeout)
	if err != nil {
		log.Fatalf("proxy error: %v", err)
	}

	log.Printf("loaded %d keys, proxy=%v", len(cfg.Keys), cfg.Proxy != "")

	var rr uint64

	handler := func(w http.ResponseWriter, r *http.Request) {

		log.Printf("%s %s", r.Method, r.URL.Path) // 打一行请求日志

		if r.URL.IsAbs() {
			http.Error(w, "absolute url not allowed", 400)
			return
		}

		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		upURL := upstreamBase + r.URL.Path
		if r.URL.RawQuery != "" {
			q := r.URL.Query()
			q.Del("key")
			upURL += "?" + q.Encode()
		}

		start := atomic.AddUint64(&rr, 1) - 1
		n := uint64(len(cfg.Keys))

		for i := uint64(0); i < n; i++ {
			key := cfg.Keys[(start+i)%n]

			req, _ := http.NewRequestWithContext(
				context.Background(),
				r.Method,
				upURL,
				bytes.NewReader(body),
			)

			for h, vv := range r.Header {
				if hopByHop[h] {
					continue
				}
				for _, v := range vv {
					req.Header.Add(h, v)
				}
			}

			req.Host = upstreamHost
			req.Header.Set("Host", upstreamHost)
			req.Header.Set("x-goog-api-key", key)

			resp, err := client.Do(req)
			if err != nil {
				if i == n-1 {
					http.Error(w, "upstream error", 502)
					return
				}
				continue
			}

			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode == 503 {
				drain(resp.Body)
				if i == n-1 {
					copyResp(w, resp)
					return
				}
				continue
			}

			copyResp(w, resp)
			return
		}
	}

	http.HandleFunc("/", handler)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("listening on http://%s -> %s", addr, upstreamBase)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func loadConfig(p string) (*keyFile, error) {
	if p == "" {
		exe, _ := os.Executable()
		p = filepath.Join(filepath.Dir(exe), "keys.json")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var cfg keyFile
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cfg.Keys))
	for _, k := range cfg.Keys {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	cfg.Keys = out
	return &cfg, nil
}

func newHTTPClient(proxy string, timeout time.Duration) (*http.Client, error) {
	tr := &http.Transport{}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
	}, nil
}

func copyResp(w http.ResponseWriter, resp *http.Response) {
	defer drain(resp.Body)
	for k, vv := range resp.Header {
		if hopByHop[k] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func drain(rc io.ReadCloser) {
	io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
}
