package main

import (
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/Finomosec/extended-btrfs-node-exporter/collector"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		port = "9198"
	}

	cfg := collector.LoadConfig()
	c := collector.New(cfg)
	prometheus.MustRegister(c)

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		c.SetCurrentScraper(r.RemoteAddr)
		promhttp.Handler().ServeHTTP(w, r)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>Extended BTRFS Exporter</title></head>
		<body><h1>Extended BTRFS Exporter</h1>
		<p><a href="/metrics">Metrics</a></p>
		<p><a href="/debug/pprof/">Profiler</a></p>
		</body></html>`))
	})

	log.Printf("Extended BTRFS Exporter listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
