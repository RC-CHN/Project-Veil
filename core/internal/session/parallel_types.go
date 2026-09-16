package session

import ()

type parallelTransaction struct {
	Sequence           uint64
	Member             int
	StartedNS, EndedNS int64
	Receipt            string `json:",omitempty"`
	HTTP               transaction
	Error              string `json:",omitempty"`
}
