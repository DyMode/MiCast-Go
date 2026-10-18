package dlna

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLargePositionReplyUsesOriginalRendererContentLength(t *testing.T) {
	s := testRenderer(t)
	s.uri = "http://192.168.1.20/song.mp3"
	s.metadata = "<item><title>" + strings.Repeat("歌曲", 2048) + "</title></item>"
	server := httptest.NewServer(s)
	defer server.Close()
	body := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:GetPositionInfo xmlns:u="` + serviceTypes["AVTransport"] + `"><InstanceID>0</InstanceID></u:GetPositionInfo></s:Body></s:Envelope>`
	response, err := http.Post(server.URL+"/dlna/AVTransport/control", "text/xml", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.ContentLength != int64(len(data)) || len(response.TransferEncoding) != 0 {
		t.Fatalf("position response differs from original HTTP framing: length=%d actual=%d encoding=%v", response.ContentLength, len(data), response.TransferEncoding)
	}
}

func TestSubscriptionReplyHasAnExplicitEmptyBody(t *testing.T) {
	s := testRenderer(t)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer callback.Close()
	server := httptest.NewServer(s)
	defer server.Close()
	request, _ := http.NewRequest("SUBSCRIBE", server.URL+"/dlna/AVTransport/event", nil)
	request.Header.Set("NT", "upnp:event")
	request.Header.Set("CALLBACK", "<"+callback.URL+"/notify>")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	if response.ContentLength != 0 || len(response.TransferEncoding) != 0 {
		t.Fatalf("subscription response must be complete before initial NOTIFY: length=%d encoding=%v", response.ContentLength, response.TransferEncoding)
	}
}
