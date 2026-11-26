package object

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
)

func projectionReadOptions(opts ReadFileOptions) error {
	if opts.DataSetTags == nil {
		if opts.SeekUnselectedSequences {
			return parser.ErrDataSetProjectionOptions
		}
		return nil
	}
	if opts.AllowIncompletePixelData {
		return parser.ErrIncompletePixelDataOptions
	}
	if !opts.SkipPixelData || opts.AllowPartialRead || opts.StopBeforeTag != nil || opts.PrefixMaxTokens != 0 ||
		opts.DeferPixelData || opts.DeferWaveformData || opts.FrameSink != nil || opts.EncapsulatedSink != nil ||
		opts.InlineValueBytesThreshold != 0 || opts.BulkDataDescriptor != nil {
		return parser.ErrDataSetProjectionOptions
	}
	return nil
}

func readDataSetProjection(reader *parser.Reader, opts ReadFileOptions) (core.DataSet, error) {
	if err := projectionReadOptions(opts); err != nil {
		return core.DataSet{}, err
	}
	if opts.DataSetTags == nil {
		return reader.ReadDataSet()
	}
	return reader.ReadDataSetProjected(opts.DataSetTags)
}
