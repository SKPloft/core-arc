package main

import (
	"testing"

	"github.com/datarhei/core/v16/log"
	"github.com/datarhei/core/v16/restream/app"
	"github.com/stretchr/testify/require"
)

func TestMigrateProcessConfigFFmpeg6To8(t *testing.T) {
	config := &app.Config{
		ID:        "test",
		FFVersion: "^6.1.1",
		Input: []app.ConfigIO{
			{
				ID:      "input",
				Address: "rtsp://127.0.0.1/stream",
				Options: []string{"-rtsp_transport", "tcp", "-timeout", "5000000"},
			},
		},
	}

	migrated, err := migrateProcessConfig(log.New("Migration"), config, "8.0.1")

	require.NoError(t, err)
	require.True(t, migrated)
	require.Equal(t, "^8.0.1", config.FFVersion)
	// 6 -> 8 doesn't change any process option.
	require.Equal(t, []string{"-rtsp_transport", "tcp", "-timeout", "5000000"}, config.Input[0].Options)
}

func TestMigrateProcessConfigFFmpeg7To8(t *testing.T) {
	config := &app.Config{
		ID:        "test",
		FFVersion: "^7.1.1",
		Options:   []string{"-loglevel", "level+info"},
	}

	migrated, err := migrateProcessConfig(log.New("Migration"), config, "8.0.1")

	require.NoError(t, err)
	require.True(t, migrated)
	require.Equal(t, "^8.0.1", config.FFVersion)
	// The log level option is a valid FFmpeg 8 option and must not be rewritten.
	require.Equal(t, []string{"-loglevel", "level+info"}, config.Options)
}

func TestMigrateProcessConfigFFmpeg4To8(t *testing.T) {
	config := &app.Config{
		ID:        "test",
		FFVersion: "^4.4.1",
		Input: []app.ConfigIO{
			{
				ID:      "input",
				Address: "rtsp://127.0.0.1/stream",
				Options: []string{"-stimeout", "5000000"},
			},
		},
	}

	migrated, err := migrateProcessConfig(log.New("Migration"), config, "8.0.1")

	require.NoError(t, err)
	require.True(t, migrated)
	require.Equal(t, "^8.0.1", config.FFVersion)
	// The version 4 -> 5 RTSP option rewrite still applies when the target is 8.
	require.Equal(t, []string{"-timeout", "5000000"}, config.Input[0].Options)
}

func TestMigrateProcessConfigSameVersion(t *testing.T) {
	config := &app.Config{
		ID:        "test",
		FFVersion: "^8.0.1",
	}

	migrated, err := migrateProcessConfig(log.New("Migration"), config, "8.0.1")

	require.NoError(t, err)
	require.False(t, migrated)
	require.Equal(t, "^8.0.1", config.FFVersion)
}

func TestMigrateProcessConfigUnknownTargetMajor(t *testing.T) {
	config := &app.Config{
		ID:        "test",
		FFVersion: "^8.0.1",
	}

	migrated, err := migrateProcessConfig(log.New("Migration"), config, "9.0.0")

	require.Error(t, err)
	require.False(t, migrated)
	require.Contains(t, err.Error(), "unknown FFmpeg version found: 9")
	require.Equal(t, "^8.0.1", config.FFVersion, "the stored version must not be modified")
}
