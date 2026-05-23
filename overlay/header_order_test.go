package http_test

import (
	"fmt"
	"io"
	"log"
	"net"

	. "github.com/dteh/dhttp"
	"github.com/dteh/dhttp/httptest"
	"github.com/dteh/dhttp/httptrace"
	httputil "github.com/dteh/dhttp/httputil"

	"testing"
)

func dumpRequestHandler(w ResponseWriter, r *Request) {
	// Dump the entire HTTP request
	dump, err := httputil.DumpRequest(r, true)
	if err != nil {
		Error(w, fmt.Sprint(err), StatusInternalServerError)
		return
	}

	// Respond back
	w.WriteHeader(StatusOK)
	_, _ = w.Write(dump)
}

func TestHeaderOrderHTTP1(t *testing.T) {
	hk := ""
	trace := &httptrace.ClientTrace{
		WroteHeaderField: func(key string, values []string) {
			hk += key + " "
		},
	}

	server := httptest.NewServer(HandlerFunc(dumpRequestHandler))
	defer server.Close()

	r, _ := NewRequest("GET", server.URL, nil)
	r.Header = Header{
		"Host":            {"overridden.com"},
		"User-agent":      {"my user agent"},
		"Custom-Header-1": {"V1"},
		"Custom-Header-2": {"V2"},
		"Another-header":  {"Another value"},
		"XXX":             {"YYY"},
		HeaderOrderKey: {
			"user-agent",
			"accept-encoding",
			"custom-header-2",
			"custom-header-1",
			"xxx",
		},
		PHeaderOrderKey: {
			":method",
			":path",
			":authority",
			":scheme",
		},
	}
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))

	cl := server.Client()
	resp, err := cl.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	log.Println(string(body))

	want := "Host User-agent Accept-Encoding Custom-Header-2 Custom-Header-1 XXX Another-header "
	if hk != want {
		t.Errorf("Header keys not in expected order\ngot : %v\nwant: %v", hk, want)
	}
}

func TestHeaderOrderHTTP2(t *testing.T) {
	hk := ""
	trace := &httptrace.ClientTrace{
		WroteHeaderField: func(key string, values []string) {
			hk += key + " "
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:12345")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	server := httptest.NewUnstartedServer(HandlerFunc(dumpRequestHandler))
	server.Listener = ln
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	r, _ := NewRequest("GET", server.URL, nil)
	r.Header = Header{
		"Host":            {"overridden.com"},
		"User-agent":      {"my user agent"},
		"Custom-Header-1": {"V1"},
		"Custom-Header-2": {"V2"},
		"Another-header":  {"Another value"},
		"XXX":             {"YYY"},
		HeaderOrderKey: {
			"user-agent",
			"accept-encoding",
			"custom-header-2",
			"custom-header-1",
			"xxx",
		},
		PHeaderOrderKey: {
			":method",
			":path",
			":authority",
			":scheme",
		},
	}
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))

	cl := server.Client()
	resp, err := cl.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	log.Println(string(body))

	want := ":method :path :authority :scheme " +
		"user-agent accept-encoding custom-header-2 custom-header-1 xxx another-header "
	if hk != want {
		t.Errorf("Header keys not in expected order\ngot : %v\nwant: %v", hk, want)
	}
}
