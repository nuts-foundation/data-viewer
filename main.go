package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
)

type appEvent string

const (
	StartEvent appEvent = "start"
)

var showHelp bool = false
var showDebug bool = false
var hcursor int = 0
var vcursor int = 0
var lastPressed string

func main() {
	// Setup termui which provides primitives for terminal-based UI applications
	if err := ui.Init(); err != nil {
		log.Fatalf("failed to initialize termui: %v", err)
	}

	// Upon returning from main perform teardown operations for termui
	defer ui.Close()

	// Create channels for events from the UI as well as internal app events
	uiEvents := ui.PollEvents()
	appEvents := make(chan appEvent, 10)

	// Put a start event in the app events channel
	appEvents <- StartEvent

	// Handle events as they occur
	for {
		// Wait for an event to occur
		select {
		// Process UI events (keyboard/mouse input, etc.)
		case event := <-uiEvents:
			log.Printf("got ui event: %v", event)

			switch event.Type {
			case ui.KeyboardEvent:
				pressed := event.ID
				keyboardEventHandler(pressed)

			case ui.MouseEvent:
				position := event.Payload.(ui.Mouse)
				mouseEventHandler(position)

			case ui.ResizeEvent:
				dimensions := event.Payload.(ui.Resize)
				resizeEventHandler(dimensions)
			}

		// Process app events (startup etc.)
		case event := <-appEvents:
			log.Printf("got app event: %v", event)
		}

		// Render the application content
		render()
	}
}

func resizeEventHandler(dimensions ui.Resize) {}

func mouseEventHandler(position ui.Mouse) {}

var keyboardReadLineBuffer string

func keyboardEventHandler(pressed string) {
	if pressed == "#" {
		keyboardReadLineBuffer = pressed
	} else if keyboardReadLineBuffer != "" && strings.Contains("0123456789", pressed) {
		keyboardReadLineBuffer += pressed
	} else if keyboardReadLineBuffer != "" && pressed == "<Enter>" && !strings.HasSuffix(keyboardReadLineBuffer, "\n") {
		keyboardReadLineBuffer += "\n"
	} else {
		keyboardReadLineBuffer = ""

		if pressed == "q" || pressed == "Q" {
			ui.Close()
			os.Exit(0)
		} else if pressed == "?" || pressed == "<F1>" {
			showHelp = !showHelp
		} else if pressed == "ß" /* Option-D */ {
			showDebug = !showDebug
		} else if pressed == "<Left>" {
			hcursor--
		} else if pressed == "<Right>" {
			hcursor++
		} else if pressed == "<Up>" {
			vcursor--
		} else if pressed == "<Down>" {
			vcursor++
		}
	}

	lastPressed = pressed
}

func render() {
	// Clear any existing content on the terminal
	ui.Clear()

	renderDAG()

	// Optionally show the help screen on top of the app
	if showHelp {
		// Determine the size of the terminal in characters
		width, height := ui.TerminalDimensions()

		p := widgets.NewParagraph()
		p.Title = "| Help |"
		p.Text = "q | Q          - quit\n" +
			"? | <F1>       - show/hide help\n" +
			"\n" +
			"#𝑁<Enter>     - select transaction number 𝑁 \n" +
			"\n" +
			"y              - copy raw transaction to clipboard (OSC52)" +
			"Home | g       - go to transaction 0.0\n" // TODO: Implement this
		p.SetRect(0, 0, width-1, height-1)
		ui.Render(p)
	}

	if showDebug {
		// Determine the size of the terminal in characters
		width, height := ui.TerminalDimensions()
		p := widgets.NewParagraph()
		p.Title = "| Debug |"
		p.Text = "test keyboard: " + lastPressed + "\n" +
			"test readline: " + keyboardReadLineBuffer
		p.SetRect(0, 0, width-1, height-1)
		ui.Render(p)
	}
}

type transactionMap map[int][]string

var transactions transactionMap
var dagLamportClock int
var dagSubIndex int
var dagMaxLamportClock int = 9999 // TODO: This must not be hard coded

func renderDAG() {
	// Handle the user manually entering a transaction number
	if strings.HasSuffix(keyboardReadLineBuffer, "\n") {
		s := strings.TrimLeft(strings.TrimRight(keyboardReadLineBuffer, "\n"), "#")
		if n, err := strconv.ParseInt(s, 10, 32); err == nil {
			dagLamportClock = int(n)
			dagSubIndex = 0
		} else {
			log.Panicf("strconv error: %v", err)
		}
		keyboardReadLineBuffer = ""
	}

	// Handle the user browsing the DAG
	if hcursor != 0 {
		// Handle the user navigating left
		if hcursor < 0 {
			// Decrement the sub index within a particular lamport clock if possible
			if dagSubIndex > 0 {
				dagSubIndex--

				// Otherwise decrement the lamport clock if possible, resetting the sub index
			} else if dagLamportClock > 0 {
				dagLamportClock--

				// Reset the sub index to select the "rightmost" transaction within the
				// new lamport clock
				// TODO: FIX BUG HERE: dagSubIndex = len(transactions[dagLamportClock])-1
				dagSubIndex = 0 // TODO: Temporary hack for bug ^^
			}

			// Handle the user navigating right
		} else {
			// Increment the sub index within a particular lamport clock if possible
			if dagSubIndex+1 < len(transactions[dagLamportClock]) {
				dagSubIndex++

				// Otherwise increment the lamport clock if possible, resetting the sub index
			} else if dagLamportClock < dagMaxLamportClock {
				dagLamportClock++

				// Reset the sub index to select the "leftmost" transaction within the
				// new lamport clock
				dagSubIndex = 0
			}
		}

		// Reset the hcursor to 0 so that future navigation can be handled properly
		hcursor = 0
	}

	// If needed load the transactions for the desired lamport clock
	if _, ok := transactions[dagLamportClock]; !ok {
		// Load the transactions for this lamport clock into the transactions map
		transactions[dagLamportClock] = fetchTransactionsInRange(dagLamportClock, dagLamportClock+1)
	}

	// Support OSC52 clipboard copy of raw transaction data
	if lastPressed == "y" {
		print("\033]52;c;" + base64.StdEncoding.EncodeToString([]byte(transactions[dagLamportClock][dagSubIndex])) + "\a")
		lastPressed = "" // TODO: This should not be necessary and is a bit hacky
	}

	// Create a new paragraph UI widget, which can render arbitrary text
	p := widgets.NewParagraph()

	// Show the transaction # as a decimal, so that the lamport clock and sub index are visible
	if len(transactions[dagLamportClock]) > 1 {
		p.Title = fmt.Sprintf("| Transaction %d.%d |", dagLamportClock, dagSubIndex)
		// Unless there's only one, in which case just show the lamport clock
	} else {
		p.Title = fmt.Sprintf("| Transaction %d |", dagLamportClock)
	}

	// Split the transaction on dots (".") in which the first part is the base64 encoded JSON data
	transactionParts := strings.Split(transactions[dagLamportClock][dagSubIndex], ".")

	// If the transaction split was successful then perform base64 and JSON decoding
	if transactionParts != nil {
		// Decode the raw base64 data of the transaction
		if rawJSON, err := base64.RawStdEncoding.DecodeString(transactionParts[0]); err == nil {
			// Nicely format and indent the JSON
			var prettyJSON bytes.Buffer
			if err := json.Indent(&prettyJSON, rawJSON, "", "    "); err == nil {
				p.Text = prettyJSON.String()
			} else {
				p.Text = err.Error()
			}
		} else {
			// Render any decode errors
			p.Text = err.Error()
		}
	} else {
		p.Text = "error: string split failed"
	}

	// Determine the size of the terminal in characters
	width, height := ui.TerminalDimensions()

	// Use all available terminal space for the render
	p.SetRect(0, 0, width, height)

	// Print the UI to the terminal
	ui.Render(p)
}

// fetchTransactionsInRange returns the transactions where start <= lamport clock < end
func fetchTransactionsInRange(start int, end int) []string {
	// Build the URL and place the start/end of the lamport clock range in the query string
	url := fmt.Sprintf("http://127.0.0.1:1323/internal/network/v1/transaction?start=%d&end=%d", start, end)

	// Call the API endpoint
	response, err := http.Get(url)

	// If there is a response with a body ensure it is deallocated later
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}

	// If an error occurred then report an error condition
	if err != nil {
		log.Panicf("HTTP request failed: %v", err)
	}

	// Read the response body contents, risking memory allocation issues
	body, err := io.ReadAll(response.Body)

	// Handle any errors that occurred in the response body reading
	if err != nil {
		log.Panicf("failed to read response body: %v", err)
	}

	// Parse the JSON from the body
	var transactions []string
	json.Unmarshal(body, &transactions)

	// Return the transactions within the matching lambert clock range
	return transactions
}

func init() {
	transactions = make(transactionMap)
}
