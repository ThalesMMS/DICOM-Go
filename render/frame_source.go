package render

import (
	"context"
	"fmt"
)

func loadFrameContext(ctx context.Context, frame *Frame) (*Frame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frame == nil {
		return nil, fmt.Errorf("render: nil frame")
	}
	if frame.LoadPixels == nil {
		return frame, frame.DecodeErr
	}
	loaded, err := frame.LoadPixels(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if loaded == nil || loaded.LoadPixels != nil {
		return nil, fmt.Errorf("render: invalid deferred pixel frame")
	}
	if loaded.Metadata != frame.Metadata || loaded.Rescale != frame.Rescale {
		return nil, fmt.Errorf("render: deferred pixels changed planned frame metadata")
	}
	return loaded, loaded.DecodeErr
}
