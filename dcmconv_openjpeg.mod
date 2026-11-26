module github.com/ThalesMMS/dicom-go

go 1.22

require (
	golang.org/x/sys v0.30.0
	golang.org/x/text v0.3.8
)

require github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000 v0.0.0

require github.com/mrjoshuak/go-jpeg2000 v1.3.0 // indirect

replace github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000 => ./examples/codec-adapters/jpeg2000
