package service

import "testing"

func TestGeneratedProfileIsValidAndCopied(t *testing.T) {
	for range 1000 {
		p := GenerateTrafficProfile()
		if e := p.Validate(); e != nil {
			t.Fatal(e)
		}
		c := Config{Role: "server", Secret: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Traffic: &p}
		if e := c.Defaults(); e != nil {
			t.Fatal(e)
		}
		original := *c.Traffic
		p.Quantum.Min = 0
		if *c.Traffic != original {
			t.Fatal("config retained caller's mutable profile")
		}
	}
}
