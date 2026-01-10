package logger

import (
	"fmt"
	"os"
	"strings"
)

type LoggerChannel struct {
	Channel chan string
}

func NewLoggerChannel() *LoggerChannel {
	return &LoggerChannel{
		Channel: make(chan string, 1000),
	}
}

func (w *LoggerChannel) Write(p []byte) (n int, err error) {
	// Copy slice to avoid race conditions if p is reused
	msg := string(p)
	// Strip trailing newline if present, as viewport adds one
	msg = strings.TrimRight(msg, "\n")

	select {
	case w.Channel <- msg:
	default:
		// Channel is full. Remove oldest message to prevent blocking and ensure new logs get through.
		fmt.Fprintf(os.Stderr, "log channel full, dropping oldest message\n")
		select {
		case <-w.Channel:
		default:
			// Channel was empty? (rare race condition, but safe to ignore)
		}

		// Try sending the new message again
		select {
		case w.Channel <- msg:
		default:
			// If still full, we drop the message to avoid blocking
			fmt.Fprintf(os.Stderr, "log channel full, dropping new message\n")
		}
	}
	return len(p), nil
}
