package xiaomi

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (c *Client) SearchTrack(ctx context.Context, t Tokens, title, artist string) (string, error) {
	title, artist = normalizeMetadata(title, artist)
	if title == "" {
		return "", nil
	}
	for _, p := range []string{"作词", "作曲", "编曲", "演唱", "词：", "曲："} {
		if strings.HasPrefix(title, p) {
			return "", nil
		}
	}
	query := title
	if artist != "" {
		query += "-" + artist
	}
	m, err := c.mina(ctx, t, "/music/search", url.Values{"query": {query}, "queryType": {"1"}, "offset": {"0"}, "count": {"6"}, "timestamp": {strconv.FormatInt(time.Now().UnixMilli(), 10)}})
	if err != nil {
		return "", err
	}
	var data struct {
		Songs []struct {
			Name    string          `json:"name"`
			AudioID json.RawMessage `json:"audioID"`
			Artist  struct {
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"songList"`
	}
	if err = json.Unmarshal(m["data"], &data); err != nil {
		return "", err
	}
	for _, song := range data.Songs {
		if !strings.EqualFold(normalizeSpace(song.Name), title) {
			continue
		}
		sa := strings.ToLower(song.Artist.Name)
		a := strings.ToLower(artist)
		if a != "" && (sa == "" || (!strings.Contains(sa, a) && !strings.Contains(a, sa))) {
			continue
		}
		id := str(map[string]json.RawMessage{"id": song.AudioID}, "id")
		if id != "" {
			return id, nil
		}
	}
	return "", nil
}
