package adapter

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// TestServeConcurrentEmitDoesNotCorruptLines is the F2 regression: Serve's
// emit() used to write to a shared bufio.Writer from the main thread and
// from goroutines the run function spawns (agent-CLI stderr scanners) with
// no synchronization, so a stderr progress line and the result JSON could
// interleave mid-line. 16 goroutines x 100 progress lines must all arrive
// as intact JSON lines.
func TestServeConcurrentEmitDoesNotCorruptLines(t *testing.T) {
	oldStdin, oldStdout := os.Stdin, os.Stdout
	defer func() { os.Stdin, os.Stdout = oldStdin, oldStdout }()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin, os.Stdout = inR, outW

	// Collect stdout lines concurrently (pipe buffer is only 64KB).
	var lines []string
	var linesMu sync.Mutex
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			linesMu.Lock()
			lines = append(lines, sc.Text())
			linesMu.Unlock()
		}
	}()

	const goroutines, perGoroutine = 16, 100
	run := func(env *envelope.Envelope, progress func(string)) (*Result, error) {
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < perGoroutine; i++ {
					progress(strings.Repeat("p", 200))
				}
			}(g)
		}
		wg.Wait()
		return &Result{Summary: "done"}, nil
	}

	serveDone := make(chan int, 1)
	go func() { serveDone <- Serve("fake", []string{"code"}, run) }()

	// Feed the run request (Serve reads exactly one line, then runs).
	req, _ := json.Marshal(RunRequest{Type: MsgRun, Envelope: testEnv()})
	if _, err := inW.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	code := <-serveDone
	if code != 0 {
		t.Fatalf("Serve exit code = %d, want 0", code)
	}
	outW.Close()
	<-scanDone

	linesMu.Lock()
	defer linesMu.Unlock()
	want := 1 + goroutines*perGoroutine + 1 // hello + progress + result
	if len(lines) != want {
		t.Fatalf("got %d output lines, want %d", len(lines), want)
	}
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d is not valid JSON (interleaved emit): %v\nline: %.200s", i, err, line)
		}
	}
	if lines[0] == "" || !strings.Contains(lines[0], `"hello"`) {
		t.Fatalf("first line should be hello, got: %.200s", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], `"result"`) {
		t.Fatalf("last line should be result, got: %.200s", lines[len(lines)-1])
	}
}
