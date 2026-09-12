package main

import (
	htmlstd "html"
	"runtime/debug"
	"strings"
)

var (
	appVersion   = "dev"
	appCommit    string
	appBuildTime string
)

type BuildMetadata struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	ShortCommit string `json:"-"`
	BuildTime   string `json:"build_time"`
}

type HealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

func currentBuildMetadata() BuildMetadata {
	version := cleanBuildMetadataValue(appVersion)
	commit := cleanBuildMetadataValue(appCommit)
	buildTime := cleanBuildMetadataValue(appBuildTime)
	commitInjected := commit != ""
	modified := false

	if info, ok := debug.ReadBuildInfo(); ok {
		if (version == "" || version == "dev") && info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = cleanBuildMetadataValue(info.Main.Version)
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" {
					commit = cleanBuildMetadataValue(setting.Value)
				}
			case "vcs.modified":
				modified = !commitInjected && setting.Value == "true"
			}
		}
	}

	if version == "" {
		version = "dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	if buildTime == "" {
		buildTime = "unknown"
	}
	shortCommit := commit
	if len(shortCommit) > 12 {
		shortCommit = shortCommit[:12]
	}
	if modified && shortCommit != "unknown" {
		shortCommit += "-dirty"
		commit += "-dirty"
	}

	return BuildMetadata{
		Version:     version,
		Commit:      commit,
		ShortCommit: shortCommit,
		BuildTime:   buildTime,
	}
}

func cleanBuildMetadataValue(value string) string {
	value = strings.TrimSpace(value)
	return strings.Map(func(character rune) rune {
		if character == '\r' || character == '\n' || character == 0 {
			return -1
		}
		return character
	}, value)
}

func renderHTMLBuildMetadata(page []byte, metadata BuildMetadata) []byte {
	replacer := strings.NewReplacer(
		"{{CODEX_METER_VERSION}}", htmlstd.EscapeString(metadata.Version),
		"{{CODEX_METER_COMMIT}}", htmlstd.EscapeString(metadata.Commit),
		"{{CODEX_METER_COMMIT_SHORT}}", htmlstd.EscapeString(metadata.ShortCommit),
		"{{CODEX_METER_BUILD_TIME}}", htmlstd.EscapeString(metadata.BuildTime),
	)
	return []byte(replacer.Replace(string(page)))
}
