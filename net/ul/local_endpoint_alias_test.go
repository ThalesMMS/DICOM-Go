package ul

import "github.com/ThalesMMS/dicom-go/internal/sourceport"

// The source port retry lives in internal/sourceport, shared with the other
// clients of this module (#2011). These names keep the tests of the built-in
// dial (#2007) on the policy the dial uses.
type localEndpointError = sourceport.Kind

const (
	notLocalEndpoint         = sourceport.NotLocalEndpoint
	localAddressInUse        = sourceport.AddressInUse
	localAddressNotAvailable = sourceport.AddressNotAvailable
	localNoBuffers           = sourceport.NoBuffers
)

var (
	sourcePortRetryDelays        = sourceport.RetryDelays
	classifyLocalEndpointError   = sourceport.Classify
	retryableSourceEndpointError = sourceport.Retryable
	validRemoteAddress           = sourceport.ValidRemoteAddress
)
