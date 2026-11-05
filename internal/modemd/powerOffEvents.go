package modemd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/TheCacophonyProject/event-reporter/v3/eventclient"
	"github.com/TheCacophonyProject/modemd/connrequester"
)

const (
	powerOffEventsFile = "/var/lib/modemd/power-off-events"
	maxPowerOffEvents  = 50 // Oldest are dropped after this, e.g. if the modem never connects.
)

// Replaced in tests.
var (
	addEvent      = eventclient.AddEvent
	wifiConnected = connrequester.CheckWifiConnection
)

// powerOffEvent is a modem power off that hasn't been reported yet. They are saved to
// powerOffEventsFile so they aren't lost when the camera turns off.
type powerOffEvent struct {
	Time      time.Time
	Reason    string
	OnSeconds int
}

// powerOffModem powers off the modem and reports it, with reason saying why.
func (mc *ModemController) powerOffModem(reason string) error {
	wasPowered := mc.IsPowered
	onFor := time.Since(mc.poweredOnTime)
	if err := mc.SetModemPower(false); err != nil {
		return err
	}
	if !wasPowered {
		return nil
	}
	log.Infof("Modem powered off after %s: %s", onFor.Round(time.Second), reason)
	mc.recordPowerOff(powerOffEventsFile, powerOffEvent{
		Time:      time.Now().UTC(),
		Reason:    reason,
		OnSeconds: int(onFor.Seconds()),
	})
	return nil
}

// recordPowerOff saves the event, then reports it if there is WiFi. Without WiFi it waits until the
// modem next connects. Sending it now would make event-reporter request a connection, which would
// power the modem back on, and powering off would then make another event.
func (mc *ModemController) recordPowerOff(path string, event powerOffEvent) {
	events := append(loadPowerOffEvents(path), event)
	if len(events) > maxPowerOffEvents {
		log.Infof("Dropping %d old modem power off events.", len(events)-maxPowerOffEvents)
		events = events[len(events)-maxPowerOffEvents:]
	}
	savePowerOffEvents(path, events)
	if wifiConnected() {
		sendPowerOffEvents(path)
	}
}

// sendPowerOffEvents reports the saved events, keeping any that couldn't be added to the event queue.
func sendPowerOffEvents(path string) {
	events := loadPowerOffEvents(path)
	if len(events) == 0 {
		return
	}
	var unsent []powerOffEvent
	for _, event := range events {
		err := addEvent(eventclient.Event{
			Timestamp: event.Time,
			Type:      "modemPoweredOff",
			Details: map[string]any{
				"reason":    event.Reason,
				"onSeconds": event.OnSeconds,
			},
		})
		if err != nil {
			log.Errorf("Failed to make modemPoweredOff event: %v", err)
			unsent = append(unsent, event)
		}
	}
	savePowerOffEvents(path, unsent)
}

func loadPowerOffEvents(path string) []powerOffEvent {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Errorf("Failed to read modem power off events: %v", err)
		}
		return nil
	}
	var events []powerOffEvent
	if err := json.Unmarshal(data, &events); err != nil {
		log.Errorf("Failed to parse modem power off events: %v", err)
		return nil
	}
	return events
}

func savePowerOffEvents(path string, events []powerOffEvent) {
	if len(events) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Errorf("Failed to remove modem power off events: %v", err)
		}
		return
	}
	data, err := json.Marshal(events)
	if err != nil {
		log.Errorf("Failed to encode modem power off events: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		log.Errorf("Failed to create directory for modem power off events: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		log.Errorf("Failed to save modem power off events: %v", err)
	}
}
