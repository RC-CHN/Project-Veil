package session

import (
	"crypto/sha256"
	b "veil.local/core/internal/behavior"
)

func dig(p []byte) b.Value { h := sha256.Sum256(p); return b.Value{Bytes: h[:]} }

func expr(scope string, index int) b.Expr { return b.Expr{Scope: scope, Index: index} }

func eq(l, r b.Expr) b.Equal { return b.Equal{Left: l, Right: r} }

func assign(i int, e b.Expr) b.Assign { return b.Assign{Register: i, Value: e} }
