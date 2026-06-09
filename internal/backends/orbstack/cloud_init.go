package orbstackbackend

import (
	"fmt"
	"sort"
	"strings"
)

type machineProvisionSpec struct {
	StartCmd       string
	EnvVars        map[string]string
	VolumeMounts   []VolumeMount
	VolumeHostDirs map[string]string
}

func renderEnvdService(cfg OrbstackRuntimeConfig, startCmd string, envVars map[string]string) string {
	startCmd = strings.TrimSpace(startCmd)
	var b strings.Builder

	b.WriteString("[Unit]\n")
	b.WriteString("Description=E2B Environment Daemon\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=1\n")

	keys := make([]string, 0, len(envVars))
	for key := range envVars {
		key = strings.TrimSpace(key)
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString("Environment=\"")
		b.WriteString(systemdQuote(key))
		b.WriteByte('=')
		b.WriteString(systemdQuote(envVars[key]))
		b.WriteString("\"\n")
	}

	b.WriteString("ExecStart=/usr/local/bin/envd -isnotfc -port ")
	b.WriteString(fmt.Sprintf("%d", cfg.EnvdPort))
	if startCmd != "" {
		b.WriteString(" -cmd \"")
		b.WriteString(systemdQuote(startCmd))
		b.WriteByte('"')
	}
	b.WriteByte('\n')
	b.WriteByte('\n')
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")

	return b.String()
}

func renderVolumeMountCommands(cfg OrbstackRuntimeConfig, volumeMounts []VolumeMount, volumeHostDirs map[string]string) []string {
	volumeMounts = normalizeVolumeMounts(volumeMounts)
	if len(volumeMounts) == 0 {
		return nil
	}

	result := make([]string, 0, len(volumeMounts))
	for _, mount := range volumeMounts {
		target := strings.TrimSpace(mount.Path)
		volumeID := strings.TrimSpace(mount.VolumeID)
		if target == "" || volumeID == "" {
			continue
		}

		source := sandboxVolumeSourcePath(cfg, volumeID, volumeHostDirs[volumeID])
		result = append(result,
			"mkdir -p "+shQuote(parentDir(target)),
			"rm -rf "+shQuote(target),
			"ln -sfn "+shQuote(source)+" "+shQuote(target),
		)
	}
	return result
}

func parentDir(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return "/"
	}
	index := strings.LastIndex(value, "/")
	if index <= 0 {
		return "/"
	}
	return value[:index]
}

func systemdQuote(value string) string {
	replacer := strings.NewReplacer(
		`\\`, `\\\\`,
		`"`, `\"`,
		"\n", `\n`,
	)
	return replacer.Replace(value)
}

func shQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func yamlSingleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
