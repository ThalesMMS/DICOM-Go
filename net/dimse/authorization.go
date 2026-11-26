package dimse

import (
	"context"
	"errors"
)

// ErrStoreAccessDenied reports a refused protected effect without retaining
// policy details. In a retrieve it terminates the operation before the
// denied C-STORE and prevents subsequent sub-operations from starting.
var ErrStoreAccessDenied = errors.New("dicom dimse: access denied")

func checkStoreAccess(ctx context.Context, check func(context.Context) error) (err error) {
	if check == nil {
		return nil
	}
	defer func() {
		if recover() != nil {
			err = ErrStoreAccessDenied
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	checkErr := check(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if checkErr != nil {
		return ErrStoreAccessDenied
	}
	return nil
}
