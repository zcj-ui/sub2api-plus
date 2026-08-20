// Package sysutil provides system-level utilities for process management.
package sysutil

import (
	"log"
	"os"
	"runtime"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrRestartUnsupportedPlatform = infraerrors.Conflict(
	"SYSTEM_RESTART_UNSUPPORTED_PLATFORM",
	"restart through the API is supported only on Linux; restart this deployment using its normal procedure",
)

// RestartServiceSupported reports whether this process can use the API restart
// mechanism. The mechanism exits the process and relies on a Linux service
// manager to start it again, so non-Linux platforms must not report success.
func RestartServiceSupported() error {
	return restartServiceSupportError(runtime.GOOS)
}

func restartServiceSupportError(goos string) error {
	if goos != "linux" {
		return infraerrors.Clone(ErrRestartUnsupportedPlatform)
	}
	return nil
}

// RestartService triggers a service restart by gracefully exiting.
//
// This relies on systemd's Restart=always configuration to automatically
// restart the service after it exits. This is the industry-standard approach:
//   - Simple and reliable
//   - No sudo permissions needed
//   - No complex process management
//   - Leverages systemd's native restart capability
//
// Prerequisites:
//   - Linux OS with systemd
//   - Service configured with Restart=always in systemd unit file
func RestartService() error {
	if err := RestartServiceSupported(); err != nil {
		return err
	}

	log.Println("Initiating service restart by graceful exit...")
	log.Println("systemd will automatically restart the service (Restart=always)")

	// Give a moment for logs to flush and response to be sent
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()

	return nil
}

// RestartServiceAsync is a fire-and-forget version of RestartService.
// It logs errors instead of returning them, suitable for goroutine usage.
func RestartServiceAsync() {
	if err := RestartService(); err != nil {
		log.Printf("Service restart failed: %v", err)
		log.Println("Please restart the service manually: sudo systemctl restart sub2api")
	}
}
