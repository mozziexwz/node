package disaster

import (
	"path/filepath"
	"testing"
	"time"
)

func TestScheduleRejectsStaleTimezoneFile(t *testing.T) {
	actual, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 13, 36, 0, 0, actual)
	zone, offset := scheduleTimeZone(now, []string{"Local", "Europe/Berlin"})
	if zone != "UTC+08:00" || offset != 8*3600 {
		t.Fatal(zone, offset)
	}
	zone, offset = scheduleTimeZone(now, []string{"Local", "Asia/Shanghai", "Europe/Berlin"})
	if zone != "Asia/Shanghai" || offset != 8*3600 {
		t.Fatal(zone, offset)
	}
	file := filepath.Join(testPrivateDirectory(t), "history.json")
	if err := saveHistory(file, BackupHistory{Version: 1, Runs: []BackupRun{}, Schedule: BackupSchedule{Enabled: true, Time: "12:01", Zone: zone, Offset: offset}}); err != nil {
		t.Fatal(err)
	}
	h, err := ReadHistory(file)
	if err != nil {
		t.Fatal(err)
	}
	next := time.UnixMilli(h.Schedule.NextRunAt).In(actual)
	if next.Hour() != 12 || next.Minute() != 1 {
		t.Fatal("schedule changed server-local hour", next)
	}
}

func TestScheduleNamedZoneAndOffsetFallback(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for _, month := range []time.Month{time.January, time.July} {
		now := time.Date(2026, month, 15, 12, 0, 0, 0, berlin)
		zone, offset := scheduleTimeZone(now, []string{"Europe/Berlin"})
		_, want := now.Zone()
		if zone != "Europe/Berlin" || offset != want {
			t.Fatal(zone, offset)
		}
	}
	for _, offset := range []int{-3*3600 - 1800, 5*3600 + 1800, 0} {
		now := time.Now().In(time.FixedZone("ambiguous", offset))
		zone, got := scheduleTimeZone(now, []string{"UTC", "invalid-zone"})
		if got != offset {
			t.Fatal(zone, got)
		}
		if _, err := time.LoadLocation(zone); err == nil {
			t.Fatal("fallback must use its explicit offset", zone)
		}
	}
}
