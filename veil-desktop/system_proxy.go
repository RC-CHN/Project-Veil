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
