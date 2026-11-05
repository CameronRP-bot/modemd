package connrequester

import (
	"reflect"
	"runtime"
	"testing"
	"time"
)

const (
	testTimeout  = time.Millisecond // Short enough that WaitUntilUp only checks once.
	testRetry    = 5 * time.Millisecond
	testLongWait = time.Hour
)

func newTestRequester(t *testing.T, check func() bool) *ConnectionRequester {
	t.Helper()
	oldInterval := connectionCheckInterval
	connectionCheckInterval = time.Millisecond
	t.Cleanup(func() { connectionCheckInterval = oldInterval })
	return &ConnectionRequester{stateChange: make(chan bool, 32), checkConnection: check}
}

// requestChanges returns the Start (true) and Stop (false) calls so far.
func requestChanges(cr *ConnectionRequester) []bool {
	changes := []bool{}
	for len(cr.stateChange) > 0 {
		changes = append(changes, <-cr.stateChange)
	}
	return changes
}

func TestWaitUntilUpLoopAlreadyUp(t *testing.T) {
	cr := newTestRequester(t, func() bool { return true })
	if err := cr.WaitUntilUpLoop(testTimeout, testLongWait, -1); err != nil {
		t.Fatal(err)
	}
	if got := requestChanges(cr); len(got) != 0 {
		t.Fatalf("request changes = %v, want none", got)
	}
}

// A connection found between attempts should be used straight away, with no connection being
// requested while waiting, and requests started again before returning.
func TestWaitUntilUpLoopFindsConnectionBetweenAttempts(t *testing.T) {
	checks := 0
	var cr *ConnectionRequester
	stoppedDuringWait := false
	cr = newTestRequester(t, func() bool {
		checks++
		if checks == 2 { // The first check after the attempt from WaitUntilUp failed.
			stoppedDuringWait = reflect.DeepEqual(requestChanges(cr), []bool{false})
		}
		return checks == 4
	})

	start := time.Now()
	if err := cr.WaitUntilUpLoop(testTimeout, testLongWait, -1); err != nil {
		t.Fatal(err)
	}

	if time.Since(start) > 10*time.Second {
		t.Fatal("should not have waited for the retry interval")
	}
	if !stoppedDuringWait {
		t.Fatal("connections should not be requested while waiting")
	}
	if got, want := requestChanges(cr), []bool{true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("request changes after waiting = %v, want %v", got, want)
	}
}

func TestWaitUntilUpLoopRetriesAfterInterval(t *testing.T) {
	cr := newTestRequester(t, func() bool { return false })
	if err := cr.WaitUntilUpLoop(testTimeout, testRetry, 2); err == nil {
		t.Fatal("expected an error after running out of retries")
	}
	want := []bool{false, true, false, true}
	if got := requestChanges(cr); !reflect.DeepEqual(got, want) {
		t.Fatalf("request changes = %v, want %v", got, want)
	}
}

func TestWaitForConnectionDoesNotSleepPastDuration(t *testing.T) {
	cr := newTestRequester(t, func() bool { return false })
	connectionCheckInterval = time.Hour

	start := time.Now()
	if cr.waitForConnection(20 * time.Millisecond) {
		t.Fatal("expected no connection")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("should not sleep for longer than the remaining time")
	}
}

func TestAnyHostResponds(t *testing.T) {
	hosts := []string{"a", "b"}
	if !anyHostResponds(hosts, time.Minute, func(host string) bool { return host == "b" }) {
		t.Fatal("expected true when one host responds")
	}
	if anyHostResponds(hosts, time.Minute, func(string) bool { return false }) {
		t.Fatal("expected false when no hosts respond")
	}
}

// Pings still running when anyHostResponds returns should be able to finish, otherwise their
// goroutines build up while waiting for a connection.
func TestAnyHostRespondsLeavesNoGoroutines(t *testing.T) {
	hosts := []string{"a", "b"}
	tests := []struct {
		name    string
		timeout time.Duration
		want    bool
		ping    func(host string, release <-chan struct{}) bool
	}{
		{
			name:    "one host responds first",
			timeout: time.Minute,
			want:    true,
			ping: func(host string, release <-chan struct{}) bool {
				if host == "a" {
					return true
				}
				<-release
				return false
			},
		},
		{
			name:    "timeout",
			timeout: time.Millisecond,
			want:    false,
			ping: func(_ string, release <-chan struct{}) bool {
				<-release
				return false
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			release := make(chan struct{})
			got := anyHostResponds(hosts, tt.timeout, func(host string) bool { return tt.ping(host, release) })
			close(release)
			if got != tt.want {
				t.Fatalf("anyHostResponds = %v, want %v", got, tt.want)
			}

			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > before {
				if time.Now().After(deadline) {
					t.Fatalf("goroutines = %d, want at most %d", runtime.NumGoroutine(), before)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
