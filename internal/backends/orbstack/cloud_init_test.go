package orbstackbackend

import (
	"strings"
	"testing"
)

func TestRenderEnvdServiceEscapesQuotes(t *testing.T) {
	cfg := OrbstackRuntimeConfig{EnvdPort: 49983}

	service := renderEnvdService(cfg, `python3 -c "print('ok')"`, map[string]string{
		`JSON`: `{"name":"orb"}`,
	})

	assertContains(t, service, `ExecStart=/usr/local/bin/envd -isnotfc -port 49983 -cmd "python3 -c \"print('ok')\""`)
	assertContains(t, service, `Environment="JSON={\"name\":\"orb\"}"`)
}

func TestRenderVolumeMountCommandsUseSelectiveMountPath(t *testing.T) {
	cfg := OrbstackRuntimeConfig{
		VolumeHostPath: "/tmp/e2b-local/volumes",
	}

	commands := renderVolumeMountCommands(cfg, []VolumeMount{
		{VolumeID: "vol-1", Path: "/volumes/data"},
	}, map[string]string{
		"vol-1": "/tmp/e2b-local/volumes/data",
	})

	script := strings.Join(commands, "\n")
	assertContains(t, script, "mkdir -p '/volumes'")
	assertContains(t, script, "rm -rf '/volumes/data'")
	assertContains(t, script, "ln -sfn '/mnt/e2b-local/volumes/vol-1' '/volumes/data'")
}

func TestRenderVolumeMountCommandsUsesSelectiveIsolatedMountPath(t *testing.T) {
	cfg := OrbstackRuntimeConfig{
		Isolated:       true,
		VolumeHostPath: "/tmp/e2b-local/volumes",
	}

	commands := renderVolumeMountCommands(cfg, []VolumeMount{
		{VolumeID: "vol-1", Path: "/volumes/data"},
	}, map[string]string{
		"vol-1": "/tmp/e2b-local/volumes/data",
	})

	script := strings.Join(commands, "\n")
	assertContains(t, script, "ln -sfn '/mnt/e2b-local/volumes/vol-1' '/volumes/data'")
}

func assertContains(t *testing.T, value string, want string) {
	t.Helper()
	if !strings.Contains(value, want) {
		t.Fatalf("expected %q to contain %q", value, want)
	}
}
