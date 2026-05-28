package internal

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func slackRetryAfterLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		// Slack documents Retry-After as integer seconds. Treat any other
		// shape as untrusted display text and fall back to generic retry copy.
		return ""
	}
	if time.Duration(seconds)*time.Second > slackRetryAfterDisplayCap {
		return "at least " + humanSlackRetryAfterDuration(slackRetryAfterDisplayCap)
	}
	if seconds >= 60 {
		minutes := seconds / 60
		remainingSeconds := seconds % 60
		minuteLabel := "minutes"
		if minutes == 1 {
			minuteLabel = "minute"
		}
		if remainingSeconds == 0 {
			return fmt.Sprintf("%d %s", minutes, minuteLabel)
		}
		secondLabel := "seconds"
		if remainingSeconds == 1 {
			secondLabel = "second"
		}
		return fmt.Sprintf("%d %s %d %s", minutes, minuteLabel, remainingSeconds, secondLabel)
	}
	if seconds == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", seconds)
}

func humanTunnelBootstrapTTL(ttl string) string {
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return "the requested " + ttl
	}
	return humanTunnelBootstrapDuration(d)
}

func humanTunnelBootstrapDuration(d time.Duration) string {
	return humanDurationCeilMinutes(d)
}

func humanSlackRetryAfterDuration(d time.Duration) string {
	return humanDurationCeilMinutes(d)
}

func humanDurationCeilMinutes(d time.Duration) string {
	if d < time.Minute {
		return "under 1 minute"
	}
	// Ceil to the next minute so near-boundary keys never display as
	// "0 minutes" or understate the operator's remaining setup window.
	minutesTotal := int((d + time.Minute - 1) / time.Minute)
	hours := minutesTotal / 60
	minutes := minutesTotal % 60
	hourUnit := "hours"
	if hours == 1 {
		hourUnit = "hour"
	}
	minuteUnit := "minutes"
	if minutes == 1 {
		minuteUnit = "minute"
	}
	switch {
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%d %s %d %s", hours, hourUnit, minutes, minuteUnit)
	case hours > 0:
		return fmt.Sprintf("%d %s", hours, hourUnit)
	default:
		return fmt.Sprintf("%d %s", minutes, minuteUnit)
	}
}
