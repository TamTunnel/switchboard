package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// RunFunc is an adapter's implementation: given an envelope, report progress
// and return a result (or an error). It runs in the adapter process.
type RunFunc func(env *envelope.Envelope, progress func(string)) (*Result, error)

// Serve implements the adapter side of the stdio protocol: print hello, read
// one run request, execute run, print result or error. It returns a process
// exit code. All output goes to stdout (the daemon reads it); diagnostics
// go to stderr.
func Serve(agent string, capabilities []string, run RunFunc) int {
	w := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		data, _ := json.Marshal(v)
		w.Write(data)
		w.WriteByte('\n')
		w.Flush()
	}
	emit(Hello{Type: MsgHello, Protocol: ProtocolVersion, Agent: agent, Capabilities: capabilities})

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	if !sc.Scan() {
		emit(ErrorMsg{Type: MsgError, Message: "no run request (stdin closed)"})
		return 1
	}
	var req RunRequest
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil || req.Type != MsgRun || req.Envelope == nil {
		emit(ErrorMsg{Type: MsgError, Message: "malformed run request"})
		return 1
	}
	progress := func(text string) {
		emit(Progress{Type: MsgProgress, Text: text})
	}
	res, err := run(req.Envelope, progress)
	if err != nil {
		emit(ErrorMsg{Type: MsgError, Message: err.Error()})
		return 1
	}
	emit(ResultMsg{Type: MsgResult, Summary: res.Summary, Artifacts: res.Artifacts, Diff: res.Diff})
	return 0
}

// --- Shared agent-CLI helpers ---

// parseAgentLine best-effort parses one JSON line from an agent CLI's
// streaming output. It returns progress text, and (isResult, resultText) when
// the line is a terminal result. Lines that are not JSON or carry no usable
// text return empty strings; the caller accumulates raw output for fallback.
func parseAgentLine(line []byte) (progress string, isResult bool, resultText string) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return "", false, ""
	}
	typ, _ := m["type"].(string)
	if typ == "result" {
		for _, k := range []string{"result", "text", "output"} {
			if s, ok := m[k].(string); ok && s != "" {
				return "", true, s
			}
		}
		return "", true, ""
	}
	// Claude-style: {"message": {"content": [{"type": "text", "text": "..."}]}}
	if msg, ok := m["message"].(map[string]any); ok {
		if content, ok := msg["content"].([]any); ok {
			var parts []string
			for _, c := range content {
				if cm, ok := c.(map[string]any); ok {
					if cm["type"] == "text" {
						if s, ok := cm["text"].(string); ok {
							parts = append(parts, s)
						}
					}
				}
			}
			if len(parts) > 0 {
				return joinStrings(parts, ""), false, ""
			}
		}
	}
	if s, ok := m["text"].(string); ok && s != "" {
		return s, false, ""
	}
	return "", false, ""
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func agentBin(envName, def string) string {
	if v := os.Getenv(envName); v != "" {
		return v
	}
	return def
}

func debugf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sb-adapter: "+format+"\n", args...)
}
