package deej

import "go.uber.org/zap"

// volumeOSD is only implemented on Windows
type volumeOSD struct{}

func newVolumeOSD(logger *zap.SugaredLogger) *volumeOSD { return &volumeOSD{} }

func (o *volumeOSD) start() {}

// Show is a no-op on Linux
func (o *volumeOSD) Show(label string, value float32, durationMs int) {}
