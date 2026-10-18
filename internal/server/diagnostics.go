package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"micastgo/internal/app"
	"micastgo/internal/dlna"
	"micastgo/internal/playback"
	"micastgo/internal/version"
)

type diagnosticRoute struct {
	ID            string            `json:"id"`
	Playback      playback.Status   `json:"playback"`
	TrackDuration float64           `json:"track_duration,omitempty"`
	Protocol      *dlna.Diagnostics `json:"dlna_protocol,omitempty"`
}

type diagnosticDevice struct {
	ID       string           `json:"id"`
	Hardware string           `json:"hardware"`
	Format   string           `json:"configured_format"`
	Observed *app.Observation `json:"observed,omitempty"`
}

type diagnosticReport struct {
	Version string             `json:"version"`
	At      time.Time          `json:"at"`
	Routes  []diagnosticRoute  `json:"routes"`
	Devices []diagnosticDevice `json:"devices"`
}

func (s *Server) collectDiagnostics() diagnosticReport {
	routes := []diagnosticRoute{}
	if s.routes != nil {
		for _, entry := range s.routes.Status() {
			status := entry.Playback
			trackDuration := status.Track.Duration
			status.Track = playback.Track{}
			routes = append(routes, diagnosticRoute{ID: entry.ID, Playback: status, TrackDuration: trackDuration, Protocol: entry.Protocol})
		}
	} else if s.engine != nil {
		status := s.engine.Status()
		trackDuration := status.Track.Duration
		status.Track = playback.Track{}
		routes = append(routes, diagnosticRoute{ID: "default", Playback: status, TrackDuration: trackDuration})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].ID < routes[j].ID })
	devices := []diagnosticDevice{}
	if s.app != nil {
		for _, entry := range s.app.Snapshot().Devices {
			devices = append(devices, diagnosticDevice{ID: entry.ID, Hardware: entry.Hardware, Format: entry.Format, Observed: entry.Observed})
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	return diagnosticReport{Version: version.Version, At: time.Now(), Routes: routes, Devices: devices}
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.collectDiagnostics())
}

// exportDiagnostics writes a text report the user can open from the app's
// data folder. It never contains media URLs, callbacks, credentials or names.
func (s *Server) exportDiagnostics(w http.ResponseWriter, r *http.Request) {
	if s.dataDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未配置应用数据目录"})
		return
	}
	name, err := s.writeDiagnosticsReport()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"name": name})
}

func (s *Server) writeDiagnosticsReport() (string, error) {
	dir := diagnosticsDir(s.dataDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	report := s.collectDiagnostics()
	body := formatDiagnosticReport(report, s.dataDir)
	name := "diagnostics-" + report.At.Format("20060102-150405.000") + ".txt"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		return "", err
	}
	// latest.txt is the file to open when the user just wants the newest report.
	_ = os.WriteFile(filepath.Join(dir, "latest.txt"), []byte(body), 0600)
	pruneDiagnostics(dir, 10)
	return name, nil
}

// diagnosticsDir keeps the folder name the user asked for. Reports live in a
// dedicated micast-go folder so uninstall can preserve them explicitly.
func diagnosticsDir(dataDir string) string {
	return filepath.Join(dataDir, "micast-go")
}

func pruneDiagnostics(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "diagnostics-") && strings.HasSuffix(name, ".txt") {
			names = append(names, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) <= keep {
		return
	}
	for _, name := range names[keep:] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

func formatDiagnosticReport(report diagnosticReport, dataDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MiCast Go 诊断报告\n")
	fmt.Fprintf(&b, "version=%s\n", report.Version)
	fmt.Fprintf(&b, "time=%s\n", report.At.Format(time.RFC3339))
	fmt.Fprintf(&b, "data_dir=%s\n", dataDir)
	fmt.Fprintf(&b, "note=不包含媒体地址、回调地址、账号凭据、歌曲名称或元数据内容。\n\n")
	for _, route := range report.Routes {
		status := route.Playback
		fmt.Fprintf(&b, "[route %s]\n", route.ID)
		fmt.Fprintf(&b, "protocol=%s state=%s activity=%s finished=%t error=%q end_reason=%q\n", status.Protocol, status.State, status.Activity, status.Finished, status.Error, status.EndReason)
		fmt.Fprintf(&b, "position=%.3f track_duration=%.3f format=%s volume_mode=%s\n", status.Position, route.TrackDuration, status.Format, status.VolumeMode)
		fmt.Fprintf(&b, "session=%s pipeline=%s track_id=%s\n", status.SessionID, status.PipelineID, status.TrackID)
		if len(status.Streams) > 0 {
			fmt.Fprintf(&b, "streams:\n")
			ids := make([]string, 0, len(status.Streams))
			for id := range status.Streams {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				pull := status.Streams[id]
				fmt.Fprintf(&b, "  %s bytes=%d health=%s connections=%d buffered_ms=%d retries=%d history_skips=%d bridges=%d completed=%t last_read=%s\n",
					id, pull.Bytes, pull.Health, pull.Connections, pull.BufferedMS, pull.Retries, pull.HistorySkips, pull.Bridges, pull.Completed, formatDiagnosticTime(pull.LastRead))
			}
		}
		if len(status.Events) > 0 {
			fmt.Fprintf(&b, "playback_events:\n")
			for _, event := range status.Events {
				fmt.Fprintf(&b, "  %s kind=%s speaker=%s attempt=%d\n", formatDiagnosticTime(event.At), event.Kind, event.Speaker, event.Attempt)
			}
		}
		if route.Protocol != nil {
			fmt.Fprintf(&b, "dlna: state=%s position=%.3f subscriptions=%d queued=%t\n", route.Protocol.State, route.Protocol.Position, route.Protocol.Subscriptions, route.Protocol.Queued)
			if len(route.Protocol.Events) > 0 {
				fmt.Fprintf(&b, "protocol_events:\n")
				for _, event := range route.Protocol.Events {
					sequence := ""
					if event.Sequence != nil {
						sequence = fmt.Sprintf(" seq=%d", *event.Sequence)
					}
					fmt.Fprintf(&b, "  %s service=%s action=%s code=%d state=%s position=%s duration=%s meta=%s%s\n",
						formatDiagnosticTime(event.At), event.Service, event.Action, event.Code, event.State, event.Position, event.Duration, event.Meta, sequence)
				}
			}
		}
		b.WriteString("\n")
	}
	if len(report.Devices) > 0 {
		fmt.Fprintf(&b, "[devices]\n")
		for _, device := range report.Devices {
			fmt.Fprintf(&b, "id=%s hardware=%s format=%s", device.ID, device.Hardware, device.Format)
			if device.Observed != nil {
				volume := "-"
				if device.Observed.Volume != nil {
					volume = fmt.Sprintf("%d", *device.Observed.Volume)
				}
				fmt.Fprintf(&b, " observed_state=%s observed_volume=%s observed_at=%s", device.Observed.State, volume, formatDiagnosticTime(device.Observed.At))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func formatDiagnosticTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format("2006-01-02 15:04:05.000")
}
