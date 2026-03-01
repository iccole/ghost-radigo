// Package logger wraps the standard log package with level filtering.
// Set the LOG_LEVEL environment variable to control verbosity:
//
//	LOG_LEVEL=debug   — all messages
//	LOG_LEVEL=info    — info, warn, error  (default)
//	LOG_LEVEL=warn    — warn, error only
//	LOG_LEVEL=error   — errors only
//	LOG_LEVEL=off     — silence all output
package logger

import (
	"log"
	"os"
	"strings"
)

type level int

const (
	levelDebug level = iota
	levelInfo
	levelWarn
	levelError
	levelOff
)

var current = levelInfo

func init() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		current = levelDebug
	case "warn", "warning":
		current = levelWarn
	case "error":
		current = levelError
	case "off", "none", "silent":
		current = levelOff
	default:
		current = levelInfo
	}
}

func Debug(format string, args ...any) {
	if current <= levelDebug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func Info(format string, args ...any) {
	if current <= levelInfo {
		log.Printf("[INFO]  "+format, args...)
	}
}

func Warn(format string, args ...any) {
	if current <= levelWarn {
		log.Printf("[WARN]  "+format, args...)
	}
}

func Error(format string, args ...any) {
	if current <= levelError {
		log.Printf("[ERROR] "+format, args...)
	}
}
