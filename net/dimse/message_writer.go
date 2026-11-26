package dimse

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/telemetry"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// sendMessageDataSet preserves command-only messages and encodes a typed
// dataset directly into the bounded message fragmenter.
func sendMessageDataSet(ctx context.Context, assoc *ul.Association, pcID byte, command []core.Element, dataset *object.Object, syntax transfer.Syntax) error {
	if dataset == nil {
		return SendCommandSetWithContext(ctx, assoc, pcID, command)
	}
	if dataset.Len() == 0 && !syntax.Deflated {
		// Preserve the existing typed-empty dataset contract. It uses the
		// legacy separate streams (including the existing empty final PDV),
		// rather than introducing an empty PDV in a packed envelope. Such
		// empty streams are not newly qualified as PS3.8 E.1 sender behavior.
		if err := SendCommandSetWithContext(ctx, assoc, pcID, command); err != nil {
			return err
		}
		return SendDataSetWithContext(ctx, assoc, pcID, dataset, syntax)
	}
	return sendMessageWithContext(ctx, assoc, pcID, command, syntax, func(w io.Writer) error {
		return object.WriteDataSet(w, dataset, syntax)
	})
}

// sendMessageWithContext sends a complete command/dataset pair. The caller
// retains the existing operation/message gate; acquiring it again here would
// deadlock AsyncSession. PS3.8 E.1 permits the last command PDV and the first
// dataset PDV in one P-DATA-TF, with their types and last flags kept separate.
// The producer is synchronous and must cooperate with caller cancellation.
func sendMessageWithContext(ctx context.Context, assoc *ul.Association, pcID byte, elements []core.Element, syntax transfer.Syntax, produce func(io.Writer) error) error {
	if assoc == nil {
		return fmt.Errorf("dicom dimse: nil association")
	}
	if produce == nil {
		return fmt.Errorf("dicom dimse: nil dataset producer")
	}
	if ctx == nil {
		ctx = assoc.Context
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	timer := startElapsedTimer(time.Now)
	data, err := EncodeCommandSet(elements)
	if err != nil {
		return err
	}
	command := NewPDataWriterWithContext(commandWriteContext(ctx), assoc, pcID, true, peerMaxPDUWithHeader(assoc))
	defer command.discard()
	if _, err := command.Write(data); err != nil {
		return err
	}
	observed := func() {
		observeCommand(assoc, telemetry.Outbound, pcID, object.FromElements(elements, nil), timer.elapsed())
	}
	writer := NewPDataWriterWithContext(dataSetWriteContext(ctx), assoc, pcID, false, peerMaxPDUWithHeader(assoc))
	defer writer.discard()
	// Maximum Length counts the PDV list, excluding the outer six-byte header.
	// Reserve two PDV headers and at least two (even) dataset bytes.
	if int(assoc.SendPDULimit())-2*int(ul.PDVHeaderSize)-len(command.buf) >= 2 {
		writer.commandPrefix, command.buf = command.buf, nil
		writer.commandSent = observed
		writer.firstContext = mixedMessageWriteContext(ctx)
	} else {
		if err := command.Finish(); err != nil {
			return err
		}
		observed()
	}
	if err := produce(writer); err != nil {
		return fmt.Errorf("dicom dimse: send dataset: %w", err)
	}
	// Do not silently change the command's dataset-present bit or emit an
	// empty PDV. Odd native/raw bytes would violate PS3.8 E.1; only Deflated
	// retains the existing PS3.5 A.5 trailing NULL rule.
	if writer.written == 0 {
		return fmt.Errorf("dicom dimse: dataset producer emitted no bytes")
	}
	if !syntax.Deflated && writer.written%2 != 0 {
		return fmt.Errorf("dicom dimse: dataset producer emitted an odd byte count")
	}
	if err := writer.finishDataSet(syntax); err != nil {
		return fmt.Errorf("dicom dimse: finish dataset P-DATA: %w", err)
	}
	return nil
}

func mixedMessageWriteContext(ctx context.Context) context.Context {
	controls := scpControlsFromContext(ctx)
	timeout := controls.CommandProgressTimeout
	if data := controls.DataSetProgressTimeout; data > 0 && (timeout <= 0 || data < timeout) {
		timeout = data
	}
	if timeout > 0 {
		return ul.WithWriteProgressTimeout(ctx, timeout)
	}
	return ctx
}
