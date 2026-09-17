package backend

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/didww/prober/web"
)

// httpHandler is the browser listener: the REST/SSE API under /api and the
// embedded Vue SPA at everything else, all mounted under base_path.
func (s *Server) httpHandler() http.Handler {
	app := chi.NewRouter()
	app.Use(securityHeaders(contentSecurityPolicy()))
	app.Mount("/api", s.api.Routes())
	app.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})
	app.Handle("/*", spaHandler(s.cfg.BasePath))

	if s.cfg.BasePath == "" {
		return app
	}
	// Mount the whole app under the sub-path, stripping it so the routes above
	// stay written from the root.
	root := chi.NewRouter()
	root.Mount(s.cfg.BasePath, http.StripPrefix(s.cfg.BasePath, app))
	return root
}

func securityHeaders(csp string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", csp)
			next.ServeHTTP(w, r)
		})
	}
}

var scriptTagRe = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)

// contentSecurityPolicy builds the CSP. script-src is 'self' plus a sha256 hash
// of each inline script in index.html (the pre-paint theme stamp), so inline
// scripts run without opening the policy to 'unsafe-inline'. Everything else is
// served from this origin; Vue attaches component styles at runtime, hence
// unsafe-inline for styles only.
func contentSecurityPolicy() string {
	scriptSrc := "'self'"
	if dist, err := web.Dist(); err == nil {
		if b, err := fs.ReadFile(dist, "index.html"); err == nil {
			for _, hash := range inlineScriptHashes(b) {
				scriptSrc += " '" + hash + "'"
			}
		}
	}
	return "default-src 'self'; script-src " + scriptSrc + "; " +
		"style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'self'"
}

// inlineScriptHashes returns the CSP hash of every <script> in html that has no
// src attribute, matching what the browser computes over the tag's contents.
func inlineScriptHashes(html []byte) []string {
	var out []string
	for _, m := range scriptTagRe.FindAllSubmatch(html, -1) {
		if bytes.Contains(m[1], []byte("src=")) {
			continue // an external script, allowed by 'self'
		}
		sum := sha256.Sum256(m[2])
		out = append(out, "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
	}
	return out
}

// spaHandler serves the built SPA out of the embedded FS, with history fallback
// to index.html for client-side routes and a <base href> injected so relative
// asset and API URLs resolve whether mounted at "/" or under base.
func spaHandler(base string) http.Handler {
	dist, err := web.Dist()
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "web UI not built: run `make web`", http.StatusNotFound)
		})
	}
	files := http.FileServer(http.FS(dist))
	index := loadIndex(dist, base)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p == "" || !exists(dist, p) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(index)
			return
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

func loadIndex(dist fs.FS, base string) []byte {
	b, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return []byte("web UI not built")
	}
	href := "/"
	if base != "" {
		href = base + "/"
	}
	head := []byte(`<base href="` + href + `">`)
	if i := bytes.Index(b, []byte("<head>")); i >= 0 {
		out := make([]byte, 0, len(b)+len(head))
		out = append(out, b[:i+len("<head>")]...)
		out = append(out, head...)
		out = append(out, b[i+len("<head>"):]...)
		return out
	}
	return b
}

func exists(dist fs.FS, p string) bool {
	f, err := dist.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	return err == nil && !st.IsDir()
}
