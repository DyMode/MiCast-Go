package xiaomi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMusicMatchRequiresTitleAndArtistAndPreservesID(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		if r.URL.Path != "/music/search" || r.Form.Get("query") != "歌曲-歌手" {
			t.Error("incorrect music search request")
		}
		w.Write([]byte(`{"code":0,"data":{"songList":[{"name":"其他歌曲","audioID":"wrong","artist":{"name":"歌手"}},{"name":"歌曲","audioID":1234567890123456789,"artist":{"name":"歌手"}}]}}`))
	}))
	defer server.Close()
	client := NewClient()
	client.MinaBase = server.URL
	tokens := Tokens{UserID: "test", ServiceToken: "secret"}
	id, err := client.SearchTrack(context.Background(), tokens, "歌曲", "歌手")
	if err != nil || id != "1234567890123456789" {
		t.Fatal("music ID mismatch", id, err)
	}
	id, err = client.SearchTrack(context.Background(), tokens, "作词：某某", "")
	if err != nil || id != "" || calls != 1 {
		t.Fatal("credit line was searched")
	}
}
