// Command switchboard is the daemon binary: `switchboard daemon`.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/TamTunnel/switchboard/internal/config"
	"github.com/TamTunnel/switchboard/internal/daemon"
)

// homeDir resolves the switchboard home: --home, $SWITCHBOARD_HOME, ~/.switchboard.
func homeDir(flag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("SWITCHBOARD_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "switchboard: cannot find home directory:", err)
		os.Exit(1)
	}
	return filepath.Join(home, ".switchboard")
}

func main() {
	var homeFlag string
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the switchboard daemon (queue, routing, adapter supervision)",
		RunE: func(cmd *cobra.Command, args []string) error {
			home := homeDir(homeFlag)
			cfg, err := config.Load(config.Path(home))
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			d, err := daemon.New(home, cfg, log)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return d.Run(ctx)
		},
	}
	daemonCmd.Flags().StringVar(&homeFlag, "home", "", "switchboard home directory (default $SWITCHBOARD_HOME or ~/.switchboard)")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("switchboard 0.1.0 (protocol sb/1)")
		},
	}

	root := &cobra.Command{Use: "switchboard"}
	root.AddCommand(daemonCmd, versionCmd)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
