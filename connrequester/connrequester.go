package connrequester

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/TheCacophonyProject/go-utils/logging"
	"github.com/godbus/dbus"
)

var log = logging.NewLogger("info")

const (
	dbusPath      = "/org/cacophony/modemd"
	dbusDest      = "org.cacophony.modemd"
	methodBase    = "org.cacophony.modemd"
	wifiInterface = "wlan0"

	pingTimeout = 5
)

var (
	maxRetryInterval        = time.Hour * 2
	requestinterval         = time.Second * 20
	connectionCheckInterval = time.Second * 10
	hosts                   = []string{
		"8.8.8.8",
		"8.8.4.4",
	}
)

type ConnectionRequester struct {
	stateChange     chan bool
	sendRequests    bool
	checkConnection func() bool
}

// NewConnectionRequester will return a ConnectionRequester.
// No connection will be requested until Start is called
func NewConnectionRequester() *ConnectionRequester {
	cr := &ConnectionRequester{
		stateChange:     make(chan bool, 1),
		sendRequests:    false,
		checkConnection: CheckConnection,
	}
	go cr.requestConnections()
	return cr
}

// WaitUntilUp will wait until a connection has been made returning an error
// if no connection is made in the given duration
func (cr *ConnectionRequester) WaitUntilUp(timeout time.Duration) error {
	connectionTimeout := time.After(timeout)
	for {
		if cr.checkConnection() {
			return nil
		}
		select {
		case <-connectionTimeout:
			return errors.New("connection failed")
		case <-time.After(time.Second):
		}
	}
}

// WaitUntilUpLoop will wait until a connection has been made returning an error
// if no connection is made.
// timeout is the time given to make a connection each try.
// retryAfter is the duration between attempts, it will multipy by 1.5 after
// each try with a maximum of 2 hours. A connection isn't requested during this
// time, to save power, but it is still checked for so one that comes up (e.g.
// WiFi) gets used straight away. Connections are being requested on return.
// maxRetries is how many times it will try to make a connection. If -1 it will
// try until a connection is made.
func (cr *ConnectionRequester) WaitUntilUpLoop(
	timeout time.Duration,
	retryAfter time.Duration,
	maxRetries int) error {
	retry := 0
	for {
		if err := cr.WaitUntilUp(timeout); err == nil {
			return nil
		}
		if maxRetries != -1 && retry >= maxRetries {
			return errors.New("no connection made")
		}
		retry++
		cr.Stop() // Stopping requesting to save power
		log.Printf("connection failed. Checking every %v and requesting again in %v", connectionCheckInterval, retryAfter)
		connected := cr.waitForConnection(retryAfter)
		cr.Start()
		if connected {
			return nil
		}
		retryAfter = retryAfter * 3 / 2
		if retryAfter > maxRetryInterval {
			retryAfter = maxRetryInterval
		}
	}
}

// waitForConnection returns true once there is a connection, or false if there
// still isn't one after the given duration. It doesn't request a connection.
func (cr *ConnectionRequester) waitForConnection(duration time.Duration) bool {
	deadline := time.Now().Add(duration)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		time.Sleep(min(connectionCheckInterval, remaining))
		if cr.checkConnection() {
			return true
		}
	}
}

// CheckWifiConnection will return true if the wifi is connected to the internet
func CheckWifiConnection() bool {
	return isInterfaceUp(wifiInterface) && pingAllHosts(wifiInterface)
}

func isInterfaceUp(interfaceName string) bool {
	outBytes, err := exec.Command("ip", "a", "show", interfaceName).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(outBytes), "state UP")
}

func CheckConnection() bool {
	return pingAllHosts("") && checkDNS()
}

func checkDNS() bool {
	res := exec.Command("nslookup", "google.com", "-timeout=5", "-retry=3").Run() == nil
	if !res {
		log.Println("DNS lookup failed")
	}
	return res
}

func pingAllHosts(interfaceName string) bool {
	return anyHostResponds(hosts, time.Second*pingTimeout, func(host string) bool {
		return ping(interfaceName, host)
	})
}

// anyHostResponds returns true as soon as pingHost does for one of the hosts, or false once it
// has failed for all of them or the timeout has passed.
func anyHostResponds(hosts []string, pingTimeout time.Duration, pingHost func(host string) bool) bool {
	// Buffered so that the goroutines can still exit if this has already returned.
	pingChan := make(chan bool, len(hosts))
	for _, host := range hosts {
		go func(host string) {
			pingChan <- pingHost(host)
		}(host)
	}
	fails := 0
	success := false
	timeout := time.After(pingTimeout)
	for {
		select {
		case success = <-pingChan:
			if success {
				return true
			}
			fails++
			if fails >= len(hosts) {
				return false
			}
		case <-timeout:
			return false
		}
	}
}

func ping(interfaceName string, host string) bool {
	params := []string{}
	if interfaceName != "" {
		params = []string{
			"-I",
			interfaceName,
		}
	}
	params = append(params, "-n",
		"-q",
		"-c1",
		fmt.Sprintf("-w%d", pingTimeout),
		host)
	return exec.Command("ping", params...).Run() == nil
}

// Start will start requesting for a connection to be made.
func (cr *ConnectionRequester) Start() {
	cr.stateChange <- true
}

// Stop will stop requesting for a connection to be made.
func (cr *ConnectionRequester) Stop() {
	cr.stateChange <- false
}

func (cr *ConnectionRequester) requestConnections() {
	for {
		newRequestTime := make(<-chan time.Time)
		if cr.sendRequests {
			if !CheckWifiConnection() {
				if err := sendOnRequest(); err != nil {
					log.Println("error with sending dbus signal: ", err)
				}
			}
			newRequestTime = time.After(requestinterval)
		}
		select {
		case cr.sendRequests = <-cr.stateChange:
		case <-newRequestTime:
		}
	}
}

func getDbusObj() (dbus.BusObject, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, err
	}
	obj := conn.Object(dbusDest, dbusPath)
	return obj, nil
}

func sendOnRequest() error {
	obj, err := getDbusObj()
	if err != nil {
		return err
	}
	return obj.Call(methodBase+".StayOn", 0).Store()
}
