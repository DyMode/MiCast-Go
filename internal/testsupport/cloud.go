// Package testsupport supplies isolated fake accounts for protocol integration tests.
package testsupport

import (
	"context"
	"errors"
	"micastgo/internal/app"
	"micastgo/internal/store"
	"micastgo/internal/xiaomi"
	"testing"
	"time"
)

type Play struct{ ID, URL, AudioID string }
type Cloud struct {
	Plays         chan Play
	DevicesList   []xiaomi.Device
	BeforePlay    func(context.Context, Play) error
	BeforeCommand func(context.Context, string, string, map[string]any) error
}

func (c *Cloud) StartQR(context.Context) (xiaomi.QR, error) {
	return xiaomi.QR{}, errors.New("test has no real account")
}
func (c *Cloud) PollQR(context.Context, xiaomi.QR) (string, *xiaomi.Tokens, xiaomi.QR, error) {
	return "", nil, xiaomi.QR{}, errors.New("test has no real account")
}
func (c *Cloud) Exchange(context.Context, xiaomi.Tokens) (xiaomi.Tokens, error) {
	return xiaomi.Tokens{}, errors.New("test has no real account")
}
func (c *Cloud) Devices(context.Context, xiaomi.Tokens) ([]xiaomi.Device, error) {
	if c.DevicesList != nil {
		return c.DevicesList, nil
	}
	return []xiaomi.Device{{ID: "speaker", Name: "测试音箱", Hardware: "OH2"}}, nil
}
func (c *Cloud) Command(ctx context.Context, _ xiaomi.Tokens, id, method string, args map[string]any) error {
	if c.BeforeCommand != nil {
		return c.BeforeCommand(ctx, id, method, args)
	}
	return nil
}
func (c *Cloud) PlayURL(ctx context.Context, _ xiaomi.Tokens, id, hardware, url, audioID string) error {
	if c.BeforePlay != nil {
		if err := c.BeforePlay(ctx, Play{id, url, audioID}); err != nil {
			return err
		}
	}
	select {
	case c.Plays <- Play{id, url, audioID}:
	default:
	}
	return nil
}
func NewApp(t *testing.T, format string) (*app.App, *Cloud) {
	t.Helper()
	dir := t.TempDir()
	config, err := store.NewConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = config.Update(func(c *store.Config) error {
		c.Owner = "test-user"
		c.Format = format
		c.Speakers["speaker"] = store.Speaker{ID: "speaker", Name: "测试音箱", Hardware: "OH2", Joined: true, WholeHouse: true, Volume: 20, Format: "auto"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	vault, err := store.NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.Save(xiaomi.Tokens{UserID: "test-user", ServiceToken: "test-service", RefreshedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cloud := &Cloud{Plays: make(chan Play, 64)}
	application, err := app.New(config, vault, cloud)
	if err != nil {
		t.Fatal(err)
	}
	return application, cloud
}
