//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/sys/windows/svc/eventlog"
)

// eventLogHandler is a minimal slog.Handler that writes records to the Windows
// Event Log. Levels map to the three Event Log severities: Debug/Info -> Info,
// Warn -> Warning, Error -> Error. Attributes are appended as key=value pairs.
type eventLogHandler struct {
	log   *eventlog.Log
	level slog.Level
	attrs []slog.Attr
	group string
}

// newEventLogHandler opens the Event Log source registered by
// `tncd service install` (eventlog.InstallAsEventCreate). If opening fails, it
// returns an error so the caller can fall back to stderr.
func newEventLogHandler(source string, level slog.Level) (*eventLogHandler, error) {
	l, err := eventlog.Open(source)
	if err != nil {
		return nil, err
	}
	return &eventLogHandler{log: l, level: level}, nil
}

func (h *eventLogHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.level
}

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	writeAttr := func(a slog.Attr) {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		fmt.Fprintf(&b, " %s=%v", key, a.Value.Any())
	}
	for _, a := range h.attrs {
		writeAttr(a)
	}
	r.Attrs(func(a slog.Attr) bool { writeAttr(a); return true })
	msg := b.String()

	const eid = 1
	switch {
	case r.Level >= slog.LevelError:
		return h.log.Error(eid, msg)
	case r.Level >= slog.LevelWarn:
		return h.log.Warning(eid, msg)
	default:
		return h.log.Info(eid, msg)
	}
}

func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *eventLogHandler) WithGroup(name string) slog.Handler {
	nh := *h
	if h.group == "" {
		nh.group = name
	} else {
		nh.group = h.group + "." + name
	}
	return &nh
}
