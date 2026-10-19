package core

import (
	"fmt"
	"net"
	"testing"
	"veil/internal/mux"
)

// Isolate pool selection with real mux streams occupying all but one slot.
// Handshakes, target dialing and application IO are excluded from the timing.
func BenchmarkPoolAcquireNearlyFull(b *testing.B) {
	for _, lanes := range []int{64, 128, 512} {
		b.Run(fmt.Sprint(lanes), func(b *testing.B) {
			cfg := ClientConfig{Config: Config{MaxConnections: lanes}}
			if err := cfg.Config.defaults(); err != nil {
				b.Fatal(err)
			}
			p := newPool(cfg, nil, nil)
			defer p.close()
			for i := range lanes {
				a, z := net.Pipe()
				peer, err := mux.New(z, mux.Options{Server: true, Profile: mux.DefaultProfile()})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { peer.Close(); peer.Wait() })
				m, err := mux.New(a, mux.Options{Profile: mux.DefaultProfile()})
				if err != nil {
					b.Fatal(err)
				}
				s := &session{mux: m, active: mux.MaxStreams}
				if i == lanes-1 {
					s.active--
				}
				s.established.Store(true)
				p.all[s], p.total = true, p.total+1
				go func() {
					for range s.active {
						st, err := peer.Accept(b.Context())
						if err != nil {
							return
						}
						if err := st.Respond(0); err != nil {
							return
						}
					}
				}()
				for range s.active {
					if _, err := m.Open(b.Context(), []byte("target")); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				s, err := p.get(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				p.put(s)
			}
		})
	}
}
