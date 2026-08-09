package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
)

// TestDefaultSettings executes the fresh-install configuration literal - which no
// other test does - and pins two things a silent edit would otherwise ship:
//
//  1. that each config duration/int maps to the RIGHT settings field. Every input
//     is distinct, so swapping any two same-typed fields (three retention
//     durations and two intervals sit adjacent in the literal) changes an asserted
//     value. The retention swap is the real hazard: it would keep latency data for
//     a year and delete speed history after a day, with the symptom weeks away.
//  2. the hardcoded shipped defaults, so a change to any of them is deliberate.
func TestDefaultSettings(t *testing.T) {
	cfg := config.Config{
		Interval:             7 * time.Second,
		SpeedtestInterval:    42 * time.Minute,
		Timeout:              9 * time.Second,
		Retention:            11 * time.Hour,
		SpeedRetention:       22 * time.Hour,
		DowntimeRetention:    33 * time.Hour,
		DownAfter:            3,
		UpAfter:              4,
		LatencyEnabled:       true,
		SpeedtestEnabled:     true,
		SpeedtestOnReconnect: true,
		IPv6Mode:             "on",
	}
	v := defaultSettings(cfg)

	// Mapping: each distinct input lands on its own field. A swap flips one of these.
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"Latency<-Interval", v.Latency, cfg.Interval},
		{"Speed<-SpeedtestInterval", v.Speed, cfg.SpeedtestInterval},
		{"Timeout<-Timeout", v.Timeout, cfg.Timeout},
		{"Retention<-Retention", v.Retention, cfg.Retention},
		{"SpeedRetention<-SpeedRetention", v.SpeedRetention, cfg.SpeedRetention},
		{"DowntimeRetention<-DowntimeRetention", v.DowntimeRetention, cfg.DowntimeRetention},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v (field mapping swapped?)", c.name, c.got, c.want)
		}
	}
	if v.DownAfter != cfg.DownAfter || v.UpAfter != cfg.UpAfter {
		t.Errorf("DownAfter/UpAfter = %d/%d, want %d/%d", v.DownAfter, v.UpAfter, cfg.DownAfter, cfg.UpAfter)
	}
	if v.IPv6Mode != cfg.IPv6Mode {
		t.Errorf("IPv6Mode = %q, want %q", v.IPv6Mode, cfg.IPv6Mode)
	}

	// Shipped defaults - each is a product decision, not incidental.
	wantBool := map[string]bool{
		"Monitoring": v.Monitoring, "DNSProbe": v.DNSProbe, "NetinfoEnabled": v.NetinfoEnabled,
		"UpdateCheckEnabled": v.UpdateCheckEnabled, "LogRedactPII": v.LogRedactPII,
		"IperfUDP": v.IperfUDP, "OoklaLoss": v.OoklaLoss,
	}
	for name, got := range wantBool {
		if !got {
			t.Errorf("default %s = false, want true", name)
		}
	}
	if v.SpeedBestOf {
		t.Error("SpeedBestOf defaults true; best-of-3 costs 3x data and must be opt-in")
	}
	if v.LogLevel != "off" {
		t.Errorf("LogLevel default = %q, want \"off\"", v.LogLevel)
	}
	if v.SpeedEngine != "ookla" {
		t.Errorf("SpeedEngine default = %q, want \"ookla\"", v.SpeedEngine)
	}
	if v.SpeedDirection != "both" {
		t.Errorf("SpeedDirection default = %q, want \"both\"", v.SpeedDirection)
	}
	if v.ExitTarget != "1.1.1.1" {
		t.Errorf("ExitTarget default = %q, want \"1.1.1.1\"", v.ExitTarget)
	}
	if v.DegradedPingMS != 150 || v.SpeedBusyMbps != 5 {
		t.Errorf("DegradedPingMS/SpeedBusyMbps = %v/%v, want 150/5", v.DegradedPingMS, v.SpeedBusyMbps)
	}

	// Access is EXPLICIT, not guessed: loopback-only by default everywhere, and
	// off ONLY when the operator set -access network. No container heuristic.
	if !defaultSettings(cfg).AccessLocalOnly {
		t.Error("default (no -access) must be loopback-only everywhere, containers included")
	}
	net := cfg
	net.Access = "network"
	if defaultSettings(net).AccessLocalOnly {
		t.Error("-access network must default AccessLocalOnly=false")
	}
	loc := cfg
	loc.Access = "local"
	if !defaultSettings(loc).AccessLocalOnly {
		t.Error("-access local must default AccessLocalOnly=true")
	}
}

// TestLogLevelOffKeepsErrors pins B2: the default log level "off" must still
// surface WARN and ERROR. It silences routine INFO/DEBUG, but a failed sample
// write or a wedged webhook must not vanish just because logging is at its
// default - those Errors are the only evidence a later support request has.
// Regression proof: map "off" back to logLevelOff and both sub-tests fail.
func TestLogLevelOffKeepsErrors(t *testing.T) {
	lvl := new(slog.LevelVar)

	applyLogLevel(lvl, "off")
	if lvl.Level() > slog.LevelError {
		t.Errorf("\"off\" set level %v, above LevelError - Errors would be dropped", lvl.Level())
	}
	if lvl.Level() > slog.LevelWarn {
		t.Errorf("\"off\" set level %v, above LevelWarn - Warnings would be dropped", lvl.Level())
	}
	// Still quiet for the routine stuff: INFO and DEBUG must not pass at "off".
	if lvl.Level() <= slog.LevelInfo {
		t.Errorf("\"off\" set level %v, at or below LevelInfo - routine chatter would print", lvl.Level())
	}

	applyLogLevel(lvl, "on")
	if lvl.Level() != slog.LevelDebug {
		t.Errorf("\"on\" set level %v, want LevelDebug", lvl.Level())
	}
}
