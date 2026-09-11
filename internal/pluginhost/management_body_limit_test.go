package pluginhost

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Public plugin routes are unauthenticated by design: the plugin owns the
// session check, which cannot run until the request is forwarded. The body must
// therefore be bounded before any credential is evaluated, otherwise an
// anonymous caller could exhaust host memory.
func TestReadPluginRequestBodyRejectsOversizeBody(t *testing.T) {
	oversize := bytes.Repeat([]byte("a"), int(maxPluginRequestBodyBytes)+1)
	request := httptest.NewRequest(http.MethodPost, "/v0/control-plane/organizations", bytes.NewReader(oversize))

	body, tooLarge, errRead := readPluginRequestBody(request)
	if errRead != nil {
		t.Fatalf("readPluginRequestBody() error = %v, want a clean oversize report", errRead)
	}
	if !tooLarge {
		t.Fatal("tooLarge = false, want an oversize body to be rejected")
	}
	if body != nil {
		t.Fatalf("body = %d bytes, want nothing buffered for an oversize request", len(body))
	}
}

// A body exactly at the limit is legitimate and must still be accepted.
func TestReadPluginRequestBodyAcceptsBodyAtLimit(t *testing.T) {
	atLimit := bytes.Repeat([]byte("b"), int(maxPluginRequestBodyBytes))
	request := httptest.NewRequest(http.MethodPost, "/v0/control-plane/organizations", bytes.NewReader(atLimit))

	body, tooLarge, errRead := readPluginRequestBody(request)
	if errRead != nil || tooLarge {
		t.Fatalf("readPluginRequestBody() = tooLarge %v err %v, want acceptance", tooLarge, errRead)
	}
	if int64(len(body)) != maxPluginRequestBodyBytes {
		t.Fatalf("body = %d bytes, want %d", len(body), maxPluginRequestBodyBytes)
	}
}

func TestReadPluginRequestBodyHandlesSmallAndAbsentBodies(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v0/control-plane/organizations", strings.NewReader(`{"name":"x"}`))
	body, tooLarge, errRead := readPluginRequestBody(request)
	if errRead != nil || tooLarge || string(body) != `{"name":"x"}` {
		t.Fatalf("small body = %q tooLarge %v err %v", string(body), tooLarge, errRead)
	}

	empty := httptest.NewRequest(http.MethodGet, "/v0/control-plane/organizations", nil)
	empty.Body = nil
	body, tooLarge, errRead = readPluginRequestBody(empty)
	if errRead != nil || tooLarge || len(body) != 0 {
		t.Fatalf("absent body = %q tooLarge %v err %v", string(body), tooLarge, errRead)
	}
}

// A failing reader must surface as a read error, not as an oversize rejection.
func TestReadPluginRequestBodyReportsReadFailure(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v0/control-plane/organizations", nil)
	request.Body = io.NopCloser(failingReader{})

	_, tooLarge, errRead := readPluginRequestBody(request)
	if errRead == nil {
		t.Fatal("errRead = nil, want the underlying read failure")
	}
	if tooLarge {
		t.Fatal("tooLarge = true, want a read failure to be distinguishable")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
