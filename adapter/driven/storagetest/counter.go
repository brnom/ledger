package storagetest

import "sync/atomic"

// atomic64 hands out unique numbers so parallel tests get distinct ledgers.
type atomic64 struct{ n atomic.Int64 }

func (counter *atomic64) next() int64 { return counter.n.Add(1) }
