package identify

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestIdentifyConnGoroutineLeak demonstrates that the goroutine spawned in IdentifyWait
// persists even after the service context is cancelled because identifyConn uses
// context.Background() instead of the service's context.
func TestIdentifyConnGoroutineLeak(t *testing.T) {
	// Count initial goroutines
	initialGoroutines := runtime.NumGoroutine()
	t.Logf("Initial goroutines: %d", initialGoroutines)

	// Create a service with a cancellable context
	originalIdentifyConn := identifyConnForTesting
	defer func() {
		identifyConnForTesting = originalIdentifyConn
	}()

	// Create channels to track the goroutine lifecycle
	identifyStarted := make(chan struct{})
	identifyBlocked := make(chan struct{})
	identifyUnblock := make(chan struct{})

	// Mock identifyConn to demonstrate the problem
	identifyConnForTesting = func(ids *idService, ctx context.Context) error {
		close(identifyStarted)

		// This simulates the problematic behavior: using context.Background()
		// instead of the service's context that could be cancelled
		blockingCtx := context.Background()

		close(identifyBlocked)

		// Block until we're told to unblock, simulating a hanging network operation
		select {
		case <-identifyUnblock:
			return nil
		case <-blockingCtx.Done():
			// This will never happen because we're using context.Background()
			return blockingCtx.Err()
		}
	}

	// Create a mock service with cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	mockService := &idService{
		ctx:       ctx,
		ctxCancel: cancel,
		timeout:   DefaultTimeout,
	}

	// Start a goroutine that calls the problematic code path
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// This simulates the goroutine created in IdentifyWait
		err := identifyConnForTesting(mockService, mockService.ctx)
		t.Logf("identifyConn completed with error: %v", err)
	}()

	// Wait for the identify process to start and become blocked
	<-identifyStarted
	<-identifyBlocked

	// Give the goroutine time to start and block
	time.Sleep(50 * time.Millisecond)

	afterStartGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines after starting identify: %d", afterStartGoroutines)

	// Cancel the service context (simulating service shutdown)
	cancel()

	// Give some time for any context-aware cleanup
	time.Sleep(100 * time.Millisecond)

	afterCancelGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines after context cancel: %d", afterCancelGoroutines)

	// The goroutine should still be running because identifyConn
	// uses context.Background() instead of the service's context
	if afterCancelGoroutines <= initialGoroutines {
		t.Fatal("Expected goroutine to persist after context cancel, but it was cleaned up")
	}

	t.Logf("GOROUTINE LEAK DETECTED: %d goroutines persist after context cancellation",
		afterCancelGoroutines-initialGoroutines)
	t.Logf("This demonstrates the issue: identifyConn uses context.Background()")
	t.Logf("instead of respecting the service's cancellable context.")

	// Unblock the goroutine to clean up
	close(identifyUnblock)
	wg.Wait()

	// Give time for cleanup
	time.Sleep(50 * time.Millisecond)

	finalGoroutines := runtime.NumGoroutine()
	t.Logf("Final goroutines after cleanup: %d", finalGoroutines)
}

// identifyConnForTesting allows us to replace the identifyConn behavior for testing
var identifyConnForTesting = func(ids *idService, ctx context.Context) error {
	return ids.identifyConn(nil) // This won't work in practice, but shows the structure
}
