package xiaomi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Shared by all clients and all MiNA endpoints, including readback and search.
var cloudRequests = make(chan struct{}, 4)

type CommandRejected struct{ Code string }

func (e *CommandRejected) Error() string {
	return fmt.Sprintf("音箱未接受指令（code=%s）", e.Code)
}

func (c *Client) PlayURL(ctx context.Context, t Tokens, id, hardware, raw, audioID string) error {
	key := t.UserID + "/" + id + "/" + hardware
	c.preferenceMu.Lock()
	music, known := c.playPreferences[key]
	c.preferenceMu.Unlock()
	if !known {
		music = musicModels[hardware]
	}
	err := c.playURL(ctx, t, id, raw, audioID, music)
	var rejected *CommandRejected
	// A timeout has an ambiguous outcome: do not submit a second play command.
	if errors.As(err, &rejected) && ctx.Err() == nil {
		music = !music
		err = c.playURL(ctx, t, id, raw, audioID, music)
	}
	if err == nil {
		c.preferenceMu.Lock()
		if c.playPreferences == nil {
			c.playPreferences = map[string]bool{}
		}
		if len(c.playPreferences) >= 512 {
			clear(c.playPreferences)
		}
		c.playPreferences[key] = music
		c.preferenceMu.Unlock()
	}
	return err
}

type PlayerStatus struct {
	Volume *int   `json:"volume,omitempty"`
	State  string `json:"state,omitempty"`
}

func (c *Client) PlayerStatus(ctx context.Context, t Tokens, id string) (PlayerStatus, error) {
	m, err := c.mina(ctx, t, "/remote/ubus", url.Values{"deviceId": {id}, "method": {"player_get_play_status"}, "path": {"mediaplayer"}, "message": {"{}"}})
	if err != nil {
		return PlayerStatus{}, err
	}
	var payload any
	if err = json.Unmarshal(m["data"], &payload); err != nil {
		return PlayerStatus{}, errors.New("音箱状态无法识别")
	}
	status := parsePlayerStatus(payload, 0)
	if status.Volume == nil && status.State == "" {
		return status, errors.New("音箱未返回可识别的状态")
	}
	return status, nil
}

func parsePlayerStatus(value any, depth int) PlayerStatus {
	var out PlayerStatus
	if depth > 6 {
		return out
	}
	if text, ok := value.(string); ok {
		var nested any
		if len(text) < 65536 && json.Unmarshal([]byte(text), &nested) == nil {
			return parsePlayerStatus(nested, depth+1)
		}
		return out
	}
	m, ok := value.(map[string]any)
	if !ok {
		return out
	}
	number := func(v any) (int, bool) {
		switch n := v.(type) {
		case float64:
			return int(n), n == float64(int(n))
		case string:
			i, err := strconv.Atoi(n)
			return i, err == nil
		}
		return 0, false
	}
	for _, key := range []string{"volume", "volume_level", "volumeLevel"} {
		if v, ok := number(m[key]); ok && v >= 0 && v <= 100 {
			out.Volume = &v
			break
		}
	}
	for _, key := range []string{"status", "play_status", "playStatus"} {
		if v, ok := number(m[key]); ok {
			switch v {
			case 0:
				out.State = "stopped"
			case 1:
				out.State = "playing"
			case 2:
				out.State = "paused"
			}
		} else if text, ok := m[key].(string); ok {
			switch strings.ToLower(text) {
			case "playing", "paused", "stopped":
				out.State = strings.ToLower(text)
			}
		}
		if out.State != "" {
			break
		}
	}
	for _, key := range []string{"info", "data", "play_info", "player"} {
		nested := parsePlayerStatus(m[key], depth+1)
		if out.Volume == nil {
			out.Volume = nested.Volume
		}
		if out.State == "" {
			out.State = nested.State
		}
	}
	return out
}
