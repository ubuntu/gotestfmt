package tokenizer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Tokenize starts a reader of Event in the background that reads until the input is closed. This method starts a
// goroutine in the background and should be stopped by closing the input reader.
func Tokenize(input io.Reader) <-chan Event {
	output := make(chan Event)
	go decode(input, output)
	return output
}

type state string

const (
	stateInit         state = "init"
	stateRun          state = "run"
	stateBetweenTests state = "between_tests"
)

type stateChange struct {
	regexp       *regexp.Regexp
	inputState   state
	action       Action
	newState     state
	precondition func(jsonLine *jsonTestEvent) bool
}

var stateMachine = []stateChange{
	{
		regexp.MustCompile(`^go: downloading (?P<Package>[^\s]+) (?P<Version>.*)$`),
		stateInit,
		ActionDownload,
		stateInit,
		nil,
	},
	{
		regexp.MustCompile(`^go: (?P<Package>[^@]+)@(?P<Version>[^:]+): (?P<Output>.*)`),
		stateInit,
		ActionDownloadFailed,
		stateInit,
		nil,
	},
	{
		regexp.MustCompile(`^# (?P<Package>.*)$`),
		stateInit,
		ActionPackage,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^# (?P<Package>.*)$`),
		stateBetweenTests,
		ActionPackage,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^=== RUN\s+(?P<Test>.*)$`),
		stateInit,
		ActionRun,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^=== RUN\s+(?P<Test>.*)$`),
		stateBetweenTests,
		ActionRun,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^=== RUN\s+(?P<Test>.*)$`),
		stateRun,
		ActionRun,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^=== RUN\s+(?P<Test>.*)$`),
		stateBetweenTests,
		ActionRun,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^=== PAUSE\s+(?P<Test>.*)$`),
		stateRun,
		ActionPause,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^=== PAUSE\s+(?P<Test>.*)$`),
		stateBetweenTests,
		ActionPause,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^=== CONT\s+(?P<Test>.*)$`),
		stateBetweenTests,
		ActionCont,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^=== CONT\s+(?P<Test>.*)$`),
		stateRun,
		ActionCont,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- FAIL:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateInit,
		ActionFail,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- FAIL:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateRun,
		ActionFail,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- FAIL:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateBetweenTests,
		ActionFail,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- PASS:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateInit,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- PASS:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateRun,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- PASS:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateBetweenTests,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- SKIP:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateRun,
		ActionSkip,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\s*--- SKIP:\s+(?P<Test>[^\s]+) \(((?P<Cached>cached)|(?P<Elapsed>[^\s]*))\)$`),
		stateBetweenTests,
		ActionSkip,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^ok\s+(?P<Package>[^\s]+)\s+(\((?P<Cached>cached)\)|(?P<Elapsed>[^\s]*))(|([\s]+)coverage: ((?P<Coverage>.*)% of statements|\[no statements]))$`),
		stateRun,
		ActionPass,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^ok\s+(?P<Package>[^\s]+)\s+(\((?P<Cached>cached)\)|(?P<Elapsed>[^\s]*))(|([\s]+)coverage: ((?P<Coverage>.*)% of statements|\[no statements]))$`),
		stateInit,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^ok\s+(?P<Package>[^\s]+)\s+(\((?P<Cached>cached)\)|(?P<Elapsed>[^\s]*))(|\s+coverage: ((?P<Coverage>.*)% of statements|\[no statements]))$`),
		stateBetweenTests,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^ok\s+(?P<Package>[^\s]+)\s+(\((?P<Cached>cached)\)|(?P<Elapsed>[^\s]*))(|\s+coverage: ((?P<Coverage>.*)% of statements|\[no statements]))$`),
		stateBetweenTests,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^\?\s+(?P<Package>[^\s]+)\s+\[(?P<Output>.*)]$`),
		stateInit,
		ActionSkip,
		stateBetweenTests,
		func(jsonLine *jsonTestEvent) bool { return jsonLine == nil || jsonLine.Test == "" },
	},
	{
		regexp.MustCompile(`^\?\s+(?P<Package>[^\s]+)\s+\[(?P<Output>.*)]$`),
		stateBetweenTests,
		ActionSkip,
		stateBetweenTests,
		func(jsonLine *jsonTestEvent) bool { return jsonLine == nil || jsonLine.Test == "" },
	},
	{
		regexp.MustCompile(`^\?\s+(?P<Package>[^\s]+)\s+\[(?P<Output>.*)]$`),
		stateRun,
		ActionSkip,
		stateRun,
		func(jsonLine *jsonTestEvent) bool { return jsonLine == nil || jsonLine.Test == "" },
	},
	{
		regexp.MustCompile(`^\?\s+(?P<Package>[^\s]+)\s+(?P<Output>.*)$`),
		stateInit,
		ActionSkip,
		stateBetweenTests,
		func(jsonLine *jsonTestEvent) bool { return jsonLine == nil || jsonLine.Test == "" },
	},
	{
		regexp.MustCompile(`^\?\s+(?P<Package>[^\s]+)\s+(?P<Output>.*)$`),
		stateBetweenTests,
		ActionSkip,
		stateBetweenTests,
		func(jsonLine *jsonTestEvent) bool { return jsonLine == nil || jsonLine.Test == "" },
	},
	{
		regexp.MustCompile(`^FAIL\s+(?P<Package>[^\s]+)\s+\((?P<Elapsed>[^\s]*)\)$`),
		stateInit,
		ActionFail,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^FAIL\s+(?P<Package>[^\s]+)\s+(?P<Elapsed>[^\s]*)$`),
		stateRun,
		ActionFail,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^FAIL\s+(?P<Package>[^\s]+)\s+(?P<Elapsed>[^\s]*)$`),
		stateBetweenTests,
		ActionFail,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^FAIL\s+(?P<Package>[^\s]+)\s+\[(?P<Output>.*)]$`),
		stateBetweenTests,
		ActionFail,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^PASS\s+(?P<Package>[^\s]+)\s+\(((?P<Elapsed>[0-9.smh]+)|(?P<Cached>cached))\)$`),
		stateInit,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^PASS\s+(?P<Package>[^\s]+)\s+\(((?P<Elapsed>[0-9.smh]+)|(?P<Cached>cached))\)$`),
		stateRun,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^PASS\s+(?P<Package>[^\s]+)\s+\(((?P<Elapsed>[^\s]*)|(?P<Cached>cached))\)$`),
		stateBetweenTests,
		ActionPass,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^SKIP\s+(?P<Package>[^\s]+)\s+\(((?P<Elapsed>[^\s]*)|(?P<Cached>cached))\)$`),
		stateInit,
		ActionSkip,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^SKIP\s+(?P<Package>[^\s]+)\s+\(((?P<Elapsed>[^\s]*)|(?P<Cached>cached))\)$`),
		stateRun,
		ActionSkip,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^SKIP\s+(?P<Package>[^\s]+)\s+\(((?P<Elapsed>[^\s]*)|(?P<Cached>cached))\)$`),
		stateBetweenTests,
		ActionSkip,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^FAIL$`),
		stateInit,
		ActionFailFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^FAIL$`),
		stateRun,
		ActionFailFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^FAIL$`),
		stateBetweenTests,
		ActionFailFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^PASS$`),
		stateInit,
		ActionPassFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^PASS$`),
		stateRun,
		ActionPassFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^PASS$`),
		stateBetweenTests,
		ActionPassFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^SKIP$`),
		stateInit,
		ActionSkipFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^SKIP$`),
		stateRun,
		ActionSkipFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^SKIP$`),
		stateBetweenTests,
		ActionSkipFinal,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^coverage: (?P<Coverage>.*)% of statements$`),
		stateBetweenTests,
		ActionCoverage,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^coverage: (?P<Coverage>.*)% of statements$`),
		stateRun,
		ActionCoverage,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^coverage: (?P<Coverage>.*)% of statements$`),
		stateInit,
		ActionCoverage,
		stateInit,
		nil,
	},
	{
		regexp.MustCompile(`^coverage: \[no statements]$`),
		stateBetweenTests,
		ActionCoverageNoStatements,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^coverage: \[no statements]$`),
		stateRun,
		ActionCoverageNoStatements,
		stateRun,
		nil,
	},
	{
		regexp.MustCompile(`^(?P<Output>.*)$`),
		stateInit,
		ActionStdout,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^(?P<Output>.*)$`),
		stateBetweenTests,
		ActionStdout,
		stateBetweenTests,
		nil,
	},
	{
		regexp.MustCompile(`^(?P<Output>.*)$`),
		stateRun,
		ActionStdout,
		stateRun,
		nil,
	},
}

func decode(input io.Reader, output chan<- Event) {
	defer close(output)
	var lastBuffer []byte
	buffer := make([]byte, 4096)
	currentState := stateInit
	for {
		n, err := input.Read(buffer)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				panic(fmt.Errorf("failed to read from input (%w)", err))
			}
			break
		}
		if n == 0 {
			break
		}

		lines := bytes.Split(append(lastBuffer, buffer[:n]...), []byte("\n"))
		lastBuffer = lines[len(lines)-1]
		lines = lines[:len(lines)-1]
		for _, line := range lines {
			line = bytes.TrimSuffix(line, []byte("\r"))
			currentState = parseLine(currentState, line, output)
		}
	}
	_ = parseLine(currentState, lastBuffer, output)
}

func tryParseJSONLine(line []byte) *jsonTestEvent {
	if len(line) == 0 || line[0] != 123 {
		return nil
	}

	// Try to decode JSON line
	decoder := json.NewDecoder(bytes.NewReader(line))
	jsonLine := &jsonTestEvent{}
	if err := decoder.Decode(jsonLine); err != nil {
		return nil
	}
	return jsonLine
}

func parseLine(currentState state, line []byte, output chan<- Event) state {
	jsonLine := tryParseJSONLine(line)
	if jsonLine != nil {
		if jsonLine.Output == nil {
			return currentState
		}
		line = []byte(strings.TrimRight(*jsonLine.Output, "\r\n"))
	}

	for _, stateTransition := range stateMachine {
		if stateTransition.inputState != currentState {
			continue
		}

		// Don't match the "package" action against a JSON lines - issue #52
		if jsonLine != nil && stateTransition.action == ActionPackage {
			continue
		}

		if stateTransition.precondition != nil && !stateTransition.precondition(jsonLine) {
			continue
		}

		if match := stateTransition.regexp.FindSubmatch(line); len(match) != 0 {
			elapsed, err := getTimeElapsed(stateTransition.regexp, match, "Elapsed")
			if err == nil {
				coverageString := string(extract(stateTransition.regexp, match, "Coverage"))
				coverage := -1.0
				if coverageString != "" {
					coverage, err = strconv.ParseFloat(coverageString, 64)
					if err != nil {
						continue
					}
				}
				var coveragePtr *float64
				if coverage >= 0 {
					coveragePtr = &coverage
				}

				pkg := string(extract(stateTransition.regexp, match, "Package"))
				if jsonLine != nil && pkg == "" {
					pkg = jsonLine.Package
				}
				version := string(extract(stateTransition.regexp, match, "Version"))
				test := string(extract(stateTransition.regexp, match, "Test"))
				if jsonLine != nil && test == "" {
					test = jsonLine.Test
				}
				cached := string(extract(stateTransition.regexp, match, "Cached")) == "cached"
				received := time.Now()
				if jsonLine != nil && jsonLine.Time != nil {
					received = *jsonLine.Time
				}
				if jsonLine != nil && jsonLine.Elapsed != nil && *jsonLine.Elapsed > 0 {
					elapsed = time.Duration(*jsonLine.Elapsed * float64(time.Second))
				}
				evt := Event{
					Received: received,
					Action:   stateTransition.action,
					Package:  pkg,
					Version:  version,
					Test:     test,
					Cached:   cached,
					Coverage: coveragePtr,
					Elapsed:  elapsed,
					Output:   extract(stateTransition.regexp, match, "Output"),
					JSON:     jsonLine != nil,
				}

				output <- evt
				return stateTransition.newState
			}
		}
	}
	if len(line) != 0 {
		panic(fmt.Errorf("failed to match line: %v", line))
	}
	return currentState
}

func getTimeElapsed(r *regexp.Regexp, match [][]byte, name string) (time.Duration, error) {
	val := extract(r, match, name)
	if val == nil {
		return 0, nil
	}
	return time.ParseDuration(string(val))
}

func extract(r *regexp.Regexp, match [][]byte, name string) []byte {
	idx := r.SubexpIndex(name)
	if idx < 0 {
		return nil
	}
	return match[idx]
}

type jsonTestEvent struct {
	Time    *time.Time `json:",omitempty"`
	Action  string
	Package string   `json:",omitempty"`
	Test    string   `json:",omitempty"`
	Elapsed *float64 `json:",omitempty"`
	Output  *string  `json:",omitempty"`
}
