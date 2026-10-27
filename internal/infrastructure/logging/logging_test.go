package logging

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/FacePhys/TokenUseSys/internal/infrastructure/config"
)

func TestAtLeastFiltersLowerLevels(t *testing.T) {
	var buf bytes.Buffer
	log := AtLeast(New(config.LogConfig{Level: "debug", Format: "text"}, &buf), slog.LevelWarn).With("component", "river")
	log.Info("例行计数")
	log.Warn("任务卡住")
	require.NotContains(t, buf.String(), "例行计数")
	require.Contains(t, buf.String(), "任务卡住")
	require.Contains(t, buf.String(), "component=river")
}
