// Command sb is the Switchboard CLI: send, inbox, status, logs.
// Any agent with shell access hands off by running `sb send` — no plugin or
// MCP integration is needed to start.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/TamTunnel/switchboard/internal/client"
	"github.com/TamTunnel/switchboard/internal/daemon"
	"github.com/TamTunnel/switchboard/internal/envelope"
)

func homeDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("SWITCHBOARD_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sb: cannot find home directory:", err)
		os.Exit(1)
	}
	return filepath.Join(home, ".switchboard")
}

func defaultAgent() string {
	if v := os.Getenv("SWITCHBOARD_AGENT"); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "cli"
}

func newClient(home string) *client.Client {
	return client.New(daemon.SocketPath(home))
}

func main() {
	var homeFlag string

	sendCmd := &cobra.Command{
		Use:   "send",
		Short: "Send a task to another agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			to, _ := cmd.Flags().GetString("to")
			title, _ := cmd.Flags().GetString("title")
			instr, _ := cmd.Flags().GetString("instructions")
			instrFile, _ := cmd.Flags().GetString("instructions-file")
			from, _ := cmd.Flags().GetString("from")
			shape, _ := cmd.Flags().GetString("shape")
			timeout, _ := cmd.Flags().GetInt("timeout")
			workspace, _ := cmd.Flags().GetString("workspace")
			gitRef, _ := cmd.Flags().GetString("git-ref")
			files, _ := cmd.Flags().GetStringSlice("file")
			next, _ := cmd.Flags().GetStringSlice("next")
			diffFile, _ := cmd.Flags().GetString("diff-file")
			if to == "" {
				return fmt.Errorf("--to is required (agent name or cap:<tag>)")
			}
			if title == "" {
				return fmt.Errorf("--title is required")
			}
			switch shape {
			case envelope.ShapeDelegate, envelope.ShapePipeline, envelope.ShapeReview:
			default:
				return fmt.Errorf("unknown --shape %q (delegate|pipeline|review)", shape)
			}
			if instrFile != "" {
				data, err := os.ReadFile(instrFile)
				if err != nil {
					return err
				}
				instr = string(data)
			} else if instr == "" {
				// No --instructions: read stdin (lets agents pipe task text).
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				instr = strings.TrimSpace(string(data))
			}
			if instr == "" {
				return fmt.Errorf("instructions required: --instructions, --instructions-file, or stdin")
			}
			if workspace == "" {
				wd, err := os.Getwd()
				if err == nil {
					workspace = wd
				}
			}
			env := envelope.New(from, to, shape)
			env.Task = envelope.Task{Title: title, Instructions: instr}
			env.Task.Constraints.TimeoutSeconds = timeout
			env.Context = envelope.Context{Workspace: workspace, GitRef: gitRef, Files: files}
			env.Next = next
			if diffFile != "" {
				data, err := os.ReadFile(diffFile)
				if err != nil {
					return err
				}
				env.Context.Diff = string(data)
			}
			if err := env.Validate(); err != nil {
				return fmt.Errorf("envelope invalid: %w", err)
			}
			c := newClient(homeDir(homeFlag))
			id, err := c.Send(env)
			if err != nil {
				return err
			}
			fmt.Println(id)
			return nil
		},
	}
	sendCmd.Flags().String("to", "", "recipient agent name or cap:<tag> (required)")
	sendCmd.Flags().String("title", "", "task title (required)")
	sendCmd.Flags().String("instructions", "", "task instructions in markdown")
	sendCmd.Flags().String("instructions-file", "", "read instructions from a file")
	sendCmd.Flags().String("from", defaultAgent(), "sender agent name")
	sendCmd.Flags().String("shape", envelope.ShapeDelegate, "handoff shape: delegate|pipeline|review")
	sendCmd.Flags().Int("timeout", 0, "adapter timeout in seconds (0 = daemon default)")
	sendCmd.Flags().String("workspace", "", "workspace path for file refs (default: cwd)")
	sendCmd.Flags().String("git-ref", "", "git ref for context")
	sendCmd.Flags().StringSlice("file", nil, "file ref (repeatable)")
	sendCmd.Flags().StringSlice("next", nil, "pipeline next hop (repeatable, pipeline shape)")
	sendCmd.Flags().String("diff-file", "", "diff file for review shape")

	inboxCmd := &cobra.Command{
		Use:   "inbox",
		Short: "List incoming work and result notifications for an agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			agent, _ := cmd.Flags().GetString("agent")
			c := newClient(homeDir(homeFlag))
			envs, err := c.Inbox(agent)
			if err != nil {
				return err
			}
			if len(envs) == 0 {
				fmt.Println("(inbox empty)")
				return nil
			}
			for _, e := range envs {
				kind := "task"
				if e.Kind == envelope.KindResult {
					kind = "result"
				}
				line := fmt.Sprintf("%s  %-6s  %-8s  %-12s  %s", e.ID, kind, e.Status, e.From, e.Task.Title)
				if e.Kind == envelope.KindResult && e.Result != nil {
					line += "  —  " + firstLine(e.Result.Summary)
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	inboxCmd.Flags().String("agent", defaultAgent(), "agent whose inbox to list")

	statusCmd := &cobra.Command{
		Use:   "status <id>",
		Short: "Show an envelope as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient(homeDir(homeFlag))
			env, err := c.Status(args[0])
			if err != nil {
				return err
			}
			data, _ := json.MarshalIndent(env, "", "  ")
			fmt.Println(string(data))
			return nil
		},
	}

	logsCmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show an envelope's adapter run log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient(homeDir(homeFlag))
			logs, err := c.Logs(args[0])
			if err != nil {
				return err
			}
			fmt.Print(logs)
			return nil
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("sb 0.1.0 (protocol sb/1)")
		},
	}

	root := &cobra.Command{Use: "sb"}
	root.PersistentFlags().StringVar(&homeFlag, "home", "", "switchboard home directory (default $SWITCHBOARD_HOME or ~/.switchboard)")
	root.AddCommand(sendCmd, inboxCmd, statusCmd, logsCmd, versionCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
