package main

import (
	"errors"
	"net"
	"veil-desktop/internal/systemproxy"
	"veil-service/control"
)

func (a *App) SystemProxy() systemproxy.Status { return a.proxy.Status() }

func (a *App) SetSystemProxyMode(mode string) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	endpoint := ""
	if a.proxy.Status().Connection != "" {
		var err error
		if mode == "auto" {
			endpoint, err = a.catalogProxyEndpoint()
			if err != nil {
				return err
			}
		}
		return a.proxy.SetMode(mode, endpoint)
	}
	if mode == "auto" && a.manager.Status().State == "running" {
		var err error
		endpoint, err = a.proxyEndpoint()
		if err != nil {
			return err
		}
	}
	return a.proxy.SetMode(mode, endpoint)
}

func (a *App) ClearSystemProxy() error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	return a.proxy.Clear()
}

func (a *App) proxyEndpoint() (string, error) {
	// A pending saved profile is not the running listener. Remember its type when
	// start/restart succeeds rather than routing through an unapplied new profile.
	if !a.activeHTTP {
		return "", errors.New("automatic system proxy requires an HTTP or mixed client listener")
	}
	host, port, err := net.SplitHostPort(a.manager.Status().Listen)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	} else if host == "::" {
		host = "::1"
	}
	return net.JoinHostPort(host, port), nil
}

func (a *App) syncSystemProxy(q control.Request, r control.Response) control.Response {
	if a.manager.Status().State != "running" {
		if err := a.proxy.Restore(); err != nil && !errors.Is(err, systemproxy.ErrChanged) {
			failed := control.Fail("system_proxy_failed", err)
			status := a.manager.Status()
			failed.Status = &status
			return failed
		}
	}
	if r.Error != nil {
		return r
	}
	if q.Action == "start" || q.Action == "restart" {
		endpoint, err := a.proxyEndpoint()
		if a.proxy.Status().Mode == "auto" {
			if err == nil {
				err = a.proxy.Apply(endpoint)
			}
			if err != nil {
				// Do not report a fully connected automatic mode with an unusable proxy.
				a.manager.Handle(control.Request{Version: 1, Action: "stop"})
				restoreErr := a.proxy.Restore()
				failed := control.Fail("system_proxy_failed", errors.Join(err, restoreErr))
				status := a.manager.Status()
				failed.Status = &status
				return failed
			}
		}
	}
	return r
}

func (a *App) SetSystemProxyConnection(id string) error {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if id != "" {
		r := a.manager.Handle(control.Request{Version: 1, Action: "connections"})
		if r.Error != nil {
			return errors.New(r.Error.Message)
		}
		usable := false
		for _, row := range r.Connections {
			if row.ID != id || row.Kind != "connection" {
				continue
			}
			for _, inlet := range row.Inlets {
				usable = usable || inlet.Protocol == "mixed" || inlet.Protocol == "http"
			}
		}
		if !usable {
			return errors.New("choose a connection with an HTTP or mixed listener")
		}

	}
	if err := a.proxy.SelectConnection(id); err != nil {
		return err
	}
	return a.syncCatalogProxy()
}

func (a *App) catalogProxyEndpoint() (string, error) {
	id := a.proxy.Status().Connection
	r := a.manager.Handle(control.Request{Version: 1, Action: "connections"})
	if r.Error != nil {
		return "", errors.New(r.Error.Message)
	}
	for _, row := range r.Connections {
		if row.ID != id {
			continue
		}
		if row.State != "running" {
			return "", nil
		}
		for _, inlet := range row.Inlets {
			if inlet.Protocol != "mixed" && inlet.Protocol != "http" {
				continue
			}
			host, port, err := net.SplitHostPort(inlet.Listen)
			if err != nil {
				return "", err
			}
			if host == "" || host == "0.0.0.0" {
				host = "127.0.0.1"
			} else if host == "::" {
				host = "::1"
			}
			return net.JoinHostPort(host, port), nil
		}
		return "", errors.New("selected connection needs an HTTP or mixed listener for the system proxy")
	}
	return "", nil
}
func (a *App) syncCatalogProxy() error {
	if a.proxy.Status().Mode != "auto" {
		return nil
	}
	endpoint, err := a.catalogProxyEndpoint()
	if endpoint == "" || err != nil {
		restored := a.proxy.Restore()
		if errors.Is(restored, systemproxy.ErrChanged) {
			restored = nil
		}
		return errors.Join(err, restored)
	}
	if err = a.proxy.Apply(endpoint); err != nil {
		restored := a.proxy.Restore()
		if errors.Is(restored, systemproxy.ErrChanged) {
			restored = nil
		}
		return errors.Join(err, restored)
	}
	return nil
}
