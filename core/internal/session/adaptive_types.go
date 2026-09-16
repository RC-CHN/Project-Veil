package session

import ()

type adaptiveTransaction struct {
	parallelTransaction
	Mode, ObjectSequence uint64
	PayloadBytes, Budget int
	PayloadSHA256        string
	EOF                  bool
	ModelWaitUS          int64
}
