package modemd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tarm/serial"
)

var (
	ErrATPortNotFound      = errors.New("the AT port was not found in the given time limit")
	ErrATQueueTimeout      = errors.New("timeout waiting in queue for AT command to be run")
	ErrATTimeout           = errors.New("timeout waiting in que for AT command response")
	ErrATCommandFailed     = errors.New("AT command failed")
	ErrATErrorResponse     = errors.New("error response from AT command")
	ErrATTestCommandFailed = errors.New("test AT command failed")
)

type ATError struct {
	Cause  error
	Cmd    string
	Detail string
}

func (e *ATError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("failed to run AT command '%s' because %s", e.Cmd, e.Cause)
	}
	return fmt.Sprintf("failed to run AT command '%s' because %s, extra details: %s", e.Cmd, e.Cause, e.Detail)
}

func (e *ATError) Unwrap() error { return e.Cause }

// Struct for AT command requests. When/if a command is run, the result will be sent to the reply channel
type atRequest struct {
	cmd     string      // The AT command to be run.
	reply   chan result // Where the result will be sent.
	timeout time.Time   // When the command gets processed, if this time has been passed it will skip the command.
	retries int         // Will retry the command if not getting an OK response from the command.
}

// Struct for AT command response
type result struct {
	resp string
	err  error
}

// Struct for managing the AT commands
type atManager struct {
	requests chan atRequest
}

func newATManager() *atManager {
	am := &atManager{
		requests: make(chan atRequest, 100),
	}
	go am.processRequestsLoop()
	return am
}

func (am *atManager) asyncRequest(cmd string, timeout time.Time, retries int) chan (result) {
	// Make AT request
	req := atRequest{cmd: cmd, reply: make(chan result), timeout: timeout, retries: retries}
	// Send request to queue
	am.requests <- req
	// Return channel
	return req.reply
}

func (am *atManager) request(cmd string, timeoutmSec int, retries int) (string, error) {
	// Make async request
	reply := am.asyncRequest(cmd, time.Now().Add(time.Duration(timeoutmSec)*time.Millisecond), retries)
	// Wait for reply
	result := <-reply
	return result.resp, result.err
}

// Function to process the AT commands one by one.
func (am *atManager) processRequestsLoop() {
	for {
		req := <-am.requests
		req.reply <- processATRequest(req)
	}
}

func processATRequest(req atRequest) result {
	// TODO: Check if the command is available with ?
	atPort := "/dev/UsbModemAT"

	// Check if the request has timed out while waiting in the queue.
	if time.Now().After(req.timeout) {
		return result{"", &ATError{Cause: ErrATQueueTimeout}}
	}

	// Loop for trying to run the command multiple times.
	retryCount := 0
	var lastErr error // Setting this error variable so when we return, if it failed we can include the last error in the result.
	for {
		// Loop to wait for the AT port to be available.
		for {
			_, err := os.Stat(atPort)
			if err == nil {
				break // Serial port is available.
			} else if !errors.Is(err, os.ErrNotExist) {
				log.Errorf("Error checking for AT port: %v", err) // Error that isn't just "file not found".
			}

			// Wait a bit until looking again
			time.Sleep(100 * time.Millisecond)

			// Check if the request has timed out while waiting for the AT port.
			if time.Now().After(req.timeout) {
				return result{"", &ATError{Cause: ErrATPortNotFound}}
			}
		}

		// Check if the request has timed out.
		if time.Now().After(req.timeout) {
			atErr := &ATError{Cause: ErrATTimeout, Cmd: req.cmd}
			if lastErr != nil {
				// If the error is not nil, add it to the details.
				// This is intended for if it failed after multiple retries to show what the last error was.
				atErr.Detail = lastErr.Error()
			}
			return result{"", atErr}
		}

		// Check if it has been tried too many times.
		if retryCount > req.retries {
			atErr := &ATError{Cause: ErrATCommandFailed, Cmd: req.cmd}
			if lastErr != nil {
				// If the error is not nil, add it to the details.
				// This is intended for if it failed after multiple retries to show what the last error was.
				atErr.Detail = lastErr.Error()
			}
			return result{"", atErr}
		}
		retryCount++

		response, err := attemptATCommand(req, atPort)
		if err == nil {
			return result{response, nil} // Success in running AT command.
		}
		lastErr = err                      // When trying again, if timeout or retry limit is reached, we can include the last error in the result.
		time.Sleep(100 * time.Millisecond) // Wait a little bit before trying again.
	}
}

// Will attempt to run an AT command including running the ATE0 command to disable echo.
func attemptATCommand(req atRequest, atPort string) (string, error) {
	// Get serial port
	serialConfig := &serial.Config{Name: atPort, Baud: 115200, ReadTimeout: time.Second}
	serialPort, err := serial.OpenPort(serialConfig)
	if err != nil {
		return "", fmt.Errorf("failed to open serial port: %v", err)
	}
	defer serialPort.Close()

	// Disable echo. This makes it easier to process the response and also checks that the AT port is working.
	fullResponse, _, err := runATCommand(serialPort, "ATE0")
	log.Debugf("AT command 'ATE0' full response: %s", formatFullResponse(fullResponse))
	if errors.Is(err, ErrATErrorResponse) {
		// The AT command was run successfully, but it got an error response.
		// We know that this command should work so we will try again.
		// Am setting err to ErrATTestCommandFailed so if we time out/get to retry limit it will add this error to the result.
		log.Errorf("ATE0 command failed. Error: %v, Full response: '%s'", err, formatFullResponse(fullResponse))
		return "", ErrATTestCommandFailed
	}
	if err != nil {
		// There was an error with running the ATE0 command.
		return "", fmt.Errorf("failed to run ATE0 command. Error: %v, Full response: %s", err, formatFullResponse(fullResponse))
	}

	// Run the given AT command
	fullResponse, response, err := runATCommand(serialPort, req.cmd)
	log.Debugf("AT command '%s' full response: %s", req.cmd, formatFullResponse(fullResponse))
	return response, withATResponse(err, fullResponse)
}

// withATResponse adds the modem's reply to an error response, as that can say why the command failed.
func withATResponse(err error, fullResponse string) error {
	if errors.Is(err, ErrATErrorResponse) {
		return fmt.Errorf("%w: %s", err, formatFullResponse(fullResponse))
	}
	return err
}

// Will return the
func formatFullResponse(fullResponse string) string {
	lines := strings.Split(fullResponse, "\n")
	if len(lines) <= 1 {
		return fmt.Sprintf("'%s'", fullResponse)
	}
	formattedResponse := "\n" // Start it on a new line if it has multiple lines.
	for i, line := range strings.Split(fullResponse, "\n") {
		formattedResponse += "  " + line
		if i < len(lines)-1 {
			formattedResponse += "\n"
		}
	}
	return formattedResponse
}

// runATCommand will run a singular AT command. It will return the total output and also the last line that wasn't empty or the OK/ERROR response.
func runATCommand(serialPort *serial.Port, atCommand string) (totalResponse, lastLine string, err error) {
	// Send command
	if err = serialPort.Flush(); err != nil {
		return "", "", fmt.Errorf("failed to flush serial: %w", err)
	}
	if _, err = serialPort.Write([]byte(atCommand + "\r")); err != nil {
		return "", "", fmt.Errorf("failed to write AT command: %w", err)
	}
	if err := serialPort.Flush(); err != nil {
		return "", "", fmt.Errorf("failed to flush serial: %w", err)
	}

	time.Sleep(10 * time.Millisecond) // Optional

	// Setup timeout deadline
	deadline := time.Now().Add(time.Second)
	buffer := make([]byte, 512)
	var lineBuf bytes.Buffer
	var output []string

	// Read output while deadline has not been reached
	for time.Now().Before(deadline) {
		n, err := serialPort.Read(buffer)
		if err != nil {
			if err == io.EOF {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return "", "", fmt.Errorf("read error: %w", err)
		}
		if n == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		lineBuf.Write(buffer[:n])
		for {
			line, err := lineBuf.ReadString('\n')
			if err != nil {
				// Incomplete line, wait for more data
				break
			}

			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			output = append(output, line)

			switch {
			case line == "OK":
				return strings.Join(output, "\n"), strings.Join(output[:len(output)-1], "\n"), nil
			case line == "ERROR", strings.HasPrefix(line, "+CME ERROR"), strings.HasPrefix(line, "+CMS ERROR"):
				return strings.Join(output, "\n"), strings.Join(output[:len(output)-1], "\n"), ErrATErrorResponse
			}
		}
	}

	return strings.Join(output, "\n"), "", fmt.Errorf("timeout waiting for response")
}
