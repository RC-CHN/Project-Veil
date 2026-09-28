package core

import (
	"testing"
	"veil/internal/mux"
)

func TestTrafficProfileSnapshotAndUpdate(t *testing.T) {
	original := mux.DefaultProfile()
	config := Config{Traffic: &original}
	if err := config.defaults(); err != nil {
		t.Fatal(err)
	}
	snapshot := *config.Traffic
	original.Quantum.Min = 0
	if *config.Traffic != snapshot {
		t.Fatal("constructor did not copy profile")
	}
	p := newPool(ClientConfig{Config: config}, nil, nil)
	client := &Client{pool: p}
	server := &Server{}
	server.traffic.Store(config.Traffic)
	oldClient, oldServer := p.traffic.Load(), server.traffic.Load()
	update := snapshot
	update.StartupSize = TrafficRange{Min: 256, Max: 1024}
	for _, set := range []func(TrafficProfile) error{client.SetTrafficProfile, server.SetTrafficProfile} {
		if err := set(update); err != nil {
			t.Fatal(err)
		}
	}
	update.StartupSize.Min = 0
	if *oldClient != snapshot || *oldServer != snapshot {
		t.Fatal("update mutated an existing connection snapshot")
	}
	if p.traffic.Load().StartupSize.Min != 256 || server.traffic.Load().StartupSize.Min != 256 {
		t.Fatal("setter retained mutable caller storage")
	}
	for _, set := range []func(TrafficProfile) error{client.SetTrafficProfile, server.SetTrafficProfile} {
		if err := set(update); err == nil {
			t.Fatal("invalid update accepted")
		}
	}
	if p.traffic.Load().StartupSize.Min != 256 || server.traffic.Load().StartupSize.Min != 256 {
		t.Fatal("invalid update replaced valid state")
	}
}
