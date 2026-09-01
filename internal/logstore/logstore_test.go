package logstore

import (
	"sync"
	"testing"
	"time"
)

func entry(i int) Entry {
	return Entry{Time: time.Unix(int64(i), 0), Status: i}
}

func TestStoreNewestFirst(t *testing.T) {
	s := NewStore(5)
	for i := 0; i < 3; i++ {
		s.Add(entry(i))
	}
	got := s.List()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, w := range []int{2, 1, 0} {
		if got[i].Status != w {
			t.Errorf("List()[%d].Status = %d, want %d", i, got[i].Status, w)
		}
	}
}

func TestStoreCapacityOverflowDropsOldest(t *testing.T) {
	s := NewStore(3)
	for i := 0; i < 5; i++ {
		s.Add(entry(i))
	}
	got := s.List()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// The two oldest were dropped; newest first.
	for i, w := range []int{4, 3, 2} {
		if got[i].Status != w {
			t.Errorf("List()[%d].Status = %d, want %d", i, got[i].Status, w)
		}
	}
}

func TestStoreExactCapacity(t *testing.T) {
	s := NewStore(3)
	for i := 0; i < 3; i++ {
		s.Add(entry(i))
	}
	got := s.List()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, w := range []int{2, 1, 0} {
		if got[i].Status != w {
			t.Errorf("List()[%d].Status = %d, want %d", i, got[i].Status, w)
		}
	}
}

func TestStoreListReturnsCopy(t *testing.T) {
	s := NewStore(4)
	s.Add(entry(1))
	got := s.List()
	got[0].Status = 999
	if after := s.List(); after[0].Status != 1 {
		t.Errorf("mutating the returned slice changed the store: %d", after[0].Status)
	}
}

func TestStoreDefaultCapacity(t *testing.T) {
	if s := NewStore(0); s.capacity != 500 {
		t.Errorf("NewStore(0).capacity = %d, want 500", s.capacity)
	}
	if s := NewStore(-7); s.capacity != 500 {
		t.Errorf("NewStore(-7).capacity = %d, want 500", s.capacity)
	}
}

func TestStoreConcurrentAdds(t *testing.T) {
	s := NewStore(50)
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.Add(entry(base*100 + i))
			}
		}(g)
	}
	wg.Wait()
	got := s.List()
	if len(got) != 50 {
		t.Fatalf("len = %d, want 50", len(got))
	}
	for _, e := range got {
		if e.Status < 0 || e.Status >= 400 {
			t.Errorf("entry %d out of range (torn write)", e.Status)
		}
	}
}
