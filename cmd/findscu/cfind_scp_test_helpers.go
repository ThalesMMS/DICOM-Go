package main

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func sendCFindResponse(assoc *ul.Association, pcID byte, rsp dimse.CFindResponse, identifier *object.Object, syntax transfer.Syntax) error {
	if assoc == nil {
		return fmt.Errorf("nil assoc")
	}
	if identifier != nil {
		rsp.CommandDataSetType = dimse.DataSetPresent
	} else {
		rsp.CommandDataSetType = dimse.NoDataSet
	}
	data, err := dimse.EncodeCommandSet(rsp.CommandSet())
	if err != nil {
		return err
	}
	writer := dimse.NewPDataWriter(assoc, pcID, true, 0)
	if _, err := writer.Write(data); err != nil {
		return err
	}
	if err := writer.Finish(); err != nil {
		return err
	}
	if identifier == nil {
		return nil
	}
	return dimse.SendDataSet(assoc, pcID, identifier, syntax)
}
