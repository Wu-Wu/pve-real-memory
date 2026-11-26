package httpc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetch(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total":1000,"avail":400,"used":600}`))
	}))
	defer srv.Close()

	out, err := Fetch(Options{URL: srv.URL, Token: "sekret", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"used":600`) {
		t.Errorf("out = %q", out)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestFetchNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := Fetch(Options{URL: srv.URL, Timeout: 5 * time.Second}); err == nil {
		t.Fatal("must fail on 503")
	}
}

func TestFetchNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>login</html>"))
	}))
	defer srv.Close()
	_, err := Fetch(Options{URL: srv.URL, Timeout: 5 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer func() { close(block); srv.Close() }()

	start := time.Now()
	_, err := Fetch(Options{URL: srv.URL, Timeout: 300 * time.Millisecond})
	if err == nil {
		t.Fatal("slow endpoint must fail")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("timeout not enforced: took %v", time.Since(start))
	}
}
