package ul

// DefaultMaxSendPDU is the default cap on outgoing P-DATA-TF PDUs (their
// variable field, as Maximum Length does): DCMTK also sends up to 128 KiB
// when the peer accepts more.
const DefaultMaxSendPDU uint32 = 128 << 10

// minSendPDU is the smallest P-DATA-TF variable field still carrying data:
// one PDV item header and one value byte.
const minSendPDU = PDVHeaderSize + 1

// resolveMaxSendPDU resolves a MaxSendPDU option: zero is DefaultMaxSendPDU.
func resolveMaxSendPDU(configured uint32) uint32 {
	if configured == 0 {
		return DefaultMaxSendPDU
	}
	return configured
}

// SendPDULimit is the largest P-DATA-TF variable field this side sends: the
// peer's Maximum Length (unlimited when it declared 0) capped by the local
// MaxSendPDU option, and never below the room for one PDV header and one
// value byte.
func (a *Association) SendPDULimit() uint32 {
	if a == nil {
		return DefaultMaxPDU
	}
	limit := a.PeerMaxPDU
	if limit == 0 {
		limit = DefaultMaxPDU
	}
	localCap := a.maxSendPDU
	if localCap == 0 {
		localCap = DefaultMaxSendPDU
	}
	if limit > localCap {
		limit = localCap
	}
	if limit < minSendPDU {
		limit = minSendPDU
	}
	return limit
}
