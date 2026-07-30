package gateway

import (
	"testing"
	"time"
)

func TestSandboxStoreLifecycleLockIsPerSandbox(t *testing.T) {
	store := NewSandboxStore()
	for _, id := range []string{"same", "other"} {
		if _, err := store.Create(SandboxRecord{ID: id}); err != nil {
			t.Fatalf("create sandbox %q: %v", id, err)
		}
	}

	first, ok := store.lockSandbox("same")
	if !ok {
		t.Fatal("lock first sandbox")
	}

	secondAcquired := make(chan *sandboxEntry, 1)
	go func() {
		entry, exists := store.lockSandbox("same")
		if !exists {
			secondAcquired <- nil
			return
		}
		secondAcquired <- entry
	}()

	select {
	case second := <-secondAcquired:
		if second != nil {
			second.lifecycleMu.Unlock()
		}
		first.lifecycleMu.Unlock()
		t.Fatal("second caller acquired the same sandbox lock before it was released")
	case <-time.After(25 * time.Millisecond):
	}

	other, ok := store.lockSandbox("other")
	if !ok {
		first.lifecycleMu.Unlock()
		t.Fatal("lock unrelated sandbox")
	}
	other.lifecycleMu.Unlock()

	first.lifecycleMu.Unlock()
	select {
	case second := <-secondAcquired:
		if second == nil {
			t.Fatal("sandbox disappeared while waiting for its lifecycle lock")
		}
		second.lifecycleMu.Unlock()
	case <-time.After(time.Second):
		t.Fatal("second caller did not acquire the released sandbox lock")
	}
}

func TestSandboxStoreLifecycleLockFollowsReplacement(t *testing.T) {
	const sandboxID = "replaced"

	store := NewSandboxStore()
	if _, err := store.Create(SandboxRecord{ID: sandboxID}); err != nil {
		t.Fatalf("create original sandbox: %v", err)
	}

	original, ok := store.lockSandbox(sandboxID)
	if !ok {
		t.Fatal("lock original sandbox")
	}

	type lockResult struct {
		entry  *sandboxEntry
		exists bool
	}
	waiting := make(chan struct{})
	acquired := make(chan lockResult, 1)
	go func() {
		close(waiting)
		entry, exists := store.lockSandbox(sandboxID)
		acquired <- lockResult{entry: entry, exists: exists}
	}()
	<-waiting

	if _, err := store.Delete(sandboxID); err != nil {
		original.lifecycleMu.Unlock()
		t.Fatalf("delete original sandbox: %v", err)
	}
	if _, err := store.Create(SandboxRecord{ID: sandboxID}); err != nil {
		original.lifecycleMu.Unlock()
		t.Fatalf("create replacement sandbox: %v", err)
	}
	original.lifecycleMu.Unlock()

	select {
	case result := <-acquired:
		if !result.exists {
			t.Fatal("replacement sandbox was not locked")
		}

		store.mu.RLock()
		current := store.sandboxes[sandboxID]
		store.mu.RUnlock()
		if result.entry != current {
			result.entry.lifecycleMu.Unlock()
			t.Fatal("caller acquired the deleted sandbox entry")
		}
		result.entry.lifecycleMu.Unlock()
	case <-time.After(time.Second):
		t.Fatal("caller did not follow the replacement sandbox entry")
	}
}
