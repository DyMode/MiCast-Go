package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"micastgo/internal/app"
	"micastgo/internal/netutil"
	"micastgo/internal/receivers"
	"micastgo/internal/server"
	"micastgo/internal/store"
	"micastgo/internal/version"
	"micastgo/internal/xiaomi"
)

func main() {
	if err := run(); err != nil {
		slog.Error("启动失败", "error", err)
		os.Exit(1)
	}
}
func run() error {
	listen := flag.String("listen", "127.0.0.1:42310", "管理页面监听地址")
	dir := flag.String("data-dir", "data", "配置和凭证目录")
	backupUpgrade := flag.Bool("backup-upgrade", false, "备份升级配置后退出")
	restoreUpgrade := flag.Bool("restore-upgrade", false, "恢复升级缺失配置后退出")
	showKey := flag.Bool("show-access-key", false, "显示管理密钥后退出")
	setPassword := flag.Bool("set-admin-password", false, "从标准输入设置管理密码后退出")
	ffmpeg := flag.String("ffmpeg", "ffmpeg", "音频运行时路径")
	advertise := flag.String("advertise-ip", "", "音箱和手机可访问的本机 IPv4 地址")
	raopAddress := flag.String("airplay-listen", "0.0.0.0:42510", "经典 AirPlay 监听地址")
	noReceivers := flag.Bool("no-receivers", false, "仅启动管理服务")
	noDiscovery := flag.Bool("no-discovery", false, "关闭组播广播，仅用于开发测试")
	uds := flag.String("gateway-socket", "", "飞牛统一网关 Unix Socket")
	pidFile := flag.String("pid-file", "", "服务 PID 文件")
	printVersion := flag.Bool("version", false, "显示版本后退出")
	flag.Parse()
	if *printVersion {
		fmt.Println(version.Version)
		return nil
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if *backupUpgrade {
		return store.BackupUpgrade(abs)
	}
	if *restoreUpgrade {
		return store.RestoreUpgrade(abs)
	}
	if *setPassword {
		password, err := io.ReadAll(io.LimitReader(os.Stdin, 1025))
		if err != nil {
			return err
		}
		return store.SetAdminPassword(abs, string(password))
	}
	key, err := store.LoadOrCreateKey(filepath.Join(abs, "access.key"))
	if err != nil {
		return err
	}
	if *showKey {
		fmt.Println(hex.EncodeToString(key))
		return nil
	}
	config, err := store.NewConfig(abs)
	if err != nil {
		return err
	}
	vault, err := store.NewVault(abs)
	if err != nil {
		return err
	}
	client := xiaomi.NewClient()
	defer client.HTTP.CloseIdleConnections()
	application, err := app.New(config, vault, client)
	if err != nil {
		return err
	}
	listener, err := netutil.ListenTCP("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var routes *receivers.Manager
	if !*noReceivers {
		host, iface, err := advertiseIP(*advertise)
		if err != nil {
			listener.Close()
			return err
		}
		base := publicBase(host, listener)
		routes, err = receivers.New(ctx, application, receivers.Options{Host: host, Base: base, Binary: *ffmpeg, CacheDir: filepath.Join(abs, "cache"), Identity: hex.EncodeToString(key), AirplayAddress: *raopAddress, Interface: iface, Discovery: !*noDiscovery})
		if err != nil {
			listener.Close()
			return err
		}
		defer routes.Close()
		slog.Info("投放网络已配置", "host", host, "stream", base, "airplay", *raopAddress)
	}
	access, err := server.NewAccessControl(abs, key)
	if err != nil {
		listener.Close()
		return err
	}
	management := server.New(application, key, server.Options{Routes: routes, Access: access})
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if routes != nil && (strings.HasPrefix(r.URL.Path, "/stream/") || strings.HasPrefix(r.URL.Path, "/dlna/") || strings.HasPrefix(r.URL.Path, "/receivers/")) {
			routes.ServeHTTP(w, r)
			return
		}
		management.ServeHTTP(w, r)
	})
	handler := gatewayHandler(root)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768, BaseContext: func(net.Listener) context.Context { return ctx }}
	var gateway *http.Server
	if *uds != "" {
		socket, err := unixListener(*uds)
		if err != nil {
			listener.Close()
			return err
		}
		defer os.Remove(*uds)
		gateway = &http.Server{Handler: gatewayHandler(server.New(application, key, server.Options{Routes: routes, Access: access, TrustedGateway: true})), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
		go func() {
			if err := gateway.Serve(socket); err != nil && err != http.ErrServerClosed {
				slog.Error("网关已停止", "error", err)
				cancel()
			}
		}()
	}
	workerDone := make(chan struct{})
	if *pidFile != "" {
		if err := store.AtomicWrite(*pidFile, []byte(fmt.Sprint(os.Getpid()))); err != nil {
			listener.Close()
			return err
		}
		defer func() {
			data, _ := os.ReadFile(*pidFile)
			if string(data) == fmt.Sprint(os.Getpid()) {
				os.Remove(*pidFile)
			}
		}()
	}
	go func() { defer close(workerDone); application.Run(ctx) }()
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
		if gateway != nil {
			_ = gateway.Shutdown(shutdown)
		}
	}()
	slog.Info("MiCast Go 已启动", "address", listener.Addr().String(), "data", abs)
	slog.Info("管理页可设置或关闭登录密码")
	err = srv.Serve(listener)
	cancel()
	<-workerDone
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
