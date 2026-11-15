package orchard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse-tart/internal/config"
)

func TestHTTPBoundaries(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/prefix/v1/vms" {
					t.Error(r.URL.Path)
				}
				w.WriteHeader(status)
				io.WriteString(w, "secret backend error")
			}))
			defer s.Close()
			c, e := New(config.Orchard{URL: s.URL + "/prefix/v1/", AllowInsecureHTTP: true})
			if e != nil {
				t.Fatal(e)
			}
			_, e = c.List(t.Context())
			if !IsStatus(e, status) {
				t.Fatal(e)
			}
		})
	}
	for _, body := range []string{"null", "{}", "[", `[] junk`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		c, _ := New(config.Orchard{URL: s.URL, AllowInsecureHTTP: true})
		if _, e := c.List(t.Context()); e == nil {
			t.Error("accepted", body)
		}
		s.Close()
	}
}
func TestBasicAndRedirect(t *testing.T) {
	t.Setenv("ORCHARD_TEST_TOKEN", "sentinel")
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer dst.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "service" || p != "sentinel" {
			t.Error("auth missing")
		}
		http.Redirect(w, r, dst.URL, 307)
	}))
	defer s.Close()
	c, e := New(config.Orchard{URL: s.URL, AllowInsecureHTTP: true, Username: "service", Token: "sentinel"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.List(t.Context()); !IsStatus(e, 307) {
		t.Fatal(e)
	}
}
func TestTLSAndCancellation(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "[]") }))
	defer s.Close()
	c, _ := New(config.Orchard{URL: s.URL})
	if _, e := c.List(t.Context()); e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	c.HTTP = s.Client()
	if _, e := c.List(t.Context()); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, e := c.List(ctx); e == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestSuspendPreservesUnknownFieldsAndUID(t *testing.T) {
	var puts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			fmt.Fprint(w, `{"uid":"owned","powerState":"running","os":"darwin","futureField":{"keep":true}}`)
			return
		}
		puts++
		var got map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if string(got["powerState"]) != `"suspended"` || string(got["futureField"]) != `{"keep":true}` {
			t.Error("partial PUT", got)
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	c, err := New(config.Orchard{URL: server.URL, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Suspend(t.Context(), "name", "replacement"); err == nil || puts != 0 {
		t.Fatal("UID mismatch wrote")
	}
	if err = c.Suspend(t.Context(), "name", "owned"); err != nil || puts != 1 {
		t.Fatal(err, puts)
	}
}

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type diagnosticBody struct {
	data  []byte
	read  int
	cause error
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		if b.cause != nil {
			return 0, b.cause
		}
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	b.read += n
	return n, nil
}
func (*diagnosticBody) Close() error { return nil }
func TestDiagnosticHTTPErrorCapture(t *testing.T) {
	for _, size := range []int{16383, 16384, 16385, 1 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := &diagnosticBody{data: []byte(strings.Repeat("x", size))}
			c, _ := New(config.Orchard{URL: "http://fixture.invalid", AllowInsecureHTTP: true})
			calls := 0
			c.HTTP.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 503, Body: body, Header: make(http.Header)}, nil
			})
			_, err := c.List(t.Context())
			var status *Error
			prefix := strings.Repeat("x", min(size, 16384))
			want := prefix
			if size > 16384 {
				want += "[truncated after 16384 bytes]"
			}
			if !IsStatus(err, 503) || !errors.As(err, &status) || !strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "[truncated") != (size > 16384) || body.read > 16385 || calls != 1 {
				t.Fatalf("capture size=%d read=%d calls=%d err length=%d", size, body.read, calls, len(err.Error()))
			}
		})
	}
	raw := errors.New("reader failed\nrequest=\"synthetic\"")
	for _, size := range []int{7, 16385} {
		t.Run(fmt.Sprintf("read-fault-%d", size), func(t *testing.T) {
			c, _ := New(config.Orchard{URL: "http://fixture.invalid", AllowInsecureHTTP: true})
			body := &diagnosticBody{data: []byte(strings.Repeat("y", size)), cause: raw}
			c.HTTP.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 503, Body: body, Header: make(http.Header)}, nil
			})
			_, err := c.List(t.Context())
			if !IsStatus(err, 503) || (size < 16385 && !errors.Is(err, raw)) || !strings.Contains(err.Error(), strings.Repeat("y", min(size, 16384))) {
				t.Fatal(err)
			}
		})
	}
}
func TestDiagnosticHTTPReturnedCauses(t *testing.T) {
	raw := errors.New("transport says\n\"preserve this\"")
	c, _ := New(config.Orchard{URL: "http://fixture.invalid", AllowInsecureHTTP: true})
	c.HTTP.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) { return nil, raw })
	_, err := c.List(t.Context())
	if !errors.Is(err, raw) || !errors.Is(err, Err) || !strings.Contains(err.Error(), raw.Error()) {
		t.Fatal(err)
	}
	c.Base = ":"
	if err = c.call(t.Context(), "GET", "/vms", nil, nil); err == nil || !strings.Contains(err.Error(), "construct request") {
		t.Fatal(err)
	}
	c.Base = "http://fixture.invalid"
	if err = c.call(t.Context(), "POST", "/vms", make(chan int), nil); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatal(err)
	}
	for _, body := range []string{"[{malformed PRIVATE-SUCCESS-BODY", strings.Repeat("z", MaxBody+1)} {
		c.HTTP.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		_, err = c.List(t.Context())
		if err == nil || strings.Contains(err.Error(), "PRIVATE-SUCCESS-BODY") {
			t.Fatal(err)
		}
	}
}

type diagnosticPartialBody struct {
	data  []byte
	cause error
	read  int
}

func (b *diagnosticPartialBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	b.read += n
	if len(b.data) == 0 {
		return n, b.cause
	}
	return n, nil
}
func (*diagnosticPartialBody) Close() error { return nil }
func TestDiagnosticPartialErrorBodyWithReadFailure(t *testing.T) {
	for _, size := range []int{8, 16385} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			raw := errors.New("partial response reader\n\"cause\"")
			body := &diagnosticPartialBody{data: []byte(strings.Repeat("p", size)), cause: raw}
			c, _ := New(config.Orchard{URL: "http://fixture.invalid", AllowInsecureHTTP: true})
			c.HTTP.Transport = diagnosticTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 503, Body: body, Header: make(http.Header)}, nil
			})
			_, e := c.List(t.Context())
			if !IsStatus(e, 503) || !errors.Is(e, raw) || !strings.Contains(e.Error(), strings.Repeat("p", min(size, 16384))) || strings.Contains(e.Error(), "[truncated") != (size > 16384) || body.read > 16385 {
				t.Fatal(e)
			}
		})
	}
}
