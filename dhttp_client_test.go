package http

// Tests relating to changes made by dhttp

import (
	"crypto/tls"
	"testing"

	"encoding/json"
)

// Tests various content encodings and decodings
func TestReadBody(t *testing.T) {
	cases := map[string]struct {
		url         string
		expectedKey string
	}{
		"gzip":    {url: "https://httpbin.dev/gzip", expectedKey: "gzipped"},
		"brotli":  {url: "https://httpbin.dev/brotli", expectedKey: "brotli"},
		"deflate": {url: "https://httpbin.dev/deflate", expectedKey: "deflated"},
		"zstd":    {url: "https://httpbin.dev/zstd", expectedKey: "zstd"},
	}

	for name, c := range cases {
		for _, proto := range []string{"h1", "h2"} {
			t.Run(name+"_"+proto, func(t *testing.T) {
				t.Parallel()
				var cl Client
				if proto == "h1" {
					cl = Client{Transport: &Transport{
						TLSClientConfig: &tls.Config{},
						TLSNextProto:    make(map[string]func(authority string, c *tls.Conn) RoundTripper), // Disable HTTP/2
					}}
				} else {
					cl = Client{
						Transport: &Transport{},
					}
				}

				resp, err := cl.Get(c.url)
				if err != nil {
					t.Fatalf("Get failed: %v", err)
				}
				defer resp.Body.Close()

				response := map[string]any{}
				err = json.NewDecoder(resp.Body).Decode(&response)
				if err != nil {
					t.Fatalf("Decode failed: %v", err)
				}

				if response[c.expectedKey].(bool) != true {
					t.Errorf("Expected key %q not found in response: %v", c.expectedKey, response)
				}
			})
		}
	}
}
