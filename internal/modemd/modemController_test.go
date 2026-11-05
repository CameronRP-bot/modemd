package modemd

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSimChecker returns each response in order, repeating the last one once they run out.
type fakeSimChecker struct {
	statuses []string
	errs     []error
	calls    int
}

func (f *fakeSimChecker) check() (string, error) {
	i := f.calls
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	f.calls++
	return f.statuses[i], f.errs[i]
}

func TestWaitForSimCardReadyImmediately(t *testing.T) {
	f := &fakeSimChecker{statuses: []string{"READY"}, errs: []error{nil}}
	if !waitForSimCard(f.check, 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("expected SIM card to be found")
	}
	if f.calls != 1 {
		t.Fatalf("expected 1 check, got %d", f.calls)
	}
}

func TestWaitForSimCardReadyAfterRetries(t *testing.T) {
	f := &fakeSimChecker{
		statuses: []string{"", "SIM not inserted", "READY"},
		errs:     []error{errors.New("+CME ERROR: SIM busy"), nil, nil},
	}
	if !waitForSimCard(f.check, 100*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("expected SIM card to be found")
	}
	if f.calls != 3 {
		t.Fatalf("expected 3 checks, got %d", f.calls)
	}
}

func TestWaitForSimCardTimeout(t *testing.T) {
	f := &fakeSimChecker{statuses: []string{"SIM not inserted"}, errs: []error{nil}}
	start := time.Now()
	if waitForSimCard(f.check, 50*time.Millisecond, 10*time.Millisecond) {
		t.Fatal("expected SIM card not to be found")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("took %s, expected to give up after about 50ms", elapsed)
	}
	if f.calls < 2 {
		t.Fatalf("expected multiple checks before giving up, got %d", f.calls)
	}
}

func TestShouldBeOnRetriesAfterSimCardFailure(t *testing.T) {
	mc := ModemController{
		RetryInterval:        20 * time.Minute,
		RetrySimCardInterval: 2 * time.Hour,
		StartTime:            time.Now(),
		InitialOnDuration:    3 * time.Hour,
	}
	mc.failedToFindSimCard = true
	mc.lastFailedSimCard = time.Now()
	if on, reason := mc.shouldBeOnWithReason(); on {
		t.Fatalf("expected modem to stay off straight after SIM failure: %s", reason)
	}

	// Past the normal retry interval, but the SIM card retry interval is longer.
	mc.lastFailedSimCard = time.Now().Add(-21 * time.Minute)
	if on, reason := mc.shouldBeOnWithReason(); on {
		t.Fatalf("expected modem to stay off until the SIM card retry interval has passed: %s", reason)
	}

	mc.lastFailedSimCard = time.Now().Add(-121 * time.Minute)
	if on, reason := mc.shouldBeOnWithReason(); !on {
		t.Fatalf("expected modem to be allowed on after the retry interval: %s", reason)
	}
}

func TestShouldBeOnGivesUpAfterSimCardFailsInARow(t *testing.T) {
	mc := ModemController{
		RetrySimCardInterval: 2 * time.Hour,
		StartTime:            time.Now(),
		InitialOnDuration:    24 * time.Hour,
	}
	mc.failedToFindSimCard = true
	mc.lastFailedSimCard = time.Now().Add(-3 * time.Hour)

	mc.simCardFailsInARow = maxSimCardFailsInARow - 1
	if on, reason := mc.shouldBeOnWithReason(); !on {
		t.Fatalf("expected modem to retry after the retry interval before giving up: %s", reason)
	}

	mc.simCardFailsInARow = maxSimCardFailsInARow
	if on, reason := mc.shouldBeOnWithReason(); on {
		t.Fatalf("expected modem to stay off after failing %d times in a row: %s", maxSimCardFailsInARow, reason)
	}
	if !mc.waitingToRetrySimCard() {
		t.Fatal("expected to keep waiting to retry the SIM card after giving up")
	}

	// A stay-on request still turns the modem on, but the SIM card isn't checked again.
	mc.stayOnUntil = time.Now().Add(time.Hour)
	if on, reason := mc.shouldBeOnWithReason(); !on {
		t.Fatalf("expected a stay-on request to override giving up: %s", reason)
	}
	if !mc.waitingToRetrySimCard() {
		t.Fatal("expected not to retry the SIM card during a stay-on request after giving up")
	}
}

func TestSimCardFailsInARowSavedAndLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modemd", "sim-card-fails")
	if fails := loadSimCardFailsInARow(path); fails != 0 {
		t.Fatalf("expected 0 when the file doesn't exist, got %d", fails)
	}

	saveSimCardFailsInARow(path, maxSimCardFailsInARow)
	if fails := loadSimCardFailsInARow(path); fails != maxSimCardFailsInARow {
		t.Fatalf("expected %d, got %d", maxSimCardFailsInARow, fails)
	}

	if err := os.WriteFile(path, []byte("not a number"), 0644); err != nil {
		t.Fatal(err)
	}
	if fails := loadSimCardFailsInARow(path); fails != 0 {
		t.Fatalf("expected 0 for an invalid file, got %d", fails)
	}
}

func TestSimCardRetriedOnceAfterRestartWhenGaveUp(t *testing.T) {
	// After a restart the saved count is loaded, but nothing has failed yet this run.
	mc := ModemController{RetrySimCardInterval: 2 * time.Hour}
	mc.simCardFailsInARow = maxSimCardFailsInARow
	if mc.waitingToRetrySimCard() {
		t.Fatal("expected the SIM card to be checked once after a restart")
	}
	if attempts := mc.simCardPowerOnAttemptsAllowed(); attempts != 1 {
		t.Fatalf("expected 1 power on attempt after giving up, got %d", attempts)
	}

	mc.simCardFailsInARow = maxSimCardFailsInARow - 1
	if attempts := mc.simCardPowerOnAttemptsAllowed(); attempts != simCardPowerOnAttempts {
		t.Fatalf("expected %d power on attempts before giving up, got %d", simCardPowerOnAttempts, attempts)
	}
	mc.simCardFailsInARow = maxSimCardFailsInARow

	// One more failure and it gives up again straight away.
	mc.failedToFindSimCard = true
	mc.lastFailedSimCard = time.Now().Add(-3 * time.Hour)
	mc.simCardFailsInARow++
	if !mc.waitingToRetrySimCard() {
		t.Fatal("expected to give up again after failing once after a restart")
	}
}

func TestShouldBeOnRetriesAfterFindModemFailure(t *testing.T) {
	mc := ModemController{
		RetryFindModemInterval: 3 * time.Hour,
		StartTime:              time.Now(),
		InitialOnDuration:      time.Hour,
	}
	mc.failedToFindModem = true
	mc.lastFailedFindModem = time.Now()
	if on, reason := mc.shouldBeOnWithReason(); on {
		t.Fatalf("expected modem to stay off straight after failing to find it: %s", reason)
	}

	mc.lastFailedFindModem = time.Now().Add(-4 * time.Hour)
	if on, reason := mc.shouldBeOnWithReason(); !on {
		t.Fatalf("expected modem to be allowed on after the retry interval: %s", reason)
	}
}

func TestNetworkDiagnosticsWithoutModem(t *testing.T) {
	mc := ModemController{}
	diagnostics := mc.networkDiagnostics()
	if len(diagnostics) != len(networkDiagnosticCommands) {
		t.Fatalf("expected %d results, got %d", len(networkDiagnosticCommands), len(diagnostics))
	}
	for _, cmd := range networkDiagnosticCommands {
		if !strings.Contains(diagnostics[cmd], "failed") {
			t.Fatalf("expected %s to say why it failed, got '%s'", cmd, diagnostics[cmd])
		}
	}
}

func TestWithATResponse(t *testing.T) {
	err := withATResponse(ErrATErrorResponse, "+CME ERROR: SIM busy")
	if !errors.Is(err, ErrATErrorResponse) {
		t.Fatalf("expected the error type to be kept, got %v", err)
	}
	if !strings.Contains(err.Error(), "+CME ERROR: SIM busy") {
		t.Fatalf("expected the modem's reply in the error, got %v", err)
	}

	other := errors.New("read error")
	if got := withATResponse(other, "ignored"); got != other {
		t.Fatalf("expected other errors to be unchanged, got %v", got)
	}
	if got := withATResponse(nil, "OK"); got != nil {
		t.Fatalf("expected no error, got %v", got)
	}
}

func TestSignalFound(t *testing.T) {
	if !signalFound(24, nil) {
		t.Error("expected a signal for a strength of 24")
	}
	if !signalFound(0, nil) {
		t.Error("expected a signal for a strength of 0, the lowest reading")
	}
	if signalFound(99, nil) {
		t.Error("expected no signal for a strength of 99")
	}
	if signalFound(0, errors.New("AT error")) {
		t.Error("expected no signal when reading it failed")
	}
}

func TestHasIPv4Address(t *testing.T) {
	ipv4 := &net.IPNet{IP: net.ParseIP("192.168.225.23"), Mask: net.CIDRMask(24, 32)}
	ipv6 := &net.IPNet{IP: net.ParseIP("fe80::92e7:7888:cdfd:589b"), Mask: net.CIDRMask(64, 128)}

	if hasIPv4Address(nil) {
		t.Error("expected false with no addresses")
	}
	if hasIPv4Address([]net.Addr{ipv6}) {
		t.Error("expected false with only a link-local IPv6 address")
	}
	if !hasIPv4Address([]net.Addr{ipv6, ipv4}) {
		t.Error("expected true with an IPv4 address")
	}
}

func TestNextPingTestDelay(t *testing.T) {
	tests := []struct {
		interval time.Duration
		fails    int
		want     time.Duration
	}{
		{5 * time.Minute, 0, 5 * time.Minute},
		{5 * time.Minute, 1, pingRecheckInterval},
		{5 * time.Minute, 3, pingRecheckInterval},
		{10 * time.Second, 1, 10 * time.Second},
	}
	for _, tt := range tests {
		if got := nextPingTestDelay(tt.interval, tt.fails); got != tt.want {
			t.Errorf("nextPingTestDelay(%v, %d) = %v, want %v", tt.interval, tt.fails, got, tt.want)
		}
	}
}

func TestSetupTimer(t *testing.T) {
	var timer setupTimer
	start := time.Now()

	if got := timer.finish(start); got != "" {
		t.Errorf("finish before any steps = %q, want empty", got)
	}

	timer.startStep(1, start)
	timer.startStep(2, start.Add(2*time.Second))
	timer.startStep(9, start.Add(12*time.Second))
	want := "step 1 2s, step 2 10s, step 9 3s, total 15s"
	if got := timer.finish(start.Add(15 * time.Second)); got != want {
		t.Errorf("finish = %q, want %q", got, want)
	}

	if got := timer.finish(start.Add(time.Minute)); got != "" {
		t.Errorf("second finish = %q, want empty", got)
	}

	// Going back to step 1 starts again, as happens after a power cycle.
	timer.startStep(1, start)
	timer.startStep(2, start.Add(time.Second))
	timer.startStep(1, start.Add(time.Minute))
	want = "step 1 5s, total 5s"
	if got := timer.finish(start.Add(time.Minute + 5*time.Second)); got != want {
		t.Errorf("finish after restarting = %q, want %q", got, want)
	}
}
