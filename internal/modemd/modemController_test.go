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

func TestWaitWhileNeeded(t *testing.T) {
	checks := 0
	if !waitWhileNeeded(5*time.Millisecond, time.Millisecond, func() bool {
		checks++
		return true
	}) {
		t.Error("expected true when still needed for the whole wait")
	}
	if checks == 0 {
		t.Error("expected it to check if still needed")
	}

	start := time.Now()
	checks = 0
	if waitWhileNeeded(time.Hour, time.Millisecond, func() bool {
		checks++
		return checks < 3
	}) {
		t.Error("expected false once no longer needed")
	}
	if checks != 3 {
		t.Errorf("checks = %d, want 3", checks)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("should stop waiting as soon as it is no longer needed")
	}
}

func TestShouldBeOnAfterConnecting(t *testing.T) {
	mc := ModemController{
		StartTime:         time.Now().Add(-time.Hour),
		InitialOnDuration: 20 * time.Minute,
		RequestOnDuration: 2 * time.Minute,
		MinConnDuration:   time.Minute,
		MaxOffDuration:    24 * time.Hour,
		RetryInterval:     20 * time.Minute,
	}
	mc.connectedTime = time.Now()
	mc.lastSuccessfulPing = mc.connectedTime
	if !mc.ShouldBeOn() {
		t.Error("expected on during the minimum connection time")
	}

	mc.connectedTime = time.Now().Add(-2 * time.Minute)
	mc.lastSuccessfulPing = mc.connectedTime
	if mc.ShouldBeOn() {
		t.Error("expected off when nothing needs the modem")
	}

	mc.lastOnRequestTime = time.Now()
	if !mc.ShouldBeOn() {
		t.Error("expected on while connections are being requested")
	}
}

// The AT manager's queue is never read, so a request that reaches it would block the test.
func modemForShutdownTest() *Modem {
	return &Modem{ATReady: true, SimCardStatus: SimCardReady, ATManager: &atManager{requests: make(chan atRequest)}}
}

func TestRunATCommandRefusedWhileShuttingDown(t *testing.T) {
	mc := &ModemController{Modem: modemForShutdownTest()}
	mc.shuttingDown.Store(true)

	if _, err := mc.RunATCommand("AT+CSQ", 1000, 1); !errors.Is(err, errModemShuttingDown) {
		t.Errorf("err = %v, want %v", err, errModemShuttingDown)
	}
	if mc.atAvailable() {
		t.Error("expected AT commands to be unavailable while shutting down")
	}

	mc.shuttingDown.Store(false)
	if !mc.atAvailable() {
		t.Error("expected AT commands to be available again")
	}
}

// managementd asks for the status every 5 seconds, including while the modem shuts down.
func TestGetStatusSendsNoATCommandsWhileShuttingDown(t *testing.T) {
	mc := &ModemController{Modem: modemForShutdownTest()}
	mc.shuttingDown.Store(true)

	status, err := mc.GetStatus()
	if err != nil {
		t.Fatal(err)
	}
	modem := status["modem"].(map[string]interface{})
	if modem["atReady"] != false {
		t.Errorf("atReady = %v, want false", modem["atReady"])
	}
	if _, ok := modem["voltage"]; ok {
		t.Error("should not read the voltage while shutting down")
	}
	if _, ok := status["signal"]; ok {
		t.Error("should not read the signal while shutting down")
	}
	if _, ok := status["simCard"].(map[string]interface{})["ICCID"]; ok {
		t.Error("should not read the SIM card while shutting down")
	}
}

func TestModemPoweredOffEvent(t *testing.T) {
	now := time.Date(2026, 11, 6, 13, 0, 0, 0, time.FixedZone("NZDT", 13*3600))
	event := modemPoweredOffEvent(now, "No reason the modem should be on.", 90*time.Second+400*time.Millisecond)

	if event.Type != "modemPoweredOff" {
		t.Errorf("type = %q", event.Type)
	}
	if !event.Timestamp.Equal(now) || event.Timestamp.Location() != time.UTC {
		t.Errorf("timestamp = %v, want %v in UTC", event.Timestamp, now)
	}
	if event.Details["reason"] != "No reason the modem should be on." || event.Details["onSeconds"] != 90 {
		t.Errorf("details = %v", event.Details)
	}
}
