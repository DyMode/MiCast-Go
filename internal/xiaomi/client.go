// Package xiaomi implements the MiCast Xiaomi account and MiNA protocols.
// Protocol reference: ../MiCast/micast/xiaomi (MIT; see LICENSE).
package xiaomi

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const userAgent = "APP/com.xiaomi.mihome APPV/6.0.103 iosPassportSDK/3.9.0 iOS/14.4 miHSTS"

type HTTPStatusError struct{ StatusCode int }

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("小米云端 HTTP %d", e.StatusCode) }

var ErrRejected = errors.New("小米账号凭证已失效，请重新登录")
var ErrRateLimited = errors.New("小米账号请求过于频繁，请稍后重试")

type Tokens struct {
	Rejected     bool      `json:"rejected,omitempty"`
	UserID       string    `json:"user_id"`
	PassToken    string    `json:"pass_token"`
	DeviceID     string    `json:"device_id"`
	Security     string    `json:"security"`
	ServiceToken string    `json:"service_token"`
	RefreshedAt  time.Time `json:"refreshed_at"`
}
type QR struct {
	LoginURL string
	PollURL  string
	DeviceID string
}
type Device struct {
	ID       string `json:"deviceID"`
	Name     string `json:"name"`
	Hardware string `json:"hardware"`
	IP       string `json:"address"`
}
type Client struct {
	preferenceMu    sync.Mutex
	playPreferences map[string]bool
	HTTP            *http.Client
	AccountBase     string
	MinaBase        string
}

func NewClient() *Client {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 25 * time.Second, MaxConnsPerHost: 6, MaxIdleConnsPerHost: 6, IdleConnTimeout: 60 * time.Second}
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second, Transport: transport, Jar: jar, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 8 {
			return errors.New("too many redirects")
		}
		if !trustedURL(r.URL.String()) {
			return errors.New("unexpected Xiaomi redirect host")
		}
		return nil
	}}, AccountBase: "https://account.xiaomi.com", MinaBase: "https://api2.mina.mi.com"}
}
func trustedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "xiaomi.com" || strings.HasSuffix(h, ".xiaomi.com") || h == "mi.com" || strings.HasSuffix(h, ".mi.com")
}
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (c *Client) request(ctx context.Context, method, raw string, form url.Values, cookies map[string]string) (map[string]json.RawMessage, *http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, nil, errors.New("无效的小米服务地址")
	}
	req.Header.Set("User-Agent", userAgent)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, errors.New("无法连接小米云端，请检查网络后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return nil, resp, ErrRateLimited
	}
	if resp.StatusCode != 200 {
		return nil, resp, &HTTPStatusError{StatusCode: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil {
		return nil, resp, errors.New("读取小米云端回应失败")
	}
	if len(b) > 2*1024*1024 {
		return nil, resp, errors.New("小米云端回应过大")
	}
	var value map[string]json.RawMessage
	err = json.Unmarshal([]byte(strings.TrimPrefix(string(b), "&&&START&&&")), &value)
	if err != nil {
		return nil, resp, errors.New("小米云端回应无法识别")
	}
	return value, resp, nil
}
func str(m map[string]json.RawMessage, key string) string {
	b := m[key]
	if len(b) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(b, &n) == nil {
		return n.String()
	}
	return ""
}
func code(m map[string]json.RawMessage) int { n, _ := strconv.Atoi(str(m, "code")); return n }
func (c *Client) StartQR(ctx context.Context) (QR, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	device := randomHex(8)
	q := url.Values{"_qrsize": {"480"}, "qs": {"%3Fsid%3Dxiaomiio%26_json%3Dtrue"}, "callback": {"https://sts.api.io.mi.com/sts"}, "_hasLogo": {"false"}, "sid": {"xiaomiio"}, "serviceParam": {""}, "_locale": {"zh_CN"}, "_dc": {strconv.FormatInt(time.Now().UnixMilli(), 10)}}
	m, _, err := c.request(ctx, "GET", c.AccountBase+"/longPolling/loginUrl?"+q.Encode(), nil, map[string]string{"sdkVersion": "accountsdk-18.8.15", "deviceId": device})
	if err != nil {
		return QR{}, err
	}
	login, lp := str(m, "loginUrl"), str(m, "lp")
	if login == "" || !trustedURL(login) || !trustedURL(lp) {
		return QR{}, errors.New("未获取到有效的小米登录二维码")
	}
	return QR{login, lp, device}, nil
}
func (c *Client) PollQR(ctx context.Context, qr QR) (string, *Tokens, QR, error) {
	if !trustedURL(qr.PollURL) {
		return "", nil, qr, errors.New("无效的登录轮询地址")
	}
	parentCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 22*time.Second)
	defer cancel()
	m, _, err := c.request(ctx, "GET", qr.PollURL, nil, nil)
	if err != nil {
		var httpErr *HTTPStatusError
		if errors.Is(err, ErrRateLimited) || (errors.As(err, &httpErr) && httpErr.StatusCode == 403) {
			return "waiting_limited", nil, qr, nil
		}

		if errors.Is(ctx.Err(), context.DeadlineExceeded) && ctx.Err() != context.Canceled {
			return "waiting", nil, qr, nil
		}
		return "", nil, qr, err
	}
	if code(m) == 70016 {
		return "expired", nil, qr, nil
	}
	if pass := str(m, "passToken"); pass != "" {
		user := str(m, "userId")
		if user == "" {
			return "", nil, qr, errors.New("登录结果缺少账号信息")
		}
		sum := md5.Sum([]byte("micast-go-" + user))
		device := hex.EncodeToString(sum[:])[:16]
		tokens, err := c.Exchange(parentCtx, Tokens{UserID: user, PassToken: pass, DeviceID: device})
		return "confirmed", &tokens, qr, err
	}
	if lp := str(m, "lp"); lp != "" {
		if !trustedURL(lp) {
			return "", nil, qr, errors.New("无效的登录轮询地址")
		}
		qr.PollURL = lp
	}
	return "waiting", nil, qr, nil
}
func (c *Client) Exchange(ctx context.Context, t Tokens) (Tokens, error) {
	cookies := map[string]string{"sdkVersion": "accountsdk-18.8.15", "deviceId": t.DeviceID, "userId": t.UserID, "passToken": t.PassToken}
	m, _, err := c.request(ctx, "GET", c.AccountBase+"/pass/serviceLogin?sid=micoapi&_json=true", nil, cookies)
	if err != nil {
		return t, err
	}
	if code(m) == 70022 {
		return t, ErrRateLimited
	}
	// Only explicit account credential rejections expire a saved login.
	if code(m) == 70016 || code(m) == 70001 {
		return t, ErrRejected
	}
	security, location, nonce := str(m, "ssecurity"), str(m, "location"), str(m, "nonce")
	if code(m) != 0 || security == "" || nonce == "" || !trustedURL(location) {
		return t, errors.New("小米登录验证未完成，请稍后重试")
	}
	digest := sha1.Sum([]byte("nonce=" + nonce + "&" + security))
	u, _ := url.Parse(location)
	q := u.Query()
	q.Set("clientSign", base64.StdEncoding.EncodeToString(digest[:]))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return t, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return t, errors.New("无法完成小米登录验证")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	service := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "serviceToken" {
			service = cookie.Value
		}
	}
	if service == "" && c.HTTP.Jar != nil {
		for _, target := range []*url.URL{u, resp.Request.URL} {
			for _, cookie := range c.HTTP.Jar.Cookies(target) {
				if cookie.Name == "serviceToken" {
					service = cookie.Value
				}
			}
		}
	}
	if service == "" {
		return t, errors.New("小米未返回服务凭证")
	}
	t.Security = security
	t.ServiceToken = service
	t.Rejected = false
	t.RefreshedAt = time.Now()
	return t, nil
}
func (c *Client) mina(ctx context.Context, t Tokens, path string, form url.Values) (map[string]json.RawMessage, error) {
	select {
	case cloudRequests <- struct{}{}:
		defer func() { <-cloudRequests }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if t.UserID == "" || t.ServiceToken == "" {
		return nil, ErrRejected
	}
	requestID := "app_ios_" + randomHex(15)
	method := "GET"
	raw := c.MinaBase + path
	if form != nil {
		method = "POST"
		form.Set("requestId", requestID)
	} else {
		sep := "?"
		if strings.Contains(raw, "?") {
			sep = "&"
		}
		raw += sep + "requestId=" + requestID
	}
	m, _, err := c.request(ctx, method, raw, form, map[string]string{"userId": t.UserID, "serviceToken": t.ServiceToken})
	if err != nil {
		return nil, err
	}
	if _, ok := m["code"]; !ok || code(m) != 0 {
		return nil, fmt.Errorf("小米播放服务请求失败（code=%s）", str(m, "code"))
	}
	return m, nil
}
func (c *Client) Devices(ctx context.Context, t Tokens) ([]Device, error) {
	m, err := c.mina(ctx, t, "/admin/v2/device_list?master=0", nil)
	if err != nil {
		return nil, err
	}
	var result []Device
	if err = json.Unmarshal(m["data"], &result); err != nil || result == nil {
		return nil, errors.New("音箱列表格式无法识别")
	}
	return result, nil
}
func (c *Client) Command(ctx context.Context, t Tokens, id, method string, message map[string]any) error {
	b, err := json.Marshal(message)
	if err != nil {
		return err
	}
	m, err := c.mina(ctx, t, "/remote/ubus", url.Values{"deviceId": {id}, "method": {method}, "path": {"mediaplayer"}, "message": {string(b)}})
	if err != nil {
		return err
	}
	var detail map[string]json.RawMessage
	if json.Unmarshal(m["data"], &detail) == nil && code(detail) != 0 {
		return &CommandRejected{Code: str(detail, "code")}
	}
	return nil
}

var musicModels = map[string]bool{"LX04": true, "LX05": true, "L05B": true, "L05C": true, "L06": true, "L06A": true, "X08A": true, "X10A": true, "X08C": true, "X08E": true, "X8F": true, "X4B": true, "OH2": true, "OH2P": true, "X6A": true}

func (c *Client) playURL(ctx context.Context, t Tokens, id, raw, audioID string, musicMethod bool) error {
	if !musicMethod {
		return c.Command(ctx, t, id, "player_play_url", map[string]any{"media": "app_ios", "url": raw, "type": 2})
	}
	if audioID == "" {
		audioID = "1582971365183456177"
	}
	music := map[string]any{"payload": map[string]any{"audio_type": "", "audio_items": []any{map[string]any{"item_id": map[string]any{"audio_id": audioID, "cp": map[string]any{"album_id": "-1", "episode_index": 0, "id": "355454500", "name": "xiaowei"}}, "stream": map[string]any{"url": raw}}}, "list_params": map[string]any{"listId": "-1", "loadmore_offset": 0, "origin": "xiaowei", "type": "MUSIC"}}, "play_behavior": "REPLACE_ALL"}
	b, _ := json.Marshal(music)
	return c.Command(ctx, t, id, "player_play_music", map[string]any{"startaudioid": audioID, "music": string(b)})
}
