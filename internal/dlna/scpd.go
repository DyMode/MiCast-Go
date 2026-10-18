package dlna

import (
	"fmt"
	"sort"
	"strings"
)

// Arguments are encoded as name:direction:related-state-variable.
var actions = map[string]map[string][]string{
	"AVTransport": {
		"SetNextAVTransportURI": {"InstanceID:in:A_ARG_TYPE_InstanceID", "NextURI:in:NextAVTransportURI", "NextURIMetaData:in:NextAVTransportURIMetaData"},
		"Next":                  {"InstanceID:in:A_ARG_TYPE_InstanceID"},
		"SetAVTransportURI":     {"InstanceID:in:A_ARG_TYPE_InstanceID", "CurrentURI:in:AVTransportURI", "CurrentURIMetaData:in:AVTransportURIMetaData"},
		"Play":                  {"InstanceID:in:A_ARG_TYPE_InstanceID", "Speed:in:TransportPlaySpeed"}, "Pause": {"InstanceID:in:A_ARG_TYPE_InstanceID"}, "Stop": {"InstanceID:in:A_ARG_TYPE_InstanceID"},
		"Seek":                       {"InstanceID:in:A_ARG_TYPE_InstanceID", "Unit:in:A_ARG_TYPE_SeekMode", "Target:in:A_ARG_TYPE_SeekTarget"},
		"GetTransportInfo":           {"InstanceID:in:A_ARG_TYPE_InstanceID", "CurrentTransportState:out:TransportState", "CurrentTransportStatus:out:TransportStatus", "CurrentSpeed:out:TransportPlaySpeed"},
		"GetPositionInfo":            {"InstanceID:in:A_ARG_TYPE_InstanceID", "Track:out:CurrentTrack", "TrackDuration:out:CurrentTrackDuration", "TrackMetaData:out:CurrentTrackMetaData", "TrackURI:out:CurrentTrackURI", "RelTime:out:RelativeTimePosition", "AbsTime:out:AbsoluteTimePosition", "RelCount:out:RelativeCounterPosition", "AbsCount:out:AbsoluteCounterPosition"},
		"GetMediaInfo":               {"InstanceID:in:A_ARG_TYPE_InstanceID", "NrTracks:out:NumberOfTracks", "MediaDuration:out:CurrentMediaDuration", "CurrentURI:out:AVTransportURI", "CurrentURIMetaData:out:AVTransportURIMetaData", "NextURI:out:NextAVTransportURI", "NextURIMetaData:out:NextAVTransportURIMetaData", "PlayMedium:out:PlaybackStorageMedium", "RecordMedium:out:RecordStorageMedium", "WriteStatus:out:RecordMediumWriteStatus"},
		"GetDeviceCapabilities":      {"InstanceID:in:A_ARG_TYPE_InstanceID", "PlayMedia:out:PossiblePlaybackStorageMedia", "RecMedia:out:PossibleRecordStorageMedia", "RecQualityModes:out:PossibleRecordQualityModes"},
		"GetTransportSettings":       {"InstanceID:in:A_ARG_TYPE_InstanceID", "PlayMode:out:CurrentPlayMode", "RecQualityMode:out:CurrentRecordQualityMode"},
		"GetCurrentTransportActions": {"InstanceID:in:A_ARG_TYPE_InstanceID", "Actions:out:CurrentTransportActions"},
	},
	"RenderingControl": {
		"GetVolume": {"InstanceID:in:A_ARG_TYPE_InstanceID", "Channel:in:A_ARG_TYPE_Channel", "CurrentVolume:out:Volume"}, "SetVolume": {"InstanceID:in:A_ARG_TYPE_InstanceID", "Channel:in:A_ARG_TYPE_Channel", "DesiredVolume:in:Volume"},
		"GetMute": {"InstanceID:in:A_ARG_TYPE_InstanceID", "Channel:in:A_ARG_TYPE_Channel", "CurrentMute:out:Mute"}, "SetMute": {"InstanceID:in:A_ARG_TYPE_InstanceID", "Channel:in:A_ARG_TYPE_Channel", "DesiredMute:in:Mute"},
	},
	"ConnectionManager": {
		"GetProtocolInfo": {"Source:out:SourceProtocolInfo", "Sink:out:SinkProtocolInfo"}, "GetCurrentConnectionIDs": {"ConnectionIDs:out:CurrentConnectionIDs"},
		"GetCurrentConnectionInfo": {"ConnectionID:in:A_ARG_TYPE_ConnectionID", "RcsID:out:A_ARG_TYPE_RcsID", "AVTransportID:out:A_ARG_TYPE_AVTransportID", "ProtocolInfo:out:A_ARG_TYPE_ProtocolInfo", "PeerConnectionManager:out:A_ARG_TYPE_ConnectionManager", "PeerConnectionID:out:A_ARG_TYPE_ConnectionID", "Direction:out:A_ARG_TYPE_Direction", "Status:out:A_ARG_TYPE_ConnectionStatus"},
	},
}

func hasAction(service, action string) bool { _, ok := actions[service][action]; return ok }
func scpd(service string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><scpd xmlns="urn:schemas-upnp-org:service-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><actionList>`)
	vars := map[string]bool{}
	names := make([]string, 0, len(actions[service]))
	for name := range actions[service] {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, action := range names {
		args := actions[service][action]
		fmt.Fprintf(&b, "<action><name>%s</name><argumentList>", action)
		for _, arg := range args {
			p := strings.Split(arg, ":")
			vars[p[2]] = true
			fmt.Fprintf(&b, "<argument><name>%s</name><direction>%s</direction><relatedStateVariable>%s</relatedStateVariable></argument>", p[0], p[1], p[2])
		}
		b.WriteString("</argumentList></action>")
	}
	b.WriteString("</actionList><serviceStateTable>")
	variables := make([]string, 0, len(vars))
	for name := range vars {
		variables = append(variables, name)
	}
	sort.Strings(variables)
	for _, name := range variables {
		kind := "string"
		if name == "AVTransportURI" || name == "NextAVTransportURI" || name == "CurrentTrackURI" {
			kind = "uri"
		}
		if name == "A_ARG_TYPE_InstanceID" || name == "CurrentTrack" || name == "NumberOfTracks" {
			kind = "ui4"
		}
		if strings.Contains(name, "CounterPosition") || name == "A_ARG_TYPE_ConnectionID" || name == "A_ARG_TYPE_RcsID" || name == "A_ARG_TYPE_AVTransportID" {
			kind = "i4"
		}
		if name == "Volume" {
			kind = "ui2"
		}
		if name == "Mute" {
			kind = "boolean"
		}
		events := "no"
		if service == "ConnectionManager" && (name == "CurrentConnectionIDs" || name == "SourceProtocolInfo" || name == "SinkProtocolInfo") {
			events = "yes"
		}
		fmt.Fprintf(&b, `<stateVariable sendEvents="%s"><name>%s</name><dataType>%s</dataType>`, events, name, kind)
		allowed := map[string][]string{
			"TransportState":  {"STOPPED", "PLAYING", "PAUSED_PLAYBACK", "TRANSITIONING", "NO_MEDIA_PRESENT"},
			"TransportStatus": {"OK", "ERROR_OCCURRED"}, "TransportPlaySpeed": {"1"},
			"A_ARG_TYPE_SeekMode": {"REL_TIME"}, "A_ARG_TYPE_Channel": {"Master"},
			"PlaybackStorageMedium": {"NETWORK", "NOT_IMPLEMENTED"}, "RecordStorageMedium": {"NOT_IMPLEMENTED"}, "RecordMediumWriteStatus": {"NOT_IMPLEMENTED"},
			"A_ARG_TYPE_Direction": {"Input", "Output"}, "A_ARG_TYPE_ConnectionStatus": {"OK", "ContentFormatMismatch", "InsufficientBandwidth", "UnreliableChannel", "Unknown"},
		}
		if values := allowed[name]; len(values) > 0 {
			b.WriteString("<allowedValueList>")
			for _, value := range values {
				fmt.Fprintf(&b, "<allowedValue>%s</allowedValue>", value)
			}
			b.WriteString("</allowedValueList>")
		}
		if name == "Volume" {
			b.WriteString("<allowedValueRange><minimum>0</minimum><maximum>100</maximum><step>1</step></allowedValueRange>")
		}
		b.WriteString("</stateVariable>")
	}
	if service != "ConnectionManager" {
		b.WriteString(`<stateVariable sendEvents="yes"><name>LastChange</name><dataType>string</dataType></stateVariable>`)
	}
	b.WriteString("</serviceStateTable></scpd>")
	return b.String()
}
