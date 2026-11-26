// Package datasettime converts a DICOM dataset's civil date/time values to a
// different fixed UTC offset without modifying the source. It builds on dcmtime
// and core, and can be used with object.Object.ToDataSet without an import cycle.
//
// Conversion preserves encoded precision. Explicit DT offsets already identify
// their own timezone and remain unchanged. DA/TM pairs convert together; standalone
// TM values convert as clocks modulo one day, without supplying an unknown date.
// Values that cannot be represented at their original precision cause an atomic
// error instead of being silently reinterpreted under a new dataset offset.
// Unpaired DA defaults to rejection; PreserveCalendarDate explicitly retains
// its civil date, including when the paired TM is empty. ConvertWithReport
// records those choices without claiming equivalence of an unknown instant.
package datasettime
