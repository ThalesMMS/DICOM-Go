package dimse

const (
	// StatusCMoveCancel is the final C-MOVE cancel status.
	StatusCMoveCancel uint16 = 0xFE00
	// StatusCMoveSubOperationsCompleteOneOrMoreFailures reports that retrieve
	// sub-operations completed with one or more failed or warning operations.
	StatusCMoveSubOperationsCompleteOneOrMoreFailures uint16 = 0xB000
	// StatusCMoveMoveDestinationUnknown reports an unsupported Move Destination AE.
	StatusCMoveMoveDestinationUnknown uint16 = 0xA801
	// StatusCMoveIdentifierDoesNotMatchSOPClass reports an Identifier that
	// does not fit the C-MOVE Information Model, such as a missing or unknown
	// Query/Retrieve Level (PS3.4 C.4.2.1.4).
	StatusCMoveIdentifierDoesNotMatchSOPClass uint16 = 0xA900
	// StatusCMoveUnableToProcess is a common C-MOVE processing failure status.
	StatusCMoveUnableToProcess uint16 = 0xC000

	// StatusCGetCancel is the final C-GET cancel status.
	StatusCGetCancel uint16 = 0xFE00
	// StatusCGetSubOperationsCompleteOneOrMoreFailures reports that retrieve
	// sub-operations completed with one or more failed or warning operations.
	StatusCGetSubOperationsCompleteOneOrMoreFailures uint16 = 0xB000
	// StatusCGetIdentifierDoesNotMatchSOPClass reports an invalid Identifier
	// for the C-GET information model or negotiated retrieval method
	// (PS3.4 C.4.3.1.4).
	StatusCGetIdentifierDoesNotMatchSOPClass uint16 = 0xA900
	// StatusCGetUnableToProcess is a common C-GET processing failure status.
	StatusCGetUnableToProcess uint16 = 0xC000
)
