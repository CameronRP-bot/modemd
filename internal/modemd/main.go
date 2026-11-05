/*
modemd - Communicates with USB modems
Copyright (C) 2019, The Cacophony Project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program. If not, see <http://www.gnu.org/licenses/>.
*/

package modemd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/TheCacophonyProject/event-reporter/v3/eventclient"
	"github.com/TheCacophonyProject/go-config"
	"github.com/TheCacophonyProject/go-utils/logging"
	arg "github.com/alexflint/go-arg"
	"periph.io/x/periph/host"
)

type Args struct {
	ConfigDir    string `arg:"-c,--config" help:"path to configuration directory"`
	Timestamps   bool   `arg:"-t,--timestamps" help:"include timestamps in log output"`
	RestartModem bool   `arg:"-r,--restart" help:"cycle the power to the USB port"`
	logging.LogArgs
}

var version = "<not set>"
var log = logging.NewLogger("info")
var defaultArgs = Args{
	ConfigDir: config.DefaultConfigDir,
}

func (Args) Version() string {
	return version
}

const modemSetupSteps = 10

const (
	pingTimeoutSec      = 5                // Timeout for pinging each test host.
	pingRetryDelay      = 2 * time.Second  // Time between ping tests while waiting for a connection.
	pingPowerOnAttempts = 2                // Times to power on the modem to get a ping before waiting RetryInterval.
	pingRecheckInterval = 30 * time.Second // Time between regular ping tests after one has failed.
	modemInUseInterval  = 30 * time.Second // Time between checks that a connected modem is still needed.
)

func procArgs(input []string) (Args, error) {
	args := defaultArgs

	parser, err := arg.NewParser(arg.Config{}, &args)
	if err != nil {
		return Args{}, err
	}
	err = parser.Parse(input)
	if errors.Is(err, arg.ErrHelp) {
		parser.WriteHelp(os.Stdout)
		os.Exit(0)
	}
	if errors.Is(err, arg.ErrVersion) {
		fmt.Println(version)
		os.Exit(0)
	}
	return args, err
}

func Run(inputArgs []string, ver string) error {
	version = ver
	args, err := procArgs(inputArgs)
	if err != nil {
		return fmt.Errorf("failed to parse args: %v", err)
	}
	log = logging.NewLogger(args.LogLevel)

	log.Infof("Running version: %s", version)

	if _, err := host.Init(); err != nil {
		return err
	}

	conf, err := ParseModemdConfig(args.ConfigDir)
	if err != nil {
		return err
	}

	log.Printf("%+v\n", conf)

	// We had issue when the raspberry pi was starting up and the modem was being powered
	// on at the same time when powered from a 1S Li-ion battery. This would sometimes cause
	// power supply issues causing odd modem issues, or get stuck in a boot loop.
	// This just waits for the RPI to be up for at least 10 seconds.
	rpiUptime, err := rpiUptime()
	log.Infof("RPI Uptime: %s", rpiUptime)
	if err != nil {
		log.Errorf("Failed to get rpi uptime: %s. Waiting 10 seconds", err)
		time.Sleep(10 * time.Second)
	}
	if rpiUptime < 2*time.Minute {
		log.Info("Camera has been on for less than 2 minutes. Waiting 10 seconds. This is to prevent power supply issues when the RPi is still starting up and starting the modem at the same time.")
		time.Sleep(10 * time.Second)
	}

	// For now we are just loading this one modem, ignoring the ones set in the config.
	m := []ModemConfig{
		{Name: "Qualcomm", NetDev: "usb0", VendorID: "1e0e", ProductID: "9018"},
	}

	mc := ModemController{
		StartTime: time.Now(),
		//ModemsConfig:           conf.ModemsConfig,
		ModemsConfig:      m,
		TestHosts:         conf.TestHosts,
		TestInterval:      conf.TestInterval,
		PowerPin:          conf.PowerPin,
		InitialOnDuration: conf.InitialOnDuration,
		FindModemDuration: conf.FindModemDuration,
		ConnectionTimeout: conf.ConnectionTimeout,
		PingWaitTime:      conf.PingWaitTime,
		//PingRetries:            conf.PingRetries,
		RequestOnDuration:      conf.RequestOnDuration,
		RetryInterval:          conf.RetryInterval,
		RetryFindModemInterval: conf.RetryFindModemInterval,
		RetrySimCardInterval:   retrySimCardInterval,
		MinConnDuration:        conf.MinConnDuration,
		MaxOffDuration:         conf.MaxOffDuration,
	}

	mc.simCardFailsInARow = loadSimCardFailsInARow(simCardFailsFile)
	if mc.simCardFailsInARow > 0 {
		log.Infof("Previously failed to find a SIM card %d times in a row.", mc.simCardFailsInARow)
	}

	log.Println("Starting dbus service.")
	if err := startService(&mc); err != nil {
		return err
	}

	if !mc.ShouldBeOn() || args.RestartModem {
		if err := mc.SetModemPower(false); err != nil {
			return err
		}
	}

	// Setting the label for the main loop
	simCardAttempts := 0
	pingAttempts := 0
MainModemLoop:
	for {
		// =========== Power off modem if it shouldn't be on then wait until it should be on ===========
		if !mc.ShouldBeOn() {
			log.Println("Powering off USB modem.")
			if err := mc.powerOffModem(mc.onOffReason); err != nil {
				return err
			}
			mc.Modem = nil
			simCardAttempts = 0
			pingAttempts = 0
			// Wait until the modem no longer should be off
			for !mc.ShouldBeOn() {
				time.Sleep(time.Second)
			}
		}

		// =========== Power on modem ===========
		printSetupStep(1, "Powering on USB modem.")
		if err := mc.SetModemPower(true); err != nil {
			return err
		}

		// =========== Finding modem ===========
		// Failed to find the modem recently so we shouldn't try to find it again yet.
		if mc.waitingToRetryFindModem() {
			log.Infof("Failed to find the USB modem. Not trying to find it again until %v has passed.", mc.RetryFindModemInterval)
			// Wait until the modem doesn't need to be on, or it's time to try finding it again.
			// We just wait here so the modem status can still be queried.
			for mc.ShouldBeOn() && mc.waitingToRetryFindModem() {
				time.Sleep(time.Second)
			}
			continue MainModemLoop
		}
		printSetupStep(2, "Finding USB modem.")
		findingModemTimeout := time.Now().Add(mc.FindModemDuration)
		productID := ""
	FindModemLoop:
		for {
			vendorProductIDs, err := getUSBVendorProductIDs()
			if err != nil {
				log.Errorf("Failed to list usb devices: %v", err)
			}

			// Loop through the different modems that we support (just the one for now) to see if we can find the modem
			for _, modemConfig := range mc.ModemsConfig {
				for _, vendorProductID := range vendorProductIDs {
					if modemConfig.VendorID == vendorProductID.VendorID {
						log.Infof("Found modem with vendorID '%s'", modemConfig.VendorID)
						mc.Modem = NewModem(modemConfig)
						productID = vendorProductID.ProductID
						break FindModemLoop
					}
				}
			}

			// Timeout for finding the modem through USB
			if time.Now().After(findingModemTimeout) {
				// Log the devices found on lsusb. This is simply to help debug modem issues.
				log.Infof("Failed to find modem in given time '%s', here are the usb devices on the system:", mc.FindModemDuration)
				out, err := exec.Command("lsusb").CombinedOutput()
				if err != nil {
					log.Errorf("Failed to list usb devices: %v", err)
				}
				for _, line := range strings.Split(string(out), "\n") {
					line := strings.TrimSpace(line)
					if line != "" {
						log.Infof("\t%s", line)
					}
				}

				// Set that it failed to find the modem and return to the start of the main loop.
				mc.failedToFindModem = true
				mc.lastFailedFindModem = time.Now()
				log.Println("Making noModemFound event.")
				err = eventclient.AddEvent(eventclient.Event{
					Timestamp: time.Now(),
					Type:      "noModemFound",
				})
				if err != nil {
					log.Errorf("Failed to make noModemFound event: %v", err)
				}
				continue MainModemLoop
			}
		}
		mc.failedToFindModem = false

		// ========== Checking for AT response from modem. =============
		printSetupStep(3, "Checking for AT response from modem.")
		checkATTimeout := time.Now().Add(time.Minute)
		mc.Modem.ATManager = newATManager()
		for {
			// Try to see if AT command is available yet
			_, err := mc.Modem.ATManager.request("AT", 1000, 0)
			if err == nil {
				log.Println("AT command responding.")
				mc.Modem.ATReady = true
				// Enable verbose error messages so a failed AT command says why.
				if _, err := mc.RunATCommand("AT+CMEE=2", 1000, 1); err != nil {
					log.Errorf("Failed to enable verbose AT errors: %v", err)
				}
				break
			}

			if time.Now().After(checkATTimeout) {
				log.Error("Failed to find AT command in given time.")
				log.Println("Making noModemATCommandResponse event.")
				err := eventclient.AddEvent(eventclient.Event{
					Timestamp: time.Now(),
					Type:      "noModemATCommandResponse",
				})
				if err != nil {
					log.Errorf("Failed to make noModemATCommandResponse event: %v", err)
				}
				// TODO, if AT command is not responding then what?
				// If it is in the correct USB mode then just continue? if not then how should I handle this?
				// If it is not in the correct mode or does't have the USB0 up and running then we should reset the modem?
				break // For now we just continue to the next step

			}
			time.Sleep(time.Second)
		}

		printSetupStep(4, "Disabling GPS.")
		err := mc.DisableGPS()
		if err != nil {
			log.Error("Failed to disable GPS: ", err)
			// Not a critical error so will continue to next step.
			// TODO, if disabling GPS failed, then we should try again?
		}

		// ========== Checking that the modem is in the correct mode ==========
		printSetupStep(5, "Checking that the modem is in the correct mode.")
		if productID == mc.Modem.ProductID {
			log.Infof("Modem is in the correct mode. '%s'", productID)
		} else {
			log.Infof("Modem is not in the correct mode. '%s' != '%s'", productID, mc.Modem.ProductID)
			log.Infof("Moving modem to the new mode '%s'", mc.Modem.ProductID)
			err := mc.SetUSBMode(mc.Modem.ProductID)
			if err != nil {
				log.Errorf("Failed to set USB mode: %v", err)
			}

			// Trigger reset with AT+CRESET
			_, err = mc.RunATCommand("AT+CRESET", 2000, 3)
			if err != nil {
				log.Errorf("Failed to reset modem: %v", err)
			}

			// Wait for modem to go offline then back online with the correct product ID
			modemOffBackUpTimeout := time.Now().Add(time.Minute)
			for {
				if time.Now().After(modemOffBackUpTimeout) {
					log.Error("Failed to find modem in given time after changing USB mode.")
					// TODO, how should I try to recover from this?
					continue MainModemLoop
				}

				vendorProductIDs, err := getUSBVendorProductIDs()
				if err != nil {
					log.Errorf("Failed to get USB vendor product IDs: %v", err)
					time.Sleep(time.Second)
					continue
				}
				for _, vendorProductID := range vendorProductIDs {
					if vendorProductID.VendorID == mc.Modem.VendorID && vendorProductID.ProductID == mc.Modem.ProductID {
						log.Infof("Modem is back online with correct product ID. '%s'", mc.Modem.ProductID)
						continue MainModemLoop
					}
				}
			}
		}

		// =========== Checking SIM card in modem ===========
		// If the modem failed to find a SIM card recently, then we shouldn't try to find it again yet.
		if mc.waitingToRetrySimCard() {
			if mc.gaveUpOnSimCard() {
				log.Infof("Modem failed to find a SIM card %d times in a row. Will not try to find it again until modemd restarts.", mc.simCardFailsInARow)
			} else {
				log.Infof("Modem failed to find a SIM card. Will not try to find it again until %v has passed.", mc.RetrySimCardInterval)
			}
			// We just wait here so the modem status can still be queried.
			for mc.ShouldBeOn() && mc.waitingToRetrySimCard() {
				time.Sleep(time.Second)
			}
			continue MainModemLoop
		}
		simCardAttempts++
		printSetupStep(6, fmt.Sprintf("Checking SIM card (power on attempt %d of %d).", simCardAttempts, mc.simCardPowerOnAttemptsAllowed()))
		if waitForSimCard(mc.CheckSimCard, simCardCheckTimeout, simCardCheckInterval) {
			mc.Modem.SimCardStatus = SimCardReady
		}
		if mc.Modem.SimCardStatus != SimCardReady {
			if simCardAttempts < mc.simCardPowerOnAttemptsAllowed() {
				mc.networkDiagnostics()
				log.Info("SIM card not found. Cycling modem power to try again.")
				if err := mc.powerOffModem("Cycling power because the SIM card was not found."); err != nil {
					return err
				}
				mc.Modem = nil
				continue MainModemLoop
			}
			simCardAttempts = 0
			mc.Modem.SimCardStatus = SimCardFailed
			makeModemFailureEvent("noModemSimCard", &mc)
			mc.failedToFindSimCard = true
			mc.lastFailedSimCard = time.Now()
			mc.simCardFailsInARow++
			saveSimCardFailsInARow(simCardFailsFile, mc.simCardFailsInARow)
			if mc.gaveUpOnSimCard() {
				log.Infof("Failed to find a SIM card %d times in a row. Will not try again until modemd restarts.", mc.simCardFailsInARow)
			} else {
				log.Infof("Failed to find a SIM card (%d of %d times in a row before giving up until modemd restarts).", mc.simCardFailsInARow, maxSimCardFailsInARow)
			}
			continue MainModemLoop // Go back to start of main loop.
		}
		simCardAttempts = 0
		mc.failedToFindSimCard = false
		if mc.simCardFailsInARow != 0 {
			mc.simCardFailsInARow = 0
			saveSimCardFailsInARow(simCardFailsFile, 0)
		}
		log.Info("SIM card ready.")

		// ========== Checking signal strength ===========
		printSetupStep(7, "Checking signal strength.")
		getSignalStrengthTimeout := time.Now().Add(2 * time.Minute)
		lastRegistrationState := ""
		for {
			strengthStr, bitErrorRate, status, err := mc.signalStrength()
			if signalFound(strengthStr, err) {
				log.Printf("Signal strength: %d", strengthStr)
				log.Printf("Bit error rate: %d", bitErrorRate)
				log.Printf("Signal status: %s", status)
				makeModemEvent("modemSignal", &mc)
				break
			}
			lastRegistrationState = logRegistrationChange(&mc, lastRegistrationState)
			log.Debugf("Signal strength not found (error: %v), waiting 3 seconds then looking again.", err)
			time.Sleep(3 * time.Second)

			if time.Now().After(getSignalStrengthTimeout) {
				log.Infof("Timed out waiting for signal strength. Last error: %v", err)
				mc.lastFailedConnection = time.Now()
				makeModemFailureEvent("noModemSignal", &mc)
				continue MainModemLoop
			}
		}

		// ========== Checking that the network is up ===========
		printSetupStep(8, "Checking that the network is up.")
		networkUpTimeout := time.Now().Add(2 * time.Minute)
		var addrs []net.Addr
		for {
			if time.Now().After(networkUpTimeout) {
				// Took too long to find the network. Wait before trying again to save power.
				makeModemFailureEvent("noModemNetwork", &mc)
				log.Errorf("Took too long to get an IPv4 address on %s, found addresses: %v", mc.Modem.Netdev, addrs)
				mc.lastFailedConnection = time.Now()
				continue MainModemLoop
			}

			iface, err := net.InterfaceByName(mc.Modem.Netdev)
			if err != nil {
				log.Debugf("Network interface not found, waiting a second then looking again. Error: %v", err)
				// Network is not up yet, wait a second then look again.
				time.Sleep(time.Second)
				continue
			}
			addrs, err = iface.Addrs()
			if err != nil {
				log.Errorf("Failed to get network addresses: %v", err)
				time.Sleep(time.Second)
				continue
			}
			// The modem gives out an IPv4 address, and a link-local IPv6 address alone can't reach the test hosts.
			if !hasIPv4Address(addrs) {
				log.Debugf("No IPv4 address found, waiting a second then looking again. Addresses: %v", addrs)
				time.Sleep(time.Second)
				continue
			}
			for _, addr := range addrs {
				log.Infof("Network address: %s", addr.String())
			}
			break
		}

		// ============ Checking ping through the network ============
		pingAttempts++
		printSetupStep(9, fmt.Sprintf("Checking ping through the network (power on attempt %d of %d).", pingAttempts, pingPowerOnAttempts))
		pingingTimeout := time.Now().Add(mc.ConnectionTimeout)
		for {
			// Check if the modem should still be on.
			if !mc.ShouldBeOn() {
				log.Info("Canceling ping test as modem should be off.")
				continue MainModemLoop
			}

			if time.Now().After(pingingTimeout) {
				// Took too long to ping.
				makeModemFailureEvent("noModemPing", &mc)
				log.Errorf("Took too long to ping.")
				if pingAttempts < pingPowerOnAttempts {
					// The modem can start up in a state where it doesn't register on the
					// network, so try again now as the camera might not be on for long.
					log.Info("Cycling modem power to try again.")
					if err := mc.powerOffModem("Cycling power because the modem could not ping."); err != nil {
						return err
					}
					mc.Modem = nil
					continue MainModemLoop
				}
				pingAttempts = 0
				mc.lastFailedConnection = time.Now()
				continue MainModemLoop
			}

			lastRegistrationState = logRegistrationChange(&mc, lastRegistrationState)

			if mc.PingTest(pingTimeoutSec) { // This ping test run the ping test through the modem, not the wifi if available.
				log.Info("Modem has connected to a network.")
				logSetupTimings()
				pingAttempts = 0
				mc.connectedTime = time.Now()
				mc.lastSuccessfulPing = mc.connectedTime
				makeModemEvent("modemConnectedToNetwork", &mc)
				sendModemConnectedSignal() // This send a dbus signal that allows programs to trigger events when the modem connects.
				break
			} else {
				log.Infof("Ping test failed. Trying again until the %s timeout.", mc.ConnectionTimeout)
				time.Sleep(pingRetryDelay) // Ping can fail straight away, e.g. if the interface has gone.
			}
		}

		// Network is all ready now I guess?

		// ========== Running regular ping tests =============
		log.Infof("Running ping tests every %s, or every %s after one fails.", mc.TestInterval, nextPingTestDelay(mc.TestInterval, 1))
		pingFailCount := 0
		for {
			if !waitWhileNeeded(nextPingTestDelay(mc.TestInterval, pingFailCount), modemInUseInterval, mc.ShouldBeOn) {
				continue MainModemLoop // The start of the main loop powers off the modem.
			}
			log.Debug("Running a regular ping test.")
			if mc.PingTest(pingTimeoutSec) {
				mc.lastSuccessfulPing = time.Now()
				pingFailCount = 0
			} else {
				pingFailCount++
				log.Infof("Ping test failed %d times in a row.", pingFailCount)
			}

			if pingFailCount > 3 {
				log.Infof("Ping test failed %d times in a row. Reporting failure.", pingFailCount)
				makeModemFailureEvent("noModemPing", &mc)
				mc.lastFailedConnection = time.Now()
				continue MainModemLoop
			}
		}
	}
}

func makeModemEvent(eventType string, mc *ModemController) {
	addModemEvent(eventType, mc, nil)
}

// makeModemFailureEvent also logs and adds diagnostics about the modem, to help explain why it
// failed. It needs to be called before the modem is powered off.
func makeModemFailureEvent(eventType string, mc *ModemController) {
	logSetupTimings()
	addModemEvent(eventType, mc, mc.networkDiagnostics())
}

func addModemEvent(eventType string, mc *ModemController, diagnostics map[string]string) {
	log.Printf("Making modem event '%s'.", eventType)
	signalStrength, bitErrorRate, status, err := mc.signalStrength()
	if err != nil {
		log.Printf("Failed to get signal strength: %s", err)
	}
	band, err := mc.readBand()
	if err != nil {
		log.Printf("Failed to get band: %s", err)
	}
	simStatus, err := mc.CheckSimCard()
	if err != nil {
		log.Printf("Failed to get sim status: %s", err)
	}
	apn, err := mc.getAPN()
	if err != nil {
		log.Printf("Failed to get apn: %s", err)
	}
	simProvider, err := mc.readSimProvider()
	if err != nil {
		log.Printf("Failed to get sim provider: %s", err)
	}
	provider, accessTechnology, err := mc.readProvider()
	if err != nil {
		log.Printf("Failed to get provider: %s", err)
	}
	iccid, err := mc.readSimICCID()
	if err != nil {
		log.Printf("Failed to get iccid: %s", err)
	}

	details := map[string]any{
		"signalStatus":     status,
		"signalStrengthDB": bitErrorRate,
		"signalStrength":   signalStrength,
		"band":             band,
		"simStatus":        simStatus,
		"apn":              apn,
		"provider":         provider,
		"accessTechnology": accessTechnology,
		"simProvider":      simProvider,
		"iccid":            iccid,
	}
	if diagnostics != nil {
		details["diagnostics"] = diagnostics
	}
	eventclient.AddEvent(eventclient.Event{
		Timestamp: time.Now(),
		Type:      eventType,
		Details:   details,
	})
}

func rpiUptime() (time.Duration, error) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, err
	}
	return time.Duration(info.Uptime) * time.Second, nil
}

func printSetupStep(i int, text string) {
	setupTimes.startStep(i, time.Now())
	log.Infof("Modem set up step (%d/%d): %s", i, modemSetupSteps, text)
}

// setupTimes is only used from the main modem loop.
var setupTimes setupTimer

// logSetupTimings logs how long the modem set up steps took, if the modem was being set up.
func logSetupTimings() {
	if summary := setupTimes.finish(time.Now()); summary != "" {
		log.Infof("Modem set up timings: %s", summary)
	}
}

// setupTimer records how long each modem set up step takes, from when step 1 starts.
type setupTimer struct {
	steps        []string
	start        time.Time
	current      int
	currentStart time.Time
}

func (t *setupTimer) startStep(step int, now time.Time) {
	if step == 1 {
		*t = setupTimer{start: now}
	}
	t.finishCurrent(now)
	t.current = step
	t.currentStart = now
}

func (t *setupTimer) finishCurrent(now time.Time) {
	if t.current != 0 {
		t.steps = append(t.steps, fmt.Sprintf("step %d %s", t.current, now.Sub(t.currentStart).Round(time.Second)))
	}
}

// finish returns a summary of the steps then clears them, or "" if no steps were running.
func (t *setupTimer) finish(now time.Time) string {
	if t.current == 0 {
		return ""
	}
	t.finishCurrent(now)
	summary := fmt.Sprintf("%s, total %s", strings.Join(t.steps, ", "), now.Sub(t.start).Round(time.Second))
	*t = setupTimer{}
	return summary
}

// signalFound returns whether a reading from signalStrength shows a signal. An error doesn't,
// as signalStrength returns a strength of 0 with it, and 99 means no signal.
func signalFound(strength int, err error) bool {
	return err == nil && strength != 99
}

func hasIPv4Address(addrs []net.Addr) bool {
	for _, addr := range addrs {
		if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
			return true
		}
	}
	return false
}

// nextPingTestDelay returns how long to wait before the next regular ping test. After a failure
// it is sooner, so a lost connection is found and reported before the camera might turn off.
func nextPingTestDelay(testInterval time.Duration, pingFailCount int) time.Duration {
	if pingFailCount > 0 {
		return min(testInterval, pingRecheckInterval)
	}
	return testInterval
}

// waitWhileNeeded waits for the duration, checking stillNeeded at each interval, and returns false
// as soon as stillNeeded does.
func waitWhileNeeded(duration, interval time.Duration, stillNeeded func() bool) bool {
	deadline := time.Now().Add(duration)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		time.Sleep(min(interval, remaining))
		if !stillNeeded() {
			return false
		}
	}
}

// logRegistrationChange logs the modem's network registration state if it has changed from last,
// to show how the modem got on while waiting for a connection, and returns the state.
func logRegistrationChange(mc *ModemController, last string) string {
	state := mc.registrationState()
	if state != last {
		log.Infof("Modem network registration: %s", state)
	}
	return state
}

type VendorProductID struct {
	VendorID  string
	ProductID string
}

func getUSBVendorProductIDs() ([]VendorProductID, error) {
	out, err := exec.Command("lsusb").Output()
	if err != nil {
		return nil, err
	}

	var vendorProductIDs []VendorProductID
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		// Lines look like:  "Bus 001 Device 006: ID 1e0e:9011 Qualcomm / Option"
		parts := strings.Fields(line)
		for i, tok := range parts {
			if tok == "ID" && i+1 < len(parts) {
				ids := strings.SplitN(parts[i+1], ":", 2)
				if len(ids) == 2 {
					vendorProductIDs = append(vendorProductIDs, VendorProductID{
						VendorID:  ids[0],
						ProductID: ids[1],
					})
				}
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return vendorProductIDs, nil
}
