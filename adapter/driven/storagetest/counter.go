package storagetest

import "sync/atomic"

// atomic64 hands out unique numbers so parallel tests get distinct ledgers.
type atomic64 struct{ v atomic.Int64 }

func (a *atomic64) next() int64 { return a.v.Add(1) }
