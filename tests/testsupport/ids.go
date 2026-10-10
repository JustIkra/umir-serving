package testsupport

import (
	"strconv"
	"sync"
)

type IDGenerator struct {
	mu     sync.Mutex
	prefix string
	next   uint64
}

func NewIDs(prefix string) *IDGenerator {
	return &IDGenerator{prefix: prefix}
}

func (g *IDGenerator) Next() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return g.prefix + strconv.FormatUint(g.next, 10)
}
