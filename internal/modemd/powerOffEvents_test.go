package modemd

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/TheCacophonyProject/event-reporter/v3/eventclient"
)

// fakeEvents replaces addEvent and wifiConnected for a test.
func fakeEvents(t *testing.T, wifi bool, addErr error) *[]eventclient.Event {
	t.Helper()
	oldAdd, oldWifi := addEvent, wifiConnected
	t.Cleanup(func() { addEvent, wifiConnected = oldAdd, oldWifi })
	added := &[]eventclient.Event{}
	wifiConnected = func() bool { return wifi }
	addEvent = func(event eventclient.Event) error {
		if addErr != nil {
			return addErr
		}
		*added = append(*added, event)
		return nil
	}
	return added
}

func TestPowerOffEventSentStraightAwayWithWifi(t *testing.T) {
	added := fakeEvents(t, true, nil)
	path := filepath.Join(t.TempDir(), "events")
	offTime := time.Date(2026, 11, 6, 1, 2, 3, 0, time.UTC)

	mc := ModemController{}
	mc.recordPowerOff(path, powerOffEvent{Time: offTime, Reason: "not needed", OnSeconds: 90})

	if len(*added) != 1 {
		t.Fatalf("added %d events, want 1", len(*added))
	}
	got := (*added)[0]
	if got.Type != "modemPoweredOff" || !got.Timestamp.Equal(offTime) {
		t.Errorf("event = %+v", got)
	}
	if got.Details["reason"] != "not needed" || got.Details["onSeconds"] != 90 {
		t.Errorf("details = %v", got.Details)
	}
	if left := loadPowerOffEvents(path); len(left) != 0 {
		t.Errorf("%d events left saved, want 0", len(left))
	}
}

// Without WiFi the event must wait, as sending it would power the modem back on. It should survive
// modemd restarting and keep the time the modem powered off.
func TestPowerOffEventWaitsForConnectionWithoutWifi(t *testing.T) {
	added := fakeEvents(t, false, nil)
	path := filepath.Join(t.TempDir(), "events")
	first := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)

	mc := ModemController{}
	mc.recordPowerOff(path, powerOffEvent{Time: first, Reason: "one"})
	mc.recordPowerOff(path, powerOffEvent{Time: second, Reason: "two"})
	if len(*added) != 0 {
		t.Fatalf("added %d events without a connection, want 0", len(*added))
	}

	sendPowerOffEvents(path) // Called when the modem connects.
	if len(*added) != 2 {
		t.Fatalf("added %d events, want 2", len(*added))
	}
	if !(*added)[0].Timestamp.Equal(first) || !(*added)[1].Timestamp.Equal(second) {
		t.Errorf("timestamps = %v, %v", (*added)[0].Timestamp, (*added)[1].Timestamp)
	}

	sendPowerOffEvents(path)
	if len(*added) != 2 {
		t.Errorf("events were sent twice, added %d", len(*added))
	}
}

func TestPowerOffEventKeptIfItCannotBeAdded(t *testing.T) {
	fakeEvents(t, true, errors.New("no dbus"))
	path := filepath.Join(t.TempDir(), "events")

	mc := ModemController{}
	mc.recordPowerOff(path, powerOffEvent{Time: time.Now(), Reason: "one"})

	if left := loadPowerOffEvents(path); len(left) != 1 {
		t.Errorf("%d events saved, want 1", len(left))
	}
}

func TestPowerOffEventsAreCapped(t *testing.T) {
	fakeEvents(t, false, nil)
	path := filepath.Join(t.TempDir(), "events")
	start := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)

	mc := ModemController{}
	for i := range maxPowerOffEvents + 5 {
		mc.recordPowerOff(path, powerOffEvent{Time: start.Add(time.Duration(i) * time.Minute)})
	}

	events := loadPowerOffEvents(path)
	if len(events) != maxPowerOffEvents {
		t.Fatalf("saved %d events, want %d", len(events), maxPowerOffEvents)
	}
	if want := start.Add(5 * time.Minute); !events[0].Time.Equal(want) {
		t.Errorf("oldest event = %v, want %v", events[0].Time, want)
	}
}
