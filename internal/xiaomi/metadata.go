package xiaomi

import "strings"

func normalizeSpace(value string) string { return strings.Join(strings.Fields(value), " ") }

func normalizeMetadata(title, artist string) (string, string) {
	title, artist = normalizeSpace(title), normalizeSpace(artist)
	// Strip only duplicated title fields, never guess a new song from lyrics.
	for _, sep := range []string{"--", " - ", "—", " – "} {
		parts := strings.Split(artist, sep)
		if len(parts) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(parts[0]), title) {
			artist = strings.TrimSpace(parts[1])
			break
		}
		if strings.EqualFold(strings.TrimSpace(parts[1]), title) {
			artist = strings.TrimSpace(parts[0])
			break
		}
	}
	lower := strings.ToLower(title)
	for _, prefix := range []string{"作词", "作曲", "编曲", "演唱", "词：", "曲：", "制作人", "混音", "录音", "监制", "lyrics:", "composer:"} {
		if strings.HasPrefix(lower, prefix) {
			return "", artist
		}
	}
	return title, artist
}
