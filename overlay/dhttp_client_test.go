package http

// Tests relating to changes made by dhttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"
	"testing"

	"github.com/dteh/dhttp/httptrace"
	tls "github.com/refraction-networking/utls"

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
						TLSNextProto:    make(map[string]func(authority string, c *tls.UConn) RoundTripper), // Disable HTTP/2
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

// Test that the "Accept-Encoding" header is set to
// "gzip, deflate, br, zstd" by default in both http1 and http2
func TestDefaultAcceptEncodingHeader(t *testing.T) {
	for _, proto := range []string{"h1", "h2"} {
		t.Run("Accept-Encoding_"+proto, func(t *testing.T) {
			t.Parallel()

			var cl Client
			if proto == "h1" {
				cl = Client{Transport: &Transport{
					TLSClientConfig: &tls.Config{},
					TLSNextProto:    make(map[string]func(authority string, c *tls.UConn) RoundTripper), // Disable HTTP/2
				}}
			} else {
				cl = Client{
					Transport: &Transport{},
				}
			}

			resp, err := cl.Get("https://httpbin.dev/headers")
			if err != nil {
				t.Fatalf("Get failed: %v", err)
			}

			body := struct {
				Headers map[string][]string `json:"headers"`
			}{}

			err = json.NewDecoder(resp.Body).Decode(&body)
			if err != nil {
				t.Fatalf("Decode failed: %v", err)
			}

			aeHeader := body.Headers["Accept-Encoding"]
			if len(aeHeader) != 1 {
				t.Fatalf("Expected 1 Accept-Encoding header, got %d", len(aeHeader))
			}
			expected := "gzip, deflate, br, zstd"
			if aeHeader[0] != expected {
				t.Errorf("Expected Accept-Encoding header to be %q, got %q", expected, aeHeader[0])
			}
		})
	}
}

type Ja3Response struct {
	Ja3              string `json:"ja3"`
	Ja3N             string `json:"ja3n"`
	Ja3Digest        string `json:"ja3_digest"`
	Ja3NDigest       string `json:"ja3n_digest"`
	ScrapflyFp       string `json:"scrapfly_fp"`
	ScrapflyFpDigest string `json:"scrapfly_fp_digest"`
	TLS              TLS    `json:"tls"`
}
type TLS struct {
	Version                    string   `json:"version"`
	Ciphers                    []string `json:"ciphers"`
	Curves                     []string `json:"curves"`
	Extensions                 []string `json:"extensions"`
	Points                     []string `json:"points"`
	Protocols                  []string `json:"protocols"`
	Versions                   []string `json:"versions"`
	HandshakeDuration          string   `json:"handshake_duration"`
	IsSessionResumption        bool     `json:"is_session_resumption"`
	SessionTicketSupported     bool     `json:"session_ticket_supported"`
	SupportSecureRenegotiation bool     `json:"support_secure_renegotiation"`
	SupportedTLSVersions       []int    `json:"supported_tls_versions"`
	SupportedProtocols         []string `json:"supported_protocols"`
	SignatureAlgorithms        []int    `json:"signature_algorithms"`
	PskKeyExchangeMode         string   `json:"psk_key_exchange_mode"`
	CertCompressionAlgorithms  string   `json:"cert_compression_algorithms"`
	EarlyData                  bool     `json:"early_data"`
	UsingPsk                   bool     `json:"using_psk"`
	SelectedProtocol           string   `json:"selected_protocol"`
	SelectedCurveGroup         int      `json:"selected_curve_group"`
	SelectedCipherSuite        int      `json:"selected_cipher_suite"`
	KeyShares                  []int    `json:"key_shares"`
}

func getja3(ctx context.Context, cl *Client, apiURL string) (TLS, error) {
	req, _ := NewRequest("GET", apiURL, nil)
	req = req.WithContext(ctx)

	resp, err := cl.Do(req)
	if err != nil {
		return TLS{}, err
	}
	defer resp.Body.Close()
	r := Ja3Response{}
	err = json.NewDecoder(resp.Body).Decode(&r)
	if err != nil {
		return TLS{}, err
	}
	return r.TLS, nil
}

func newTransportWithHelloAndProxy(proto string, hello tls.ClientHelloID, proxy ...string) *Transport {
	var proxyfn func(*Request) (*url.URL, error)
	if len(proxy) > 0 {
		p, _ := url.Parse(proxy[0])
		proxyfn = ProxyURL(p)
	}
	if proto == "http/1.1" { // Disable HTTP/2
		return &Transport{
			TLSClientConfig: &tls.Config{},
			TLSNextProto:    nil,
			Proxy:           proxyfn,
			ClientHelloSettings: ClientHelloSettings{
				HelloID: hello,
			},
		}
	}
	return &Transport{
		Proxy: proxyfn,
		ClientHelloSettings: ClientHelloSettings{
			HelloID: hello,
		},
	}
}

// Test that UTLS parrots are being correctly applied for h1/h2 with/without a proxy
func TestClientHelloID(t *testing.T) {
	ja3API := "https://tools.scrapfly.io/api/fp/ja3"

	hellos := map[string]tls.ClientHelloID{
		"chrome":  tls.HelloChrome_Auto,
		"firefox": tls.HelloFirefox_Auto,
		"edge":    tls.HelloEdge_Auto,
		"ios":     tls.HelloIOS_Auto,
	}
	for _, proto := range []string{"http/1.1", "h2"} {
		for _, useProxy := range []bool{false, true} {
			t.Run("ClientHelloID_"+proto+"_Proxy:"+fmt.Sprint(useProxy), func(t *testing.T) {
				t.Parallel()

				proxy := []string{}
				if useProxy {
					// If using an mitm proxy make sure ssl decryption is disabled otherwise
					// it will hijack the tls negotiation with its own handshake
					proxy = []string{"http://127.0.0.1:8888"}
				}

				hk := ""
				trace := &httptrace.ClientTrace{
					TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
						hk += cs.NegotiatedProtocol + " "
					},
				}
				ctx := httptrace.WithClientTrace(context.Background(), trace)

				cl := &Client{}
				ja3s := map[string]TLS{}
				for helloName := range hellos {
					cl = &Client{Transport: newTransportWithHelloAndProxy(proto, hellos[helloName], proxy...)}
					ja3, err := getja3(ctx, cl, ja3API)
					if err != nil {
						t.Fatalf("Get failed: %v", err)
					}
					ja3s[helloName] = ja3
				}

				want := fmt.Sprintf("%v %v %v %v ", proto, proto, proto, proto)
				if hk != want {
					t.Errorf("NegotiatedProtocol not in expected order\ngot : %v\nwant: %v", hk, want)
				}

				for helloID, hello := range hellos {
					spec, _ := tls.UTLSIdToSpec(hello)
					if len(ja3s[helloID].Ciphers) != len(spec.CipherSuites) {
						t.Errorf("Expected %d Ciphers, got %d", len(spec.CipherSuites), len(ja3s[helloID].SignatureAlgorithms))
					}
					if len(ja3s[helloID].Extensions) != len(spec.Extensions) {
						t.Errorf("Expected %d Extensions, got %d", len(spec.Extensions), len(ja3s[helloID].Extensions))
					}

				}
			})
		}
	}
}

// Concurrent map read/write crash issue #3
func TestWriteSubsetConcurrentHeaderWrite(t *testing.T) {
	wg := sync.WaitGroup{}
	wg.Add(100)
	for i := 0; i < 100; i++ {
		go func() {
			h := Header{
				"User-Agent":   {"dhttp"},
				HeaderOrderKey: {"User-Agent"},
			}
			h.WriteSubset(io.Discard, respExcludeHeader)
			wg.Done()
		}()
	}
	wg.Wait()
}

func TestUserAgentMissingHeader(t *testing.T) {
	for _, proto := range []string{"h1", "h2"} {
		t.Run("Missing-UserAgent-Proto_"+proto, func(t *testing.T) {
			t.Parallel()

			var cl Client
			if proto == "h1" {
				cl = Client{Transport: &Transport{
					TLSClientConfig: &tls.Config{},
					TLSNextProto:    make(map[string]func(authority string, c *tls.UConn) RoundTripper), // Disable HTTP/2
				}}
			} else {
				cl = Client{
					Transport: &Transport{},
				}
			}

			req, _ := NewRequest("GET", "https://httpbin.dev/headers", nil)
			req.Header = Header{
				"User-Agent": {"xyz"},
				HeaderOrderKey: {
					"User-Agent",
				},
			}
			resp, err := cl.Do(req)
			if err != nil {
				t.Fatalf("Get failed: %v", err)
			}

			body := struct {
				Headers map[string][]string `json:"headers"`
			}{}

			err = json.NewDecoder(resp.Body).Decode(&body)
			if err != nil {
				t.Fatalf("Decode failed: %v", err)
			}

			ua := body.Headers["User-Agent"]
			if len(ua) != 1 {
				t.Fatalf("Expected 1 User-Agent header, got %d", len(ua))
			}
			expected := "xyz"
			if ua[0] != expected {
				t.Errorf("Expected User-Agent header to be %q, got %q", expected, ua[0])
			}

		})
	}
}

func TestTransferWriterHeaderShim(t *testing.T) {
	cl := Client{
		Transport: &Transport{
			TLSClientConfig: &tls.Config{},
			TLSNextProto:    make(map[string]func(authority string, c *tls.UConn) RoundTripper), // Disable HTTP/2
		},
	}
	req, _ := NewRequest("POST", "https://httpbin.dev/post", bytes.NewBufferString("sdfs"))
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("Expected status code 200, got %d", resp.StatusCode)
		t.Logf("Response: %s", resp.Status)
		return
	}
	io.ReadAll(resp.Body) // Read the body to ensure no errors occur
	t.Fail()
}
