package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/JustIkra/umir-serving/bootstrap"
)

type serviceKeys struct{ Discovery, Lifecycle, Gateway, Warehouse string }

func compose(config ownerConfig, keys serviceKeys, now func() time.Time, epoch string, client *http.Client) (*bootstrap.HTTPHandler, error) {
	return bootstrap.NewProcessHTTP(bootstrap.ProcessHTTPOptions{
		HTTPOptions:     bootstrap.HTTPOptions{WarehouseInternalURL: config.ServiceURLs.Warehouse, WarehouseTimeout: 5 * time.Second, RequestTimeout: 5 * time.Second, MaxRequestBytes: 1 << 20, Now: now, NewID: entityID, HTTPClient: client},
		AuthInternalURL: config.ServiceURLs.Auth, AuthTimeout: 5 * time.Second, ServingAuthKey: keys.Discovery, AuthRuntimeKey: keys.Lifecycle, GatewayServingKey: keys.Gateway, ServingWarehouseKey: keys.Warehouse, TZDataVersion: "go1.26.9-embedded", Epoch: epoch, RegistryPollInterval: time.Second,
	})
}
func entityID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
func ownerReady(ctx context.Context, app *bootstrap.HTTPHandler, key, epoch string) bool {
	if ctx.Err() != nil {
		return false
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://serving/internal/serving/v2/runtime", nil)
	if err != nil {
		return false
	}
	r.Header.Set("X-Umir-Service-Key", key)
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	var state struct {
		Service string `json:"service"`
		Epoch   string `json:"epoch"`
		Ready   bool   `json:"ready"`
		TZData  string `json:"tzdataVersion"`
	}
	return w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &state) == nil && state.Service == "serving" && state.Epoch == epoch && state.Ready && state.TZData == "go1.26.9-embedded" && ctx.Err() == nil
}
func readPrivate(path string, limit int) (result []byte, err error) {
	for parent := filepath.Dir(path); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		info, checkErr := os.Lstat(parent)
		if checkErr != nil || !info.IsDir() {
			return nil, errors.New("invalid private directory")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("invalid private directory")
		}
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("private file unavailable")
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, errors.New("private file unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || int(stat.Uid) != os.Getuid() || int(stat.Gid) != os.Getgid() || info.Size() < 1 || info.Size() > int64(limit) {
		return nil, errors.New("invalid private file")
	}
	result, err = io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(result) > limit {
		return nil, errors.New("private file exceeds bound")
	}
	return result, nil
}
func privateListener(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || int(stat.Uid) != os.Getuid() || int(stat.Gid) != os.Getgid() {
		return nil, errors.New("invalid control directory")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("control socket already exists")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(true)
	if err := os.Chmod(path, 0600); err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	return listener, nil
}
func serveListeners(ctx context.Context, business, control http.Handler, tcp, unix net.Listener) error {
	servers := []*http.Server{{Handler: business, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}, {Handler: control, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16384}}
	active, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, s := range servers {
		s.BaseContext = func(net.Listener) context.Context { return active }
	}
	done := make(chan error, 2)
	go func() { done <- servers[0].Serve(tcp) }()
	go func() { done <- servers[1].Serve(unix) }()
	var result error
	received := 0
	select {
	case <-ctx.Done():
	case err := <-done:
		received++
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	cancel()
	deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for _, s := range servers {
		if err := s.Shutdown(deadline); err != nil {
			result = errors.Join(result, err, s.Close())
		}
	}
	for received < 2 {
		err := <-done
		received++
		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.Join(result, err)
		}
	}
	return result
}
