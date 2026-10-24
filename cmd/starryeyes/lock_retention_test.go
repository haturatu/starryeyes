package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestLockStorageRemainsBounded(t *testing.T) {
	s := &Server{}
	lifecycle := map[*sync.RWMutex]bool{}
	chunks := map[*sync.Mutex]bool{}
	for i := 0; i < 100000; i++ {
		key := fmt.Sprint(i)
		lifecycle[s.lifecycleLock(key)] = true
		chunks[s.chunkLock(key, i)] = true
	}
	if len(lifecycle) > 256 || len(chunks) > 256 {
		t.Fatal("unbounded lock identities")
	}
	if s.lifecycleLock("same") != s.lifecycleLock("same") || s.chunkLock("same", 1) != s.chunkLock("same", 1) {
		t.Fatal("same key changed lock")
	}
}
func TestLifecycleWritersWaitForChunkReaders(t *testing.T) {
	s := &Server{}
	lock := s.lifecycleLock("job")
	lock.RLock()
	if lock.TryLock() {
		t.Fatal("writer bypassed active reader")
	}
	lock.RUnlock()
	if !lock.TryLock() {
		t.Fatal("reader did not release lock")
	}
	lock.Unlock()
}
