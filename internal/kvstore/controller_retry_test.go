package kvstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func setLocalRetryTimingForTest(t *testing.T, initial, maxDelay, openTimeout time.Duration) {
	t.Helper()
	prevInitial, prevMax, prevOpen := localRetryInitialDelay, localRetryMaxDelay, boltOpenTimeout
	localRetryInitialDelay, localRetryMaxDelay, boltOpenTimeout = initial, maxDelay, openTimeout
	t.Cleanup(func() {
		_ = ShutdownLocal()
		localRetryInitialDelay, localRetryMaxDelay, boltOpenTimeout = prevInitial, prevMax, prevOpen
	})
}

// holdBoltLock opens path with a raw bbolt handle so the file lock is held,
// mimicking the previous container during a start-first restart.
func holdBoltLock(t *testing.T, path string) *bbolt.DB {
	t.Helper()
	db, errOpen := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if errOpen != nil {
		t.Fatalf("bbolt.Open(%q) holder error = %v", path, errOpen)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func waitForLocalBoltStore(t *testing.T, timeout time.Duration) *BoltStore {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		client, kvMode, errClient := CurrentKVClient()
		if errClient == nil && kvMode && client != nil {
			store, ok := client.(*BoltStore)
			if !ok {
				t.Fatalf("CurrentKVClient() type = %T, want *BoltStore", client)
			}
			return store
		}
		if time.Now().After(deadline) {
			t.Fatalf("local kv store did not open within %s: last CurrentKVClient() = (%v, %v, %v)", timeout, client, kvMode, errClient)
		}
		<-ticker.C
	}
}

func TestControllerConfigureLocalRetriesOpenUntilLockReleased(t *testing.T) {
	resetControllerForTest(t)
	setLocalRetryTimingForTest(t, 10*time.Millisecond, 40*time.Millisecond, 50*time.Millisecond)

	dbPath := filepath.Join(t.TempDir(), "kv.db")
	holder := holdBoltLock(t, dbPath)

	if errCfg := ConfigureLocal(true, dbPath); errCfg == nil {
		t.Fatalf("ConfigureLocal(true, locked path) error = nil, want open timeout while lock is held")
	}
	client, kvMode, errClient := CurrentKVClient()
	if !kvMode || client != nil || errClient == nil {
		t.Fatalf("CurrentKVClient() while locked = (%v, %v, %v), want (nil, true, error)", client, kvMode, errClient)
	}

	// Release the lock the way the old task does when it stops; no config re-apply follows.
	if errClose := holder.Close(); errClose != nil {
		t.Fatalf("holder.Close() error = %v", errClose)
	}

	store := waitForLocalBoltStore(t, 5*time.Second)
	if store.Path() != dbPath {
		t.Fatalf("store.Path() = %q, want %q", store.Path(), dbPath)
	}
	ctx := context.Background()
	if ok, errSet := store.KVSet(ctx, "cpa:test:retry", []byte("ok"), KVSetOptions{EX: time.Minute}); errSet != nil || !ok {
		t.Fatalf("KVSet() after background open = (%v, %v), want (true, nil)", ok, errSet)
	}
	got, found, errGet := store.KVGet(ctx, "cpa:test:retry")
	if errGet != nil || !found || string(got) != "ok" {
		t.Fatalf("KVGet() after background open = (%q, %v, %v), want (ok, true, nil)", got, found, errGet)
	}
}

func pendingLocalRetry() *localOpenRetry {
	controllerMu.RLock()
	defer controllerMu.RUnlock()
	return localRetry
}

func TestControllerShutdownLocalStopsOpenRetry(t *testing.T) {
	resetControllerForTest(t)
	setLocalRetryTimingForTest(t, 10*time.Millisecond, 40*time.Millisecond, 50*time.Millisecond)

	dbPath := filepath.Join(t.TempDir(), "kv.db")
	holder := holdBoltLock(t, dbPath)

	if errCfg := ConfigureLocal(true, dbPath); errCfg == nil {
		t.Fatalf("ConfigureLocal(true, locked path) error = nil, want open timeout")
	}
	retry := pendingLocalRetry()
	if retry == nil {
		t.Fatalf("no background open retry started after failed open")
	}

	if errShutdown := ShutdownLocal(); errShutdown != nil {
		t.Fatalf("ShutdownLocal() error = %v", errShutdown)
	}
	select {
	case <-retry.done:
	default:
		t.Fatalf("background open retry still running after ShutdownLocal returned")
	}
	if pendingLocalRetry() != nil {
		t.Fatalf("localRetry not cleared after ShutdownLocal")
	}

	if errClose := holder.Close(); errClose != nil {
		t.Fatalf("holder.Close() error = %v", errClose)
	}
	client, kvMode, errClient := CurrentKVClient()
	if client != nil || kvMode || errClient != nil {
		t.Fatalf("CurrentKVClient() after shutdown = (%v, %v, %v), want (nil, false, nil)", client, kvMode, errClient)
	}
	// The stopped retry must not hold the file: a fresh open succeeds immediately.
	reopened := holdBoltLock(t, dbPath)
	_ = reopened.Close()
}

func TestControllerConfigureLocalSamePathKeepsPendingRetry(t *testing.T) {
	resetControllerForTest(t)
	// Long retry delay so the pending retry stays parked while the config is re-applied.
	setLocalRetryTimingForTest(t, time.Hour, time.Hour, 50*time.Millisecond)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kv.db")
	holdBoltLock(t, dbPath)

	if errCfg := ConfigureLocal(true, dbPath); errCfg == nil {
		t.Fatalf("ConfigureLocal(true, locked path) error = nil, want open timeout")
	}
	retry := pendingLocalRetry()
	if retry == nil {
		t.Fatalf("no background open retry started after failed open")
	}

	if errAgain := ConfigureLocal(true, dbPath); errAgain == nil {
		t.Fatalf("ConfigureLocal(true, same locked path) error = nil, want pending open error")
	}
	if pendingLocalRetry() != retry {
		t.Fatalf("re-applying the same path replaced the pending retry")
	}

	// Switching to another path cancels the pending retry and opens the new path.
	otherPath := filepath.Join(dir, "other.db")
	if errOther := ConfigureLocal(true, otherPath); errOther != nil {
		t.Fatalf("ConfigureLocal(true, otherPath) error = %v", errOther)
	}
	select {
	case <-retry.done:
	default:
		t.Fatalf("pending retry still running after switching path")
	}
	if pendingLocalRetry() != nil {
		t.Fatalf("localRetry not cleared after switching path")
	}
	store := waitForLocalBoltStore(t, time.Second)
	if store.Path() != otherPath {
		t.Fatalf("store.Path() = %q, want %q", store.Path(), otherPath)
	}
}

func TestNextLocalRetryDelayIsCapped(t *testing.T) {
	maxDelay := 30 * time.Second
	delay := time.Second
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		delay = nextLocalRetryDelay(delay, maxDelay)
		if delay != w {
			t.Fatalf("step %d: nextLocalRetryDelay = %s, want %s", i, delay, w)
		}
	}
	if got := nextLocalRetryDelay(time.Duration(1<<62), maxDelay); got != maxDelay {
		t.Fatalf("nextLocalRetryDelay(overflow) = %s, want %s", got, maxDelay)
	}
}
