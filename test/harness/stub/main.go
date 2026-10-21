// Command stub is the behaviour suite's stand-in for Ailuros, tinyauth and
// the echo upstream. It serves the role's traffic on -listen and a control
// API on -control:
//
//	GET  /calls   the requests received so far, as JSON
//	POST /answer  {"status": 403, "delay_ms": 0, "headers": {...},
//	              "routes": {"/block/": 403}} (ailuros; a route overrides the
//	              status for requests whose X-Original-URI starts with its key)
//	POST /stop    stop listening on -listen, so connections are refused
//
// Roles:
//
//	ailuros   answers every request with the configured answer (default 200)
//	tinyauth  answers 200 when the request has "Authorization: ok", else 401
//	echo      answers 200 with a JSON echo of the request
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type call struct {
	Method string      `json:"method"`
	URI    string      `json:"uri"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
}

type answer struct {
	Status  int               `json:"status"`
	DelayMS int               `json:"delay_ms"`
	Headers map[string]string `json:"headers"`
	Routes  map[string]int    `json:"routes"`
}

type state struct {
	mu     sync.Mutex
	calls  []call
	answer answer
}

func (s *state) record(r *http.Request) call {
	body, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, URI: r.RequestURI, Header: r.Header.Clone(), Body: body}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	return c
}

func main() {
	role := flag.String("role", "", "ailuros, tinyauth or echo")
	listen := flag.String("listen", ":8080", "address for the role's traffic")
	control := flag.String("control", ":9999", "address for the control API")
	flag.Parse()

	s := &state{answer: answer{Status: http.StatusOK}}

	var h http.HandlerFunc
	switch *role {
	case "ailuros":
		h = func(w http.ResponseWriter, r *http.Request) {
			s.record(r)
			s.mu.Lock()
			a := s.answer
			s.mu.Unlock()
			orig := r.Header.Get("X-Original-URI")
			for prefix, status := range a.Routes {
				if strings.HasPrefix(orig, prefix) {
					a.Status = status
				}
			}
			if a.DelayMS > 0 {
				select {
				case <-time.After(time.Duration(a.DelayMS) * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			for k, v := range a.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(a.Status)
		}
	case "tinyauth":
		h = func(w http.ResponseWriter, r *http.Request) {
			s.record(r)
			if r.Header.Get("Authorization") == "ok" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		}
	case "echo":
		h = func(w http.ResponseWriter, r *http.Request) {
			c := s.record(r)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(c)
		}
	default:
		log.Fatalf("unknown role %q", *role)
	}

	ctl := http.NewServeMux()
	ctl.HandleFunc("GET /calls", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.calls)
	})
	ctl.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
		var a answer
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil || a.Status < 200 || a.Status > 599 {
			http.Error(w, "bad answer", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.answer = a
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	ctl.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	ctl.HandleFunc("POST /stop", func(w http.ResponseWriter, _ *http.Request) {
		_ = srv.Close()
		w.WriteHeader(http.StatusNoContent)
	})

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	log.Fatal(http.ListenAndServe(*control, ctl))
}
