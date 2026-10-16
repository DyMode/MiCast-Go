package version

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseMetadataMatchesService(t *testing.T) {
	for path, expected := range map[string]string{
		"../../packaging/fnos/manifest": "version=" + Version + "\n",
		"../server/web/index.html":      "class=\"version\">" + Version + "</span>",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.ReplaceAll(string(data), "\r\n", "\n"), expected) {
			t.Errorf("%s does not match service version %s", path, Version)
		}
	}
}
