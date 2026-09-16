package session

import (
	"crypto/tls"
	"errors"

	sm "veil.local/core/internal/streammux"
)

type MuxConfig struct {
	Streams         int
	Opened          uint32
	ConnectionBytes uint64
	CarrierIdleMS   int
}

func (c *MuxConfig) defaults(window int, maxBytes uint64) error {
	if c.Streams == 0 {
		c.Streams = 8
	}
	if c.Opened == 0 {
		c.Opened = 4096
	}
	if c.ConnectionBytes == 0 {
		c.ConnectionBytes = 64 << 20
	}
	if c.CarrierIdleMS == 0 {
		c.CarrierIdleMS = 5000
	}
	if !c.settings(window, maxBytes).Valid() || c.CarrierIdleMS < 100 || c.CarrierIdleMS > 60000 {
		return errors.New("mux resource limits")
	}
	return nil
}
func (c MuxConfig) settings(window int, maxBytes uint64) sm.Settings {
	return sm.Settings{Streams: c.Streams, Window: window, StreamBytes: maxBytes, ConnectionBytes: c.ConnectionBytes, Opened: c.Opened}
}
func checkInnerBundle(b Bundle, version int) error {
	if version == 1 && b.Version == 1 && b.InnerProtocol == "" || version == 2 && b.Version == 2 && b.InnerProtocol == "streammux-v1" {
		return nil
	}
	return errors.New("inner protocol and configuration version differ")
}
func runtimeMaterial(cs *tls.ConnectionState, id string, version int) ([32]byte, error) {
	if version == 1 {
		return parallelMaterial(cs, id)
	}
	var out [32]byte
	if version != 2 || cs == nil || cs.Version != tls.VersionTLS13 || cs.NegotiatedProtocol != "h2" {
		return out, errors.New("expected mux TLS1.3/HTTP2")
	}
	p, e := cs.ExportKeyingMaterial("EXPORTER-veil-object-mux-v1", []byte(id), 32)
	if e != nil {
		return out, e
	}
	copy(out[:], p)
	return out, nil
}
