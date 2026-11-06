package kvstore

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	// configureMu serializes ConfigureLocal and ShutdownLocal so a stopped
	// background retry is never replaced concurrently.
	configureMu sync.Mutex

	controllerMu sync.RWMutex
	homeProvider func() (KVClient, bool, error)
	localEnabled bool
	localStore   *BoltStore
	localPath    string
	localErr     error
	localRetry   *localOpenRetry

	localRetryInitialDelay = time.Second
	localRetryMaxDelay     = 30 * time.Second
)

// localOpenRetry reopens the local BoltStore in the background after a failed
// open, e.g. while a previous process still holds the bbolt file lock during a
// start-first restart.
type localOpenRetry struct {
	path         string
	lastErr      string
	initialDelay time.Duration
	maxDelay     time.Duration
	stop         chan struct{}
	done         chan struct{}
}

// RegisterHomeProvider registers the callback used by CurrentKVClient to inspect
// whether Home mode is active and obtain the active Home KV client.
func RegisterHomeProvider(fn func() (KVClient, bool, error)) {
	controllerMu.Lock()
	homeProvider = fn
	controllerMu.Unlock()
}

// ConfigureLocal enables, reconfigures, or disables the local bbolt-backed KV store.
// When opening the store fails, ConfigureLocal returns the error and keeps
// retrying in the background with capped exponential backoff until the open
// succeeds or the store is reconfigured or shut down.
func ConfigureLocal(enabled bool, rawPath string) error {
	configureMu.Lock()
	defer configureMu.Unlock()

	resolvedPath, errResolve := ResolvePath(rawPath)
	if enabled && errResolve == nil && pendingRetryMatches(resolvedPath) {
		return currentLocalErr()
	}
	stopLocalRetry()

	controllerMu.Lock()
	defer controllerMu.Unlock()

	if !enabled {
		return shutdownLocalLocked()
	}

	if errResolve != nil {
		if localStore != nil {
			if errClose := localStore.Close(); errClose != nil {
				log.Errorf("local kv store close failed: %v", errClose)
			}
			localStore = nil
			localPath = ""
		}
		localEnabled = true
		localErr = errResolve
		log.Errorf("local kv store resolve path failed: %v", errResolve)
		return errResolve
	}

	if localStore != nil && localPath == resolvedPath && localErr == nil {
		localEnabled = true
		return nil
	}

	if localStore != nil {
		if errClose := localStore.Close(); errClose != nil {
			log.Errorf("local kv store close failed: %v", errClose)
		}
		localStore = nil
		localPath = ""
	}

	store, errOpen := OpenBoltStore(resolvedPath)
	if errOpen != nil {
		localEnabled = true
		localErr = errOpen
		log.Errorf("local kv store open failed (path=%s): %v; retrying in background", resolvedPath, errOpen)
		startLocalRetryLocked(resolvedPath, errOpen)
		return errOpen
	}

	localEnabled = true
	localStore = store
	localPath = resolvedPath
	localErr = nil
	return nil
}

// ShutdownLocal closes any open local BoltStore and resets local KV state to disabled.
// Any pending background open retry is stopped before ShutdownLocal returns.
func ShutdownLocal() error {
	configureMu.Lock()
	defer configureMu.Unlock()
	stopLocalRetry()

	controllerMu.Lock()
	defer controllerMu.Unlock()
	return shutdownLocalLocked()
}

func shutdownLocalLocked() error {
	var errClose error
	if localStore != nil {
		errClose = localStore.Close()
	}
	localEnabled = false
	localStore = nil
	localPath = ""
	localErr = nil
	return errClose
}

func pendingRetryMatches(resolvedPath string) bool {
	controllerMu.RLock()
	defer controllerMu.RUnlock()
	return localRetry != nil && localRetry.path == resolvedPath && localStore == nil
}

func currentLocalErr() error {
	controllerMu.RLock()
	defer controllerMu.RUnlock()
	return localErr
}

// stopLocalRetry cancels the pending background open retry, if any, and waits
// for it to exit. It must be called without holding controllerMu.
func stopLocalRetry() {
	controllerMu.Lock()
	retry := localRetry
	localRetry = nil
	controllerMu.Unlock()
	if retry == nil {
		return
	}
	close(retry.stop)
	<-retry.done
}

func startLocalRetryLocked(resolvedPath string, errOpen error) {
	retry := &localOpenRetry{
		path:         resolvedPath,
		lastErr:      errOpen.Error(),
		initialDelay: localRetryInitialDelay,
		maxDelay:     localRetryMaxDelay,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	localRetry = retry
	go retry.run()
}

func nextLocalRetryDelay(current, maxDelay time.Duration) time.Duration {
	next := current * 2
	if next <= 0 || next > maxDelay {
		return maxDelay
	}
	return next
}

func (r *localOpenRetry) run() {
	defer close(r.done)
	delay := r.initialDelay
	for attempt := 1; ; attempt++ {
		timer := time.NewTimer(delay)
		select {
		case <-r.stop:
			timer.Stop()
			return
		case <-timer.C:
		}

		store, errOpen := OpenBoltStore(r.path)
		if errOpen != nil {
			if msg := errOpen.Error(); msg != r.lastErr {
				r.lastErr = msg
				log.Warnf("local kv store background open still failing (path=%s): %v", r.path, errOpen)
			}
			controllerMu.Lock()
			if localRetry == r {
				localErr = errOpen
			}
			controllerMu.Unlock()
			delay = nextLocalRetryDelay(delay, r.maxDelay)
			continue
		}

		controllerMu.Lock()
		if localRetry != r {
			controllerMu.Unlock()
			if errClose := store.Close(); errClose != nil {
				log.Errorf("local kv store close failed: %v", errClose)
			}
			return
		}
		localStore = store
		localPath = r.path
		localErr = nil
		localRetry = nil
		controllerMu.Unlock()
		log.Infof("local kv store opened in background after %d attempt(s) (path=%s)", attempt, r.path)
		return
	}
}

// CurrentKVClient returns the active KVClient using the three-tier routing order:
//  1. Home KV when Home mode is active (never falling back to local bbolt).
//  2. Local BoltStore when server.kv-store.disabled is false.
//  3. (nil, false, nil) for default in-memory storage.
func CurrentKVClient() (KVClient, bool, error) {
	controllerMu.RLock()
	provider := homeProvider
	controllerMu.RUnlock()

	if provider != nil {
		if client, homeMode, errHome := provider(); homeMode {
			return client, true, errHome
		}
	}

	controllerMu.RLock()
	defer controllerMu.RUnlock()

	if !localEnabled {
		return nil, false, nil
	}
	if localErr != nil {
		return nil, true, fmt.Errorf("local kv store unavailable: %w", localErr)
	}
	if localStore == nil {
		return nil, true, fmt.Errorf("local kv store unavailable: %w", errStoreClosed)
	}
	return localStore, true, nil
}
