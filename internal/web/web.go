package web

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"time"

	"tagwatch/internal/monitor"
)

//go:embed index.html
var page string

var view = template.Must(template.New("index").Funcs(template.FuncMap{
	"time": func(t time.Time) string {
		if t.IsZero() {
			return "Not checked yet"
		}
		return t.UTC().Format("2006-01-02 15:04:05 UTC")
	},
}).Parse(page))

func Handler(m *monitor.Monitor) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		var err error
		m.View(func(report monitor.Report) { err = view.Execute(&buf, report) })
		if err != nil {
			http.Error(w, "Cannot render report", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Write(buf.Bytes())
	})
	return mux
}
