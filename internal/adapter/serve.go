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
		if err := sc.Err(); err != nil {
			// A scanner error (e.g. a run request line over the 1MB token
			// limit) is a loud failure, not a silent empty run.
			emit(ErrorMsg{Type: MsgError, Message: "run request unreadable: " + err.Error()})
			return 1
		}
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

// agentLine is one parsed line of agent-CLI output. At most one of the
// result/error signals is set; progress text is advisory.
type agentLine struct {
	progress   string
	isResult   bool
	resultText string
	isError    bool
	errorText  string
}

// parseAgentLine best-effort parses one JSON line from an agent CLI's
// streaming output. Lines that are not JSON or carry no usable text return
// the zero value; the caller accumulates raw output for fallback.
//
// Supported shapes:
//   - Generic: {"type":"result",...} is a terminal result.
//   - Claude-style: {"message": {"content": [{"type": "text", ...}]}}.
//   - Codex exec --json: {"type":"item.completed","item":{"type":
//     "agent_message","text":...}} is the terminal result (last one wins),
//     {"type":"item.completed","item":{"type":"reasoning",...}} is progress,
//     and {"type":"turn.failed",...} is a terminal ERROR — never a degraded
//     success, even when the CLI exits 0.
func parseAgentLine(line []byte) agentLine {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return agentLine{}
	}
	typ, _ := m["type"].(string)
	if typ == "result" {
		for _, k := range []string{"result", "text", "output"} {
			if s, ok := m[k].(string); ok && s != "" {
				return agentLine{isResult: true, resultText: s}
			}
		}
		return agentLine{isResult: true}
	}
	// Codex exec --json events.
	if typ == "item.completed" {
		if item, ok := m["item"].(map[string]any); ok {
			switch item["type"] {
			case "agent_message":
				if s, ok := item["text"].(string); ok && s != "" {
					return agentLine{isResult: true, resultText: s}
				}
				return agentLine{isResult: true}
			case "reasoning":
				if s, ok := item["text"].(string); ok && s != "" {
					return agentLine{progress: s}
				}
			}
		}
		return agentLine{}
	}
	if typ == "turn.failed" {
		msg := "turn failed"
		switch e := m["error"].(type) {
		case map[string]any:
			if s, ok := e["message"].(string); ok && s != "" {
				msg = s
			}
		case string:
			if e != "" {
				msg = e
			}
		}
		return agentLine{isError: true, errorText: msg}
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
				return agentLine{progress: joinStrings(parts, "")}
			}
		}
	}
	if s, ok := m["text"].(string); ok && s != "" {
		return agentLine{progress: s}
	}
	return agentLine{}
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
