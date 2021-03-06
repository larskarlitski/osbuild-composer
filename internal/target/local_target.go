package target

import "github.com/google/uuid"

type LocalTargetOptions struct {
	ComposeId       uuid.UUID `json:"compose_id"`
	ImageBuildId    int       `json:"image_build_id"`
	Filename        string    `json:"filename"`
	StreamOptimized bool      `json:"stream_optimized"` // return image as stream optimized
}

func (LocalTargetOptions) isTargetOptions() {}

func NewLocalTarget(options *LocalTargetOptions) *Target {
	return newTarget("org.osbuild.local", options)
}
